// Package spendguard is a Cloud Run function that stops the lab's spend when
// the monthly budget runs out.
//
// Google Cloud budgets only send alerts; they never stop anything. This function
// listens to the budget's Pub/Sub notifications and, once the cost reaches the
// budget (times THRESHOLD), scales the GKE node pool to zero. Nothing is deleted,
// so `make resume` brings the cluster back.
package spendguard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	container "cloud.google.com/go/container/apiv1"
	"github.com/GoogleCloudPlatform/functions-framework-go/functions"
	cloudevents "github.com/cloudevents/sdk-go/v2"
)

const (
	// The function timeout is 540 s (the maximum for event-driven functions).
	// Stop waiting well before that, so the result is logged.
	actionTimeout = 7 * time.Minute
	pollInterval  = 5 * time.Second
	retryInitial  = 5 * time.Second
	retryMax      = time.Minute
)

func init() {
	// `go test` builds this package too; the tests build their own handlers.
	if testing.Testing() {
		return
	}
	logger := newLogger(os.Stdout)
	h, err := newHandler(os.Getenv, logger, newClusterManager)
	if err != nil {
		logger.Error("spend guard config is invalid; refusing to start", "error", err.Error())
		// Crash on purpose: the deploy fails, instead of a guard that looks
		// healthy but can never act.
		panic(err)
	}
	logger.Info("spend guard ready", h.cfg.logAttrs()...)
	functions.CloudEvent("ScaleToZero", h.handle)
}

// newClusterManager is the real GKE client. It is built on the first
// notification that crosses the threshold; the usual below-threshold message
// never needs it.
func newClusterManager(ctx context.Context) (clusterManager, error) {
	c, err := container.NewClusterManagerClient(ctx)
	if err != nil {
		return nil, err
	}
	return c, nil
}

type handler struct {
	cfg       Config
	log       *slog.Logger
	newClient func(context.Context) (clusterManager, error)
	now       func() time.Time

	timeout  time.Duration
	poll     time.Duration
	retryMin time.Duration
	retryMax time.Duration

	mu     sync.Mutex
	client clusterManager
}

func newHandler(getenv func(string) string, logger *slog.Logger, newClient func(context.Context) (clusterManager, error)) (*handler, error) {
	cfg, err := loadConfig(getenv)
	if err != nil {
		return nil, err
	}
	return &handler{
		cfg:       cfg,
		log:       logger,
		newClient: newClient,
		now:       time.Now,
		timeout:   actionTimeout,
		poll:      pollInterval,
		retryMin:  retryInitial,
		retryMax:  retryMax,
	}, nil
}

// handle processes one budget notification. It returns an error only when a
// redelivery could help (a transient API failure). Bad, stale and
// below-threshold messages and permanent failures return nil: budget messages
// arrive several times a day, and redelivering a message that can never
// succeed would only loop.
func (h *handler) handle(ctx context.Context, e cloudevents.Event) error {
	log := h.log.With("event_id", e.ID())

	b, err := parseNotification(e)
	if err != nil {
		log.ErrorContext(ctx, "ignoring message: not a valid budget notification",
			"error", err.Error(), "outcome", outcomeIgnored)
		return nil
	}
	log = log.With(b.logAttrs()...)

	if reason := b.staleReason(h.now(), h.cfg.MaxMessageAge); reason != "" {
		log.WarnContext(ctx, "ignoring stale budget notification: "+reason,
			"over_threshold", b.reached(h.cfg.Threshold), "outcome", outcomeStale)
		return nil
	}

	if b.Currency != expectedCurrency {
		log.ErrorContext(ctx, "ignoring budget notification: currency is not "+expectedCurrency,
			"outcome", outcomeIgnored)
		return nil
	}

	log = log.With("threshold", h.cfg.Threshold, "dry_run", h.cfg.DryRun, "node_pool", h.cfg.nodePoolName())
	if !b.reached(h.cfg.Threshold) {
		log.InfoContext(ctx, "budget below threshold; nothing to do", "outcome", outcomeBelowThreshold)
		return nil
	}
	log.WarnContext(ctx, "budget threshold reached")

	api, err := h.clusterClient(ctx)
	if err != nil {
		log.ErrorContext(ctx, "spend guard could not create the GKE client",
			"error", err.Error(), "outcome", outcomeFailed, "retryable", true)
		return fmt.Errorf("create GKE client: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	g := &guard{api: api, cfg: h.cfg, log: log, poll: h.poll, retryMin: h.retryMin, retryMax: h.retryMax}
	out, err := g.scaleToZero(ctx)
	if err != nil {
		retry := retryable(err)
		log.ErrorContext(ctx, "spend guard failed to scale the node pool to zero",
			"error", err.Error(), "outcome", outcomeFailed, "retryable", retry)
		if retry {
			return err
		}
		return nil
	}

	switch out {
	case outcomeDryRun:
		// Logged by the guard with the planned steps.
	case outcomeResizeRunning:
		log.WarnContext(ctx, "resize to zero started and still running; GKE finishes it without the function",
			"outcome", out)
	default:
		log.WarnContext(ctx, "spend guard scaled the node pool to zero", "outcome", out)
	}
	return nil
}

// clusterClient returns the shared GKE client, creating it on first use. A
// failed attempt is not cached, so the next message tries again.
func (h *handler) clusterClient(ctx context.Context) (clusterManager, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.client != nil {
		return h.client, nil
	}
	// The client outlives this request, so it must not stop when the request does.
	c, err := h.newClient(context.WithoutCancel(ctx))
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, errors.New("no client returned")
	}
	h.client = c
	return c, nil
}
