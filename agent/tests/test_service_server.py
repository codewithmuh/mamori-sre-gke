from __future__ import annotations

import json
import logging
import threading
import urllib.error
import urllib.request
from collections.abc import Callable, Iterator
from typing import Any

import pytest

from conftest import FakeHttp, FakeModel, cm_payload, final, ok, response, text, tool_block
from mamori_agent.config import Settings
from mamori_agent.executor import ExecutorClient
from mamori_agent.incidents import parse_cloud_monitoring, parse_manual
from mamori_agent.investigator import Investigator
from mamori_agent.server import make_server
from mamori_agent.service import AgentService, DailySpend, ExclusionHolder, InvestigationStore
from mamori_agent.slack import SlackNotifier
from mamori_agent.tools import NoInput, Tool, ToolRegistry


def tools(
    holder: ExclusionHolder | None = None, seen: list[list[str]] | None = None
) -> ToolRegistry:
    def run(_: Any) -> str:
        if holder is not None and seen is not None:
            seen.append(holder.get())
        return "ok"

    return ToolRegistry(
        [Tool("k8s_pods", "pods in a namespace for tests", NoInput, run, "kubernetes")], 1000
    )


def service(
    settings: Settings,
    model: FakeModel,
    http: FakeHttp | None = None,
    holder: ExclusionHolder | None = None,
    registry: ToolRegistry | None = None,
    today: Callable[[], str] = lambda: "2026-12-10",
) -> AgentService:
    http = http or FakeHttp()
    return AgentService(
        settings,
        investigator=Investigator(settings, model, registry or tools()),
        executor=ExecutorClient(http, "http://executor", "", sleep=lambda _: None),
        slack=SlackNotifier(http, "xoxb-t", "#incidents"),
        exclusions=holder or ExclusionHolder(),
        today=today,
    )


def test_intake_rules(settings: Settings) -> None:
    svc = service(settings, FakeModel([]))
    assert svc.submit(parse_cloud_monitoring(cm_payload(state="closed"))).reason == "closed"
    assert svc.submit(parse_cloud_monitoring(cm_payload(renotify=True))).reason == "duplicate"
    first = svc.submit(parse_cloud_monitoring(cm_payload()))
    assert first.accepted and first.investigation_id
    assert svc.submit(parse_cloud_monitoring(cm_payload())).reason == "duplicate"
    running = svc.store.get(first.investigation_id)
    assert running is not None and running.status == "running" and running.severity == "SEV2"


def test_queue_full_and_daily_budget(make_settings: Callable[..., Settings]) -> None:
    svc = service(make_settings(queue_size=1, daily_cost_limit_usd=1.0), FakeModel([]))
    assert svc.submit(parse_manual({"incidentId": "a", "summary": "s"})).accepted
    assert svc.submit(parse_manual({"incidentId": "b", "summary": "s"})).reason == "queue_full"
    svc.spend.add(1.0)
    assert svc.submit(parse_manual({"incidentId": "c", "summary": "s"})).reason == "daily_budget"


def test_daily_spend_rolls_over() -> None:
    day = ["2026-12-10"]
    spend = DailySpend(1.0, lambda: day[0])
    spend.add(2.0)
    assert spend.exhausted()
    day[0] = "2026-12-11"
    assert not spend.exhausted()


def test_investigation_end_to_end(settings: Settings, caplog: pytest.LogCaptureFixture) -> None:
    http = FakeHttp(
        {
            "/v1/proposals": ok({"proposalId": "p-9"}, status=202),
            "chat.postMessage": ok({"ok": True}),
        }
    )
    model = FakeModel([response([tool_block("t", "k8s_pods", {})], stop="tool_use"), final()])
    svc = service(settings, model, http)
    incident = parse_cloud_monitoring(cm_payload())
    accepted = svc.submit(incident)
    caplog.set_level(logging.INFO)
    stop = threading.Event()
    worker = threading.Thread(target=svc.run_worker, args=(stop,))
    worker.start()
    svc._queue.join()
    stop.set()
    worker.join()

    record = svc.store.get(accepted.investigation_id or "")
    assert record is not None
    assert record.status == "completed" and record.finished_at
    assert record.severity == "SEV1" and record.cujs == ["checkout", "browse"]
    assert record.proposal_id == "p-9" and record.proposal_error is None
    assert record.tool_calls == 1 and record.usage.cost_usd > 0
    lines = [r for r in caplog.records if getattr(r, "event", "") == "investigation_completed"]
    assert len(lines) == 1
    logged = vars(lines[0])
    for key in (
        "id",
        "incidentId",
        "source",
        "startedAt",
        "finishedAt",
        "cujs",
        "severity",
        "impact",
        "hypotheses",
        "proposedAction",
        "proposalId",
        "model",
        "usage",
        "toolCalls",
    ):
        assert key in logged, key
    assert logged["proposedAction"] == {
        "type": "remove-fault",
        "namespace": "boutique",
        "target": "virtualservice/f01-currency-abort",
        "params": {},
    }
    assert set(logged["usage"]) == {"inputTokens", "outputTokens", "costUsd"}
    assert [c["url"] for c in http.calls] == [
        "http://executor/v1/proposals",
        "https://slack.com/api/chat.postMessage",
    ]
    assert svc.spend.exhausted() is False


