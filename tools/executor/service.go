package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// Notifier is the approval channel (Slack). Tests use a fake.
type Notifier interface {
	// Post sends a new proposal with Approve and Reject buttons and returns the message ts.
	Post(ctx context.Context, channel string, p Proposal) (string, error)
	// Update rewrites the proposal's message to show its current status.
	Update(ctx context.Context, p Proposal) error
	// Reply posts in the proposal message's thread.
	Reply(ctx context.Context, p Proposal, text string) error
	// Ephemeral shows a message to one user only.
	Ephemeral(ctx context.Context, channel, user, text string) error
}

// Interaction is one button click, reduced to what the executor checks.
type Interaction struct {
	UserID     string
	ChannelID  string
	MessageTS  string
	ActionID   string
	ProposalID string
}

// Button action ids on the proposal message.
const (
	actionIDApprove = "mamori_executor_approve"
	actionIDReject  = "mamori_executor_reject"
	actorExecutor   = "executor"
	actorAgent      = "agent"
	executeTimeout  = 10 * time.Second
	// interactionTimeout bounds one click from start to recorded outcome. On shutdown serve
	// waits up to shutdownTimeout for it, inside the pod's 30 s termination grace period.
	interactionTimeout = 20 * time.Second
	shutdownTimeout    = 25 * time.Second
)

// Service holds the rules: every path into a write goes through approve and execute.
type Service struct {
	config   configSource
	store    stateStore
	cluster  cluster
	notifier Notifier // nil when Slack is not configured: nothing can be approved
	log      *slog.Logger
	metrics  *metrics
	now      func() time.Time
	newID    func() string

	wg sync.WaitGroup // interactions in flight, waited for on shutdown
}

func newProposalID(now time.Time) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error since Go 1.24
	return "p-" + now.UTC().Format("20060102t150405") + "-" + hex.EncodeToString(b)
}

// audit writes one audit line. Every line carries the proposal's identity, action and
// status, so a single line answers "what, for which incident, and what happened".
func (s *Service) audit(ctx context.Context, level slog.Level, event, actor, msg string, p Proposal, extra ...any) {
	attrs := []any{
		"event", event,
		"proposalId", p.ID,
		"incidentId", p.IncidentID,
		"investigationId", p.InvestigationID,
		"action", p.Action,
		"status", string(p.Status),
	}
	if actor != "" {
		attrs = append(attrs, "actor", actor)
	}
	if p.Reason != "" {
		attrs = append(attrs, "reason", p.Reason)
	}
	if p.Error != "" {
		attrs = append(attrs, "error", p.Error)
	}
	s.log.Log(ctx, level, msg, append(attrs, extra...)...)
	s.metrics.event(event, p.Action.Type, p.Reason)
}

// Propose handles POST /v1/proposals after the request's shape was checked. It returns the
// accepted (or, for a repeated request, the existing) proposal, or a *refusal.
func (s *Service) Propose(ctx context.Context, req proposalRequest) (Proposal, error) {
	now := s.now()
	p := Proposal{
		ID:              s.newID(),
		Status:          statusPending,
		IncidentID:      req.IncidentID,
		InvestigationID: req.InvestigationID,
		Action:          req.Action,
		Rationale:       req.Rationale,
		CreatedAt:       now,
	}
	s.audit(ctx, slog.LevelInfo, eventReceived, actorAgent, "proposal received: "+describe(p.Action), p, "rationale", p.Rationale)
	if r := validateRequest(req); r != nil {
		return s.refused(ctx, p, r)
	}

	out, err := s.propose(ctx, p)
	var r *refusal
	if errors.As(err, &r) {
		return s.refused(ctx, p, r)
	}
	if err != nil {
		s.log.ErrorContext(ctx, "could not handle proposal", "event", "proposal_error", "proposalId", p.ID, "error", err.Error())
		return p, err
	}
	return out, nil
}

func (s *Service) refused(ctx context.Context, p Proposal, r *refusal) (Proposal, error) {
	p.Status, p.Reason, p.Error = statusBlocked, r.reason, r.msg
	s.audit(ctx, slog.LevelWarn, eventBlocked, actorExecutor, "proposal refused: "+r.msg, p)
	return p, r
}

