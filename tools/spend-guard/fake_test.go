package spendguard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/googleapis/gax-go/v2"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeGKE is an in-memory node pool that records the calls made to it.
type fakeGKE struct {
	mu sync.Mutex

	autoscaling bool
	size        int32

	getErr      error
	autoErrs    []error // returned by SetNodePoolAutoscaling, one per call, before it succeeds
	sizeErrs    []error // same for SetNodePoolSize
	alwaysBusy  bool    // every mutation fails with "operation running"
	nilOp       bool    // mutations return neither an operation nor an error
	pollsToDone int     // GetOperation calls before an operation is DONE; -1 = never
	opErr       *statuspb.Status
	opStatusMsg string
	getOpErrs   []error // returned by GetOperation, one per call, before it succeeds

	calls []string
	reqs  []any
	ops   map[string]int
	seq   int
}

func newFakeGKE(autoscaling bool) *fakeGKE {
	return &fakeGKE{autoscaling: autoscaling, size: 3, ops: map[string]int{}}
}

func (f *fakeGKE) record(call string, req any) {
	f.calls = append(f.calls, call)
	f.reqs = append(f.reqs, req)
}

func pop(errs *[]error) error {
	if len(*errs) == 0 {
		return nil
	}
	err := (*errs)[0]
	*errs = (*errs)[1:]
	return err
}

func (f *fakeGKE) newOp() *containerpb.Operation {
	f.seq++
	name := fmt.Sprintf("operation-%d", f.seq)
	f.ops[name] = f.pollsToDone
	if f.pollsToDone == 0 {
		return f.doneOp(name)
	}
	return &containerpb.Operation{Name: name, Status: containerpb.Operation_RUNNING}
}

func (f *fakeGKE) doneOp(name string) *containerpb.Operation {
	return &containerpb.Operation{
		Name:          name,
		Status:        containerpb.Operation_DONE,
		Error:         f.opErr,
		StatusMessage: f.opStatusMsg, //nolint:staticcheck // the guard still reads this deprecated field
	}
}

var errBusy = status.Error(codes.FailedPrecondition, "Cluster is running incompatible operation operation-0.")

func (f *fakeGKE) GetNodePool(_ context.Context, req *containerpb.GetNodePoolRequest, _ ...gax.CallOption) (*containerpb.NodePool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("GetNodePool", req)
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &containerpb.NodePool{
		Name:        "spot-pool",
		Autoscaling: &containerpb.NodePoolAutoscaling{Enabled: f.autoscaling, MinNodeCount: 1, MaxNodeCount: 5},
	}, nil
}

func (f *fakeGKE) SetNodePoolAutoscaling(_ context.Context, req *containerpb.SetNodePoolAutoscalingRequest, _ ...gax.CallOption) (*containerpb.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("SetNodePoolAutoscaling", req)
	if f.alwaysBusy {
		return nil, errBusy
	}
	if err := pop(&f.autoErrs); err != nil {
		return nil, err
	}
	if f.nilOp {
		return nil, nil
	}
	f.autoscaling = req.GetAutoscaling().GetEnabled()
	return f.newOp(), nil
}

func (f *fakeGKE) SetNodePoolSize(_ context.Context, req *containerpb.SetNodePoolSizeRequest, _ ...gax.CallOption) (*containerpb.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("SetNodePoolSize", req)
	if f.alwaysBusy {
		return nil, errBusy
	}
	if err := pop(&f.sizeErrs); err != nil {
		return nil, err
	}
	if f.nilOp {
		return nil, nil
	}
	f.size = req.GetNodeCount()
	return f.newOp(), nil
}

