from __future__ import annotations

import json
import sys
import threading
from collections.abc import Iterator, Sequence
from datetime import UTC, datetime, timedelta
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any, ClassVar
from urllib.parse import parse_qs, unquote, urlsplit

import pytest

from mamori_evals.clients import (
    AgentApi,
    ApiError,
    CommandResult,
    CommandToken,
    ExecutorApi,
    ExecutorLogs,
    FaultRefusedError,
    GamedayCli,
    GamedayError,
    JsonHttp,
    ManualFaultError,
    PrometheusApi,
    managed_prometheus_url,
    parse_executor_log,
    run_command,
    split_command,
)
from mamori_evals.jsonutil import DocumentError

from .conftest import make_investigation

FAKE_GAMEDAY = [sys.executable, str(Path(__file__).with_name("fake_gameday.py"))]


class Handler(BaseHTTPRequestHandler):
    routes: ClassVar[dict[str, tuple[int, Any]]] = {}
    seen: ClassVar[list[tuple[str, dict[str, list[str]], str | None]]] = []

    def do_GET(self) -> None:
        parts = urlsplit(self.path)
        path = unquote(parts.path)
        self.seen.append((path, parse_qs(parts.query), self.headers.get("Authorization")))
        status, body = self.routes.get(path, (404, {"error": "not found", "reason": "not-found"}))
        data = body.encode() if isinstance(body, str) else json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, format: str, *args: Any) -> None:
        pass


@pytest.fixture
def server() -> Iterator[str]:
    Handler.routes.clear()
    Handler.seen.clear()
    httpd = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    yield f"http://127.0.0.1:{httpd.server_address[1]}"
    httpd.shutdown()
    httpd.server_close()


def test_agent_api(server: str) -> None:
    Handler.routes["/v1/investigations"] = (200, {"investigations": [make_investigation().raw]})
    api = AgentApi(JsonHttp(server, token=lambda: "t0k"))
    (inv,) = api.investigations_since(datetime(2000, 1, 1, tzinfo=UTC))
    assert inv.id == "inv-1"
    _, query, auth = Handler.seen[-1]
    assert query == {"since": ["2000-01-01T00:00:00Z"], "limit": ["200"]}
    assert auth == "Bearer t0k"
    Handler.routes["/v1/investigations"] = (200, [make_investigation(id="bare").raw])
    assert api.investigations_since(datetime(2000, 1, 1, tzinfo=UTC))[0].id == "bare"


def test_executor_api(server: str) -> None:
    Handler.routes["/v1/proposals/p 1"] = (200, {"proposalId": "p 1", "status": "pending"})
    api = ExecutorApi(JsonHttp(server))
    proposal = api.proposal("p 1")
    assert proposal is not None
    assert proposal.status == "pending"
    assert api.proposal("missing") is None
    Handler.routes["/v1/proposals/boom"] = (500, {"error": "x"})
    with pytest.raises(ApiError, match="HTTP 500") as err:
        api.proposal("boom")
    assert err.value.status == 500


def test_http_errors(server: str) -> None:
    Handler.routes["/text"] = (200, "not json")
    with pytest.raises(ApiError, match="not JSON"):
        JsonHttp(server).get("/text")
    with pytest.raises(ValueError, match="http or https"):
        JsonHttp("file:///etc/passwd")
    with pytest.raises(ApiError) as err:
        JsonHttp("http://127.0.0.1:9", timeout=2).get("/x")
    assert err.value.status is None


def test_prometheus(server: str) -> None:
    Handler.routes["/prom/api/v1/query"] = (
        200,
        {
            "status": "success",
            "data": {
                "resultType": "vector",
                "result": [
                    {"metric": {"cuj": "browse"}, "value": [1, "0.98"]},
                    {"metric": {"cuj": "checkout"}, "value": [1, "NaN"]},
                ],
            },
        },
    )
    series = PrometheusApi(JsonHttp(server + "/prom")).query("up")
    assert series[0] == ({"cuj": "browse"}, 0.98)
    assert Handler.seen[-1][1] == {"query": ["up"]}
    Handler.routes["/prom/api/v1/query"] = (200, {"status": "error", "error": "bad query"})
    with pytest.raises(ApiError, match="bad query"):
        PrometheusApi(JsonHttp(server + "/prom")).query("up")
    Handler.routes["/prom/api/v1/query"] = (
        200,
        {"status": "success", "data": {"result": [{"metric": {}, "value": [1]}]}},
    )
    with pytest.raises(DocumentError, match="unexpected sample"):
        PrometheusApi(JsonHttp(server + "/prom")).query("up")


def test_managed_prometheus_url() -> None:
    assert managed_prometheus_url("mamori-lab-123456") == (
        "https://monitoring.googleapis.com/v1/projects/mamori-lab-123456/location/global/prometheus"
    )


