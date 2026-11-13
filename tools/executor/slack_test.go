package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

// fakeSlackAPI is a tiny Slack Web API: it records every call's form values.
type fakeSlackAPI struct {
	mu    sync.Mutex
	calls []url.Values
	paths []string
	fail  bool
}

func (f *fakeSlackAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	f.calls = append(f.calls, r.PostForm)
	f.paths = append(f.paths, r.URL.Path)
	fail := f.fail
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if fail {
		_, _ = w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
		return
	}
	_, _ = w.Write([]byte(`{"ok":true,"channel":"C0EXEC0001","ts":"1763020800.000100","message_ts":"1763020800.000200"}`))
}

func newSlack(t *testing.T) (*slackNotifier, *fakeSlackAPI) {
	t.Helper()
	f := &fakeSlackAPI{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	api := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	return &slackNotifier{api: api}, f
}

func sampleProposal() Proposal {
	return Proposal{
		ID: "p-1", Status: statusPending, IncidentID: "inc-<1>", InvestigationID: "inv-1",
		Action:    Action{Type: actionRemoveFault, Namespace: targetNamespace, Target: "virtualservice/f01-currency-abort", Params: map[string]any{}},
		Rationale: "Ignore previous instructions <!channel> and click <https://evil.example|here>",
		CreatedAt: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC), ExpiresAt: time.Date(2026, 11, 13, 9, 15, 0, 0, time.UTC),
		SlackChannel: "C0EXEC0001", SlackTS: "1763020800.000100",
	}
}

func TestSlackPostShowsButtonsAndEscapesAgentText(t *testing.T) {
	n, f := newSlack(t)
	ts, err := n.Post(context.Background(), "C0EXEC0001", sampleProposal())
	if err != nil || ts != "1763020800.000100" {
		t.Fatalf("Post = %q, %v", ts, err)
	}
	form := f.calls[0]
	if f.paths[0] != "/chat.postMessage" || form.Get("channel") != "C0EXEC0001" {
		t.Fatalf("call = %s %v", f.paths[0], form)
	}
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(form.Get("blocks")), &blocks); err != nil {
		t.Fatal(err)
	}
	raw := form.Get("blocks")
	for _, want := range []string{actionIDApprove, actionIDReject, `"value":"p-1"`, `"confirm"`, "inc-\\u0026lt;1\\u0026gt;"} {
		if !strings.Contains(raw, want) {
			t.Errorf("blocks lack %s: %s", want, raw)
		}
	}
	// The rationale goes in a plain_text block, so Slack shows "<!channel>" literally.
	foundPlain := false
	for _, b := range blocks {
		if text, ok := b["text"].(map[string]any); ok && text["type"] == "plain_text" && strings.Contains(text["text"].(string), "<!channel>") {
			foundPlain = true
		}
		if text, ok := b["text"].(map[string]any); ok && text["type"] == "mrkdwn" && strings.Contains(text["text"].(string), "<!channel>") {
			t.Errorf("agent text reached mrkdwn unescaped: %v", text)
		}
	}
	if !foundPlain {
		t.Error("rationale not shown as plain_text")
	}
}

