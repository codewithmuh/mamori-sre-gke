// Command executor runs the incident agent's proposed actions after a human approves them
// in Slack. The agent holds no write credentials (decision D9 in PLAN.md): it can only
// propose. This program has its own narrow RBAC, an allow-list with bounds, one action per
// incident, an audit log and a kill switch. See README.md and docs/executor.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// settings come from the environment (deploy/executor/deployment.yaml).
type settings struct {
	namespace     string   // POD_NAMESPACE: where executor-config and executor-state live
	httpAddr      string   // HTTP_ADDR, default :8080 (the API)
	opsAddr       string   // OPS_ADDR, default :9090 (metrics and probes)
	authMode      string   // AUTH_MODE: tokenreview (default) or none (local runs only)
	authAudience  string   // AUTH_AUDIENCE, default executor
	authAllowed   []string // AUTH_ALLOWED_USERS, comma-separated
	slackAppToken string   // SLACK_APP_TOKEN (xapp-...), Socket Mode
	slackBotToken string   // SLACK_BOT_TOKEN (xoxb-...), chat:write
	slackAPIURL   string   // SLACK_API_URL, default Slack's own; another workspace host or a local fake
	kubeconfig    string   // KUBECONFIG, for local runs outside the cluster
}

func loadSettings(getenv func(string) string) (settings, error) {
	get := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}
	s := settings{
		namespace:     get("POD_NAMESPACE", "mamori-agent"),
		httpAddr:      get("HTTP_ADDR", ":8080"),
		opsAddr:       get("OPS_ADDR", ":9090"),
		authMode:      get("AUTH_MODE", "tokenreview"),
		authAudience:  get("AUTH_AUDIENCE", "executor"),
		slackAppToken: get("SLACK_APP_TOKEN", ""),
		slackBotToken: get("SLACK_BOT_TOKEN", ""),
		slackAPIURL:   get("SLACK_API_URL", slack.APIURL),
		kubeconfig:    get("KUBECONFIG", ""),
	}
	for _, u := range strings.Split(get("AUTH_ALLOWED_USERS", "system:serviceaccount:mamori-agent:agent"), ",") {
		if u = strings.TrimSpace(u); u != "" {
			s.authAllowed = append(s.authAllowed, u)
		}
	}

	var errs []error
	if !dns1123Label.MatchString(s.namespace) {
		errs = append(errs, fmt.Errorf("POD_NAMESPACE=%q is not a namespace name", s.namespace))
	}
	switch s.authMode {
	case "tokenreview":
		if len(s.authAllowed) == 0 {
			errs = append(errs, errors.New("AUTH_ALLOWED_USERS is empty"))
		}
	case "none":
	default:
		errs = append(errs, fmt.Errorf("AUTH_MODE=%q must be tokenreview or none", s.authMode))
	}
	if (s.slackAppToken == "") != (s.slackBotToken == "") {
		errs = append(errs, errors.New("set both SLACK_APP_TOKEN and SLACK_BOT_TOKEN, or neither"))
	}
	if s.slackAppToken != "" && !strings.HasPrefix(s.slackAppToken, "xapp-") {
		errs = append(errs, errors.New("SLACK_APP_TOKEN must be an app-level token (xapp-...)"))
	}
	if s.slackBotToken != "" && !strings.HasPrefix(s.slackBotToken, "xoxb-") {
		errs = append(errs, errors.New("SLACK_BOT_TOKEN must be a bot token (xoxb-...)"))
	}
	if u, err := url.Parse(s.slackAPIURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || !strings.HasSuffix(u.Path, "/") {
		errs = append(errs, fmt.Errorf("SLACK_API_URL=%q must be an http(s) URL ending in /", s.slackAPIURL))
	}
	return s, errors.Join(errs...)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	os.Exit(run(ctx, os.Getenv, os.Stdout))
}

