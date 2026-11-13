// Command prober runs Online Boutique's critical user journeys (browse, add-to-cart, view-cart,
// checkout) through the Istio ingress gateway every minute and exports the results as Prometheus
// metrics for the journey SLOs. It also runs journeys on demand over HTTP (POST /v1/run) for the
// discovery run and game days. See README.md.
package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	code := run(ctx, os.Getenv, os.Stdout)
	stop()
	os.Exit(code)
}

// run returns the process exit code: 0 after a clean stop, 1 if serving failed, 2 for bad settings.
func run(ctx context.Context, getenv func(string) string, stdout io.Writer) int {
	cfg, err := loadConfig(getenv)
	log := newLogger(stdout, cfg.logLevel)
	if err != nil {
		log.Error("invalid configuration", "error", err.Error())
		return 2
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.listenAddr)
	if err != nil {
		log.Error("cannot listen", "addr", cfg.listenAddr, "error", err.Error())
		return 1
	}
	reg := prometheus.NewRegistry()
	p := newProber(cfg, newMetrics(reg), log)
	if err := p.serve(ctx, ln, reg); err != nil {
		log.Error("server failed", "error", err.Error())
		return 1
	}
	log.Info("prober stopped")
	return 0
}

// serve runs the scheduled probes and the HTTP server until ctx ends, then stops both: in-flight
// probes are cancelled and not recorded, so a rollout never burns error budget.
func (p *prober) serve(ctx context.Context, ln net.Listener, reg *prometheus.Registry) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	srv := &http.Server{
		Handler:           p.routes(reg),
		ReadHeaderTimeout: 5 * time.Second,
		// No ReadTimeout or WriteTimeout: an on-demand call can take minutes, and net/http cancels
		// a request's context when ReadTimeout passes, even after the body has been read.
		IdleTimeout: 2 * time.Minute,
		// Requests share ctx, so stopping cancels on-demand runs too.
		BaseContext: func(net.Listener) context.Context { return ctx },
		ErrorLog:    slog.NewLogLogger(p.log.Handler(), slog.LevelWarn),
	}

	var wg sync.WaitGroup
	p.startSchedules(ctx, &wg)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	p.ready.Store(true)
	p.log.Info("prober started",
		"version", version,
		"addr", ln.Addr().String(),
		"target", p.cfg.targetURL.String(),
		"host", p.cfg.host,
		"interval", p.cfg.interval.String(),
		"step_timeout", p.cfg.stepTimeout.String(),
		"scheduled", p.cfg.scheduled,
	)

	var err error
	select {
	case <-ctx.Done():
	case err = <-serveErr:
	}
	p.stopping.Store(true)
	cancel()
	wg.Wait()

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	if shutdownErr := srv.Shutdown(shutdownCtx); err == nil {
		err = shutdownErr
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}