def test_failed_and_crashed_investigations_still_record(
    make_settings: Callable[..., Settings],
) -> None:
    settings = make_settings(propose=True)
    http = FakeHttp({"chat.postMessage": ok({"ok": True})})
    svc = service(
        settings,
        FakeModel([response([], stop="refusal", category="cyber"), response([], stop="refusal")]),
        http,
    )
    record = svc.investigate(parse_manual({"incidentId": "x", "summary": "CUJ browse slow"}))
    assert record.status == "failed" and record.error and record.error.startswith("refusal")
    assert record.proposed_action is None and record.severity == "SEV3"
    assert [c["url"] for c in http.calls] == ["https://slack.com/api/chat.postMessage"]

    crashing = service(settings, FakeModel([RuntimeError("SDK exploded")]))
    record = crashing.investigate(parse_manual({"incidentId": "y", "summary": "s"}))
    assert record.status == "failed" and record.error == "RuntimeError: SDK exploded"


def test_executor_rejection_is_recorded(settings: Settings) -> None:
    http = FakeHttp(
        {
            "/v1/proposals": text('{"error": "no", "reason": "kill-switch"}', 423),
            "chat.postMessage": ok({"ok": True}),
        }
    )
    record = service(settings, FakeModel([final()]), http).investigate(
        parse_manual({"incidentId": "z", "summary": "s"})
    )
    assert record.proposal_id is None
    assert record.proposal_error == {"status": 423, "error": "no", "reason": "kill-switch"}


def test_exclusions_are_set_only_during_the_run(settings: Settings) -> None:
    holder = ExclusionHolder()
    seen: list[list[str]] = []
    model = FakeModel([response([tool_block("t", "k8s_pods", {})], stop="tool_use"), final()])
    svc = service(settings, model, holder=holder, registry=tools(holder, seen))
    svc.investigate(
        parse_manual({"incidentId": "e", "summary": "s", "excludeFaultIds": ["F01-currency-abort"]})
    )
    assert seen == [["F01-currency-abort"]]
    assert holder.get() == []


def test_store_order_limit_and_size() -> None:
    store = InvestigationStore(3)
    from mamori_agent.models import Investigation

    for i in range(5):
        store.put(
            Investigation(
                id=f"inv-{i}",
                incident_id="x",
                source="manual",
                started_at=f"2026-12-10T05:0{i}:00Z",
            )
        )
    assert [i.id for i in store.list(10)] == ["inv-4", "inv-3", "inv-2"]
    assert [i.id for i in store.list(1)] == ["inv-4"]
    assert [i.id for i in store.list(10, since="2026-12-10T05:03:00Z")] == ["inv-4", "inv-3"]
    assert store.get("inv-0") is None


# --- HTTP server --------------------------------------------------------------------------------


@pytest.fixture
def server(settings: Settings) -> Iterator[tuple[str, AgentService, list[bool]]]:
    svc = service(settings, FakeModel([]))
    ready = [True]
    srv = make_server(0, svc, lambda: ready[0])
    thread = threading.Thread(target=srv.serve_forever, daemon=True)
    thread.start()
    yield f"http://127.0.0.1:{srv.server_address[1]}", svc, ready
    srv.shutdown()
    srv.server_close()


def call(url: str, body: Any = None, raw: bytes | None = None) -> tuple[int, Any]:
    data = raw if raw is not None else (json.dumps(body).encode() if body is not None else None)
    request = urllib.request.Request(url, data=data, method="POST" if data is not None else "GET")  # noqa: S310
    try:
        with urllib.request.urlopen(request, timeout=5) as r:  # noqa: S310 - local test server
            payload = r.read()
            status = r.status
    except urllib.error.HTTPError as exc:
        payload, status = exc.read(), exc.code
    try:
        return status, json.loads(payload)
    except ValueError:
        return status, payload.decode()


def test_api(server: tuple[str, AgentService, list[bool]]) -> None:
    base, svc, ready = server
    assert call(f"{base}/healthz") == (200, {"status": "ok"})
    assert call(f"{base}/readyz") == (200, {"status": "ready"})
    ready[0] = False
    assert call(f"{base}/readyz")[0] == 503
    assert call(f"{base}/healthz")[0] == 503
    assert call(f"{base}/v1/investigations") == (200, {"investigations": []})

    status, body = call(
        f"{base}/v1/investigations", {"incidentId": "eval-1", "summary": "CUJ browse down"}
    )
    assert status == 202 and body["status"] == "running"
    status, record = call(f"{base}/v1/investigations/{body['id']}")
    assert status == 200 and record["incidentId"] == "eval-1" and record["finishedAt"] is None
    status, listing = call(f"{base}/v1/investigations?limit=5&since=2000-01-01T00:00:00Z")
    assert [i["id"] for i in listing["investigations"]] == [body["id"]]
    assert call(f"{base}/v1/investigations?limit=x")[0] == 400

    assert call(f"{base}/v1/investigations", {"incidentId": "eval-1", "summary": "again"}) == (
        409,
        {"error": "not started", "reason": "duplicate"},
    )
    assert call(f"{base}/v1/investigations", {"summary": "no id"})[0] == 400
    assert call(f"{base}/v1/investigations", raw=b"[1]")[0] == 400
    assert call(f"{base}/v1/investigations", raw=b"{nope")[0] == 400
    assert call(f"{base}/v1/other", {"a": 1})[0] == 404
    assert call(f"{base}/v1/investigations/none")[0] == 404
    assert call(f"{base}/nope")[0] == 404
    status, metrics_text = call(f"{base}/metrics")
    assert status == 200 and "mamori_agent_incidents_total" in metrics_text
    svc.spend.add(1000)
    assert call(f"{base}/v1/investigations", {"incidentId": "eval-2", "summary": "s"})[0] == 429
