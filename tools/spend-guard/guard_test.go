package spendguard

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"github.com/googleapis/gax-go/v2"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestScaleToZeroWithAutoscalingOff(t *testing.T) {
	gke := newFakeGKE(false)
	g, _ := newTestGuard(t, gke, nil)

	out, err := g.scaleToZero(context.Background())
	if err != nil || out != outcomeScaledToZero {
		t.Fatalf("scaleToZero() = %q, %v; want %q, nil", out, err, outcomeScaledToZero)
	}
	if got, want := gke.mutations(), []string{"SetNodePoolSize"}; !reflect.DeepEqual(got, want) {
		t.Errorf("mutations = %v, want %v", got, want)
	}
	if _, size := gke.state(); size != 0 {
		t.Errorf("size = %d, want 0", size)
	}
	req := gke.reqs[1].(*containerpb.SetNodePoolSizeRequest)
	if req.GetName() != testNodePool || req.GetNodeCount() != 0 {
		t.Errorf("SetNodePoolSize request = %v", req)
	}
	if gke.reqs[0].(*containerpb.GetNodePoolRequest).GetName() != testNodePool {
		t.Errorf("GetNodePool request = %v", gke.reqs[0])
	}
}

func TestScaleToZeroTurnsAutoscalingOffFirst(t *testing.T) {
	gke := newFakeGKE(true)
	gke.pollsToDone = 2
	g, _ := newTestGuard(t, gke, nil)

	out, err := g.scaleToZero(context.Background())
	if err != nil || out != outcomeScaledToZero {
		t.Fatalf("scaleToZero() = %q, %v", out, err)
	}
	// The resize must wait until the autoscaling operation is done.
	want := []string{
		"GetNodePool",
		"SetNodePoolAutoscaling", "GetOperation", "GetOperation",
		"SetNodePoolSize", "GetOperation", "GetOperation",
	}
	if got := gke.callNames(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v\nwant    %v", got, want)
	}
	autoReq := gke.reqs[1].(*containerpb.SetNodePoolAutoscalingRequest)
	if autoReq.GetName() != testNodePool || autoReq.GetAutoscaling().GetEnabled() {
		t.Errorf("SetNodePoolAutoscaling request = %v", autoReq)
	}
	opReq := gke.reqs[2].(*containerpb.GetOperationRequest)
	if want := "projects/mamori-lab-123456/locations/asia-northeast1-a/operations/operation-1"; opReq.GetName() != want {
		t.Errorf("GetOperation name = %q, want %q", opReq.GetName(), want)
	}
	if autoscaling, size := gke.state(); autoscaling || size != 0 {
		t.Errorf("autoscaling = %v, size = %d; want false, 0", autoscaling, size)
	}
}

func TestScaleToZeroDryRunChangesNothing(t *testing.T) {
	gke := newFakeGKE(true)
	g, logs := newTestGuard(t, gke, map[string]string{"DRY_RUN": "true"})

	out, err := g.scaleToZero(context.Background())
	if err != nil || out != outcomeDryRun {
		t.Fatalf("scaleToZero() = %q, %v; want %q, nil", out, err, outcomeDryRun)
	}
	if m := gke.mutations(); len(m) != 0 {
		t.Errorf("dry run changed the node pool: %v", m)
	}
	entry := logs.find(t, outcomeDryRun)
	steps := fmt.Sprint(entry["planned_steps"])
	if !strings.Contains(steps, "turn node autoscaling off") || !strings.Contains(steps, "set node count to 0") {
		t.Errorf("planned_steps = %s", steps)
	}
	if entry["severity"] != "WARNING" {
		t.Errorf("severity = %v, want WARNING", entry["severity"])
	}
}

