"""Tests for the PR #47 review fixes: fan-out, queue and budget, worker safety, log scope."""

from __future__ import annotations

import threading
from collections.abc import Callable
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

import pytest

from conftest import FakeHttp, FakeModel, cm_payload, final, ok, response, text, tool_block
from mamori_agent.app import today_spend_from_logs
from mamori_agent.config import Settings
from mamori_agent.executor import ExecutorClient
from mamori_agent.incidents import parse_alertmanager, parse_cloud_monitoring, parse_manual
from mamori_agent.investigator import Investigator
from mamori_agent.models import Incident, ProposedAction
from mamori_agent.pubsub import PubSubPuller
from mamori_agent.service import AgentService, ExclusionHolder
from mamori_agent.slack import SlackNotifier
from mamori_agent.tools import NoInput, Tool, ToolRegistry
from mamori_agent.tools.gcp import GoogleReads, balanced


class Clock:
    def __init__(self) -> None:
        self.now = 0.0

    def __call__(self) -> float:
        return self.now


def tools() -> ToolRegistry:
    return ToolRegistry(
        [Tool("k8s_pods", "pods in a namespace for tests", NoInput, lambda _: "ok", "x")], 500
    )


def make_service(
    settings: Settings,
    model: Any,
    http: FakeHttp | None = None,
    clock: Clock | None = None,
    executor: Any = None,
) -> AgentService:
    http = http or FakeHttp({"chat.postMessage": ok({"ok": True})})
    return AgentService(
        settings,
        investigator=Investigator(settings, model, tools()),
        executor=executor or ExecutorClient(http, "http://executor", "", sleep=lambda _: None),
        slack=SlackNotifier(http, "xoxb-t", "#incidents"),
        exclusions=ExclusionHolder(),
        clock=clock or Clock(),
    )


def incident(incident_id: str, cuj: str = "checkout", tier: str = "page", **kw: Any) -> Incident:
    labels = {"cuj": cuj, "signal": "mesh", "sli": "availability", "burn": "fast", "severity": tier}
    payload = cm_payload(
        incident_id=incident_id,
        policy_user_labels=labels,
        policy_name=f"CUJ {cuj}: mesh availability fast burn",
        documentation={},
        **kw,
    )
    return parse_cloud_monitoring(payload)


def drain(svc: AgentService) -> None:
    stop = threading.Event()
    worker = threading.Thread(target=svc.run_worker, args=(stop,))
    worker.start()
    svc._queue.join()
    stop.set()
    worker.join()


# --- 1. fan-out ----------------------------------------------------------------------------------


def test_only_page_tier_incidents_start_investigations(
    make_settings: Callable[..., Settings],
) -> None:
    svc = make_service(make_settings(coalesce_window_seconds=0), FakeModel([]))
    assert svc.submit(incident("t-1", tier="ticket")).reason == "ticket"
    assert svc.submit(incident("p-1")).reason == "investigate"
    # No tier label, or another source with its own severity values: not filtered.
    unlabeled = parse_cloud_monitoring(
        cm_payload(incident_id="u-1", policy_user_labels={"cuj": "browse"})
    )
    assert svc.submit(unlabeled).reason == "investigate"
    am = parse_alertmanager(
        {
            "labels": {"alertname": "x", "severity": "warning", "cuj": "view-cart"},
            "startsAt": "2026-12-10T05:00:00Z",
        }
    )
    assert svc.submit(am).reason == "investigate"


def test_page_only_can_be_switched_off(make_settings: Callable[..., Settings]) -> None:
    svc = make_service(make_settings(page_only=False), FakeModel([]))
    assert svc.submit(incident("t-1", tier="ticket")).reason == "investigate"


def test_overlapping_incidents_join_the_queued_investigation(settings: Settings) -> None:
    clock = Clock()
    svc = make_service(settings, FakeModel([]), clock=clock)
    first = svc.submit(incident("a", "checkout"))
    joined = svc.submit(incident("b", "checkout"))
    assert (joined.reason, joined.investigation_id) == ("merged", first.investigation_id)
    other = svc.submit(incident("c", "browse"))
    assert other.reason == "investigate" and other.investigation_id != first.investigation_id
    # A journeyless incident joins the newest one in the window.
    blank = parse_cloud_monitoring(
        cm_payload(incident_id="d", policy_user_labels={}, policy_name="x", documentation={})
    )
    assert svc.submit(blank).investigation_id == other.investigation_id
    clock.now = settings.coalesce_window_seconds + 1
    assert svc.submit(incident("e", "checkout")).reason == "investigate"
    record = svc.store.get(first.investigation_id or "")
    assert record is not None and record.merged_incident_ids == ["b"]
    # Manual runs never merge.
    m1 = svc.submit(parse_manual({"incidentId": "m1", "summary": "CUJ browse down"}))
    m2 = svc.submit(parse_manual({"incidentId": "m2", "summary": "CUJ browse down"}))
    assert m1.reason == m2.reason == "investigate"
    status = svc.status()
    assert (status["queued"], status["running"]) == (5, 0)


