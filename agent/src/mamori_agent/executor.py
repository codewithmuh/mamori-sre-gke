"""Send the proposed action to the executor (#25): POST /v1/proposals.

The agent never acts; the executor checks the action against its allow-list and bounds, asks a
human in Slack, and runs it with its own narrow RBAC (D9). Answers:
202 {"proposalId", "status": "pending"}; 4xx {"error", "reason"} with reason invalid-request,
unauthorized, not-allowed, out-of-bounds, already-acted or kill-switch. 4xx is final; a 5xx or
no answer gets one retry.
"""

from __future__ import annotations

import os
import re
import time
from collections.abc import Callable
from dataclasses import dataclass
from typing import Any

from .config import ACTION_NAMESPACE
from .http import Http, HttpError
from .models import ProposedAction

_TARGET = re.compile(r"^[a-z]+/[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$")
_TIMEOUT = 10.0
_RETRY_DELAY = 2.0


@dataclass(frozen=True)
class ProposalResult:
    proposal_id: str | None
    error: dict[str, Any] | None
    result: str  # accepted|rejected|error|invalid|skipped


def validate_action(action: ProposedAction) -> str | None:
    """Why the agent itself won't send this action, or None if it may go to the executor."""
    if action.type == "none":
        return "no action proposed"
    if action.namespace != ACTION_NAMESPACE:
        return f"namespace must be {ACTION_NAMESPACE}"
    if not _TARGET.match(action.target):
        return "target must look like <kind>/<name>"
    if action.type in ("restart", "scale") and not action.target.startswith("deployment/"):
        return f"{action.type} needs a deployment target"
    if action.type == "scale" and not isinstance(action.params.get("replicas"), int):
        return "scale needs an integer replicas"
    return None


class ExecutorClient:
    def __init__(
        self,
        http: Http,
        base_url: str,
        token_file: str,
        sleep: Callable[[float], None] = time.sleep,
    ) -> None:
        self._http = http
        self._url = base_url.rstrip("/") + "/v1/proposals"
        self._token_file = token_file
        self._sleep = sleep

    def _headers(self) -> dict[str, str]:
        # Projected token, audience "executor"; re-read every time because the kubelet rotates it.
        # Unreadable: send none and let the executor answer 401, rather than fail here.
        if not self._token_file or not os.path.exists(self._token_file):
            return {}
        try:
            with open(self._token_file, encoding="utf-8") as fh:
                return {"Authorization": f"Bearer {fh.read().strip()}"}
        except OSError:
            return {}

    def propose(
        self, incident_id: str, investigation_id: str, action: ProposedAction, rationale: str
    ) -> ProposalResult:
        problem = validate_action(action)
        if problem:
            result = "skipped" if action.type == "none" else "invalid"
            return ProposalResult(
                None, {"status": None, "error": problem, "reason": "agent-check"}, result
            )
        body = {
            "incidentId": incident_id,
            "investigationId": investigation_id,
            "action": {
                "type": action.type,
                "namespace": action.namespace,
                "target": action.target,
                "params": action.params,
            },
            "rationale": rationale[:2000],
        }
        last_error = ""
        for attempt in range(2):
            try:
                response = self._http.post(
                    self._url, json_body=body, headers=self._headers(), timeout=_TIMEOUT
                )
            except HttpError as exc:
                last_error = exc.message
            else:
                if response.status == 202:
                    try:
                        data = response.json() or {}
                        proposal_id = str(data.get("proposalId") or "") or None
                    except (ValueError, AttributeError):
                        proposal_id = None
                    if proposal_id is None:
                        return ProposalResult(
                            None,
                            {
                                "status": 202,
                                "error": "accepted without a proposalId: " + response.text[:200],
                                "reason": "bad-response",
                            },
                            "error",
                        )
                    return ProposalResult(proposal_id, None, "accepted")
                if 400 <= response.status < 500:
                    try:
                        data = response.json() or {}
                    except ValueError:
                        data = {}
                    return ProposalResult(
                        None,
                        {
                            "status": response.status,
                            "error": str(data.get("error") or response.text[:300]),
                            "reason": str(data.get("reason") or "unknown"),
                        },
                        "rejected",
                    )
                last_error = f"HTTP {response.status}: {response.text[:200]}"
            if attempt == 0:
                self._sleep(_RETRY_DELAY)
        return ProposalResult(
            None, {"status": None, "error": last_error, "reason": "unreachable"}, "error"
        )