func TestScaleToZeroDryRunWithAutoscalingOff(t *testing.T) {
	gke := newFakeGKE(false)
	g, logs := newTestGuard(t, gke, map[string]string{"DRY_RUN": "true"})

	if _, err := g.scaleToZero(context.Background()); err != nil {
		t.Fatal(err)
	}
	steps := fmt.Sprint(logs.find(t, outcomeDryRun)["planned_steps"])
	if strings.Contains(steps, "autoscaling") {
		t.Errorf("planned_steps = %s, want no autoscaling step", steps)
	}
}

func TestScaleToZeroRetriesWhileAnotherOperationRuns(t *testing.T) {
	gke := newFakeGKE(true)
	gke.autoErrs = []error{errBusy}
	gke.sizeErrs = []error{
		errBusy,
		status.Error(codes.ResourceExhausted, "too many concurrent operations"),
		status.Error(codes.Unavailable, "try again"),
	}
	g, logs := newTestGuard(t, gke, nil)

	out, err := g.scaleToZero(context.Background())
	if err != nil || out != outcomeScaledToZero {
		t.Fatalf("scaleToZero() = %q, %v", out, err)
	}
	want := []string{"SetNodePoolAutoscaling", "SetNodePoolAutoscaling", "SetNodePoolSize", "SetNodePoolSize", "SetNodePoolSize", "SetNodePoolSize"}
	if got := gke.mutations(); !reflect.DeepEqual(got, want) {
		t.Errorf("mutations = %v, want %v", got, want)
	}
	if n := strings.Count(logs.String(), "another cluster operation is running"); n != 4 {
		t.Errorf("logged %d retries, want 4", n)
	}
}

func TestRetryBackoffGrowsToMax(t *testing.T) {
	gke := newFakeGKE(false)
	gke.sizeErrs = []error{errBusy, errBusy, errBusy, errBusy}
	g, logs := newTestGuard(t, gke, nil)

	if _, err := g.scaleToZero(context.Background()); err != nil {
		t.Fatal(err)
	}
	var delays []string
	for _, e := range logs.entries(t) {
		if d, ok := e["retry_in"].(string); ok {
			delays = append(delays, d)
		}
	}
	if want := []string{"1ms", "2ms", "4ms", "4ms"}; !reflect.DeepEqual(delays, want) {
		t.Errorf("retry delays = %v, want %v", delays, want)
	}
}

