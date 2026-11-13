package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func newStore(client *fake.Clientset, now time.Time) *configMapStore {
	return &configMapStore{client: client, namespace: "mamori-agent", now: func() time.Time { return now }}
}

func TestStoreRetriesOnConflict(t *testing.T) {
	client := fake.NewClientset(stateConfigMap())
	conflicts := 2
	client.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts > 0 {
			conflicts--
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, stateMapName, errors.New("changed"))
		}
		return false, nil, nil
	})
	s := newStore(client, time.Now())
	calls := 0
	_, err := s.Update(context.Background(), func(st *State) error {
		calls++
		st.Proposals["p-1"] = &Proposal{ID: "p-1", Status: statusPending, CreatedAt: time.Now()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("fn ran %d times, want 3 (two conflicts)", calls)
	}
	if _, ok := s.Get("p-1"); !ok {
		t.Error("cache not updated")
	}
}

func TestStoreGivesUpAfterConflicts(t *testing.T) {
	client := fake.NewClientset(stateConfigMap())
	client.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, stateMapName, errors.New("changed"))
	})
	_, err := newStore(client, time.Now()).Update(context.Background(), func(*State) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("err = %v", err)
	}
}

func TestStoreErrors(t *testing.T) {
	ctx := context.Background()
	// No ConfigMap: the executor cannot create it (no create right), so it reports it.
	empty := newStore(fake.NewClientset(), time.Now())
	if _, err := empty.Load(ctx); err == nil {
		t.Error("Load without the ConfigMap must fail")
	}
	if _, err := empty.Update(ctx, func(*State) error { return nil }); err == nil {
		t.Error("Update without the ConfigMap must fail")
	}
	if _, ok := empty.Get("x"); ok || empty.Open() != nil {
		t.Error("empty cache")
	}

	// Garbage in the ConfigMap is reported, not overwritten.
	bad := stateConfigMap()
	bad.Data = map[string]string{stateKey: "{not json"}
	s := newStore(fake.NewClientset(bad), time.Now())
	if _, err := s.Load(ctx); err == nil {
		t.Error("Load of garbage must fail")
	}
	if _, err := s.Update(ctx, func(*State) error { return nil }); err == nil {
		t.Error("Update of garbage must fail")
	}

	// fn's error is returned unchanged and nothing is written.
	s = newStore(fake.NewClientset(stateConfigMap()), time.Now())
	sentinel := errors.New("stop")
	if _, err := s.Update(ctx, func(*State) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Errorf("err = %v", err)
	}

	// A write error other than a conflict.
	client := fake.NewClientset(stateConfigMap())
	client.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, stateMapName, errors.New("no"))
	})
	if _, err := newStore(client, time.Now()).Update(ctx, func(*State) error { return nil }); err == nil {
		t.Error("forbidden write must fail")
	}

	// Too big: refused before it reaches the API server.
	s = newStore(fake.NewClientset(stateConfigMap()), time.Now())
	_, err := s.Update(ctx, func(st *State) error {
		st.Proposals["big"] = &Proposal{ID: "big", Status: statusPending, Rationale: strings.Repeat("x", maxStateBytes)}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("err = %v", err)
	}
}

func TestStateNullMaps(t *testing.T) {
	st, err := decodeState(`{"proposals":null,"incidents":null}`)
	if err != nil || st.Proposals == nil || st.Incidents == nil {
		t.Fatalf("st = %+v, err = %v", st, err)
	}
}

func TestPrune(t *testing.T) {
	now := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	st := newState()
	old := now.Add(-stateRetention - time.Hour)
	st.Proposals["old-final"] = &Proposal{ID: "old-final", Status: statusExecuted, CreatedAt: old}
	st.Proposals["old-open"] = &Proposal{ID: "old-open", Status: statusPending, CreatedAt: old}
	st.Incidents["old"] = IncidentAction{ProposalID: "old-final", At: old}
	st.Incidents["new"] = IncidentAction{ProposalID: "x", At: now}
	for i := range maxFinalProposals + 5 {
		id := fmt.Sprintf("f-%03d", i)
		st.Proposals[id] = &Proposal{ID: id, Status: statusRejected, CreatedAt: now.Add(-time.Duration(maxFinalProposals+5-i) * time.Minute)}
	}
	st.prune(now)

	if _, ok := st.Proposals["old-final"]; ok {
		t.Error("old final proposal kept")
	}
	if _, ok := st.Proposals["old-open"]; !ok {
		t.Error("open proposals are never pruned")
	}
	if _, ok := st.Incidents["old"]; ok {
		t.Error("old incident kept")
	}
	if _, ok := st.Incidents["new"]; !ok {
		t.Error("new incident dropped")
	}
	for i := range 5 {
		if _, ok := st.Proposals[fmt.Sprintf("f-%03d", i)]; ok {
			t.Errorf("oldest final proposal f-%03d kept beyond the cap", i)
		}
	}
	if len(st.Proposals) != maxFinalProposals+1 {
		t.Errorf("kept %d proposals, want %d", len(st.Proposals), maxFinalProposals+1)
	}
}

func TestStoreWritesFieldManagerAndKey(t *testing.T) {
	client := fake.NewClientset(stateConfigMap())
	s := newStore(client, time.Now())
	if _, err := s.Update(context.Background(), func(st *State) error {
		st.Incidents["inc-1"] = IncidentAction{ProposalID: "p-1", At: time.Now()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cm, err := client.Tracker().Get(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, "mamori-agent", stateMapName)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cm.(*corev1.ConfigMap).Data[stateKey], `"inc-1"`) {
		t.Errorf("state not written: %v", cm.(*corev1.ConfigMap).Data)
	}
}
