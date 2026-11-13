from __future__ import annotations

import base64
import json
import threading
from pathlib import Path
from typing import Any

import pytest

from conftest import FakeHttp, cm_payload, down, ok, text
from mamori_agent.executor import ExecutorClient, validate_action
from mamori_agent.http import HttpError, Response
from mamori_agent.incidents import parse_cloud_monitoring
from mamori_agent.models import Hypothesis, Investigation, ProposedAction, Usage
from mamori_agent.pubsub import PubSubPuller
from mamori_agent.slack import SlackNotifier, build_blocks

EXECUTOR = "http://executor.mamori-agent.svc:8080"
REMOVE = ProposedAction(type="remove-fault", target="virtualservice/f01-currency-abort")


@pytest.mark.parametrize(
    ("action", "problem"),
    [
        (ProposedAction(type="none"), "no action proposed"),
        (
            ProposedAction(type="restart", namespace="istio-system", target="deployment/istiod"),
            "namespace",
        ),
        (ProposedAction(type="restart", target="currencyservice"), "<kind>/<name>"),
        (ProposedAction(type="restart", target="virtualservice/x"), "deployment target"),
        (ProposedAction(type="scale", target="deployment/x", params={"replicas": "3"}), "integer"),
        (ProposedAction(type="scale", target="deployment/x", params={"replicas": 3}), None),
        (REMOVE, None),
        (ProposedAction(type="rollback", target="deployment/checkoutservice"), None),
    ],
)
def test_validate_action(action: ProposedAction, problem: str | None) -> None:
    result = validate_action(action)
    assert (result is None) if problem is None else (problem in (result or ""))


def test_propose_accepted_with_token(tmp_path: Path) -> None:
    token = tmp_path / "token"
    token.write_text("projected")
    http = FakeHttp({"/v1/proposals": ok({"proposalId": "p-1", "status": "pending"}, status=202)})
    result = ExecutorClient(http, EXECUTOR, str(token), sleep=lambda _: None).propose(
        "i-1", "inv-1", REMOVE, "why"
    )
    assert (result.proposal_id, result.error, result.result) == ("p-1", None, "accepted")
    call = http.calls[0]
    assert call["url"] == f"{EXECUTOR}/v1/proposals"
    assert call["headers"] == {"Authorization": "Bearer projected"}
    assert call["json_body"] == {
        "incidentId": "i-1",
        "investigationId": "inv-1",
        "action": {
            "type": "remove-fault",
            "namespace": "boutique",
            "target": "virtualservice/f01-currency-abort",
            "params": {},
        },
        "rationale": "why",
    }


def test_propose_rejected_is_final() -> None:
    http = FakeHttp(
        {
            "/v1/proposals": ok(
                {"error": "revert the commit in Git", "reason": "not-allowed"}, status=403
            )
        }
    )
    result = ExecutorClient(http, EXECUTOR, "", sleep=lambda _: None).propose("i", "v", REMOVE, "")
    assert result.result == "rejected"
    assert result.error == {
        "status": 403,
        "error": "revert the commit in Git",
        "reason": "not-allowed",
    }
    assert len(http.calls) == 1
    odd = FakeHttp({"/v1/proposals": text("kill switch on", 423)})
    result = ExecutorClient(odd, EXECUTOR, "", sleep=lambda _: None).propose("i", "v", REMOVE, "")
    assert result.error == {"status": 423, "error": "kill switch on", "reason": "unknown"}


def test_propose_retries_once_on_5xx_and_network_errors() -> None:
    slept: list[float] = []
    http = FakeHttp({"/v1/proposals": [text("oops", 503), ok({"proposalId": "p-2"}, status=202)]})
    result = ExecutorClient(http, EXECUTOR, "", sleep=slept.append).propose("i", "v", REMOVE, "")
    assert result.proposal_id == "p-2" and slept == [2.0]
    dead = FakeHttp({"/v1/proposals": down("refused")})
    result = ExecutorClient(dead, EXECUTOR, "", sleep=lambda _: None).propose("i", "v", REMOVE, "")
    assert result.result == "error" and result.error == {
        "status": None,
        "error": "refused",
        "reason": "unreachable",
    }
    assert len(dead.calls) == 2


def test_propose_skips_none_and_invalid() -> None:
    http = FakeHttp()
    client = ExecutorClient(http, EXECUTOR, "", sleep=lambda _: None)
    assert client.propose("i", "v", ProposedAction(type="none"), "").result == "skipped"
    assert (
        client.propose("i", "v", ProposedAction(type="restart", target="x"), "").result == "invalid"
    )
    assert http.calls == []


# --- Slack --------------------------------------------------------------------------------------


def _investigation(**kw: Any) -> Investigation:
    base: dict[str, Any] = {
        "id": "inv-1",
        "incident_id": "0.abc",
        "source": "cloud-monitoring",
        "started_at": "2026-12-10T05:01:02Z",
        "cujs": ["checkout"],
        "severity": "SEV1",
        "impact": "<!channel> 60 % of checkouts fail",
        "hypotheses": [
            Hypothesis(
                rank=1,
                service="currencyservice",
                summary="abort <@U1>",
                confidence=0.8,
                evidence=["vs aborts", "logs"],
            )
        ],
        "proposed_action": REMOVE,
        "proposal_id": "p-1",
        "model": "claude-opus-5-5",
        "usage": Usage(input_tokens=1, output_tokens=1, cost_usd=0.42),
        "tool_calls": 7,
        "status": "completed",
    }
    base.update(kw)
    return Investigation(**base)