def test_command_token_refreshes() -> None:
    calls: list[Sequence[str]] = []
    clock = [0.0]

    def runner(argv: Sequence[str], timeout: float) -> CommandResult:
        calls.append(argv)
        return CommandResult(0, f"token-{len(calls)}\n", "")

    token = CommandToken(
        ["gcloud", "auth", "print-access-token"], timedelta(minutes=30), runner, lambda: clock[0]
    )
    assert token() == "token-1"
    clock[0] = 60
    assert token() == "token-1"
    clock[0] = 1800
    assert token() == "token-2"

    failing = CommandToken(
        ["x"], runner=lambda argv, timeout: CommandResult(1, "", "not logged in")
    )
    with pytest.raises(ApiError, match="not logged in"):
        failing()


def test_run_and_split_command() -> None:
    result = run_command([sys.executable, "-c", "print('hi')"], 30)
    assert result == CommandResult(0, "hi\n", "")
    assert split_command("kubectl -n 'mamori agent' logs") == [
        "kubectl",
        "-n",
        "mamori agent",
        "logs",
    ]
    with pytest.raises(ValueError, match="empty"):
        split_command("  ")


def test_executor_log_parsing() -> None:
    text = "\n".join(
        [
            "starting up",
            json.dumps({"event": "slack_connected"}),
            "{not json",
            json.dumps({"event": "blocked", "proposalId": "p", "reason": "kill-switch"}),
            json.dumps(["list"]),
            json.dumps({"event": "proposal_received", "proposalId": "p"}),
        ]
    )
    assert [e.event for e in parse_executor_log(text)] == ["blocked", "proposal_received"]


def test_executor_logs_command() -> None:
    seen: list[Sequence[str]] = []

    def runner(argv: Sequence[str], timeout: float) -> CommandResult:
        seen.append(argv)
        return CommandResult(0, json.dumps({"event": "executed", "proposalId": "p"}), "")

    logs = ExecutorLogs(["kubectl", "logs", "deploy/executor"], runner)
    assert len(logs.events_since(datetime(2000, 1, 1, tzinfo=UTC))) == 1
    assert seen[0][-1] == "--since-time=2000-01-01T00:00:00Z"
    broken = ExecutorLogs(["kubectl"], lambda argv, timeout: CommandResult(1, "", "forbidden"))
    with pytest.raises(ApiError, match="forbidden"):
        broken.events_since(datetime(2000, 1, 1, tzinfo=UTC))


class TestGameday:
    def test_start_and_stop_by_signal(self) -> None:
        cli = GamedayCli(FAKE_GAMEDAY)
        handle = cli.start("F01-ok", timedelta(minutes=22))
        assert handle.run_id == "run-1"
        assert handle.started_at == datetime(2000, 1, 1, tzinfo=UTC)
        assert handle.objects == ("virtualservice/f01-currency-abort",)
        assert handle.timeline == "gameday-runs/run-1.json"
        assert handle.output == ["applying objects"]
        assert handle.ended() is None
        end = handle.stop()
        assert end.reason == "signal"
        assert end.at is not None
        assert handle.stop().reason == "signal"  # stopping twice is harmless

    def test_removed_externally(self) -> None:
        handle = GamedayCli(FAKE_GAMEDAY).start("F05-quick", timedelta(minutes=5))
        handle.process.wait(timeout=10)
        end = handle.ended()
        assert end is not None
        assert end.reason == "removed-externally"

    def test_kill_when_it_ignores_sigterm(self) -> None:
        handle = GamedayCli(FAKE_GAMEDAY).start("F08-stubborn", timedelta(minutes=5))
        end = handle.stop(timeout=0.5)
        assert end.reason.startswith("exit -")

    @pytest.mark.parametrize(
        ("fault", "error", "text"),
        [
            ("F02-refused", FaultRefusedError, "another fault is applied"),
            (
                "F03-manual",
                ManualFaultError,
                "To inject:\n  1. drain the node\nTo revert:\n  1. uncordon",
            ),
            ("F04-crash", GamedayError, ""),
        ],
    )
    def test_start_failures(self, fault: str, error: type[GamedayError], text: str) -> None:
        with pytest.raises(error) as err:
            GamedayCli(FAKE_GAMEDAY).start(fault, timedelta(minutes=5))
        assert text in err.value.output

    def test_no_started_line(self) -> None:
        with pytest.raises(GamedayError, match="no 'started' line"):
            GamedayCli(FAKE_GAMEDAY, start_timeout=0.5).start("F06-silent", timedelta(minutes=5))

    def test_stop_commands(self) -> None:
        cli = GamedayCli(FAKE_GAMEDAY)
        cli.stop("F01-ok")
        cli.stop_all()
        with pytest.raises(GamedayError, match="stop F07-stuck failed"):
            cli.stop("F07-stuck")
