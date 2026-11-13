package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestApproveAndExecute(t *testing.T) {
	h := newHarness(t)
	p := h.propose(t, restartRequest("inc-1"))
	if p.Status != statusPending || p.SlackTS != fakeTS || p.SlackChannel != "C0EXEC0001" {
		t.Fatalf("proposal = %+v", p)
	}
	if !p.ExpiresAt.Equal(h.clock.now().Add(15 * time.Minute)) {
		t.Errorf("expiresAt = %v", p.ExpiresAt)
	}
	if len(h.notifier.posts) != 1 {
		t.Fatalf("posts = %d", len(h.notifier.posts))
	}
	if got := testutil.ToFloat64(h.svc.metrics.pending); got != 1 {
		t.Errorf("pending gauge = %v", got)
	}

	h.click(p, "U0APPROVER", actionIDApprove)

	got := h.status(t, p.ID)
	if got.Status != statusExecuted || got.DecidedBy != "U0APPROVER" || got.ExecutedAt == nil || got.Result == "" {
		t.Fatalf("after approval: %+v", got)
	}
	if len(h.cluster.executed) != 1 {
		t.Fatalf("executed %d actions", len(h.cluster.executed))
	}
	want := []string{"proposal_received:", "approved:", "executed:"}
	if ev := h.logs.events(t); !slices.Equal(ev, want) {
		t.Errorf("audit events = %v, want %v", ev, want)
	}
	if len(h.notifier.replies) != 1 || !strings.Contains(h.notifier.replies[0], "Executed") {
		t.Errorf("thread replies = %v", h.notifier.replies)
	}
	if testutil.ToFloat64(h.svc.metrics.events.WithLabelValues(eventExecuted, actionRestart, "none")) != 1 {
		t.Error("executed counter not incremented")
	}
	// Every audit line carries the identity fields the evaluation reads.
	for _, m := range h.logs.lines(t) {
		if m["event"] == eventExecuted {
			for _, k := range []string{"proposalId", "incidentId", "investigationId", "action", "status", "actor", "durationMs", "result", "severity", "message", "time"} {
				if _, ok := m[k]; !ok {
					t.Errorf("executed line lacks %q: %v", k, m)
				}
			}
		}
	}
}

func TestOneActionPerIncident(t *testing.T) {
	h := newHarness(t)
	first := h.propose(t, restartRequest("inc-1"))

	// A different proposal for the same incident while the first is open.
	other := restartRequest("inc-1")
	other.Action.Target = "deployment/frontend"
	_, err := h.svc.Propose(context.Background(), other)
	if r := asRefusal(t, err); r.reason != reasonAlreadyActed {
		t.Fatalf("reason = %s", r.reason)
	}

	h.click(first, "U0APPROVER", actionIDApprove)
	if s := h.status(t, first.ID).Status; s != statusExecuted {
		t.Fatalf("first = %s", s)
	}

	// After the action ran, nothing more for this incident, even the identical request.
	_, err = h.svc.Propose(context.Background(), restartRequest("inc-1"))
	if r := asRefusal(t, err); r.reason != reasonAlreadyActed {
		t.Fatalf("reason = %s", r.reason)
	}
	// Another incident is fine.
	h.propose(t, restartRequest("inc-2"))
}

func TestSameRequestTwiceIsIdempotent(t *testing.T) {
	h := newHarness(t)
	a := h.propose(t, restartRequest("inc-1"))
	b := h.propose(t, restartRequest("inc-1"))
	if a.ID != b.ID {
		t.Fatalf("retry got a new proposal %s, want %s", b.ID, a.ID)
	}
	if len(h.notifier.posts) != 1 {
		t.Errorf("posted %d times", len(h.notifier.posts))
	}
}

func TestKillSwitchAtProposal(t *testing.T) {
	h := newHarness(t)
	h.cfg.setEnabled(false)
	_, err := h.svc.Propose(context.Background(), restartRequest("inc-1"))
	if r := asRefusal(t, err); r.reason != reasonKillSwitch {
		t.Fatalf("reason = %s", r.reason)
	}
	if len(h.notifier.posts) != 0 || h.cluster.checks != 0 {
		t.Error("a switched-off executor must not post or check")
	}
	want := []string{"proposal_received:", "blocked:kill-switch"}
	if ev := h.logs.events(t); !slices.Equal(ev, want) {
		t.Errorf("events = %v", ev)
	}
	if testutil.ToFloat64(h.svc.metrics.enabled) != 0 {
		t.Error("enabled gauge should be 0")
	}
}