// run wires the real clients and serves until ctx ends. It returns the exit code.
func run(ctx context.Context, getenv func(string) string, out io.Writer) int {
	log := newLogger(out)
	s, err := loadSettings(getenv)
	if err != nil {
		log.Error("invalid settings", "error", err.Error())
		return 2
	}
	restCfg, err := kubeConfig(s.kubeconfig)
	if err != nil {
		log.Error("no Kubernetes config", "error", err.Error())
		return 1
	}
	restCfg.UserAgent = "mamori-executor"
	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		log.Error("Kubernetes client", "error", err.Error())
		return 1
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		log.Error("Kubernetes dynamic client", "error", err.Error())
		return 1
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	m := newMetrics(reg)

	var auth authenticator = newTokenReviewAuth(client, s.authAudience, s.authAllowed, time.Now)
	if s.authMode == "none" {
		log.Warn("AUTH_MODE=none: any caller that can reach the port may propose actions; use only for local runs")
		auth = noAuth{}
	}

	svc := &Service{
		config:  &configMapSource{client: client, namespace: s.namespace},
		store:   &configMapStore{client: client, namespace: s.namespace, now: time.Now},
		cluster: &kubeCluster{client: client, dyn: dyn, now: time.Now},
		log:     log,
		metrics: m,
		now:     time.Now,
		newID:   func() string { return newProposalID(time.Now()) },
	}
	live := &health{}
	var listener *socketListener
	if s.slackBotToken != "" {
		api := slack.New(s.slackBotToken, slack.OptionAppLevelToken(s.slackAppToken), slack.OptionAPIURL(s.slackAPIURL))
		svc.notifier = &slackNotifier{api: api}
		// Our interactions are small; a cap keeps a hostile frame from using up memory.
		listener = &socketListener{client: socketmode.New(api, socketmode.OptionReadLimit(1<<20)), svc: svc, metrics: m, log: log, health: live}
	} else {
		log.Warn("Slack tokens are not set: proposals are refused (reason kill-switch) because nobody could approve them")
	}

	var ready atomic.Bool
	return serve(ctx, log, svc, &api{svc: svc, auth: auth, log: log}, listener, promhttp.HandlerFor(reg, promhttp.HandlerOpts{}), s, &ready, live)
}

func kubeConfig(path string) (*rest.Config, error) {
	if path != "" {
		return clientcmd.BuildConfigFromFlags("", path)
	}
	return rest.InClusterConfig()
}

// serve runs the servers and loops until ctx ends, then shuts down in order.
func serve(ctx context.Context, log *slog.Logger, svc *Service, a *api, listener *socketListener,
	metricsHandler http.Handler, s settings, ready *atomic.Bool, live *health) int {
	apiSrv := &http.Server{Addr: s.httpAddr, Handler: a.routes(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	opsSrv := &http.Server{Addr: s.opsAddr, Handler: opsHandler(metricsHandler, ready, live), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}

	errc := make(chan error, 2)
	for _, srv := range []*http.Server{opsSrv, apiSrv} {
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("listen on %s: %w", srv.Addr, err)
			}
		}()
	}

	// Load the state (and settle anything a crash left half done) before serving proposals.
	go func() {
		for delay := time.Second; ; delay = min(delay*2, 30*time.Second) {
			err := svc.Recover(ctx)
			if err == nil {
				ready.Store(true)
				log.InfoContext(ctx, "executor ready", "event", "started", "namespace", s.namespace, "targetNamespace", targetNamespace)
				return
			}
			log.ErrorContext(ctx, "cannot load the state ConfigMap yet; not ready", "error", err.Error(), "retry_in", delay.String())
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}
	}()

	if listener != nil {
		go listener.Run(ctx)
	}
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if ready.Load() {
					svc.ExpireDue(ctx)
				}
			}
		}
	}()

	code := 0
	select {
	case <-ctx.Done():
	case err := <-errc:
		log.Error("server failed", "error", err.Error())
		code = 1
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	_ = apiSrv.Shutdown(shutdownCtx)
	svc.Wait(shutdownCtx) // let a running action finish and record its outcome
	_ = opsSrv.Shutdown(shutdownCtx)
	return code
}
