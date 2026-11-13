package spendguard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"cloud.google.com/go/container/apiv1/containerpb"
	"github.com/googleapis/gax-go/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// clusterManager is the part of the GKE API the guard uses.
// *container.ClusterManagerClient implements it; tests use a fake.
type clusterManager interface {
	GetNodePool(ctx context.Context, req *containerpb.GetNodePoolRequest, opts ...gax.CallOption) (*containerpb.NodePool, error)
	SetNodePoolAutoscaling(ctx context.Context, req *containerpb.SetNodePoolAutoscalingRequest, opts ...gax.CallOption) (*containerpb.Operation, error)
	SetNodePoolSize(ctx context.Context, req *containerpb.SetNodePoolSizeRequest, opts ...gax.CallOption) (*containerpb.Operation, error)
	GetOperation(ctx context.Context, req *containerpb.GetOperationRequest, opts ...gax.CallOption) (*containerpb.Operation, error)
}

type guard struct {
	api      clusterManager
	cfg      Config
	log      *slog.Logger
	poll     time.Duration // between operation status checks
	retryMin time.Duration // first wait when another operation blocks ours
	retryMax time.Duration
}

// scaleToZero turns node autoscaling off (otherwise the autoscaler could add
// nodes back), then sets the node count to 0. Both steps are safe to repeat:
// autoscaling that is already off is skipped, and resizing a pool to the size
// it already has changes nothing.
func (g *guard) scaleToZero(ctx context.Context) (string, error) {
	name := g.cfg.nodePoolName()

	pool, err := g.api.GetNodePool(ctx, &containerpb.GetNodePoolRequest{Name: name})
	if err != nil {
		return outcomeFailed, fmt.Errorf("get node pool: %w", err)
	}
	autoscaling := pool.GetAutoscaling().GetEnabled()

	if g.cfg.DryRun {
		var steps []string
		if autoscaling {
			steps = append(steps, "turn node autoscaling off")
		}
		steps = append(steps, "set node count to 0")
		g.log.WarnContext(ctx, "dry run: the spend guard would scale the node pool to zero",
			"planned_steps", steps, "outcome", outcomeDryRun)
		return outcomeDryRun, nil
	}

	if autoscaling {
		g.log.InfoContext(ctx, "turning node autoscaling off")
		op, err := g.start(ctx, "turn autoscaling off", func() (*containerpb.Operation, error) {
			return g.api.SetNodePoolAutoscaling(ctx, &containerpb.SetNodePoolAutoscalingRequest{
				Name:        name,
				Autoscaling: &containerpb.NodePoolAutoscaling{Enabled: false},
			})
		})
		if err != nil {
			return outcomeFailed, err
		}
		// The resize can only start once this operation is done.
		if err := g.wait(ctx, op); err != nil {
			return outcomeFailed, fmt.Errorf("turn autoscaling off: %w", err)
		}
	}

	g.log.InfoContext(ctx, "setting node count to 0")
	op, err := g.start(ctx, "set node count to 0", func() (*containerpb.Operation, error) {
		return g.api.SetNodePoolSize(ctx, &containerpb.SetNodePoolSizeRequest{Name: name, NodeCount: 0})
	})
	if err != nil {
		return outcomeFailed, err
	}
	if err := g.wait(ctx, op); err != nil {
		// GKE respects PodDisruptionBudgets for up to an hour while draining,
		// so the resize can outlast the function. It is already running and
		// finishes on its own.
		if errors.Is(err, context.DeadlineExceeded) {
			return outcomeResizeRunning, nil
		}
		return outcomeFailed, fmt.Errorf("set node count to 0: %w", err)
	}
	return outcomeScaledToZero, nil
}

// start calls a node pool mutation. GKE allows one operation per node pool
// (or one cluster-wide) at a time, so while another runs (an upgrade, a repair,
// an earlier resize) it retries with backoff until ctx ends.
func (g *guard) start(ctx context.Context, step string, call func() (*containerpb.Operation, error)) (*containerpb.Operation, error) {
	delay := g.retryMin
	for attempt := 1; ; attempt++ {
		op, err := call()
		if err == nil {
			if op == nil {
				return nil, fmt.Errorf("%s: GKE returned no operation", step)
			}
			return op, nil
		}
		if !blockedByOperation(err) {
			return nil, fmt.Errorf("%s: %w", step, err)
		}
		g.log.WarnContext(ctx, "another cluster operation is running; will retry",
			"step", step, "attempt", attempt, "retry_in", delay.String(), "error", err.Error())
		if serr := sleep(ctx, delay); serr != nil {
			return nil, fmt.Errorf("%s: still blocked after %d attempts: %w", step, attempt, errors.Join(serr, err))
		}
		delay = min(delay*2, g.retryMax)
	}
}

// wait polls the operation until it is done or ctx ends.
func (g *guard) wait(ctx context.Context, op *containerpb.Operation) error {
	opName := op.GetName()
	for {
		if op.GetStatus() == containerpb.Operation_DONE {
			if e := op.GetError(); e != nil && e.GetCode() != int32(codes.OK) {
				return fmt.Errorf("operation %s failed: %s", opName, e.GetMessage())
			}
			// Older API versions report failures only in the deprecated
			// status_message field.
			if msg := op.GetStatusMessage(); msg != "" { //nolint:staticcheck // see comment above
				return fmt.Errorf("operation %s failed: %s", opName, msg)
			}
			return nil
		}
		if err := sleep(ctx, g.poll); err != nil {
			return fmt.Errorf("operation %s still %s: %w", opName, op.GetStatus(), err)
		}
		next, err := g.api.GetOperation(ctx, &containerpb.GetOperationRequest{Name: g.cfg.operationName(opName)})
		switch {
		case err == nil && next != nil:
			op = next
		case err == nil:
			return fmt.Errorf("operation %s: GKE returned no status", opName)
		case ctx.Err() != nil:
			return fmt.Errorf("operation %s: %w", opName, errors.Join(ctx.Err(), err))
		case transient(err):
			g.log.WarnContext(ctx, "could not read operation status; will retry", "operation", opName, "error", err.Error())
		default:
			return fmt.Errorf("read operation %s: %w", opName, err)
		}
	}
}

// blockedByOperation reports errors that mean "another operation is running,
// try later". The Terraform Google provider retries node pool changes on the
// same codes (FAILED_PRECONDITION, RESOURCE_EXHAUSTED).
func blockedByOperation(err error) bool {
	switch status.Code(err) {
	case codes.FailedPrecondition, codes.ResourceExhausted, codes.Aborted:
		return true
	}
	return transient(err)
}

func transient(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Internal:
		return true
	}
	return false
}

// retryable reports whether redelivering the message could help. Wrong IAM
// permissions or a wrong node pool name will not fix themselves; a timeout or
// a busy cluster might.
func retryable(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	switch status.Code(err) {
	case codes.PermissionDenied, codes.Unauthenticated, codes.NotFound, codes.InvalidArgument, codes.Unimplemented:
		return false
	}
	return true
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
