package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

const (
	maxBodyBytes = 64 << 10
	// The agent sends one proposal per investigation, so this is far above normal use.
	proposalInterval = 6 * time.Second
	proposalBurst    = 10
)

func newProposalLimiter() *rate.Limiter {
	return rate.NewLimiter(rate.Every(proposalInterval), proposalBurst)
}

// api serves the contract's two endpoints on :8080. Only the agent can reach this port
// (NetworkPolicy), and POST also needs the agent's token.
type api struct {
	svc     *Service
	auth    authenticator
	log     *slog.Logger
	limiter *rate.Limiter // nil = no limit (tests)
}

func (a *api) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/proposals", a.createProposal)
	mux.HandleFunc("GET /v1/proposals/{id}", a.getProposal)
	return mux
}

type errorBody struct {
	Error  string `json:"error"`
	Reason string `json:"reason"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the client is gone if this fails; nothing to do
}

func writeRefusal(w http.ResponseWriter, r *refusal) {
	writeJSON(w, httpStatus(r.reason), errorBody{Error: r.msg, Reason: r.reason})
}

func (a *api) createProposal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// First, before the body or a TokenReview: a looping agent must not flood Slack, the
	// state or the API server.
	if a.limiter != nil && !a.limiter.Allow() {
		ref := refuse(reasonRateLimited, "too many proposals; at most %d at once and one per %s after that", a.limiter.Burst(), proposalInterval)
		a.svc.audit(ctx, slog.LevelWarn, eventBlocked, "", "proposal refused: "+ref.msg,
			Proposal{ID: a.svc.newID(), Status: statusBlocked, Reason: ref.reason, Error: ref.msg})
		writeRefusal(w, ref)
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		token = "" // no bearer token at all
	}
	caller, err := a.auth.Authenticate(ctx, strings.TrimSpace(token))
	if err != nil {
		if !errors.Is(err, errUnauthenticated) {
			a.log.ErrorContext(ctx, "could not verify the caller", "error", err.Error())
			writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: "could not verify the caller's token", Reason: reasonInternal})
			return
		}
		ref := refuse(reasonUnauthorized, "caller is not allowed: %v", err)
		a.svc.audit(ctx, slog.LevelWarn, eventBlocked, "", "proposal refused: "+ref.msg,
			Proposal{ID: a.svc.newID(), Status: statusBlocked, Reason: ref.reason, Error: ref.msg}, "remoteAddr", r.RemoteAddr)
		writeRefusal(w, ref)
		return
	}

	req, ref := decodeProposal(r)
	if ref != nil {
		a.svc.audit(ctx, slog.LevelWarn, eventBlocked, caller, "proposal refused: "+ref.msg,
			Proposal{ID: a.svc.newID(), Status: statusBlocked, Reason: ref.reason, Error: ref.msg})
		writeRefusal(w, ref)
		return
	}

	p, err := a.svc.Propose(ctx, req)
	var rf *refusal
	switch {
	case errors.As(err, &rf):
		writeRefusal(w, rf)
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "the executor could not handle the proposal; see its log", Reason: reasonInternal})
	default:
		writeJSON(w, http.StatusAccepted, map[string]string{"proposalId": p.ID, "status": string(p.Status)})
	}
}

// decodeProposal reads the body strictly: unknown fields are refused rather than ignored,
// so a field the executor does not understand can never be mistaken for a constraint.
func decodeProposal(r *http.Request) (proposalRequest, *refusal) {
	var req proposalRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return req, refuse(reasonInvalid, "could not read the body: %v", err)
	}
	if len(body) > maxBodyBytes {
		return req, refuse(reasonInvalid, "body is larger than %d bytes", maxBodyBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	dec.UseNumber() // keeps 3 as 3, so replicas can be checked to be a whole number
	if err := dec.Decode(&req); err != nil {
		return req, refuse(reasonInvalid, "body is not a valid proposal: %v", err)
	}
	if dec.More() {
		return req, refuse(reasonInvalid, "body has more than one JSON value")
	}
	if req.Action.Params == nil {
		req.Action.Params = map[string]any{}
	}
	return req, nil
}

func (a *api) getProposal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, ok := a.svc.Get(r.Context(), id)
	if !ok {
		writeJSON(w, http.StatusNotFound, errorBody{Error: fmt.Sprintf("proposal %q not found", id), Reason: reasonNotFound})
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// health is what /healthz (the liveness probe) reports. A part that cannot recover by itself
// marks it failed, and the kubelet restarts the container.
type health struct {
	mu     sync.Mutex
	reason string
}

func (h *health) fail(reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.reason == "" {
		h.reason = reason
	}
}

func (h *health) err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.reason == "" {
		return nil
	}
	return errors.New(h.reason)
}

// opsHandler serves /metrics, /healthz and /readyz on the metrics port, which Managed
// Prometheus and the kubelet reach but the agent does not need.
func opsHandler(metricsHandler http.Handler, ready *atomic.Bool, live *health) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metricsHandler)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if err := live.err(); err != nil {
			http.Error(w, "unhealthy: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			http.Error(w, "state not loaded yet", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ready\n")
	})
	return mux
}