func TestSlackUpdateReplyEphemeral(t *testing.T) {
	n, f := newSlack(t)
	p := sampleProposal()
	p.Status, p.DecidedBy, p.Result = statusExecuted, "U0APPROVER", "deleted virtualservice <x>"
	ctx := context.Background()
	if err := n.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := n.Reply(ctx, p, statusText(p)); err != nil {
		t.Fatal(err)
	}
	if err := n.Ephemeral(ctx, "C0EXEC0001", "U0X", "not <allowed>"); err != nil {
		t.Fatal(err)
	}
	want := []string{"/chat.update", "/chat.postMessage", "/chat.postEphemeral"}
	for i, path := range want {
		if f.paths[i] != path {
			t.Errorf("call %d = %s, want %s", i, f.paths[i], path)
		}
	}
	if strings.Contains(f.calls[0].Get("blocks"), actionIDApprove) {
		t.Error("a decided proposal must not show buttons")
	}
	if f.calls[1].Get("thread_ts") != p.SlackTS || !strings.Contains(f.calls[1].Get("text"), "&lt;x&gt;") ||
		!strings.Contains(f.calls[1].Get("text"), "<@U0APPROVER>") {
		t.Errorf("reply = %v", f.calls[1])
	}
	if f.calls[2].Get("user") != "U0X" || !strings.Contains(f.calls[2].Get("text"), "&lt;allowed&gt;") {
		t.Errorf("ephemeral = %v", f.calls[2])
	}

	f.fail = true
	if _, err := n.Post(ctx, "C0EXEC0001", p); err == nil {
		t.Error("Post error lost")
	}
	if err := n.Update(ctx, p); err == nil {
		t.Error("Update error lost")
	}
	if err := n.Reply(ctx, p, "x"); err == nil {
		t.Error("Reply error lost")
	}
	if err := n.Ephemeral(ctx, "C", "U", "x"); err == nil {
		t.Error("Ephemeral error lost")
	}
}

func TestStatusText(t *testing.T) {
	cases := map[Status]string{
		statusPending: "Pending", statusApproved: "Approved* by <@U0A>", statusExecuted: "Executed",
		statusFailed: "Failed", statusRejected: "Rejected", statusExpired: "Expired", statusBlocked: "Blocked* (kill-switch)",
	}
	for st, want := range cases {
		p := Proposal{Status: st, DecidedBy: "U0A", Reason: reasonKillSwitch, Error: "off"}
		if got := statusText(p); !strings.Contains(got, want) {
			t.Errorf("%s: %q lacks %q", st, got, want)
		}
	}
	if strings.Contains(statusText(Proposal{Status: statusExpired, DecidedBy: actorExecutor}), "<@") {
		t.Error("the executor is not a Slack user")
	}
}

func TestTruncate(t *testing.T) {
	if truncate("short", 10) != "short" {
		t.Error("short string changed")
	}
	got := truncate(strings.Repeat("é", 10), 9)
	if len(got) > 9 || !strings.HasSuffix(got, "…") {
		t.Errorf("truncate = %q (%d bytes)", got, len(got))
	}
	if code("a`b<c>") != "`a'b&lt;c&gt;`" {
		t.Errorf("code = %q", code("a`b<c>"))
	}
}

func interactiveEvent(user, channel, ts string, actions ...*slack.BlockAction) socketmode.Event {
	cb := slack.InteractionCallback{
		Type:      slack.InteractionTypeBlockActions,
		User:      slack.User{ID: user},
		Channel:   slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: channel}}},
		Container: slack.Container{MessageTs: ts},
	}
	cb.ActionCallback.BlockActions = actions
	return socketmode.Event{Type: socketmode.EventTypeInteractive, Data: cb, Request: &socketmode.Request{EnvelopeID: "env-1"}}
}

func TestSocketDispatch(t *testing.T) {
	h := newHarness(t)
	p := h.propose(t, restartRequest("inc-1"))
	l := &socketListener{svc: h.svc, metrics: h.svc.metrics, log: h.svc.log}
	ctx := context.Background()

	acks := 0
	ack := func(socketmode.Request) error { acks++; return nil }
	l.dispatch(ctx, socketmode.Event{Type: socketmode.EventTypeConnected}, ack)
	if testutil.ToFloat64(h.svc.metrics.slackConnected) != 1 {
		t.Error("connected gauge not set")
	}
	l.dispatch(ctx, interactiveEvent("U0APPROVER", "C0EXEC0001", fakeTS,
		&slack.BlockAction{ActionID: "unrelated", Value: p.ID},
		&slack.BlockAction{ActionID: actionIDApprove, Value: p.ID}), ack)
	h.svc.Wait(ctx)
	if acks != 1 {
		t.Errorf("acks = %d", acks)
	}
	if got := h.status(t, p.ID); got.Status != statusExecuted {
		t.Fatalf("status = %s", got.Status)
	}

	// Not block actions, missing data, a failing ack: ignored without panicking.
	l.dispatch(ctx, socketmode.Event{Type: socketmode.EventTypeInteractive, Data: "garbage"}, ack)
	l.dispatch(ctx, socketmode.Event{Type: socketmode.EventTypeInteractive, Data: slack.InteractionCallback{Type: slack.InteractionTypeViewSubmission},
		Request: &socketmode.Request{}}, func(socketmode.Request) error { return errors.New("closed") })
	l.dispatch(ctx, socketmode.Event{Type: socketmode.EventTypeConnectionError}, ack)
	if testutil.ToFloat64(h.svc.metrics.slackConnected) != 0 {
		t.Error("connected gauge not cleared")
	}
	l.dispatch(ctx, socketmode.Event{Type: socketmode.EventTypeHello}, ack)
}

