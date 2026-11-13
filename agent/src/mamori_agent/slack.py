"""Post the investigation summary to Slack with chat.postMessage (bot token, no webhook).

The executor posts its own approval message with buttons (#25); this one is for the humans to
read: severity, journeys, impact, ranked causes, the proposed action and its proposal id. All
model text is escaped so a log line can't smuggle @channel pings or links into the channel.
"""

from __future__ import annotations

from typing import Any

from .http import Http, HttpError
from .models import Incident, Investigation
from .safety import slack_escape

API = "https://slack.com/api/chat.postMessage"
_TIMEOUT = 10.0


def _section(text: str) -> dict[str, Any]:
    return {"type": "section", "text": {"type": "mrkdwn", "text": text[:2900]}}


def build_blocks(inv: Investigation, incident: Incident, summary: str) -> list[dict[str, Any]]:
    journeys = ", ".join(inv.cujs) or "unknown"
    header = (
        f"{inv.severity} proposed - {journeys} - {incident.policy_name or incident.incident_id}"
    )
    blocks: list[dict[str, Any]] = [
        {"type": "header", "text": {"type": "plain_text", "text": header[:150]}},
        _section(f"*Impact:* {slack_escape(inv.impact)}\n{slack_escape(summary)}"),
    ]
    if inv.hypotheses:
        lines = []
        for h in inv.hypotheses[:3]:
            evidence = "; ".join(h.evidence[:2])
            line = f"*{h.rank}. {slack_escape(h.service)}* ({h.confidence:.0%}) "
            line += slack_escape(h.summary)
            if evidence:
                line += f"\n_{slack_escape(evidence[:400])}_"
            lines.append(line)
        blocks += [{"type": "divider"}, _section("*Likely causes*\n" + "\n".join(lines))]
    action = inv.proposed_action
    if action and action.type != "none":
        text = f"*Proposed action:* `{action.type}` `{slack_escape(action.target)}`"
        if action.params:
            text += f" {slack_escape(str(action.params))}"
        if inv.proposal_id:
            text += (
                f"\nSent to the executor as proposal `{inv.proposal_id}`: "
                "approve or reject it there."
            )
        elif inv.proposal_error:
            reason = slack_escape(str(inv.proposal_error.get("reason")))
            error = slack_escape(str(inv.proposal_error.get("error", ""))[:300])
            text += f"\nNot sent or refused by the executor ({reason}): {error}"
            if action.type == "rollback":
                text += (
                    "\nRevert the change in Git; Argo CD self-heal undoes cluster-side rollbacks."
                )
        blocks += [{"type": "divider"}, _section(text)]
    else:
        blocks += [{"type": "divider"}, _section("*Proposed action:* none")]
    context = (
        f"investigation `{inv.id}` - model {inv.model} - {inv.tool_calls} tool calls - "
        f"~${inv.usage.cost_usd:.2f}"
    )
    if incident.url:
        context += f" - <{incident.url}|incident>"
    blocks.append({"type": "context", "elements": [{"type": "mrkdwn", "text": context}]})
    return blocks


class SlackNotifier:
    def __init__(self, http: Http, token: str, channel: str) -> None:
        self._http = http
        self._token = token
        self._channel = channel

    @property
    def enabled(self) -> bool:
        return bool(self._token and self._channel)

    def post(self, inv: Investigation, incident: Incident, summary: str) -> str:
        """Returns ok, skipped or error (never raises: Slack is a side channel)."""
        if not self.enabled:
            return "skipped"
        fallback = f"{inv.severity} {', '.join(inv.cujs)}: {inv.impact}"[:300]
        body = {
            "channel": self._channel,
            "text": slack_escape(fallback),
            "blocks": build_blocks(inv, incident, summary),
            "unfurl_links": False,
            "unfurl_media": False,
        }
        try:
            response = self._http.post(
                API,
                json_body=body,
                headers={"Authorization": f"Bearer {self._token}"},
                timeout=_TIMEOUT,
            )
            data = response.json() if response.status == 200 else {}
        except (HttpError, ValueError):
            return "error"
        return "ok" if isinstance(data, dict) and data.get("ok") else "error"