def test_blocks_escape_model_text_and_show_proposal() -> None:
    incident = parse_cloud_monitoring(cm_payload())
    blocks = build_blocks(_investigation(), incident, "summary <https://evil|click>")
    dump = json.dumps(blocks)
    assert "<!channel>" not in dump and "<@U1>" not in dump and "<https://evil" not in dump
    assert (
        blocks[0]["text"]["text"]
        == "SEV1 proposed - checkout - CUJ checkout: mesh availability fast burn"
    )
    assert "proposal `p-1`" in dump
    assert "~$0.42" in dump and "|incident>" in dump
    rejected = build_blocks(
        _investigation(
            proposal_id=None,
            proposed_action=ProposedAction(type="rollback", target="deployment/x"),
            proposal_error={"status": 403, "error": "use git", "reason": "not-allowed"},
        ),
        incident,
        "",
    )
    assert "Revert the change in Git" in json.dumps(rejected)
    none = build_blocks(
        _investigation(proposed_action=ProposedAction(type="none"), hypotheses=[]), incident, ""
    )
    assert "*Proposed action:* none" in json.dumps(none)
    scale = build_blocks(
        _investigation(
            proposed_action=ProposedAction(
                type="scale", target="deployment/x", params={"replicas": 3}
            )
        ),
        incident,
        "",
    )
    assert "'replicas': 3" in json.dumps(scale)


def test_slack_post_results() -> None:
    incident = parse_cloud_monitoring(cm_payload())
    inv = _investigation()
    assert SlackNotifier(FakeHttp(), "", "#incidents").post(inv, incident, "") == "skipped"
    http = FakeHttp({"chat.postMessage": ok({"ok": True})})
    assert SlackNotifier(http, "xoxb-test", "#incidents").post(inv, incident, "s") == "ok"
    call = http.calls[0]
    assert call["headers"] == {"Authorization": "Bearer xoxb-test"}
    assert (
        call["json_body"]["channel"] == "#incidents" and call["json_body"]["unfurl_links"] is False
    )
    assert (
        SlackNotifier(
            FakeHttp({"chat.postMessage": ok({"ok": False, "error": "not_in_channel"})}), "t", "c"
        ).post(inv, incident, "")
        == "error"
    )
    assert (
        SlackNotifier(FakeHttp({"chat.postMessage": down()}), "t", "c").post(inv, incident, "")
        == "error"
    )
    assert (
        SlackNotifier(FakeHttp({"chat.postMessage": text("bad", 500)}), "t", "c").post(
            inv, incident, ""
        )
        == "error"
    )


# --- Pub/Sub ------------------------------------------------------------------------------------

SUB = "projects/p/subscriptions/mamori-agent-incidents"


def _pull(*payloads: bytes) -> Response:
    return ok(
        {
            "receivedMessages": [
                {
                    "ackId": f"ack-{i}",
                    "message": {"data": base64.b64encode(p).decode(), "messageId": str(i)},
                }
                for i, p in enumerate(payloads)
            ]
        }
    )


def test_pull_handles_and_acks_everything() -> None:
    seen: list[bytes] = []

    def handle(data: bytes, meta: dict[str, Any]) -> None:
        if data == b"boom":
            raise RuntimeError("handler bug")
        seen.append(data)

    http = FakeHttp({":pull": _pull(b"one", b"boom"), ":acknowledge": ok({})})
    assert PubSubPuller(http, SUB, handle).pull_once() == 2
    assert seen == [b"one"]
    ack = http.calls[1]
    assert ack["url"] == f"https://pubsub.googleapis.com/v1/{SUB}:acknowledge"
    assert ack["json_body"] == {"ackIds": ["ack-0", "ack-1"]}


def test_pull_bad_base64_and_empty() -> None:
    seen: list[bytes] = []
    bad = ok({"receivedMessages": [{"ackId": "a", "message": {"data": "%%%"}}]})
    http = FakeHttp({":pull": bad, ":acknowledge": ok({})})
    PubSubPuller(http, SUB, lambda d, m: seen.append(d)).pull_once()
    assert seen == [b""]
    empty = FakeHttp({":pull": ok({})})
    assert PubSubPuller(empty, SUB, lambda d, m: None).pull_once() == 0
    assert len(empty.calls) == 1


def test_run_backs_off_on_errors_and_stops() -> None:
    stop = threading.Event()
    waits: list[float] = []

    class Stop(threading.Event):
        def wait(self, timeout: float | None = None) -> bool:
            waits.append(timeout or 0)
            if len(waits) >= 3:
                stop.set()
            return False

        def is_set(self) -> bool:
            return stop.is_set()

    http = FakeHttp(
        {":pull": [HttpError(503, "unavailable"), HttpError(503, "unavailable"), ok({})]}
    )
    puller = PubSubPuller(http, SUB, lambda d, m: None)
    puller.run(Stop())
    assert waits == [1.0, 2.0, 2.0]  # backoff 1 s, 2 s, then the idle wait after an empty pull
    assert puller.healthy is True
