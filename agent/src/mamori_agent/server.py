"""HTTP API on :8080 (stdlib server; a handful of requests a day needs no framework).

GET  /v1/investigations[?limit=50&since=RFC3339]  -> {"investigations": [...]}, newest first
GET  /v1/investigations/{id}                      -> one record, or 404
GET  /v1/status                                   -> {"queued", "running", "dailySpentUsd",
                                                      "dailyReservedUsd", "agentConfig"}
POST /v1/investigations                           -> 202 {"id", "status": "queued"} (manual run)
GET  /healthz  /readyz  /metrics

No authentication: the NetworkPolicy lets only Managed Prometheus (gmp-system) reach the port,
and people use kubectl port-forward, which Kubernetes RBAC guards.
"""

from __future__ import annotations

import json
import logging
from collections.abc import Callable
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any
from urllib.parse import parse_qs, urlparse

from prometheus_client import CONTENT_TYPE_LATEST, generate_latest

from .incidents import InvalidIncident, parse_any
from .service import AgentService

log = logging.getLogger(__name__)

_MAX_BODY = 64 * 1024
_PREFIX = "/v1/investigations"


def make_handler(service: AgentService, ready: Callable[[], bool]) -> type[BaseHTTPRequestHandler]:
    class Handler(BaseHTTPRequestHandler):
        server_version = "mamori-agent"
        sys_version = ""

        def log_message(self, format: str, *args: Any) -> None:  # noqa: A002 - stdlib name
            log.debug("http %s", format % args)

        def _send(self, status: int, body: bytes, content_type: str = "application/json") -> None:
            self.send_response(status)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(body)

        def _json(self, status: int, payload: Any) -> None:
            self._send(status, json.dumps(payload).encode())

        def do_GET(self) -> None:
            url = urlparse(self.path)
            if url.path == "/healthz":
                # Fails when the worker or the Pub/Sub thread has died, so the kubelet restarts us.
                alive = ready()
                self._json(200 if alive else 503, {"status": "ok" if alive else "thread stopped"})
            elif url.path == "/readyz":
                ok = ready()
                self._json(200 if ok else 503, {"status": "ready" if ok else "not ready"})
            elif url.path == "/v1/status":
                self._json(200, service.status())
            elif url.path == "/metrics":
                self._send(200, generate_latest(), CONTENT_TYPE_LATEST)
            elif url.path == _PREFIX:
                query = parse_qs(url.query)
                try:
                    limit = min(200, max(1, int(query.get("limit", ["50"])[0])))
                except ValueError:
                    self._json(400, {"error": "limit must be a number"})
                    return
                since = query.get("since", [None])[0]
                items = service.store.list(limit, since)
                self._json(200, {"investigations": [i.to_json() for i in items]})
            elif url.path.startswith(_PREFIX + "/"):
                inv = service.store.get(url.path.removeprefix(_PREFIX + "/"))
                if inv is None:
                    self._json(404, {"error": "not found"})
                else:
                    self._json(200, inv.to_json())
            else:
                self._json(404, {"error": "not found"})

        def do_POST(self) -> None:
            if urlparse(self.path).path != _PREFIX:
                self._json(404, {"error": "not found"})
                return
            try:
                length = int(self.headers.get("Content-Length") or 0)
            except ValueError:
                length = -1
            if length <= 0 or length > _MAX_BODY:
                self._json(400, {"error": f"body must be 1..{_MAX_BODY} bytes of JSON"})
                return
            try:
                body = json.loads(self.rfile.read(length))
                if not isinstance(body, dict):
                    raise InvalidIncident("body must be a JSON object")
                incident = parse_any(body)
            except (ValueError, InvalidIncident) as exc:
                self._json(400, {"error": str(exc)})
                return
            accepted = service.submit(incident)
            if accepted.accepted:
                state = "merged" if accepted.reason == "merged" else "queued"
                self._json(202, {"id": accepted.investigation_id, "status": state})
            else:
                status = 429 if accepted.reason in ("queue_full", "daily_budget") else 409
                self._json(status, {"error": "not started", "reason": accepted.reason})

    return Handler


def make_server(port: int, service: AgentService, ready: Callable[[], bool]) -> ThreadingHTTPServer:
    server = ThreadingHTTPServer(("0.0.0.0", port), make_handler(service, ready))  # noqa: S104 - pod IP
    server.daemon_threads = True
    return server