func TestUnreadableConfigActsAsOff(t *testing.T) {
	h := newHarness(t)
	h.cfg.err = errors.New("ConfigMap not found")
	_, err := h.svc.Propose(context.Background(), restartRequest("inc-1"))
	if r := asRefusal(t, err); r.reason != reasonKillSwitch {
		t.Fatalf("reason = %s", r.reason)
	}
}

func TestNoApprovalPath(t *testing.T) {
	h := newHarness(t)
	h.cfg.cfg.Slack.Approvers = nil
	_, err := h.svc.Propose(context.Background(), restartRequest("inc-1"))
	if r := asRefusal(t, err); r.reason != reasonKillSwitch {
		t.Fatalf("reason = %s", r.reason)
	}

	h = newHarness(t)
	h.svc.notifier = nil
	_, err = h.svc.Propose(context.Background(), restartRequest("inc-1"))
	if r := asRefusal(t, err); r.reason != reasonKillSwitch || !strings.Contains(r.msg, "Slack") {
		t.Fatalf("refusal = %v", r)
	}
}

func TestKillSwitchRightBeforeExecution(t *testing.T) {
	h := newHarness(t)
	p := h.propose(t, restartRequest("inc-1"))
	// Reads: 1 = proposal, 2 = click, 3 = right before execution. Flip at the third.
	h.cfg.onCall = func(n int, f *fakeConfig) {
		if n == 3 {
			f.cfg.Enabled = false
		}
	}
	h.click(p, "U0APPROVER", actionIDApprove)

	got := h.status(t, p.ID)
	if got.Status != statusBlocked || got.Reason != reasonKillSwitch {
		t.Fatalf("got %+v", got)
	}
	if len(h.cluster.executed) != 0 {
		t.Fatal("action ran despite the kill switch")
	}
	want := []string{"proposal_received:", "approved:", "blocked:kill-switch"}
	if ev := h.logs.events(t); !slices.Equal(ev, want) {
		t.Errorf("events = %v", ev)
	}
	// Nothing ran, so the incident is free for a new proposal.
	h.cfg.onCall = nil
	h.cfg.setEnabled(true)
	h.propose(t, restartRequest("inc-1"))
}

func TestKillSwitchAtApproval(t *testing.T) {
	h := newHarness(t)
	p := h.propose(t, restartRequest("inc-1"))
	h.cfg.setEnabled(false)
	h.click(p, "U0APPROVER", actionIDApprove)
	if got := h.status(t, p.ID); got.Status != statusBlocked || got.Reason != reasonKillSwitch {
		t.Fatalf("got %+v", got)
	}
}

func TestConfigChangedBeforeApproval(t *testing.T) {
	h := newHarness(t)
	req := restartRequest("inc-1")
	req.Action = Action{Type: actionScale, Namespace: targetNamespace, Target: "deployment/frontend", Params: map[string]any{"replicas": 3}}
	p := h.propose(t, req)
	h.cfg.cfg.Scale = map[string]Bounds{"frontend": {Min: 1, Max: 2}}
	h.click(p, "U0APPROVER", actionIDApprove)
	if got := h.status(t, p.ID); got.Status != statusBlocked || got.Reason != reasonOutOfBounds {
		t.Fatalf("got %+v", got)
	}
}

func TestApproverAllowList(t *testing.T) {
	h := newHarness(t)
	p := h.propose(t, restartRequest("inc-1"))
	h.click(p, "U0INTRUDER", actionIDApprove)
	if got := h.status(t, p.ID); got.Status != statusPending {
		t.Fatalf("a non-approver changed the status to %s", got.Status)
	}
	if len(h.cluster.executed) != 0 {
		t.Fatal("non-approver click executed an action")
	}
	if len(h.notifier.ephemerals) != 1 {
		t.Errorf("ephemerals = %v", h.notifier.ephemerals)
	}
	if ev := h.logs.events(t); !slices.Contains(ev, "approval_denied:not-approver") {
		t.Errorf("events = %v", ev)
	}
	// The second listed approver may decide.
	h.click(p, "U0SECOND", actionIDApprove)
	if got := h.status(t, p.ID); got.Status != statusExecuted {
		t.Fatalf("got %s", got.Status)
	}
}

