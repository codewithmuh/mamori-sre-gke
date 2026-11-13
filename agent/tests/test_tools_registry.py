from __future__ import annotations

from pathlib import Path

from conftest import FakeHttp, ok, text
from mamori_agent.http import HttpError
from mamori_agent.tools import NoInput, PodsInput, Tool, ToolRegistry, build_tools
from mamori_agent.tools.gcp import GoogleReads
from mamori_agent.tools.knowledge import KnowledgeReads, LocalKnowledge
from mamori_agent.tools.kube import KubeClient, KubeReads


def _registry(tmp_path: Path, chaos: bool = True, http: FakeHttp | None = None) -> ToolRegistry:
    http = http or FakeHttp()
    tools = build_tools(
        GoogleReads(http, "p", 5),
        KubeReads(KubeClient(http, "https://k", "", 5), ["boutique"], False),
        KnowledgeReads(LocalKnowledge(str(tmp_path)), True),
        lambda: [],
        chaos,
    )
    return ToolRegistry(tools, 300)


def test_schemas_are_stable_and_complete(tmp_path: Path) -> None:
    reg = _registry(tmp_path)
    schemas = reg.schemas()
    names = [s["name"] for s in schemas]
    assert names == sorted(names)
    assert len(names) == 16 and "chaos_experiments" in names
    for s in schemas:
        assert s["input_schema"]["type"] == "object"
        assert s["input_schema"].get("additionalProperties") is False
        assert "title" not in s["input_schema"]
        assert len(s["description"]) > 20
    assert schemas == _registry(tmp_path).schemas()
    assert "chaos_experiments" not in _registry(tmp_path, chaos=False).names


def test_no_write_tools_exist(tmp_path: Path) -> None:
    for name in _registry(tmp_path).names:
        assert not any(
            verb in name for verb in ("delete", "patch", "apply", "scale", "restart", "exec")
        )


def test_run_validates_input(tmp_path: Path) -> None:
    reg = _registry(tmp_path)
    unknown = reg.run("kubectl_delete", {})
    assert unknown.is_error and unknown.result == "unknown"
    bad = reg.run("k8s_pods", {"namespace": "Not A Namespace!", "extra": 1})
    assert bad.is_error and bad.result == "invalid"
    assert "namespace" in bad.text and "extra" in bad.text
    rollout = reg.run("k8s_rollout_history", {})
    assert rollout.result == "invalid"


def test_run_wraps_redacts_and_caps(tmp_path: Path) -> None:
    secret_log = "token=abcdef123456 " + "x" * 1000
    http = FakeHttp(
        {"/log": text(secret_log), "/pods": ok({"items": [{"metadata": {"name": "frontend-1"}}]})}
    )
    out = _registry(tmp_path, http=http).run("k8s_pod_logs", {"pod": "frontend-1"})
    assert out.result == "ok" and not out.is_error
    assert out.text.startswith('<untrusted-data source="pod-logs">')
    assert "abcdef123456" not in out.text
    assert "more characters cut" in out.text


def test_run_turns_errors_into_tool_errors(tmp_path: Path) -> None:
    http = FakeHttp({"monitoring.googleapis.com": text("permission denied", 403)})
    out = _registry(tmp_path, http=http).run("promql_query", {"query": "up"})
    assert out.is_error and out.result == "error"
    assert "ERROR: HTTP 403" in out.text

    def boom(_: object) -> str:
        raise HttpError(0, "timeout")

    reg = ToolRegistry([Tool("t", "a test tool that fails", NoInput, boom, "x")], 100)
    assert reg.run("t", None).text.endswith("ERROR: timeout\n</untrusted-data>")


def test_every_tool_dispatches(tmp_path: Path) -> None:
    http = FakeHttp(
        {
            "/": ok(
                {"items": [], "status": "success", "data": {"resultType": "vector", "result": []}}
            )
        }
    )
    reg = _registry(tmp_path, http=http)
    calls = {
        "promql_query": {"query": "up"},
        "promql_range": {"query": "up"},
        "logs_search": {},
        "traces_search": {},
        "k8s_pods": {},
        "k8s_events": {},
        "k8s_deployments": {},
        "k8s_rollout_history": {"deployment": "frontend"},
        "k8s_pod_logs": {"pod": "frontend"},
        "k8s_nodes": {},
        "argocd_applications": {},
        "istio_config": {},
        "cuj_map": {},
        "slo_spec": {},
        "past_incidents": {"query": "redis"},
        "chaos_experiments": {},
    }
    assert set(calls) == set(reg.names)
    for name, args in calls.items():
        assert reg.run(name, args).result in ("ok", "error"), name
    assert PodsInput().namespace == "boutique"