def test_merged_while_running_is_on_the_final_record(settings: Settings) -> None:
    holder: dict[str, AgentService] = {}

    class Joining(FakeModel):
        def __call__(self, params: dict[str, Any], timeout: float) -> Any:
            # A second incident for the same journey arrives while the first one is running.
            assert holder["svc"].status()["running"] == 1
            assert holder["svc"].submit(incident("late", "checkout")).reason == "merged"
            return super().__call__(params, timeout)

    svc = make_service(settings, Joining([final()]))
    holder["svc"] = svc
    accepted = svc.submit(incident("first", "checkout"))
    drain(svc)
    record = svc.store.get(accepted.investigation_id or "")
    assert record is not None and record.status == "completed"
    assert record.merged_incident_ids == ["late"]
    assert svc.status()["running"] == 0


def test_closed_before_start_is_cancelled(
    settings: Settings, caplog: pytest.LogCaptureFixture
) -> None:
    model = FakeModel([])
    svc = make_service(settings, model)
    accepted = svc.submit(incident("a"))
    svc.submit(incident("b"))  # merged
    assert svc.submit(incident("a", state="closed")).reason == "closed"
    assert svc.submit(incident("b", state="closed")).reason == "closed"
    drain(svc)
    record = svc.store.get(accepted.investigation_id or "")
    assert record is not None and record.status == "cancelled"
    assert "closed" in (record.error or "")
    assert model.calls == []  # no money spent
    assert svc.spend.reserved == 0.0
    assert any(getattr(r, "event", "") == "investigation_cancelled" for r in caplog.records)


def test_one_open_incident_keeps_the_investigation(settings: Settings) -> None:
    svc = make_service(settings, FakeModel([final()]))
    accepted = svc.submit(incident("a"))
    svc.submit(incident("b"))
    svc.submit(incident("a", state="closed"))
    drain(svc)
    record = svc.store.get(accepted.investigation_id or "")
    assert record is not None and record.status == "completed"


# --- 5. daily budget at dequeue, and restored after a restart ---


def test_budget_used_up_before_dequeue_cancels(settings: Settings) -> None:
    svc = make_service(settings, FakeModel([]))
    accepted = svc.submit(incident("a"))
    svc.spend.restore(settings.daily_cost_limit_usd)
    drain(svc)
    record = svc.store.get(accepted.investigation_id or "")
    assert record is not None and record.status == "cancelled" and "budget" in (record.error or "")


def test_today_spend_from_logs_pages_and_errors() -> None:
    pages = [
        ok(
            {
                "entries": [{"jsonPayload": {"usage": {"costUsd": 0.4}}}, {"jsonPayload": {}}],
                "nextPageToken": "n",
            }
        ),
        ok(
            {
                "entries": [
                    {"jsonPayload": {"usage": {"costUsd": "0.1"}}},
                    {"jsonPayload": {"usage": {"costUsd": "x"}}},
                ]
            }
        ),
    ]
    http = FakeHttp({"entries:list": pages})
    spent = today_spend_from_logs(http, "p", datetime(2026, 12, 10, 9, tzinfo=UTC))
    assert spent == pytest.approx(0.5)
    first = http.calls[0]["json_body"]
    assert 'timestamp>="2026-12-10T00:00:00Z"' in first["filter"]
    assert 'jsonPayload.event="investigation_completed"' in first["filter"]
    assert http.calls[1]["json_body"]["pageToken"] == "n"
    assert today_spend_from_logs(FakeHttp({"entries:list": text("denied", 403)}), "p") is None


# --- 9. worker safety ---


