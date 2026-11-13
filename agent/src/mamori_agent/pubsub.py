"""Pull incident notifications from Pub/Sub subscription mamori-agent-incidents (REST, unary pull).

Pull, not push: the agent needs no public or in-cluster endpoint for Google to call, and the
pod's identity only needs roles/pubsub.subscriber on this one subscription.

Every message is acknowledged once the handler has seen it, investigated or not: Cloud
Monitoring publishes open and closed incidents and repeats (renotify) to the same topic, and a
message the agent can't parse would otherwise come back forever. The trade-off: if the pod dies
mid-investigation, that incident is not retried (the alert's own paging is unaffected).
"""

from __future__ import annotations

import base64
import logging
import threading
from collections.abc import Callable
from typing import Any

from .http import Http, checked_json
from .logs import event

log = logging.getLogger(__name__)

PUBSUB = "https://pubsub.googleapis.com/v1"
_PULL_TIMEOUT = 60.0
_MAX_MESSAGES = 5
_MAX_BACKOFF = 60.0
_IDLE_WAIT = 2.0


class PubSubPuller:
    def __init__(
        self,
        http: Http,
        subscription_path: str,
        handle: Callable[[bytes, dict[str, Any]], None],
    ) -> None:
        self._http = http
        self._path = subscription_path  # projects/<id>/subscriptions/<name>
        self._handle = handle
        self.healthy = False

    def pull_once(self) -> int:
        """One pull and its acknowledgement; returns how many messages were handled."""
        data = checked_json(
            self._http.post(
                f"{PUBSUB}/{self._path}:pull",
                json_body={"maxMessages": _MAX_MESSAGES},
                timeout=_PULL_TIMEOUT,
            ),
            "pubsub pull",
        )
        received = data.get("receivedMessages") or []
        ack_ids = []
        for item in received:
            message = item.get("message") or {}
            try:
                payload = base64.b64decode(message.get("data") or "")
            except ValueError:
                payload = b""
            meta = {
                "messageId": message.get("messageId"),
                "publishTime": message.get("publishTime"),
                "attributes": message.get("attributes") or {},
            }
            try:
                self._handle(payload, meta)
            except Exception:
                log.exception("incident handler failed", extra={"event": "handler_failed", **meta})
            ack_ids.append(item.get("ackId"))
        if ack_ids:
            checked_json(
                self._http.post(
                    f"{PUBSUB}/{self._path}:acknowledge",
                    json_body={"ackIds": ack_ids},
                    timeout=_PULL_TIMEOUT,
                ),
                "pubsub acknowledge",
            )
        return len(received)

    def run(self, stop: threading.Event) -> None:
        backoff = 1.0
        event(log, "pubsub_started", subscription=self._path)
        while not stop.is_set():
            try:
                handled = self.pull_once()
            except Exception as exc:  # noqa: BLE001 - the trigger thread must not die
                self.healthy = False
                event(log, "pubsub_error", logging.WARNING, error=str(exc), retry_in_s=backoff)
                stop.wait(backoff)
                backoff = min(_MAX_BACKOFF, backoff * 2)
                continue
            self.healthy = True
            backoff = 1.0
            if not handled:
                # Pull holds the request open for a while when there is nothing to deliver, but
                # may also answer at once; don't spin on empty answers.
                stop.wait(_IDLE_WAIT)