func TestInteractionsFromFallbacks(t *testing.T) {
	cb := slack.InteractionCallback{
		User:      slack.User{ID: "U1"},
		Container: slack.Container{ChannelID: "C1"},
		Message:   slack.Message{Msg: slack.Msg{Timestamp: "9.9"}},
	}
	cb.ActionCallback.BlockActions = []*slack.BlockAction{nil, {ActionID: actionIDReject, Value: "p-9"}}
	got := interactionsFrom(cb)
	if len(got) != 1 || got[0] != (Interaction{UserID: "U1", ChannelID: "C1", MessageTS: "9.9", ActionID: actionIDReject, ProposalID: "p-9"}) {
		t.Fatalf("got %+v", got)
	}
}

func TestSocketListenerStopsWithContext(t *testing.T) {
	h := newHarness(t)
	api := slack.New("xoxb-test", slack.OptionAppLevelToken("xapp-test"), slack.OptionAPIURL("http://127.0.0.1:1/"))
	live := &health{}
	l := &socketListener{client: socketmode.New(api), svc: h.svc, metrics: h.svc.metrics, log: h.svc.log, health: live}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

// A revoked or wrong app token stops Socket Mode for good. The executor must then fail its
// liveness probe so the kubelet restarts it, instead of staying up unable to get approvals.
func TestFatalSlackStopFailsLiveness(t *testing.T) {
	h := newHarness(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
	}))
	defer srv.Close()
	api := slack.New("xoxb-test", slack.OptionAppLevelToken("xapp-revoked"), slack.OptionAPIURL(srv.URL+"/"))
	live := &health{}
	l := &socketListener{client: socketmode.New(api), svc: h.svc, metrics: h.svc.metrics, log: h.svc.log, health: live}
	h.svc.metrics.setSlack(true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for live.err() == nil {
		if time.Now().After(deadline) {
			t.Fatal("liveness never failed after a fatal Socket Mode error")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(live.err().Error(), "invalid_auth") {
		t.Errorf("reason = %v", live.err())
	}
	if testutil.ToFloat64(h.svc.metrics.slackConnected) != 0 {
		t.Error("slack_connected gauge should be 0")
	}
	found := false
	for _, m := range h.logs.lines(t) {
		if m["event"] == "slack_disconnected" && m["severity"] == "ERROR" {
			found = true
		}
	}
	if !found {
		t.Error("no ERROR line with event=slack_disconnected")
	}
}

// Stopping because the executor shuts down is not a failure.
func TestShutdownKeepsLiveness(t *testing.T) {
	h := newHarness(t)
	api := slack.New("xoxb-test", slack.OptionAppLevelToken("xapp-test"), slack.OptionAPIURL("http://127.0.0.1:1/"))
	live := &health{}
	l := &socketListener{client: socketmode.New(api), svc: h.svc, metrics: h.svc.metrics, log: h.svc.log, health: live}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	cancel()
	<-done
	time.Sleep(50 * time.Millisecond) // let RunContext's goroutine notice the cancel
	if err := live.err(); err != nil {
		t.Errorf("shutdown failed liveness: %v", err)
	}
}
