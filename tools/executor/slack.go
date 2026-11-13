package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

// slackAPI is the part of *slack.Client the notifier uses.
type slackAPI interface {
	PostMessageContext(ctx context.Context, channelID string, options ...slack.MsgOption) (string, string, error)
	UpdateMessageContext(ctx context.Context, channelID, timestamp string, options ...slack.MsgOption) (string, string, string, error)
	PostEphemeralContext(ctx context.Context, channelID, userID string, options ...slack.MsgOption) (string, error)
}

// slackNotifier posts proposals with Approve and Reject buttons. Text that comes from the
// agent (ids, target, rationale) may carry prompt-injected content, so it is shown as
// plain_text or escaped mrkdwn: it can never become a link, a mention or a button.
type slackNotifier struct {
	api slackAPI
}

// Post sends a proposal with Approve and Reject buttons.
func (n *slackNotifier) Post(ctx context.Context, channel string, p Proposal) (string, error) {
	_, ts, err := n.api.PostMessageContext(ctx, channel,
		slack.MsgOptionText(fallbackText(p), false), slack.MsgOptionBlocks(proposalBlocks(p)...))
	if err != nil {
		return "", fmt.Errorf("chat.postMessage: %w", err)
	}
	return ts, nil
}

// Update rewrites the proposal's message with its current status.
func (n *slackNotifier) Update(ctx context.Context, p Proposal) error {
	_, _, _, err := n.api.UpdateMessageContext(ctx, p.SlackChannel, p.SlackTS,
		slack.MsgOptionText(fallbackText(p), false), slack.MsgOptionBlocks(proposalBlocks(p)...))
	if err != nil {
		return fmt.Errorf("chat.update: %w", err)
	}
	return nil
}

// Reply posts in the proposal message's thread. text is mrkdwn the executor built itself
// (statusText escapes everything that came from outside), so it is not escaped again here.
func (n *slackNotifier) Reply(ctx context.Context, p Proposal, text string) error {
	_, _, err := n.api.PostMessageContext(ctx, p.SlackChannel,
		slack.MsgOptionText(text, false), slack.MsgOptionTS(p.SlackTS))
	if err != nil {
		return fmt.Errorf("chat.postMessage (thread): %w", err)
	}
	return nil
}

// Ephemeral shows text to one user only.
func (n *slackNotifier) Ephemeral(ctx context.Context, channel, user, text string) error {
	if _, err := n.api.PostEphemeralContext(ctx, channel, user, slack.MsgOptionText(escape(text), false)); err != nil {
		return fmt.Errorf("chat.postEphemeral: %w", err)
	}
	return nil
}

// escape makes text safe for Slack mrkdwn: no links, mentions or channel pings.
func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// code wraps a value in backticks after escaping, and keeps backticks out of it.
func code(s string) string { return "`" + strings.ReplaceAll(escape(s), "`", "'") + "`" }

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func fallbackText(p Proposal) string {
	return truncate(fmt.Sprintf("Executor proposal %s (%s): %s", p.ID, p.Status, escape(describe(p.Action))), 3000)
}

