"""Command line.

  python -m mamori_agent serve                         # the in-cluster service (default)
  python -m mamori_agent investigate --incident-file x.json [--no-propose] [--no-slack]
  python -m mamori_agent investigate --demo            # a sample checkout fast-burn alert
  python -m mamori_agent parse-incident x.json         # show how an alert payload is read

`investigate` runs one investigation in the foreground and prints the record as JSON. The
ai-sre-investigator's `remediate --approve` (a kubectl command run by the CLI) is gone on
purpose: the agent only proposes; the executor (#25) acts after a human approves in Slack.
"""

from __future__ import annotations

import argparse
import json
import logging
import signal
import sys
import threading
import time
from dataclasses import asdict
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from . import logs, metrics
from .app import build
from .config import Settings
from .incidents import InvalidIncident, parse_any, parse_pubsub_data
from .pubsub import PubSubPuller
from .server import make_server

DEMO_ALERT: dict[str, Any] = {
    "version": "1.2",
    "incident": {
        "incident_id": "demo-checkout-fast-burn",
        "state": "open",
        "started_at": 0,
        "policy_name": "CUJ checkout: mesh availability fast burn",
        "condition_name": "fast burn: 1h lookback",
        "severity": "CRITICAL",
        "summary": "Error budget burn rate for CUJ checkout is above 13.44x.",
        "policy_user_labels": {
            "generator": "slo-gen",
            "cuj": "checkout",
            "signal": "mesh",
            "sli": "availability",
            "alert": "burn-rate",
            "burn": "fast",
            "severity": "page",
        },
    },
}


def _load(path: str) -> dict[str, Any]:
    data = json.loads(Path(path).read_text(encoding="utf-8"))
    if not isinstance(data, dict):
        raise InvalidIncident("the file must hold a JSON object")
    return data


def cmd_parse(args: argparse.Namespace) -> int:
    incident = parse_any(_load(args.file))
    record = asdict(incident)
    record["started_at"] = incident.started_at.isoformat()
    print(json.dumps(record, indent=2))
    return 0


def cmd_investigate(args: argparse.Namespace) -> int:
    overrides: dict[str, Any] = {"pubsub_enabled": False}
    if args.no_propose:
        overrides["propose"] = False
    if args.no_slack:
        overrides["slack"] = False
    settings = Settings(**overrides)
    logs.setup(settings.log_level)
    if args.demo:
        payload = json.loads(json.dumps(DEMO_ALERT))
        payload["incident"]["started_at"] = int(datetime.now(UTC).timestamp()) - 600
    else:
        payload = _load(args.incident_file)
    incident = parse_any(payload)
    agent = build(settings)
    record = agent.service.investigate(incident)
    print(json.dumps(record.to_json(), indent=2))
    return 0 if record.status == "completed" else 1


def cmd_serve(_: argparse.Namespace) -> int:
    settings = Settings()
    logs.setup(settings.log_level)
    agent = build(settings)
    stop = threading.Event()
    threads = [
        threading.Thread(target=agent.service.run_worker, args=(stop,), name="worker", daemon=True)
    ]
    if settings.pubsub_enabled and settings.subscription and agent.project_id:
        path = settings.subscription
        if "/" not in path:
            path = f"projects/{agent.project_id}/subscriptions/{path}"
        puller = PubSubPuller(agent.google, path, lambda data, meta: _on_message(agent, data, meta))
        threads.append(
            threading.Thread(target=puller.run, args=(stop,), name="pubsub", daemon=True)
        )
    elif settings.pubsub_enabled:
        logs.event(
            logging.getLogger(__name__),
            "pubsub_disabled",
            logging.WARNING,
            reason="no project id (set AGENT_PROJECT_ID or provide ADC with a project)",
        )

    def ready() -> bool:
        return all(t.is_alive() for t in threads)

    server = make_server(settings.http_port, agent.service, ready)

    def shutdown(*_: Any) -> None:
        stop.set()
        threading.Thread(target=server.shutdown, daemon=True).start()

    signal.signal(signal.SIGTERM, shutdown)
    signal.signal(signal.SIGINT, shutdown)
    for t in threads:
        t.start()
    logs.event(logging.getLogger(__name__), "agent_serving", port=settings.http_port)
    server.serve_forever(poll_interval=0.5)
    # Let a running investigation finish its record if it can within the grace period.
    deadline = time.monotonic() + 20
    for t in threads:
        t.join(timeout=max(0.0, deadline - time.monotonic()))
    return 0


def _on_message(agent: Any, data: bytes, meta: dict[str, Any]) -> None:
    try:
        incident = parse_pubsub_data(data)
    except InvalidIncident as exc:
        metrics.INCIDENTS.labels("cloud-monitoring", "invalid").inc()
        logs.event(
            logging.getLogger(__name__),
            "incident_invalid",
            logging.WARNING,
            error=str(exc),
            message_id=meta.get("messageId"),
        )
        return
    agent.service.submit(incident)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="mamori_agent", description="Read-only AI incident agent."
    )
    sub = parser.add_subparsers(dest="cmd")
    sub.add_parser("serve", help="Run the service (HTTP API, Pub/Sub trigger, worker).")
    inv = sub.add_parser("investigate", help="Investigate one incident in the foreground.")
    source = inv.add_mutually_exclusive_group(required=True)
    source.add_argument(
        "--incident-file",
        help="Cloud Monitoring message, Alertmanager alert or manual body (JSON).",
    )
    source.add_argument(
        "--demo", action="store_true", help="Use a sample checkout fast-burn alert."
    )
    inv.add_argument(
        "--no-propose", action="store_true", help="Don't send the action to the executor."
    )
    inv.add_argument("--no-slack", action="store_true", help="Don't post to Slack.")
    parse = sub.add_parser("parse-incident", help="Print how an alert payload is parsed.")
    parse.add_argument("file")
    args = parser.parse_args(argv)
    try:
        if args.cmd == "investigate":
            return cmd_investigate(args)
        if args.cmd == "parse-incident":
            return cmd_parse(args)
        return cmd_serve(args)
    except (InvalidIncident, OSError, json.JSONDecodeError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2
