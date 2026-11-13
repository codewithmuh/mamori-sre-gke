from __future__ import annotations

import json
import logging
from pathlib import Path
from typing import Any

import pytest
import requests

from conftest import FakeHttp, FakeModel, cm_payload, final, ok
from mamori_agent import app, cli, llm, logs
from mamori_agent.config import Settings, in_cluster
from mamori_agent.http import HttpError, RequestsHttp, Response, checked_json
from mamori_agent.llm import (
    PRICES,
    ConfigError,
    UsageMeter,
    make_caller,
    price_for,
    regional_premium,
)


def test_settings_from_environment(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("AGENT_MODEL", " claude-sonnet-5-5 ")
    monkeypatch.setenv("AGENT_LOG_NAMESPACES", "boutique, prober,,")
    monkeypatch.setenv("AGENT_HIDE_FAULT_LABELS", "true")
    monkeypatch.setenv("AGENT_SLACK_BOT_TOKEN", "xoxb-secret")
    s = Settings()
    assert s.model == "claude-sonnet-5-5"
    assert s.log_namespace_list == ["boutique", "prober"]
    assert s.hide_fault_labels is True
    assert "xoxb-secret" not in repr(s)
    assert (s.provider, s.vertex_region, s.effort, s.fallback_model) == (
        "vertex",
        "global",
        "medium",
        "claude-opus-5",
    )


def test_project_resolution(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("AGENT_PROJECT_ID", raising=False)
    monkeypatch.setenv("GOOGLE_CLOUD_PROJECT", "from-env")
    assert Settings().resolved_project_id() == "from-env"
    assert Settings(project_id="explicit").resolved_project_id() == "explicit"


def test_in_cluster(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("KUBERNETES_SERVICE_HOST", raising=False)
    assert in_cluster() is False
    monkeypatch.setenv("KUBERNETES_SERVICE_HOST", "10.30.0.1")
    assert in_cluster() is True


def test_prices_and_meter() -> None:
    assert price_for("claude-opus-5-5") == PRICES["claude-opus-5-5"]
    assert price_for("claude-unknown") == llm._FALLBACK_PRICE
    meter = UsageMeter()
    assert meter.add("claude-opus-5-5", None) == 0.0
    meter.add(
        "claude-sonnet-5-5", type("U", (), {"input_tokens": 1_000_000, "output_tokens": 100_000})()
    )
    assert meter.cost_usd == pytest.approx(3.0)
    assert meter.by_model == {"claude-opus-5-5": 0.0, "claude-sonnet-5-5": pytest.approx(3.0)}


def test_regional_premium(make_settings: Any) -> None:
    assert regional_premium(make_settings()) == 1.0
    assert regional_premium(make_settings(vertex_region="us")) == 1.1
    assert regional_premium(make_settings(provider="anthropic", vertex_region="us")) == 1.0


def test_make_caller(monkeypatch: pytest.MonkeyPatch, make_settings: Any) -> None:
    with pytest.raises(ConfigError, match="project"):
        make_caller(make_settings(), "")
    assert callable(make_caller(make_settings(), "mamori-lab-123456"))
    monkeypatch.setenv("KUBERNETES_SERVICE_HOST", "10.30.0.1")
    with pytest.raises(ConfigError, match="local runs"):
        make_caller(make_settings(provider="anthropic"), "")
    monkeypatch.delenv("KUBERNETES_SERVICE_HOST")
    monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-ant-test")
    assert callable(make_caller(make_settings(provider="anthropic"), ""))


def test_requests_http_adapter() -> None:
    class Session(requests.Session):
        def __init__(self, fail: bool) -> None:
            super().__init__()
            self.fail = fail

        def request(
            self, method: str | bytes, url: str | bytes, *args: Any, **kwargs: Any
        ) -> requests.Response:
            if self.fail:
                raise requests.ConnectionError("down")
            r = requests.Response()
            r.status_code = 200
            r._content = b'{"a": 1}'
            return r

    http = RequestsHttp(Session(False), verify="/ca.crt")
    assert http.get("https://x", timeout=1).json() == {"a": 1}
    assert http.post("https://x", json_body={}, timeout=1).text == '{"a": 1}'
    broken = RequestsHttp(Session(True))
    with pytest.raises(HttpError, match="ConnectionError"):
        broken.get("https://x", timeout=1)
    with pytest.raises(HttpError):
        broken.post("https://x", timeout=1)
    with pytest.raises(HttpError, match="not JSON"):
        checked_json(Response(200, b"<html>"), "x")
    assert Response(204, b"").json() is None


def test_json_logs(capsys: pytest.CaptureFixture[str]) -> None:
    logs.setup("INFO")
    logger = logging.getLogger("t")
    logs.event(logger, "investigation_completed", id="inv-1", usage={"costUsd": 0.1})
    try:
        raise ValueError("bad")
    except ValueError:
        logger.exception("failed")
    lines = [json.loads(line) for line in capsys.readouterr().out.splitlines()]
    assert lines[0]["event"] == "investigation_completed" and lines[0]["severity"] == "INFO"
    assert lines[0]["id"] == "inv-1" and lines[0]["usage"] == {"costUsd": 0.1}
    assert lines[0]["time"].endswith("Z")
    assert lines[1]["exception"] == "ValueError: bad"
    logging.getLogger().handlers.clear()


def test_build_wires_everything(make_settings: Any, tmp_path: Path) -> None:
    settings = make_settings(repo_dir=str(tmp_path), propose=False, slack=False)
    agent = app.build(
        settings,
        {"google": (FakeHttp(), "p"), "caller": FakeModel([final()]), "kube_http": FakeHttp()},
    )
    assert agent.project_id == "p"
    record = agent.service.investigate(cli.parse_any(cm_payload()))
    assert record.status == "completed" and record.proposal_id is None
    github = app.build(
        make_settings(knowledge_source="github", chaos_tool=False),
        {
            "google": (FakeHttp(), "p"),
            "caller": FakeModel([]),
            "http": FakeHttp({"/": ok([])}),
            "kube_http": FakeHttp(),
        },
    )
    assert github.service is not None


def test_google_http_without_credentials(
    monkeypatch: pytest.MonkeyPatch, make_settings: Any
) -> None:
    import google.auth
    from google.auth.exceptions import DefaultCredentialsError

    def no_adc(**_: Any) -> Any:
        raise DefaultCredentialsError("no ADC")

    monkeypatch.setattr(google.auth, "default", no_adc)
    http, project = app.google_http(make_settings(project_id="p"))
    assert project == "p"
    with pytest.raises(HttpError, match="no Google credentials"):
        http.get("https://x", timeout=1)
    with pytest.raises(HttpError):
        http.post("https://x", timeout=1)

    class Creds:
        pass

    monkeypatch.setattr(google.auth, "default", lambda **_: (Creds(), "adc-project"))
    http, project = app.google_http(make_settings(project_id=""))
    assert project == "adc-project" and isinstance(http, RequestsHttp)


def test_kube_http_uses_ca_when_present(make_settings: Any, tmp_path: Path) -> None:
    ca = tmp_path / "ca.crt"
    ca.write_text("x")
    assert app.kube_http(make_settings(k8s_ca_file=str(ca)))._verify == str(ca)
    assert app.kube_http(make_settings(k8s_api_url="http://127.0.0.1:8001"))._verify is True


def test_cli_parse_incident(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    f = tmp_path / "alert.json"
    f.write_text(json.dumps(cm_payload()))
    assert cli.main(["parse-incident", str(f)]) == 0
    assert json.loads(capsys.readouterr().out)["cujs"] == ["checkout"]
    f.write_text("[1]")
    assert cli.main(["parse-incident", str(f)]) == 2
    assert cli.main(["parse-incident", str(tmp_path / "missing.json")]) == 2


def test_cli_investigate(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    seen: list[Settings] = []
    real_build = app.build

    def fake_build(settings: Settings, overrides: Any = None) -> Any:
        seen.append(settings)
        return real_build(
            settings,
            {"google": (FakeHttp(), "p"), "caller": FakeModel([final()]), "kube_http": FakeHttp()},
        )

    monkeypatch.setattr(cli, "build", fake_build)
    monkeypatch.setenv("AGENT_KNOWLEDGE_SOURCE", "local")
    monkeypatch.setenv("AGENT_REPO_DIR", str(tmp_path))
    assert cli.main(["investigate", "--demo", "--no-propose", "--no-slack"]) == 0
    out = capsys.readouterr().out
    record = json.loads(out[out.index("{\n") :])
    assert record["incidentId"] == "demo-checkout-fast-burn" and record["status"] == "completed"
    assert seen[0].propose is False and seen[0].slack is False and seen[0].pubsub_enabled is False
    logging.getLogger().handlers.clear()


def test_on_message(monkeypatch: pytest.MonkeyPatch) -> None:
    submitted: list[Any] = []
    agent = type(
        "A", (), {"service": type("S", (), {"submit": staticmethod(submitted.append)})()}
    )()
    cli._on_message(agent, json.dumps(cm_payload()).encode(), {"messageId": "1"})
    cli._on_message(agent, b"garbage", {"messageId": "2"})
    assert len(submitted) == 1