func (s *Service) propose(ctx context.Context, p Proposal) (Proposal, error) {
	// Kill switch first: when the executor is off, nothing else matters.
	cfg, err := s.config.Get(ctx)
	if err != nil {
		return p, refuse(reasonKillSwitch, "executor configuration is unreadable, so the executor acts as switched off: %v", err)
	}
	s.metrics.setEnabled(cfg.Enabled)
	if !cfg.Enabled {
		return p, refuse(reasonKillSwitch, "the executor is switched off (%s enabled is not \"true\")", configMapName)
	}
	if err := cfg.approvalPath(); err != nil {
		return p, refuse(reasonKillSwitch, "%v", err)
	}
	if s.notifier == nil {
		return p, refuse(reasonKillSwitch, "Slack is not configured (no tokens), so nobody could approve")
	}
	if r := checkPolicy(cfg, p.Action); r != nil {
		return p, r
	}
	if err := s.cluster.Check(ctx, p.Action); err != nil {
		return p, err
	}

	p.ExpiresAt = p.CreatedAt.Add(cfg.ttl)
	var existing *Proposal
	_, err = s.store.Update(ctx, func(st *State) error {
		existing = nil
		if ia, ok := st.Incidents[p.IncidentID]; ok {
			return refuse(reasonAlreadyActed, "incident %s already had an action (proposal %s)", p.IncidentID, ia.ProposalID)
		}
		for _, q := range st.Proposals {
			if q.IncidentID != p.IncidentID || !q.Status.open() {
				continue
			}
			// The agent retries once on a timeout; the same request again gets the same answer.
			if q.InvestigationID == p.InvestigationID && sameAction(q.Action, p.Action) {
				cp := *q
				existing = &cp
				return nil
			}
			return refuse(reasonAlreadyActed, "proposal %s for incident %s is still %s; one action per incident", q.ID, p.IncidentID, q.Status)
		}
		cp := p
		st.Proposals[p.ID] = &cp
		return nil
	})
	if err != nil {
		return p, err
	}
	if existing != nil {
		s.log.InfoContext(ctx, "same proposal again; returning the open one", "event", "proposal_duplicate",
			"proposalId", p.ID, "existingProposalId", existing.ID)
		return *existing, nil
	}

	ts, err := s.notifier.Post(ctx, cfg.Slack.Channel, p)
	if err == nil {
		_, err = s.store.Update(ctx, func(st *State) error {
			cur, ok := st.Proposals[p.ID]
			if !ok {
				return errNotOpen
			}
			cur.SlackChannel, cur.SlackTS = cfg.Slack.Channel, ts
			p = *cur
			return nil
		})
	}
	if err != nil {
		// Without a message nobody can approve it: close it so the incident stays free. The
		// caller writes the one "blocked" audit line.
		msg := fmt.Sprintf("could not post the proposal to Slack: %v", err)
		now := s.now()
		if _, uerr := s.store.Update(ctx, func(st *State) error {
			if cur, ok := st.Proposals[p.ID]; ok && cur.Status == statusPending {
				cur.Status, cur.Reason, cur.Error = statusBlocked, reasonApprovalUnavailable, msg
				cur.DecidedAt, cur.DecidedBy = &now, actorExecutor
			}
			return nil
		}); uerr != nil {
			s.log.ErrorContext(ctx, "could not record the blocked proposal", "proposalId", p.ID, "error", uerr.Error())
		}
		s.refreshPending()
		return p, refuse(reasonApprovalUnavailable, "%s", msg)
	}
	s.refreshPending()
	return p, nil
}