func TestClickMustMatchTheMessage(t *testing.T) {
	h := newHarness(t)
	p := h.propose(t, restartRequest("inc-1"))
	for _, in := range []Interaction{
		{UserID: "U0APPROVER", ChannelID: "C0OTHER001", MessageTS: fakeTS, ActionID: actionIDApprove, ProposalID: p.ID},
		{UserID: "U0APPROVER", ChannelID: "C0EXEC0001", MessageTS: "1.2", ActionID: actionIDApprove, ProposalID: p.ID},
		{UserID: "U0APPROVER", ChannelID: "C0EXEC0001", MessageTS: fakeTS, ActionID: actionIDApprove, ProposalID: "p-unknown"},
	} {
		h.svc.HandleInteraction(context.Background(), in)
	}
	h.svc.HandleInteraction(context.Background(), Interaction{UserID: "U0APPROVER", ActionID: "something_else", ProposalID: p.ID})
	if got := h.status(t, p.ID); got.Status != statusPending {
		t.Fatalf("status = %s", got.Status)
	}
	if len(h.notifier.ephemerals) != 3 {
		t.Errorf("ephemerals = %v", h.notifier.ephemerals)
	}
}

func TestReject(t *testing.T) {
	h := newHarness(t)
	p := h.propose(t, restartRequest("inc-1"))
	h.click(p, "U0APPROVER", actionIDReject)
	got := h.status(t, p.ID)
	if got.Status != statusRejected || got.DecidedBy != "U0APPROVER" {
		t.Fatalf("got %+v", got)
	}
	// A late approve does nothing.
	h.click(p, "U0APPROVER", actionIDApprove)
	if len(h.cluster.executed) != 0 {
		t.Fatal("rejected proposal ran")
	}
	// Rejection frees the incident for another proposal.
	other := restartRequest("inc-1")
	other.Action.Target = "deployment/frontend"
	h.propose(t, other)
	want := []string{"proposal_received:", "rejected:", "proposal_received:"}
	if ev := h.logs.events(t); !slices.Equal(ev, want) {
		t.Errorf("events = %v", ev)
	}
}

func TestExpiry(t *testing.T) {
	h := newHarness(t)
	p := h.propose(t, restartRequest("inc-1"))
	h.clock.add(14 * time.Minute)
	h.svc.ExpireDue(context.Background())
	if got := h.status(t, p.ID); got.Status != statusPending {
		t.Fatalf("expired too early: %s", got.Status)
	}
	h.clock.add(time.Minute)
	h.svc.ExpireDue(context.Background())
	got := h.status(t, p.ID)
	if got.Status != statusExpired || got.Reason != reasonExpired || got.DecidedBy != actorExecutor {
		t.Fatalf("got %+v", got)
	}
	h.click(p, "U0APPROVER", actionIDApprove)
	if len(h.cluster.executed) != 0 {
		t.Fatal("expired proposal ran")
	}
	if ev := h.logs.events(t); !slices.Contains(ev, "rejected:expired") {
		t.Errorf("events = %v", ev)
	}
	if testutil.ToFloat64(h.svc.metrics.pending) != 0 {
		t.Error("pending gauge not reset")
	}
}

func TestClickAfterExpiryButBeforeSweep(t *testing.T) {
	h := newHarness(t)
	p := h.propose(t, restartRequest("inc-1"))
	h.clock.add(16 * time.Minute)
	h.click(p, "U0APPROVER", actionIDApprove)
	if got := h.status(t, p.ID); got.Status != statusExpired {
		t.Fatalf("got %s", got.Status)
	}
	if len(h.cluster.executed) != 0 {
		t.Fatal("expired proposal ran")
	}
}

func TestBlockedAtExecutionReleasesIncident(t *testing.T) {
	h := newHarness(t)
	p := h.propose(t, restartRequest("inc-1"))
	h.cluster.execErr = refuse(reasonNotAllowed, "label mamori.dev/fault is missing")
	h.click(p, "U0APPROVER", actionIDApprove)
	if got := h.status(t, p.ID); got.Status != statusBlocked || got.Reason != reasonNotAllowed {
		t.Fatalf("got %+v", got)
	}
	h.cluster.execErr = nil
	h.propose(t, restartRequest("inc-1"))
}

