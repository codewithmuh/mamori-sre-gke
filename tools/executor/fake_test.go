package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const testConfigYAML = `
slack:
  channel: C0EXEC0001
  approvers: [U0APPROVER, U0SECOND]
proposalTTL: 15m
restart:
  deployments: [currencyservice, frontend]
scale:
  cartservice: {min: 1, max: 2}
  frontend: {min: 1, max: 3}
removeFault:
  kinds: [virtualservice, podchaos, deployment]
`

func testConfig(t *testing.T, enabled bool) *Config {
	t.Helper()
	cfg, err := parseConfig(map[string]string{"enabled": fmt.Sprint(enabled), "config.yaml": testConfigYAML})
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	return cfg
}

// fakeConfig is a configSource whose answer a test can change between steps.
type fakeConfig struct {
	mu    sync.Mutex
	cfg   *Config
	err   error
	calls int
	// onCall runs before each Get returns, e.g. to flip the kill switch at the nth read.
	onCall func(n int, f *fakeConfig)
}

func (f *fakeConfig) Get(context.Context) (*Config, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.onCall != nil {
		f.onCall(f.calls, f)
	}
	if f.err != nil {
		return nil, f.err
	}
	cp := *f.cfg
	return &cp, nil
}

func (f *fakeConfig) setEnabled(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfg.Enabled = on
}

// fakeNotifier records what would go to Slack.
type fakeNotifier struct {
	mu         sync.Mutex
	posts      []Proposal
	updates    []Proposal
	replies    []string
	ephemerals []string
	postErr    error
	updateErr  error
	replyErr   error
	ephErr     error
}

const fakeTS = "1763020800.000100"

func (n *fakeNotifier) Post(_ context.Context, channel string, p Proposal) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.postErr != nil {
		return "", n.postErr
	}
	p.SlackChannel = channel
	n.posts = append(n.posts, p)
	return fakeTS, nil
}

func (n *fakeNotifier) Update(_ context.Context, p Proposal) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.updates = append(n.updates, p)
	return n.updateErr
}

func (n *fakeNotifier) Reply(_ context.Context, _ Proposal, text string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.replies = append(n.replies, text)
	return n.replyErr
}

func (n *fakeNotifier) Ephemeral(_ context.Context, _, _, text string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ephemerals = append(n.ephemerals, text)
	return n.ephErr
}

// fakeCluster records actions instead of calling Kubernetes.
type fakeCluster struct {
	mu         sync.Mutex
	checkErr   error
	execErrs   []error // returned by Execute, one per call, before execErr
	execErr    error
	checks     int
	executed   []Action
	onExecute  func()
	execResult string
}

func (c *fakeCluster) Check(context.Context, Action) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checks++
	return c.checkErr
}

func (c *fakeCluster) Execute(_ context.Context, a Action) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.onExecute != nil {
		c.onExecute()
	}
	if len(c.execErrs) > 0 {
		err := c.execErrs[0]
		c.execErrs = c.execErrs[1:]
		if err != nil {
			return "", err
		}
	}
	if c.execErr != nil {
		return "", c.execErr
	}
	c.executed = append(c.executed, a)
	if c.execResult != "" {
		return c.execResult, nil
	}
	return "done: " + a.String(), nil
}

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// logBuf collects JSON log lines safely across goroutines.
type logBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuf) lines(t *testing.T) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(l.buf.Bytes()))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("log line is not JSON: %q", sc.Text())
		}
		out = append(out, m)
	}
	return out
}

// events lists the audit events in order, as "event:reason".
func (l *logBuf) events(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, m := range l.lines(t) {
		ev, _ := m["event"].(string)
		switch ev {
		case eventReceived, eventApproved, eventRejected, eventExecuted, eventBlocked, eventFailed, eventApprovalDenied:
			reason, _ := m["reason"].(string)
			out = append(out, ev+":"+reason)
		}
	}
	return out
}

type harness struct {
	svc      *Service
	cfg      *fakeConfig
	notifier *fakeNotifier
	cluster  *fakeCluster
	client   *fake.Clientset
	store    *configMapStore
	clock    *clock
	logs     *logBuf
	reg      *prometheus.Registry
}

func stateConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: stateMapName, Namespace: "mamori-agent"}}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		cfg:      &fakeConfig{cfg: testConfig(t, true)},
		notifier: &fakeNotifier{},
		cluster:  &fakeCluster{},
		client:   fake.NewClientset(stateConfigMap()),
		clock:    &clock{t: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)},
		logs:     &logBuf{},
		reg:      prometheus.NewRegistry(),
	}
	h.store = &configMapStore{client: h.client, namespace: "mamori-agent", now: h.clock.now}
	seq := 0
	h.svc = &Service{
		config:   h.cfg,
		store:    h.store,
		cluster:  h.cluster,
		notifier: h.notifier,
		log:      newLogger(h.logs),
		metrics:  newMetrics(h.reg),
		now:      h.clock.now,
		newID: func() string {
			seq++
			return fmt.Sprintf("p-test-%d", seq)
		},
	}
	if err := h.svc.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	return h
}

func restartRequest(incident string) proposalRequest {
	return proposalRequest{
		IncidentID:      incident,
		InvestigationID: "inv-" + incident,
		Action:          Action{Type: actionRestart, Namespace: targetNamespace, Target: "deployment/currencyservice", Params: map[string]any{}},
		Rationale:       "currencyservice pods are stuck; a restart should clear it",
	}
}

func (h *harness) propose(t *testing.T, req proposalRequest) Proposal {
	t.Helper()
	p, err := h.svc.Propose(context.Background(), req)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	return p
}

func (h *harness) click(p Proposal, user, actionID string) {
	h.svc.HandleInteraction(context.Background(), Interaction{
		UserID: user, ChannelID: "C0EXEC0001", MessageTS: fakeTS, ActionID: actionID, ProposalID: p.ID,
		Shown: blockTexts(proposalBlocks(p)), // what the executor posted, unchanged
	})
}

func (h *harness) status(t *testing.T, id string) Proposal {
	t.Helper()
	if _, err := h.store.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	p, ok := h.store.Get(id)
	if !ok {
		t.Fatalf("proposal %s not in state", id)
	}
	return p
}

func asRefusal(t *testing.T, err error) *refusal {
	t.Helper()
	var r *refusal
	if !errors.As(err, &r) {
		t.Fatalf("want a refusal, got %v", err)
	}
	return r
}