// HandleInteraction processes one Approve or Reject click. Slack has been acknowledged
// already; the outcome goes back as a message update, a thread reply or an ephemeral note.
func (s *Service) HandleInteraction(ctx context.Context, in Interaction) {
	if in.ActionID != actionIDApprove && in.ActionID != actionIDReject {
		return
	}
	deny := func(reason, text string, p Proposal) {
		s.log.WarnContext(ctx, "approval click refused: "+text, "event", eventApprovalDenied,
			"proposalId", in.ProposalID, "incidentId", p.IncidentID, "actor", in.UserID, "reason", reason,
			"slackChannel", in.ChannelID, "slackTs", in.MessageTS)
		s.metrics.event(eventApprovalDenied, p.Action.Type, reason)
		s.ephemeral(ctx, in, text)
	}

	cfg, err := s.config.Get(ctx)
	if err != nil {
		deny("config-unreadable", "The executor cannot read its configuration, so nothing was done.", Proposal{})
		return
	}
	if !cfg.isApprover(in.UserID) {
		deny("not-approver", "You are not on the executor's approver list, so your click was ignored.", Proposal{})
		return
	}
	p, ok := s.store.Get(in.ProposalID)
	if !ok {
		deny("unknown-proposal", "This proposal is unknown to the executor.", Proposal{})
		return
	}
	// The click must come from the exact message that showed this proposal, in the
	// configured channel: a copied button or another channel does not count.
	if in.ChannelID != cfg.Slack.Channel || in.ChannelID != p.SlackChannel || in.MessageTS != p.SlackTS {
		deny("message-mismatch", "This button does not belong to the proposal's message in the approval channel.", p)
		return
	}
	if p.Status != statusPending {
		s.ephemeral(ctx, in, fmt.Sprintf("Proposal %s is already %s.", p.ID, p.Status))
		return
	}
	if !s.now().Before(p.ExpiresAt) {
		s.expire(ctx, p)
		s.ephemeral(ctx, in, fmt.Sprintf("Proposal %s expired at %s; nothing was done.", p.ID, p.ExpiresAt.UTC().Format(time.RFC3339)))
		return
	}

	if in.ActionID == actionIDReject {
		s.close(ctx, p.ID, statusRejected, "", "", in.UserID, false)
		return
	}
	s.approve(ctx, p, cfg, in.UserID)
}

func (s *Service) ephemeral(ctx context.Context, in Interaction, text string) {
	if s.notifier == nil {
		return
	}
	if err := s.notifier.Ephemeral(ctx, in.ChannelID, in.UserID, text); err != nil {
		s.log.WarnContext(ctx, "could not send ephemeral Slack message", "error", err.Error())
	}
}

// approve re-checks everything against the configuration and cluster of this moment, claims
// the incident, and runs the action.
func (s *Service) approve(ctx context.Context, p Proposal, cfg *Config, user string) {
	if !cfg.Enabled {
		s.close(ctx, p.ID, statusBlocked, reasonKillSwitch, "the executor was switched off before approval", user, false)
		return
	}
	if r := checkPolicy(cfg, p.Action); r != nil {
		s.close(ctx, p.ID, statusBlocked, r.reason, "no longer allowed: "+r.msg, user, false)
		return
	}
	if err := s.cluster.Check(ctx, p.Action); err != nil {
		var r *refusal
		if !errors.As(err, &r) {
			// A passing API problem: leave it pending so the approver can click again.
			s.log.WarnContext(ctx, "could not check the cluster before approval", "proposalId", p.ID, "error", err.Error())
			s.notifyReply(ctx, p, "The executor could not check the cluster just now, so nothing was done. The proposal is still open: try again.")
			return
		}
		s.close(ctx, p.ID, statusBlocked, r.reason, r.msg, user, false)
		return
	}

	now := s.now()
	var approved Proposal
	_, err := s.store.Update(ctx, func(st *State) error {
		cur, ok := st.Proposals[p.ID]
		if !ok || cur.Status != statusPending {
			return errNotOpen
		}
		if ia, acted := st.Incidents[cur.IncidentID]; acted {
			return refuse(reasonAlreadyActed, "incident %s already had an action (proposal %s)", cur.IncidentID, ia.ProposalID)
		}
		cur.Status, cur.DecidedAt, cur.DecidedBy = statusApproved, &now, user
		st.Incidents[cur.IncidentID] = IncidentAction{ProposalID: cur.ID, At: now}
		approved = *cur
		return nil
	})
	var r *refusal
	switch {
	case errors.As(err, &r):
		s.close(ctx, p.ID, statusBlocked, r.reason, r.msg, user, false)
		return
	case errors.Is(err, errNotOpen):
		return
	case err != nil:
		s.log.ErrorContext(ctx, "could not record the approval; nothing was done", "proposalId", p.ID, "error", err.Error())
		s.notifyReply(ctx, p, "The executor could not record this approval, so nothing was done. Try again.")
		return
	}
	s.audit(ctx, slog.LevelInfo, eventApproved, user, "proposal approved: "+describe(approved.Action), approved)
	s.notifyUpdate(ctx, approved)
	s.execute(ctx, approved)
}