func TestForbiddenAtExecutionIsBlocked(t *testing.T) {
	h := newHarness(t)
	p := h.propose(t, restartRequest("inc-1"))
	h.cluster.execErr = apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "currencyservice", errors.New("denied by ValidatingAdmissionPolicy"))
	h.click(p, "U0APPROVER", actionIDApprove)
	got := h.status(t, p.ID)
	if got.Status != statusBlocked || got.Reason != reasonNotAllowed || !strings.Contains(got.Error, "admission") {
		t.Fatalf("got %+v", got)
	}
}

func TestFailedExecutionKeepsIncident(t *testing.T) {
	h := newHarness(t)
	p := h.propose(t, restartRequest("inc-1"))
	h.cluster.execErr = errors.New("connection reset")
	h.click(p, "U0APPROVER", actionIDApprove)
	got := h.status(t, p.ID)
	if got.Status != statusFailed || got.Reason != reasonExecError || got.ExecutedAt == nil {
		t.Fatalf("got %+v", got)
	}
	h.cluster.execErr = nil
	_, err := h.svc.Propose(context.Background(), restartRequest("inc-1"))
	if r := asRefusal(t, err); r.reason != reasonAlreadyActed {
		t.Fatalf("reason = %s", r.reason)
	}
	if ev := h.logs.events(t); !slices.Contains(ev, "failed:execution-error") {
		t.Errorf("events = %v", ev)
	}
}

func TestClusterCheckAtProposal(t *testing.T) {
	h := newHarness(t)
	h.cluster.checkErr = refuse(reasonNotAllowed, "deployment boutique/currencyservice does not exist")
	_, err := h.svc.Propose(context.Background(), restartRequest("inc-1"))
	if r := asRefusal(t, err); r.reason != reasonNotAllowed {
		t.Fatalf("reason = %s", r.reason)
	}
	// A plain API error is not a refusal: the caller gets a 500 and may retry.
	h.cluster.checkErr = errors.New("etcdserver: request timed out")
	_, err = h.svc.Propose(context.Background(), restartRequest("inc-1"))
	var r *refusal
	if err == nil || errors.As(err, &r) {
		t.Fatalf("want a plain error, got %v", err)
	}
}

func TestTransientCheckAtApprovalKeepsPending(t *testing.T) {
	h := newHarness(t)
	p := h.propose(t, restartRequest("inc-1"))
	h.cluster.checkErr = errors.New("etcdserver: request timed out")
	h.click(p, "U0APPROVER", actionIDApprove)
	if got := h.status(t, p.ID); got.Status != statusPending {
		t.Fatalf("got %s", got.Status)
	}
	h.cluster.checkErr = refuse(reasonNotAllowed, "deployment is paused")
	h.click(p, "U0APPROVER", actionIDApprove)
	if got := h.status(t, p.ID); got.Status != statusBlocked {
		t.Fatalf("got %s", got.Status)
	}
}

func TestSlackPostFailure(t *testing.T) {
	h := newHarness(t)
	h.notifier.postErr = errors.New("channel_not_found")
	p, err := h.svc.Propose(context.Background(), restartRequest("inc-1"))
	if r := asRefusal(t, err); r.reason != reasonApprovalUnavailable {
		t.Fatalf("reason = %s", r.reason)
	}
	if got := h.status(t, p.ID); got.Status != statusBlocked {
		t.Fatalf("stored status = %s", got.Status)
	}
	h.notifier.postErr = nil
	h.propose(t, restartRequest("inc-1"))
}

func TestPolicyRefusals(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		name   string
		action Action
		reason string
	}{
		{"rollback", Action{Type: actionRollback, Namespace: targetNamespace, Target: "deployment/checkoutservice", Params: map[string]any{"revision": "3"}}, reasonNotAllowed},
		{"other namespace", Action{Type: actionRestart, Namespace: "kube-system", Target: "deployment/coredns"}, reasonNotAllowed},
		{"not listed", Action{Type: actionRestart, Namespace: targetNamespace, Target: "deployment/redis-cart"}, reasonNotAllowed},
		{"too many", Action{Type: actionScale, Namespace: targetNamespace, Target: "deployment/cartservice", Params: map[string]any{"replicas": 5}}, reasonOutOfBounds},
		{"zero", Action{Type: actionScale, Namespace: targetNamespace, Target: "deployment/cartservice", Params: map[string]any{"replicas": 0}}, reasonOutOfBounds},
		{"delete anything", Action{Type: "delete", Namespace: targetNamespace, Target: "deployment/frontend"}, reasonNotAllowed},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := restartRequest("inc-policy-" + string(rune('a'+i)))
			req.Action = c.action
			_, err := h.svc.Propose(context.Background(), req)
			if r := asRefusal(t, err); r.reason != c.reason {
				t.Fatalf("reason = %s (%s), want %s", r.reason, r.msg, c.reason)
			}
		})
	}
	if len(h.notifier.posts) != 0 {
		t.Error("refused proposals must not reach Slack")
	}
}

