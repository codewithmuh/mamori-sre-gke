package spendguard

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newTestHandler builds a handler on a fake GKE with fast timings, and counts
// how often it creates a client.
func newTestHandler(t *testing.T, gke clusterManager, env map[string]string) (*handler, *logBuffer, *atomic.Int32) {
	t.Helper()
	logs := &logBuffer{}
	var created atomic.Int32
	h, err := newHandler(testEnv(env), newLogger(logs), func(context.Context) (clusterManager, error) {
		created.Add(1)
		return gke, nil
	})
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	h.timeout = time.Second
	h.poll = time.Millisecond
	h.retryMin = time.Millisecond
	h.retryMax = 2 * time.Millisecond
	return h, logs, &created
}

func TestNewHandlerDefaults(t *testing.T) {
	h, err := newHandler(testEnv(nil), silentLogger(), newClusterManager)
	if err != nil {
		t.Fatal(err)
	}
	if h.timeout != actionTimeout || h.poll != pollInterval || h.retryMin != retryInitial || h.retryMax != retryMax {
		t.Errorf("timings = %v %v %v %v", h.timeout, h.poll, h.retryMin, h.retryMax)
	}
	// The function timeout is 540 s; the guard must stop waiting before it.
	if actionTimeout >= 540*time.Second {
		t.Errorf("actionTimeout %v leaves no time to log the result", actionTimeout)
	}
}

func TestNewHandlerRejectsBadConfig(t *testing.T) {
	_, err := newHandler(testEnv(map[string]string{"THRESHOLD": "100"}), silentLogger(), newClusterManager)
	if err == nil {
		t.Fatal("newHandler accepted THRESHOLD=100")
	}
}