func TestScaleToZeroGivesUpWhenStillBlocked(t *testing.T) {
	gke := newFakeGKE(false)
	gke.alwaysBusy = true
	g, _ := newTestGuard(t, gke, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	out, err := g.scaleToZero(ctx)
	if err == nil || out != outcomeFailed {
		t.Fatalf("scaleToZero() = %q, %v; want failed with an error", out, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) || status.Code(err) != codes.FailedPrecondition {
		t.Errorf("error %v should carry both the deadline and the GKE error", err)
	}
	if !retryable(err) {
		t.Error("a cluster that stays busy should be retried later")
	}
	if _, size := gke.state(); size != 3 {
		t.Errorf("size = %d, want unchanged 3", size)
	}
}

func TestScaleToZeroIsIdempotent(t *testing.T) {
	gke := newFakeGKE(true)
	g, _ := newTestGuard(t, gke, nil)

	for i := range 3 {
		out, err := g.scaleToZero(context.Background())
		if err != nil || out != outcomeScaledToZero {
			t.Fatalf("run %d: scaleToZero() = %q, %v", i+1, out, err)
		}
	}
	// Autoscaling is turned off once; the resize is repeated and harmless.
	want := []string{"SetNodePoolAutoscaling", "SetNodePoolSize", "SetNodePoolSize", "SetNodePoolSize"}
	if got := gke.mutations(); !reflect.DeepEqual(got, want) {
		t.Errorf("mutations = %v, want %v", got, want)
	}
	if autoscaling, size := gke.state(); autoscaling || size != 0 {
		t.Errorf("autoscaling = %v, size = %d; want false, 0", autoscaling, size)
	}
}

func TestScaleToZeroOperationFails(t *testing.T) {
	gke := newFakeGKE(false)
	gke.opErr = &statuspb.Status{Code: int32(codes.Internal), Message: "instance group resize failed"}
	g, _ := newTestGuard(t, gke, nil)

	out, err := g.scaleToZero(context.Background())
	if err == nil || out != outcomeFailed || !strings.Contains(err.Error(), "instance group resize failed") {
		t.Fatalf("scaleToZero() = %q, %v", out, err)
	}
	if !retryable(err) {
		t.Error("a failed resize should be retried")
	}
}

func TestScaleToZeroAutoscalingOperationFails(t *testing.T) {
	gke := newFakeGKE(true)
	gke.pollsToDone = 1
	gke.opStatusMsg = "autoscaler update failed"
	g, _ := newTestGuard(t, gke, nil)

	out, err := g.scaleToZero(context.Background())
	if err == nil || out != outcomeFailed || !strings.Contains(err.Error(), "turn autoscaling off") {
		t.Fatalf("scaleToZero() = %q, %v", out, err)
	}
	if m := gke.mutations(); !reflect.DeepEqual(m, []string{"SetNodePoolAutoscaling"}) {
		t.Errorf("mutations = %v, want no resize after a failed autoscaling change", m)
	}
}

func TestScaleToZeroResizeOutlastsFunction(t *testing.T) {
	gke := newFakeGKE(false)
	gke.pollsToDone = -1 // a drain blocked by a PodDisruptionBudget
	g, _ := newTestGuard(t, gke, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	out, err := g.scaleToZero(ctx)
	if err != nil || out != outcomeResizeRunning {
		t.Fatalf("scaleToZero() = %q, %v; want %q, nil", out, err, outcomeResizeRunning)
	}
}

func TestScaleToZeroAutoscalingOutlastsFunction(t *testing.T) {
	gke := newFakeGKE(true)
	gke.pollsToDone = -1
	g, _ := newTestGuard(t, gke, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	out, err := g.scaleToZero(ctx)
	if err == nil || out != outcomeFailed || !retryable(err) {
		t.Fatalf("scaleToZero() = %q, %v; want a retryable failure", out, err)
	}
}

func TestScaleToZeroPermissionDenied(t *testing.T) {
	gke := newFakeGKE(false)
	gke.getErr = status.Error(codes.PermissionDenied, "container.clusters.get denied")
	g, _ := newTestGuard(t, gke, nil)

	out, err := g.scaleToZero(context.Background())
	if err == nil || out != outcomeFailed {
		t.Fatalf("scaleToZero() = %q, %v", out, err)
	}
	if retryable(err) {
		t.Error("missing permissions will not fix themselves; don't retry")
	}
	if m := gke.mutations(); len(m) != 0 {
		t.Errorf("mutations = %v, want none", m)
	}
}

func TestScaleToZeroPermanentMutationError(t *testing.T) {
	gke := newFakeGKE(false)
	gke.sizeErrs = []error{status.Error(codes.PermissionDenied, "container.clusters.update denied")}
	g, _ := newTestGuard(t, gke, nil)

	out, err := g.scaleToZero(context.Background())
	if err == nil || out != outcomeFailed || retryable(err) {
		t.Fatalf("scaleToZero() = %q, %v; want a permanent failure", out, err)
	}
	if n := len(gke.mutations()); n != 1 {
		t.Errorf("tried %d times, want 1 (no retry on permission errors)", n)
	}
}

func TestScaleToZeroNoOperationReturned(t *testing.T) {
	gke := newFakeGKE(false)
	gke.nilOp = true
	g, _ := newTestGuard(t, gke, nil)

	if out, err := g.scaleToZero(context.Background()); err == nil || out != outcomeFailed {
		t.Fatalf("scaleToZero() = %q, %v; want failure", out, err)
	}
}

func TestScaleToZeroAutoscalingCallFails(t *testing.T) {
	gke := newFakeGKE(true)
	gke.autoErrs = []error{status.Error(codes.InvalidArgument, "bad request")}
	g, _ := newTestGuard(t, gke, nil)

	if out, err := g.scaleToZero(context.Background()); err == nil || out != outcomeFailed {
		t.Fatalf("scaleToZero() = %q, %v; want failure", out, err)
	}
}

func TestWaitRetriesTransientStatusErrors(t *testing.T) {
	gke := newFakeGKE(false)
	gke.pollsToDone = 1
	gke.getOpErrs = []error{status.Error(codes.Unavailable, "blip"), status.Error(codes.Internal, "blip")}
	g, _ := newTestGuard(t, gke, nil)

	if out, err := g.scaleToZero(context.Background()); err != nil || out != outcomeScaledToZero {
		t.Fatalf("scaleToZero() = %q, %v", out, err)
	}
}

func TestWaitStopsOnPermanentStatusError(t *testing.T) {
	gke := newFakeGKE(false)
	gke.pollsToDone = 1
	gke.getOpErrs = []error{status.Error(codes.PermissionDenied, "container.operations.get denied")}
	g, _ := newTestGuard(t, gke, nil)

	out, err := g.scaleToZero(context.Background())
	if err == nil || out != outcomeFailed || !strings.Contains(err.Error(), "read operation") {
		t.Fatalf("scaleToZero() = %q, %v", out, err)
	}
}

// fixedOps answers GetOperation with a fixed result, to test odd API replies.
type fixedOps struct {
	*fakeGKE
	op  *containerpb.Operation
	err error
}

func (f fixedOps) GetOperation(context.Context, *containerpb.GetOperationRequest, ...gax.CallOption) (*containerpb.Operation, error) {
	return f.op, f.err
}

func TestWaitNoStatusReturned(t *testing.T) {
	gke := newFakeGKE(false)
	gke.pollsToDone = 1
	g, _ := newTestGuard(t, fixedOps{fakeGKE: gke}, nil)

	if out, err := g.scaleToZero(context.Background()); err == nil || out != outcomeFailed {
		t.Fatalf("scaleToZero() = %q, %v; want failure", out, err)
	}
}

func TestWaitStatusErrorAfterCancel(t *testing.T) {
	gke := newFakeGKE(false)
	gke.pollsToDone = 1
	ctx, cancel := context.WithCancel(context.Background())
	api := cancelOnGetOperation{fakeGKE: gke, cancel: cancel}
	g, _ := newTestGuard(t, api, nil)

	out, err := g.scaleToZero(ctx)
	if err == nil || out != outcomeFailed || !errors.Is(err, context.Canceled) {
		t.Fatalf("scaleToZero() = %q, %v; want a cancelled failure", out, err)
	}
}

// cancelOnGetOperation cancels the context during the status call, as when
// the function runs out of time mid-request.
type cancelOnGetOperation struct {
	*fakeGKE
	cancel context.CancelFunc
}

func (c cancelOnGetOperation) GetOperation(context.Context, *containerpb.GetOperationRequest, ...gax.CallOption) (*containerpb.Operation, error) {
	c.cancel()
	return nil, status.Error(codes.Canceled, "context canceled")
}

func TestRetryable(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{context.DeadlineExceeded, true},
		{fmt.Errorf("wrapped: %w", context.Canceled), true},
		{status.Error(codes.Unavailable, ""), true},
		{status.Error(codes.FailedPrecondition, ""), true},
		{errors.New("operation failed"), true},
		{status.Error(codes.PermissionDenied, ""), false},
		{fmt.Errorf("get node pool: %w", status.Error(codes.NotFound, "")), false},
		{status.Error(codes.InvalidArgument, ""), false},
		{status.Error(codes.Unauthenticated, ""), false},
	}
	for _, tt := range tests {
		if got := retryable(tt.err); got != tt.want {
			t.Errorf("retryable(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}
