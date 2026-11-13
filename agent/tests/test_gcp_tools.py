from __future__ import annotations

import pytest

from conftest import FakeHttp, ok, text
from mamori_agent.http import HttpError
from mamori_agent.tools.gcp import GoogleReads, format_prometheus

PROJECT = "mamori-lab-123456"


def test_format_vector_scalar_error_and_empty() -> None:
    vector = {
        "status": "success",
        "data": {
            "resultType": "vector",
            "result": [
                {"metric": {"__name__": "up", "job": "x"}, "value": [1, "1"]},
                {"metric": {"job": "y"}, "value": [1, "NaN"]},
            ],
        },
    }
    out = format_prometheus(vector)
    assert 'up{job="x"} = 1' in out
    assert '{job="y"} = NaN' in out
    assert (
        format_prometheus(
            {"status": "success", "data": {"resultType": "scalar", "result": [1, "2.5"]}}
        )
        == "scalar: 2.5"
    )
    assert (
        format_prometheus({"status": "error", "errorType": "bad_data", "error": "parse error"})
        == "query error (bad_data): parse error"
    )
    assert format_prometheus(
        {"status": "success", "data": {"resultType": "vector", "result": []}}
    ).startswith("no data")


def test_format_matrix_summarises_and_limits_series() -> None:
    values = [[1_765_000_000 + 60 * i, str(i)] for i in range(30)]
    result = [{"metric": {"destination_workload": f"svc{i}"}, "values": values} for i in range(30)]
    out = format_prometheus(
        {"status": "success", "data": {"resultType": "matrix", "result": result}}
    )
    lines = out.splitlines()
    assert len(lines) == 26
    assert "min=0 max=29" in lines[0]
    assert lines[-1].startswith("... 5 more series")


def test_promql_calls_managed_prometheus() -> None:
    http = FakeHttp(
        {
            "/prometheus/api/v1/query": ok(
                {"status": "success", "data": {"resultType": "vector", "result": []}}
            )
        }
    )
    reads = GoogleReads(http, PROJECT, 5)
    reads.promql("up")
    call = http.calls[0]
    assert call["url"] == (
        f"https://monitoring.googleapis.com/v1/projects/{PROJECT}/location/global/prometheus/api/v1/query"
    )
    assert call["params"] == {"query": "up"}
    reads.promql_range("up", 30, 60)
    assert http.calls[1]["url"].endswith("/query_range")
    assert http.calls[1]["params"]["step"] == "60s"


def test_promql_400_is_shown_and_other_errors_raise() -> None:
    bad = ok({"status": "error", "errorType": "bad_data", "error": "unexpected )"}, status=400)
    assert "unexpected )" in GoogleReads(FakeHttp({"query": bad}), PROJECT, 5).promql("up)")
    with pytest.raises(HttpError, match="400: prometheus: not json"):
        GoogleReads(FakeHttp({"query": text("not json", 400)}), PROJECT, 5).promql("x")
    with pytest.raises(HttpError, match="403"):
        GoogleReads(FakeHttp({"query": text("denied", 403)}), PROJECT, 5).promql("up")


def test_logs_search_formats_entries() -> None:
    entries = {
        "entries": [
            {
                "timestamp": "2026-12-10T05:01:02Z",
                "severity": "ERROR",
                "resource": {
                    "type": "k8s_container",
                    "labels": {
                        "namespace_name": "boutique",
                        "pod_name": "p",
                        "container_name": "server",
                    },
                },
                "textPayload": "rpc error: code = Unavailable\nsecond line",
            },
            {
                "timestamp": "t2",
                "jsonPayload": {"message": "json message"},
                "resource": {"type": "k8s_node"},
            },
            {"timestamp": "t3", "jsonPayload": {"other": 1}},
            {"timestamp": "t4", "protoPayload": {"methodName": "delete"}},
        ]
    }
    http = FakeHttp({"entries:list": ok(entries)})
    out = GoogleReads(http, PROJECT, 5).logs('resource.type="k8s_container"', 30, 10)
    body = http.calls[0]["json_body"]
    assert body["resourceNames"] == [f"projects/{PROJECT}"]
    assert (
        body["filter"].startswith('timestamp>="')
        and 'AND (resource.type="k8s_container")' in body["filter"]
    )
    lines = out.splitlines()
    expected = "ERROR [boutique/p/server] rpc error: code = Unavailable second line"
    assert lines[0] == f"2026-12-10T05:01:02Z {expected}"
    assert "[k8s_node] json message" in lines[1]
    assert '{"other": 1}' in lines[2]
    assert "delete" in lines[3]


def test_logs_search_empty() -> None:
    out = GoogleReads(FakeHttp({"entries:list": ok({})}), PROJECT, 5).logs("", 10, 5)
    assert out == "no log entries in the last 10 min for: (all)"


def test_traces_summary_with_error_spans() -> None:
    trace = {
        "traceId": "t1",
        "spans": [
            {
                "spanId": "1",
                "name": "/cart/checkout",
                "startTime": "2026-12-10T05:00:00Z",
                "endTime": "2026-12-10T05:00:02Z",
            },
            {
                "spanId": "2",
                "parentSpanId": "1",
                "name": "hipstershop.CurrencyService/Convert",
                "startTime": "2026-12-10T05:00:00Z",
                "endTime": "2026-12-10T05:00:01.5Z",
                "labels": {"rpc.grpc.status_code": "14"},
            },
            {
                "spanId": "3",
                "parentSpanId": "1",
                "name": "http",
                "labels": {"/http/status_code": "503"},
            },
            {
                "spanId": "4",
                "parentSpanId": "1",
                "name": "ok",
                "labels": {"/http/status_code": "200", "rpc.grpc.status_code": "0"},
            },
        ],
    }
    http = FakeHttp({"/traces": ok({"traces": [trace, {"traceId": "empty"}]})})
    out = GoogleReads(http, PROJECT, 5).traces("root:/cart/checkout", 30, 5)
    assert http.calls[0]["params"]["filter"] == "root:/cart/checkout"
    first, second = out.splitlines()
    assert first.startswith(
        "trace t1: /cart/checkout 2000 ms, 4 spans; slowest: /cart/checkout 2000 ms"
    )
    assert "error spans: hipstershop.CurrencyService/Convert, http" in first
    assert second == "trace empty: no spans"
    assert (
        GoogleReads(FakeHttp({"/traces": ok({})}), PROJECT, 5)
        .traces("", 5, 5)
        .startswith("no traces")
    )