func TestHandleScalesToZeroOverBudget(t *testing.T) {
	gke := newFakeGKE(true)
	h, logs, _ := newTestHandler(t, gke, nil)

	if err := h.handle(context.Background(), fileEvent(t, "testdata/pubsub-event.json")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if autoscaling, size := gke.state(); autoscaling || size != 0 {
		t.Errorf("autoscaling = %v, size = %d; want false, 0", autoscaling, size)
	}
	entry := logs.find(t, outcomeScaledToZero)
	checks := map[string]any{
		"severity":  "WARNING",
		"message":   "spend guard scaled the node pool to zero",
		"event_id":  "event-1",
		"budget":    "mamori-lab",
		"cost":      201.37,
		"currency":  "USD",
		"threshold": 1.0,
		"dry_run":   false,
		"node_pool": testNodePool,
	}
	for k, want := range checks {
		if entry[k] != want {
			t.Errorf("log %s = %v, want %v", k, entry[k], want)
		}
	}
}

func TestHandleBelowThresholdDoesNothing(t *testing.T) {
	gke := newFakeGKE(false)
	h, logs, created := newTestHandler(t, gke, nil)

	if err := h.handle(context.Background(), fileEvent(t, "testdata/pubsub-event-below-threshold.json")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if n := created.Load(); n != 0 {
		t.Errorf("created %d GKE clients, want 0", n)
	}
	if entry := logs.find(t, outcomeBelowThreshold); entry["severity"] != "INFO" {
		t.Errorf("severity = %v, want INFO", entry["severity"])
	}
}

func TestHandleLowerThreshold(t *testing.T) {
	gke := newFakeGKE(false)
	h, _, _ := newTestHandler(t, gke, map[string]string{"THRESHOLD": "0.2"})

	// 41.18 of 200 is 20.6 %.
	if err := h.handle(context.Background(), fileEvent(t, "testdata/pubsub-event-below-threshold.json")); err != nil {
		t.Fatal(err)
	}
	if _, size := gke.state(); size != 0 {
		t.Errorf("size = %d, want 0 with THRESHOLD=0.2", size)
	}
}

func TestHandleIgnoresGarbage(t *testing.T) {
	gke := newFakeGKE(false)
	h, logs, created := newTestHandler(t, gke, nil)

	events := map[string][]byte{
		"not JSON":        []byte("garbage"),
		"partial":         []byte(`{"message": {"data": "eyJjb3N0QW1vdW50IjogMzAwfQ=="}}`), // {"costAmount": 300}
		"empty":           []byte(`{}`),
		"base64 of junk":  []byte(`{"message": {"data": "AQIDBA=="}}`),
		"wrong structure": []byte(`[1, 2]`),
	}
	for name, data := range events {
		// Returning nil acks the message: redelivering it can't help.
		if err := h.handle(context.Background(), rawEvent(t, data)); err != nil {
			t.Errorf("%s: handle returned %v, want nil", name, err)
		}
	}
	if n := created.Load(); n != 0 {
		t.Errorf("created %d GKE clients for invalid messages", n)
	}
	if entry := logs.find(t, outcomeIgnored); entry["severity"] != "ERROR" {
		t.Errorf("severity = %v, want ERROR", entry["severity"])
	}
}

func TestHandleIgnoresOtherCurrency(t *testing.T) {
	gke := newFakeGKE(false)
	h, logs, created := newTestHandler(t, gke, nil)

	e := budgetEvent(t, `{"costAmount": 40000, "budgetAmount": 30000, "currencyCode": "JPY"}`)
	if err := h.handle(context.Background(), e); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if n := created.Load(); n != 0 {
		t.Errorf("created %d GKE clients for a JPY budget", n)
	}
	entry := logs.find(t, outcomeIgnored)
	if entry["currency"] != "JPY" || !strings.Contains(entry["message"].(string), "USD") {
		t.Errorf("log entry = %v", entry)
	}
}

func TestHandleDryRun(t *testing.T) {
	gke := newFakeGKE(true)
	h, logs, _ := newTestHandler(t, gke, map[string]string{"DRY_RUN": "true"})

	if err := h.handle(context.Background(), fileEvent(t, "testdata/pubsub-event.json")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if m := gke.mutations(); len(m) != 0 {
		t.Errorf("dry run changed the node pool: %v", m)
	}
	if entry := logs.find(t, outcomeDryRun); entry["dry_run"] != true {
		t.Errorf("dry_run = %v, want true", entry["dry_run"])
	}
}

func TestHandleClientError(t *testing.T) {
	h, logs, _ := newTestHandler(t, nil, nil)
	h.newClient = func(context.Context) (clusterManager, error) {
		return nil, errors.New("credentials: could not find default credentials")
	}

	err := h.handle(context.Background(), fileEvent(t, "testdata/pubsub-event.json"))
	if err == nil {
		t.Fatal("handle returned nil; a client error should be retried")
	}
	logs.find(t, outcomeFailed)
}

func TestHandleClientNil(t *testing.T) {
	h, _, _ := newTestHandler(t, nil, nil)
	h.newClient = func(context.Context) (clusterManager, error) { return nil, nil }

	if err := h.handle(context.Background(), fileEvent(t, "testdata/pubsub-event.json")); err == nil {
		t.Fatal("handle returned nil without a client")
	}
}

func TestHandleReusesClient(t *testing.T) {
	gke := newFakeGKE(false)
	h, _, created := newTestHandler(t, gke, nil)

	e := fileEvent(t, "testdata/pubsub-event.json")
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if err := h.handle(context.Background(), e); err != nil {
				t.Errorf("handle: %v", err)
			}
		})
	}
	wg.Wait()
	if n := created.Load(); n != 1 {
		t.Errorf("created %d GKE clients, want 1", n)
	}
}

func TestHandleTransientFailureIsRetried(t *testing.T) {
	gke := newFakeGKE(false)
	gke.alwaysBusy = true
	h, logs, _ := newTestHandler(t, gke, nil)
	h.timeout = 20 * time.Millisecond

	err := h.handle(context.Background(), fileEvent(t, "testdata/pubsub-event.json"))
	if err == nil {
		t.Fatal("handle returned nil; Pub/Sub should redeliver while the cluster is busy")
	}
	if entry := logs.find(t, outcomeFailed); entry["retryable"] != true || entry["severity"] != "ERROR" {
		t.Errorf("log entry = %v", entry)
	}
}

func TestHandlePermanentFailureIsNotRetried(t *testing.T) {
	gke := newFakeGKE(false)
	gke.getErr = status.Error(codes.NotFound, "node pool spot-pool not found")
	h, logs, _ := newTestHandler(t, gke, nil)

	if err := h.handle(context.Background(), fileEvent(t, "testdata/pubsub-event.json")); err != nil {
		t.Fatalf("handle returned %v; a missing node pool won't appear on retry", err)
	}
	if entry := logs.find(t, outcomeFailed); entry["retryable"] != false {
		t.Errorf("retryable = %v, want false", entry["retryable"])
	}
}

func TestHandleResizeStillRunning(t *testing.T) {
	gke := newFakeGKE(false)
	gke.pollsToDone = -1
	h, logs, _ := newTestHandler(t, gke, nil)
	h.timeout = 20 * time.Millisecond

	if err := h.handle(context.Background(), fileEvent(t, "testdata/pubsub-event.json")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	logs.find(t, outcomeResizeRunning)
}