func proposalBlocks(p Proposal) []slack.Block {
	plain := func(s string) *slack.TextBlockObject {
		return slack.NewTextBlockObject(slack.PlainTextType, s, false, false)
	}
	md := func(s string) *slack.TextBlockObject {
		return slack.NewTextBlockObject(slack.MarkdownType, s, false, true)
	}
	params, _ := json.Marshal(p.Action.Params)

	blocks := []slack.Block{
		slack.NewHeaderBlock(plain(truncate("Proposed action: "+p.Action.Type+" "+p.Action.Target, 150))),
		slack.NewSectionBlock(nil, []*slack.TextBlockObject{
			md("*Incident*\n" + code(truncate(p.IncidentID, 200))),
			md("*Investigation*\n" + code(truncate(p.InvestigationID, 200))),
			md("*Action*\n" + code(p.Action.Type)),
			md("*Target*\n" + code(truncate(p.Action.Namespace+"/"+p.Action.Target, 300))),
			md("*Params*\n" + code(truncate(string(params), 300))),
			md(fmt.Sprintf("*Expires*\n<!date^%d^{date_short_pretty} {time}|%s>", p.ExpiresAt.Unix(), p.ExpiresAt.UTC().Format(time.RFC3339))),
		}, nil),
	}
	if p.Rationale != "" {
		blocks = append(blocks, slack.NewSectionBlock(
			plain(truncate("Agent's reasoning (model output, not verified):\n"+p.Rationale, 2900)), nil, nil))
	}
	blocks = append(blocks, slack.NewContextBlock("",
		md(fmt.Sprintf("Proposal %s · what it does: %s · only listed approvers can decide · one action per incident · kill switch: %s",
			code(p.ID), escape(describe(p.Action)), code("make executor-kill")))))

	if p.Status == statusPending {
		confirm := slack.NewConfirmationBlockObject(plain("Run this action?"),
			plain(truncate(describe(p.Action)+". The executor checks the kill switch, the allow-list and the target again before it runs.", 300)),
			plain("Run it"), plain("Cancel"))
		blocks = append(blocks, slack.NewActionBlock("decision",
			slack.NewButtonBlockElement(actionIDApprove, p.ID, plain("Approve")).WithStyle(slack.StylePrimary).WithConfirm(confirm),
			slack.NewButtonBlockElement(actionIDReject, p.ID, plain("Reject")).WithStyle(slack.StyleDanger),
		))
	} else {
		blocks = append(blocks, slack.NewSectionBlock(md(statusText(p)), nil, nil))
	}
	return blocks
}

// statusText is the one-line outcome shown on the message and in the thread.
func statusText(p Proposal) string {
	who := ""
	if p.DecidedBy != "" && p.DecidedBy != actorExecutor {
		who = fmt.Sprintf(" by <@%s>", p.DecidedBy) // a Slack user id we checked, never agent text
	}
	switch p.Status {
	case statusApproved:
		return "*Approved*" + who + ". Running now."
	case statusExecuted:
		return "*Executed*" + who + ": " + escape(p.Result)
	case statusFailed:
		return "*Failed*" + who + ": " + escape(p.Error) + "\nCheck the cluster by hand; this incident gets no further automatic action."
	case statusRejected:
		return "*Rejected*" + who + ". Nothing was done."
	case statusExpired:
		return "*Expired*: " + escape(p.Error) + ". Nothing was done."
	case statusBlocked:
		return fmt.Sprintf("*Blocked* (%s): %s. Nothing was done.", escape(p.Reason), escape(p.Error))
	default:
		return "*Pending*: waiting for an approver."
	}
}

// socketListener receives button clicks over Slack Socket Mode: an outbound WebSocket that
// the app-level token opens, so the executor needs no public endpoint and no signing secret.
type socketListener struct {
	client  *socketmode.Client
	svc     *Service
	metrics *metrics
	log     *slog.Logger
	health  *health
}

// Run connects and dispatches events until ctx ends.
func (l *socketListener) Run(ctx context.Context) {
	go func() {
		// RunContext reconnects by itself and returns only on a fatal error (it never returns
		// nil), such as a revoked token. Clicks can no longer arrive, so pending proposals
		// expire; failing the liveness probe makes the kubelet restart the container, which
		// reconnects (or keeps failing visibly, e.g. in CrashLoopBackOff, until the token is fixed).
		err := l.client.RunContext(ctx)
		if ctx.Err() == nil {
			l.metrics.setSlack(false)
			l.log.ErrorContext(ctx, "Slack Socket Mode stopped; failing the liveness probe so the container restarts",
				"event", "slack_disconnected", "error", err.Error())
			l.health.fail("Slack Socket Mode stopped: " + err.Error())
		}
	}()
	for {
		select {
		case <-ctx.Done():
			l.metrics.setSlack(false)
			return
		case evt, ok := <-l.client.Events:
			if !ok {
				return
			}
			l.dispatch(ctx, evt, func(req socketmode.Request) error { return l.client.Ack(req) })
		}
	}
}