func (f *fakeGKE) GetOperation(_ context.Context, req *containerpb.GetOperationRequest, _ ...gax.CallOption) (*containerpb.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("GetOperation", req)
	if err := pop(&f.getOpErrs); err != nil {
		return nil, err
	}
	name := req.GetName()[strings.LastIndex(req.GetName(), "/")+1:]
	left, ok := f.ops[name]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "operation %s not found", name)
	}
	if left < 0 {
		return &containerpb.Operation{Name: name, Status: containerpb.Operation_RUNNING}, nil
	}
	if left > 1 {
		f.ops[name] = left - 1
		return &containerpb.Operation{Name: name, Status: containerpb.Operation_RUNNING}, nil
	}
	f.ops[name] = 0
	return f.doneOp(name), nil
}

func (f *fakeGKE) callNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// mutations lists the calls that change the node pool.
func (f *fakeGKE) mutations() []string {
	var out []string
	for _, c := range f.callNames() {
		if strings.HasPrefix(c, "Set") {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeGKE) state() (autoscaling bool, size int32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.autoscaling, f.size
}

// testEnv is a valid configuration, as Terraform sets it.
func testEnv(overrides map[string]string) func(string) string {
	env := map[string]string{
		"PROJECT_ID":   "mamori-lab-123456",
		"LOCATION":     "asia-northeast1-a",
		"CLUSTER_NAME": "mamori",
		"NODE_POOL":    "spot-pool",
	}
	for k, v := range overrides {
		env[k] = v
	}
	return func(k string) string { return env[k] }
}

const testNodePool = "projects/mamori-lab-123456/locations/asia-northeast1-a/clusters/mamori/nodePools/spot-pool"

func testConfig(t *testing.T, overrides map[string]string) Config {
	t.Helper()
	cfg, err := loadConfig(testEnv(overrides))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	return cfg
}

// logBuffer collects JSON log lines; safe for concurrent writes.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) entries(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	dec := json.NewDecoder(bytes.NewReader(b.buf.Bytes()))
	for {
		var m map[string]any
		if err := dec.Decode(&m); err == io.EOF {
			return out
		} else if err != nil {
			t.Fatalf("log line is not JSON: %v\n%s", err, b.buf.String())
		}
		out = append(out, m)
	}
}

// find returns the first log entry with the given outcome.
func (b *logBuffer) find(t *testing.T, outcome string) map[string]any {
	t.Helper()
	for _, e := range b.entries(t) {
		if e["outcome"] == outcome {
			return e
		}
	}
	t.Fatalf("no log entry with outcome %q in:\n%s", outcome, b.buf.String())
	return nil
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newTestGuard(t *testing.T, api clusterManager, overrides map[string]string) (*guard, *logBuffer) {
	t.Helper()
	logs := &logBuffer{}
	return &guard{
		api:      api,
		cfg:      testConfig(t, overrides),
		log:      newLogger(logs),
		poll:     time.Millisecond,
		retryMin: time.Millisecond,
		retryMax: 4 * time.Millisecond,
	}, logs
}

// budgetEvent wraps a budget notification body the way Eventarc delivers it.
func budgetEvent(t *testing.T, body string) cloudevents.Event {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"message": map[string]any{
			"data":       []byte(body), // encoding/json writes base64, like Pub/Sub
			"attributes": map[string]string{"budgetId": "test-budget", "schemaVersion": "1.0"},
			"messageId":  "42",
		},
		"subscription": "projects/mamori-lab-123456/subscriptions/test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return rawEvent(t, data)
}

func rawEvent(t *testing.T, data []byte) cloudevents.Event {
	t.Helper()
	e := cloudevents.NewEvent()
	e.SetID("event-1")
	e.SetType(pubsubEventType)
	e.SetSource("//pubsub.googleapis.com/projects/mamori-lab-123456/topics/budget-alerts")
	if err := e.SetData(cloudevents.ApplicationJSON, data); err != nil {
		t.Fatal(err)
	}
	return e
}

func fileEvent(t *testing.T, path string) cloudevents.Event {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return rawEvent(t, data)
}

// silentLogger discards logs for tests that don't check them.
func silentLogger() *slog.Logger { return newLogger(io.Discard) }