func TestInvalidRequestIsAudited(t *testing.T) {
	h := newHarness(t)
	req := restartRequest("")
	_, err := h.svc.Propose(context.Background(), req)
	if r := asRefusal(t, err); r.reason != reasonInvalid {
		t.Fatalf("reason = %s", r.reason)
	}
	want := []string{"proposal_received:", "blocked:invalid-request"}
	if ev := h.logs.events(t); !slices.Equal(ev, want) {
		t.Errorf("events = %v", ev)
	}
}

func TestRecoverMarksInterruptedActionsFailed(t *testing.T) {
	h := newHarness(t)
	p := h.propose(t, restartRequest("inc-1"))
	// Simulate a crash after approval, before the outcome was written.
	_, err := h.store.Update(context.Background(), func(st *State) error {
		st.Proposals[p.ID].Status = statusApproved
		st.Incidents["inc-1"] = IncidentAction{ProposalID: p.ID, At: h.clock.now()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := h.status(t, p.ID)
	if got.Status != statusFailed || got.Reason != reasonInterrupted {
		t.Fatalf("got %+v", got)
	}
	_, err = h.svc.Propose(context.Background(), restartRequest("inc-1"))
	if r := asRefusal(t, err); r.reason != reasonAlreadyActed {
		t.Fatalf("reason = %s", r.reason)
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	h := newHarness(t)
	p := h.propose(t, restartRequest("inc-1"))
	h.click(p, "U0APPROVER", actionIDApprove)

	// A new process: same ConfigMap, fresh store and service.
	h2 := newHarness(t)
	h2.client = h.client
	h2.store = &configMapStore{client: h.client, namespace: "mamori-agent", now: h.clock.now}
	h2.svc.store = h2.store
	if err := h2.svc.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, ok := h2.svc.Get(context.Background(), p.ID)
	if !ok || got.Status != statusExecuted {
		t.Fatalf("after restart: %+v %v", got, ok)
	}
	_, err := h2.svc.Propose(context.Background(), restartRequest("inc-1"))
	if r := asRefusal(t, err); r.reason != reasonAlreadyActed {
		t.Fatalf("reason = %s", r.reason)
	}
}

func TestServiceGoAndWait(t *testing.T) {
	h := newHarness(t)
	done := make(chan struct{})
	h.svc.Go(func() { close(done) })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	h.svc.Wait(ctx)
	select {
	case <-done:
	default:
		t.Fatal("Wait returned before the work finished")
	}
	// Wait gives up when its context ends.
	block := make(chan struct{})
	h.svc.Go(func() { <-block })
	short, cancel2 := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel2()
	h.svc.Wait(short)
	close(block)
}

func TestNotifierErrorsAreLoggedNotFatal(t *testing.T) {
	h := newHarness(t)
	h.notifier.updateErr = errors.New("message_not_found")
	h.notifier.replyErr = errors.New("rate_limited")
	h.notifier.ephErr = errors.New("user_not_in_channel")
	p := h.propose(t, restartRequest("inc-1"))
	h.click(p, "U0INTRUDER", actionIDApprove)
	h.click(p, "U0APPROVER", actionIDApprove)
	if got := h.status(t, p.ID); got.Status != statusExecuted {
		t.Fatalf("got %s", got.Status)
	}
}

func TestNewProposalID(t *testing.T) {
	id := newProposalID(time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC))
	if !strings.HasPrefix(id, "p-20261113t093000-") || len(id) != len("p-20261113t093000-")+8 {
		t.Fatalf("id = %q", id)
	}
	if !idPattern.MatchString(id) {
		t.Fatalf("id %q does not match the id pattern", id)
	}
}