func (l *socketListener) dispatch(ctx context.Context, evt socketmode.Event, ack func(socketmode.Request) error) {
	switch evt.Type {
	case socketmode.EventTypeConnected:
		l.metrics.setSlack(true)
		l.log.InfoContext(ctx, "connected to Slack (Socket Mode)", "event", "slack_connected")
	case socketmode.EventTypeConnectionError, socketmode.EventTypeInvalidAuth, socketmode.EventTypeDisconnect:
		l.metrics.setSlack(false)
		l.log.WarnContext(ctx, "Slack connection problem", "event", "slack_disconnected", "type", string(evt.Type))
	case socketmode.EventTypeInteractive:
		if evt.Request != nil {
			// Slack wants the ack within 3 s; the decision itself may take longer.
			if err := ack(*evt.Request); err != nil {
				l.log.WarnContext(ctx, "could not ack a Slack interaction", "error", err.Error())
			}
		}
		cb, ok := evt.Data.(slack.InteractionCallback)
		if !ok || cb.Type != slack.InteractionTypeBlockActions {
			return
		}
		for _, in := range interactionsFrom(cb) {
			l.svc.Go(func() {
				// Not cancelled on shutdown: a decision that started is finished and recorded
				// (serve waits for it), within its own time limit.
				ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), interactionTimeout)
				defer cancel()
				l.svc.HandleInteraction(ictx, in)
			})
		}
	}
}

// interactionsFrom extracts our button clicks from a block_actions payload.
func interactionsFrom(cb slack.InteractionCallback) []Interaction {
	channel := cb.Channel.ID
	if channel == "" {
		channel = cb.Container.ChannelID
	}
	ts := cb.Container.MessageTs
	if ts == "" {
		ts = cb.Message.Timestamp
	}
	var out []Interaction
	for _, a := range cb.ActionCallback.BlockActions {
		if a == nil || (a.ActionID != actionIDApprove && a.ActionID != actionIDReject) {
			continue
		}
		out = append(out, Interaction{UserID: cb.User.ID, ChannelID: channel, MessageTS: ts, ActionID: a.ActionID, ProposalID: a.Value,
			Shown: blockTexts(cb.Message.Blocks.BlockSet)})
	}
	return out
}

// blockTexts lists every piece of text and every button of a message, in order, so the
// message an approver clicked on can be compared with what the executor posted for that
// proposal. Unknown blocks and elements are listed by type, so adding one is a mismatch.
func blockTexts(blocks []slack.Block) []string {
	var out []string
	add := func(t *slack.TextBlockObject) {
		if t != nil {
			out = append(out, t.Type+":"+t.Text)
		}
	}
	for _, b := range blocks {
		switch b := b.(type) {
		case *slack.HeaderBlock:
			add(b.Text)
		case *slack.SectionBlock:
			add(b.Text)
			for _, f := range b.Fields {
				add(f)
			}
		case *slack.ContextBlock:
			for _, e := range b.ContextElements.Elements {
				if t, ok := e.(*slack.TextBlockObject); ok {
					add(t)
				} else {
					out = append(out, "element:"+string(e.MixedElementType()))
				}
			}
		case *slack.ActionBlock:
			if b.Elements == nil {
				continue
			}
			for _, e := range b.Elements.ElementSet {
				btn, ok := e.(*slack.ButtonBlockElement)
				if !ok {
					out = append(out, "element:"+string(e.ElementType()))
					continue
				}
				out = append(out, "button:"+btn.ActionID+":"+btn.Value+":"+string(btn.Style))
				add(btn.Text)
				if c := btn.Confirm; c != nil {
					add(c.Title)
					add(c.Text)
					add(c.Confirm)
					add(c.Deny)
				}
			}
		default:
			out = append(out, "block:"+string(b.BlockType()))
		}
	}
	return out
}