def test_worker_survives_errors_outside_the_investigator(
    settings: Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    svc = make_service(settings, FakeModel([final()]))
    first = svc.submit(incident("a", "checkout"))
    second = svc.submit(incident("b", "browse"))
    calls: list[str] = []
    real = svc.investigate

    def flaky(inc: Incident, inv: Any = None, reserved: float = 0.0) -> Any:
        calls.append(inc.incident_id)
        if inc.incident_id == "a":
            raise OSError("token file unreadable")
        return real(inc, inv, reserved)

    monkeypatch.setattr(svc, "investigate", flaky)
    drain(svc)
    assert calls == ["a", "b"]  # the worker went on after the failure
    failed = svc.store.get(first.investigation_id or "")
    assert failed is not None and failed.status == "failed" and "token file" in (failed.error or "")
    done = svc.store.get(second.investigation_id or "")
    assert done is not None and done.status == "completed"
    assert svc.spend.reserved == 0.0


def test_proposal_errors_are_recorded_not_raised(settings: Settings) -> None:
    class Broken:
        def propose(self, *_: Any) -> Any:
            raise ValueError("executor answered 202 with HTML")

    svc = make_service(settings, FakeModel([final()]), executor=Broken())
    record = svc.investigate(parse_manual({"incidentId": "x", "summary": "s"}))
    assert record.status == "completed"
    assert record.proposal_error == {
        "status": None,
        "error": "ValueError: executor answered 202 with HTML",
        "reason": "agent-error",
    }


def test_executor_202_without_json_and_unreadable_token(tmp_path: Path) -> None:
    action = ProposedAction(type="restart", target="deployment/frontend")
    http = FakeHttp({"/v1/proposals": text("<html>ok</html>", 202)})
    result = ExecutorClient(http, "http://e", str(tmp_path), sleep=lambda _: None).propose(
        "i", "v", action, ""
    )
    assert result.result == "error" and result.error is not None
    assert result.error["reason"] == "bad-response" and result.error["status"] == 202
    assert http.calls[0]["headers"] == {}  # the token path is a directory: no header, no crash


def test_pubsub_leaves_refused_messages_unacknowledged() -> None:
    http = FakeHttp(
        {
            ":pull": ok(
                {
                    "receivedMessages": [
                        {"ackId": "keep", "message": {"data": ""}},
                        {"ackId": "redo", "message": {"data": ""}},
                    ]
                }
            ),
            ":acknowledge": ok({}),
        }
    )
    answers = iter([True, False])
    PubSubPuller(http, "projects/p/subscriptions/s", lambda d, m: next(answers)).pull_once()
    assert http.calls[1]["json_body"] == {"ackIds": ["keep"]}
    none_acked = FakeHttp({":pull": ok({"receivedMessages": [{"ackId": "redo", "message": {}}]})})
    PubSubPuller(none_acked, "projects/p/subscriptions/s", lambda d, m: False).pull_once()
    assert len(none_acked.calls) == 1  # no acknowledge call at all


# --- 6. log scope ---


def test_logs_search_is_limited_to_the_shop_namespaces() -> None:
    http = FakeHttp({"entries:list": ok({})})
    reads = GoogleReads(http, "p", 5, ["boutique", "istio-ingress", "mamori-agent"])
    reads.logs('jsonPayload.event="investigation_completed"', 30, 10)
    filt = http.calls[0]["json_body"]["filter"]
    assert 'resource.labels.namespace_name=("boutique" OR "istio-ingress")' in filt
    assert "mamori-agent" not in filt.split("AND (")[0]
    escape = reads.logs('x") OR (resource.labels.namespace_name="mamori-agent"', 30, 10)
    assert escape.startswith("refused") and len(http.calls) == 1
    assert GoogleReads(http, "p", 5, ["mamori-agent"]).logs("", 5, 5).startswith("no log entries")
    assert '("none")' in http.calls[-1]["json_body"]["filter"]


@pytest.mark.parametrize(
    ("query", "ok_"),
    [
        ('severity>=ERROR AND (textPayload:"a)" OR textPayload:"b")', True),
        ("a)", False),
        ("(a", False),
        ('"unterminated', False),
        (r'textPayload:"say \"hi\" )"', True),
    ],
)
def test_balanced(query: str, ok_: bool) -> None:
    assert balanced(query) is ok_


def test_tool_use_cut_off_never_reaches_the_executor(settings: Settings) -> None:
    http = FakeHttp(
        {"/v1/proposals": ok({"proposalId": "p"}, status=202), "chat.postMessage": ok({"ok": True})}
    )
    model = FakeModel([response([tool_block("t", "k8s_pods", {})], stop="max_tokens"), final()])
    record = make_service(settings, model, http).investigate(
        parse_manual({"incidentId": "z", "summary": "s"})
    )
    assert record.status == "completed" and record.tool_calls == 0