// execute runs an approved action. The kill switch is read once more right before the write.
func (s *Service) execute(ctx context.Context, p Proposal) {
	cfg, err := s.config.Get(ctx)
	switch {
	case err != nil:
		s.close(ctx, p.ID, statusBlocked, reasonKillSwitch, fmt.Sprintf("configuration unreadable right before execution: %v", err), actorExecutor, true)
		return
	case !cfg.Enabled:
		s.close(ctx, p.ID, statusBlocked, reasonKillSwitch, "the executor was switched off right before execution", actorExecutor, true)
		return
	}

	start := s.now()
	execCtx, cancel := context.WithTimeout(ctx, executeTimeout)
	result, err := s.cluster.Execute(execCtx, p.Action)
	cancel()
	elapsed := s.now().Sub(start)
	if err != nil {
		var r *refusal
		if errors.As(err, &r) || apierrors.IsForbidden(err) {
			// A guard (code, RBAC or admission policy) stopped it before anything changed.
			reason, msg := blockReason(err)
			s.metrics.observe(p.Action.Type, "blocked", elapsed)
			s.close(ctx, p.ID, statusBlocked, reason, msg, actorExecutor, true)
			return
		}
		s.metrics.observe(p.Action.Type, "failed", elapsed)
		s.closeWith(ctx, p.ID, statusFailed, reasonExecError, err.Error(), actorExecutor, false, "", "durationMs", elapsed.Milliseconds())
		return
	}
	s.metrics.observe(p.Action.Type, "executed", elapsed)
	s.closeWith(ctx, p.ID, statusExecuted, "", "", actorExecutor, false, result, "durationMs", elapsed.Milliseconds(), "result", result)
}

// blockReason turns a check error into a reason and message for a blocked proposal.
func blockReason(err error) (string, string) {
	var r *refusal
	if errors.As(err, &r) {
		return r.reason, r.msg
	}
	if apierrors.IsForbidden(err) {
		return reasonNotAllowed, "denied by Kubernetes (RBAC or admission policy): " + err.Error()
	}
	return reasonInternal, "could not check the cluster: " + err.Error()
}

func (s *Service) expire(ctx context.Context, p Proposal) {
	s.close(ctx, p.ID, statusExpired, reasonExpired, fmt.Sprintf("nobody decided within %s", p.ExpiresAt.Sub(p.CreatedAt)), actorExecutor, false)
}

func (s *Service) close(ctx context.Context, id string, status Status, reason, msg, actor string, release bool) {
	s.closeWith(ctx, id, status, reason, msg, actor, release, "")
}

// closeWith moves a proposal to a final status, writes the audit line, and tells Slack.
// release frees the incident again, for guards that stopped an approved action before it
// changed anything.
func (s *Service) closeWith(ctx context.Context, id string, status Status, reason, msg, actor string, release bool, result string, extra ...any) {
	now := s.now()
	var closed Proposal
	_, err := s.store.Update(ctx, func(st *State) error {
		cur, ok := st.Proposals[id]
		if !ok || !cur.Status.open() {
			return errNotOpen
		}
		if cur.Status == statusPending {
			cur.DecidedAt, cur.DecidedBy = &now, actor
		}
		if status == statusExecuted || status == statusFailed {
			cur.ExecutedAt = &now
		}
		cur.Status, cur.Reason, cur.Error, cur.Result = status, reason, msg, result
		if release {
			if ia, ok := st.Incidents[cur.IncidentID]; ok && ia.ProposalID == cur.ID {
				delete(st.Incidents, cur.IncidentID)
			}
		}
		closed = *cur
		return nil
	})
	if errors.Is(err, errNotOpen) {
		return
	}
	if err != nil {
		s.log.ErrorContext(ctx, "could not record the outcome", "proposalId", id, "status", string(status), "error", err.Error())
		return
	}

	event, level, text := eventFor(closed)
	s.audit(ctx, level, event, actor, text, closed, extra...)
	s.refreshPending()
	s.notifyUpdate(ctx, closed)
	s.notifyReply(ctx, closed, statusText(closed))
}

