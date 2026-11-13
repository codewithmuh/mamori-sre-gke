package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// State is what must survive a restart: every recent proposal, and which incidents already
// had an action started. It lives in ConfigMap executor-state, key state.json.
type State struct {
	Proposals map[string]*Proposal      `json:"proposals"`
	Incidents map[string]IncidentAction `json:"incidents"`
}

// IncidentAction records that an action started for an incident. An entry is added when a
// proposal is approved and removed only if a guard stops it before anything ran.
type IncidentAction struct {
	ProposalID string    `json:"proposalId"`
	At         time.Time `json:"at"`
}

const (
	stateKey = "state.json"
	// A ConfigMap holds at most 1 MiB. Old final proposals are pruned well before that.
	maxStateBytes      = 768 * 1024
	stateRetention     = 30 * 24 * time.Hour
	maxFinalProposals  = 300
	maxConflictRetries = 5
)

func newState() *State {
	return &State{Proposals: map[string]*Proposal{}, Incidents: map[string]IncidentAction{}}
}

func decodeState(raw string) (*State, error) {
	st := newState()
	if raw == "" {
		return st, nil
	}
	if err := json.Unmarshal([]byte(raw), st); err != nil {
		return nil, fmt.Errorf("decode %s: %w", stateKey, err)
	}
	if st.Proposals == nil {
		st.Proposals = map[string]*Proposal{}
	}
	if st.Incidents == nil {
		st.Incidents = map[string]IncidentAction{}
	}
	return st, nil
}

// prune drops final proposals and incident records older than the retention, then the
// oldest final proposals beyond the cap. Open proposals are never dropped.
func (st *State) prune(now time.Time) {
	cutoff := now.Add(-stateRetention)
	var final []*Proposal
	for id, p := range st.Proposals {
		if p.Status.open() {
			continue
		}
		if p.CreatedAt.Before(cutoff) {
			delete(st.Proposals, id)
			continue
		}
		final = append(final, p)
	}
	if extra := len(final) - maxFinalProposals; extra > 0 {
		slices.SortFunc(final, func(a, b *Proposal) int { return a.CreatedAt.Compare(b.CreatedAt) })
		for _, p := range final[:extra] {
			delete(st.Proposals, p.ID)
		}
	}
	for id, ia := range st.Incidents {
		if ia.At.Before(cutoff) {
			delete(st.Incidents, id)
		}
	}
}

// stateStore keeps State consistent across concurrent decisions and restarts.
type stateStore interface {
	// Load reads the stored state and refreshes the cache.
	Load(ctx context.Context) (*State, error)
	// Update applies fn to the latest state and writes it back atomically. If fn returns an
	// error nothing is written and the error is returned unchanged.
	Update(ctx context.Context, fn func(*State) error) (*State, error)
	// Get returns a copy of a proposal from the cache.
	Get(id string) (Proposal, bool)
	// Open returns copies of all pending and approved proposals from the cache.
	Open() []Proposal
}

// configMapStore writes with the ConfigMap's resourceVersion (optimistic concurrency), so
// two writers can never both claim the same incident. The executor's Role may get and
// update only this one ConfigMap (resourceNames), and it cannot create it: Argo CD does.
type configMapStore struct {
	client    kubernetes.Interface
	namespace string
	now       func() time.Time

	mu    sync.Mutex // serialises writers in this process; the API server does it across processes
	cache *State
}

// Load reads executor-state and refreshes the cache.
func (s *configMapStore) Load(ctx context.Context) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cm, err := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, stateMapName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read ConfigMap %s/%s: %w", s.namespace, stateMapName, err)
	}
	st, err := decodeState(cm.Data[stateKey])
	if err != nil {
		return nil, err
	}
	s.cache = st
	return st, nil
}

// Update applies fn to the latest stored state and writes it back with the read
// resourceVersion, retrying on conflicts.
func (s *configMapStore) Update(ctx context.Context, fn func(*State) error) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var lastErr error
	for range maxConflictRetries {
		cm, err := s.client.CoreV1().ConfigMaps(s.namespace).Get(ctx, stateMapName, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("read ConfigMap %s/%s: %w", s.namespace, stateMapName, err)
		}
		st, err := decodeState(cm.Data[stateKey])
		if err != nil {
			return nil, err
		}
		if err := fn(st); err != nil {
			return nil, err
		}
		st.prune(s.now())
		raw, err := json.Marshal(st)
		if err != nil {
			return nil, fmt.Errorf("encode state: %w", err)
		}
		if len(raw) > maxStateBytes {
			return nil, fmt.Errorf("state is %d bytes, over the %d-byte limit", len(raw), maxStateBytes)
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[stateKey] = string(raw)
		_, err = s.client.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{FieldManager: fieldManager})
		if apierrors.IsConflict(err) {
			lastErr = err
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("write ConfigMap %s/%s: %w", s.namespace, stateMapName, err)
		}
		s.cache = st
		return st, nil
	}
	return nil, fmt.Errorf("write ConfigMap %s/%s: still conflicting after %d tries: %w", s.namespace, stateMapName, maxConflictRetries, lastErr)
}

// Get returns a copy of a cached proposal.
func (s *configMapStore) Get(id string) (Proposal, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == nil {
		return Proposal{}, false
	}
	p, ok := s.cache.Proposals[id]
	if !ok {
		return Proposal{}, false
	}
	return *p, true
}

// Open returns copies of the cached open proposals, oldest first.
func (s *configMapStore) Open() []Proposal {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == nil {
		return nil
	}
	var out []Proposal
	for _, p := range s.cache.Proposals {
		if p.Status.open() {
			out = append(out, *p)
		}
	}
	slices.SortFunc(out, func(a, b Proposal) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out
}

// errNotOpen means the proposal was decided by someone or something else meanwhile.
var errNotOpen = errors.New("proposal is no longer open")