// eventFor maps a final status to its audit event. An expiry is logged as "rejected" with
// reason "expired": nobody approved it, and the contract has no separate event for it.
func eventFor(p Proposal) (string, slog.Level, string) {
	switch p.Status {
	case statusExecuted:
		return eventExecuted, slog.LevelWarn, "action executed: " + p.Result
	case statusFailed:
		return eventFailed, slog.LevelError, "action failed: " + p.Error
	case statusBlocked:
		return eventBlocked, slog.LevelWarn, "proposal blocked: " + p.Error
	case statusExpired:
		return eventRejected, slog.LevelInfo, "proposal expired: " + p.Error
	default:
		return eventRejected, slog.LevelInfo, "proposal rejected by " + p.DecidedBy
	}
}

func (s *Service) notifyUpdate(ctx context.Context, p Proposal) {
	if s.notifier == nil || p.SlackTS == "" {
		return
	}
	if err := s.notifier.Update(ctx, p); err != nil {
		s.log.WarnContext(ctx, "could not update the Slack message", "proposalId", p.ID, "error", err.Error())
	}
}

func (s *Service) notifyReply(ctx context.Context, p Proposal, text string) {
	if s.notifier == nil || p.SlackTS == "" {
		return
	}
	if err := s.notifier.Reply(ctx, p, text); err != nil {
		s.log.WarnContext(ctx, "could not reply in the Slack thread", "proposalId", p.ID, "error", err.Error())
	}
}

// ExpireDue closes pending proposals whose time is up. It runs every few seconds.
func (s *Service) ExpireDue(ctx context.Context) {
	now := s.now()
	for _, p := range s.store.Open() {
		if p.Status == statusPending && !now.Before(p.ExpiresAt) {
			s.expire(ctx, p)
		}
	}
	s.refreshPending()
	if cfg, err := s.config.Get(ctx); err == nil {
		s.metrics.setEnabled(cfg.Enabled)
	} else {
		s.metrics.setEnabled(false)
	}
}

// Recover runs once at start. An approved proposal found in the state means the process
// stopped while running it: the outcome is unknown, so it is marked failed and the
// incident stays claimed. A human checks the cluster.
func (s *Service) Recover(ctx context.Context) error {
	if _, err := s.store.Load(ctx); err != nil {
		return err
	}
	for _, p := range s.store.Open() {
		if p.Status == statusApproved {
			s.close(ctx, p.ID, statusFailed, reasonInterrupted,
				"the executor restarted while this action was running; check the cluster by hand", actorExecutor, false)
		}
	}
	s.refreshPending()
	return nil
}

func (s *Service) refreshPending() {
	n := 0
	for _, p := range s.store.Open() {
		if p.Status == statusPending {
			n++
		}
	}
	s.metrics.setPending(n)
}

// Get returns a proposal for GET /v1/proposals/{id}, reloading once if it is not cached.
func (s *Service) Get(ctx context.Context, id string) (Proposal, bool) {
	if p, ok := s.store.Get(id); ok {
		return p, true
	}
	if _, err := s.store.Load(ctx); err != nil {
		return Proposal{}, false
	}
	return s.store.Get(id)
}

// Go runs fn in the background and tracks it for Wait.
func (s *Service) Go(fn func()) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		fn()
	}()
}

// Wait blocks until background work finishes or ctx ends.
func (s *Service) Wait(ctx context.Context) {
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
