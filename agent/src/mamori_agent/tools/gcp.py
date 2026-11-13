"""Read tools on Google Cloud APIs: Managed Prometheus (PromQL), Cloud Logging, Cloud Trace.

All three are plain REST calls with the pod's Workload Identity token. IAM: roles/monitoring.viewer,
roles/logging.viewer and roles/cloudtrace.user on the project (infra/bootstrap/agent_identity.tf);
none of them can write.
"""

from __future__ import annotations

import json
import math
import time
from datetime import UTC, datetime, timedelta
from typing import Any

from mamori_agent.http import Http, checked_json
from mamori_agent.models import rfc3339

MONITORING = "https://monitoring.googleapis.com/v1"
LOGGING = "https://logging.googleapis.com/v2"
TRACE = "https://cloudtrace.googleapis.com/v1"

_MAX_SERIES = 25
_MAX_POINTS = 6


def _labels(metric: dict[str, Any]) -> str:
    return ", ".join(f'{k}="{v}"' for k, v in sorted(metric.items()) if k != "__name__")


def _series_name(metric: dict[str, Any]) -> str:
    return f"{metric.get('__name__', '')}{{{_labels(metric)}}}"


def _fmt_value(raw: Any) -> str:
    try:
        number = float(raw)
    except (TypeError, ValueError):
        return str(raw)
    if math.isnan(number):
        return "NaN"
    return f"{number:.6g}"


def format_prometheus(data: dict[str, Any]) -> str:
    """Compact text for a Prometheus query answer (instant or range)."""
    if data.get("status") != "success":
        return f"query error ({data.get('errorType', 'unknown')}): {data.get('error', '')}"
    body = data.get("data") or {}
    kind = body.get("resultType")
    result = body.get("result")
    if kind in ("scalar", "string"):
        return f"{kind}: {_fmt_value(result[1]) if isinstance(result, list) else result}"
    if not result:
        return "no data (empty result): check metric and label names, or widen the window"
    lines: list[str] = []
    for series in result[:_MAX_SERIES]:
        metric = series.get("metric") or {}
        if kind == "matrix":
            values = series.get("values") or []
            numbers = [float(v) for _, v in values if _fmt_value(v) != "NaN"]
            step = max(1, len(values) // _MAX_POINTS)
            sampled = values[::step][: _MAX_POINTS - 1] + values[-1:]
            points = " ".join(
                f"{datetime.fromtimestamp(float(t), UTC):%H:%M}={_fmt_value(v)}" for t, v in sampled
            )
            summary = (
                f" min={_fmt_value(min(numbers))} max={_fmt_value(max(numbers))}" if numbers else ""
            )
            lines.append(f"{_series_name(metric)}{summary} | {points}")
        else:
            value = (series.get("value") or [None, None])[1]
            lines.append(f"{_series_name(metric)} = {_fmt_value(value)}")
    if len(result) > _MAX_SERIES:
        lines.append(f"... {len(result) - _MAX_SERIES} more series: aggregate with sum by (...)")
    return "\n".join(lines)


class GoogleReads:
    def __init__(self, http: Http, project_id: str, timeout: float) -> None:
        self._http = http
        self._project = project_id
        self._timeout = timeout

    # --- Managed Service for Prometheus --------------------------------------------------------

    def _prom(self, path: str, params: dict[str, Any]) -> str:
        url = f"{MONITORING}/projects/{self._project}/location/global/prometheus/api/v1/{path}"
        response = self._http.get(url, params=params, timeout=self._timeout)
        if response.status == 400:
            # PromQL errors come back as 400 with a Prometheus-shaped body; show them to the model.
            try:
                return format_prometheus(response.json())
            except ValueError:
                pass
        return format_prometheus(checked_json(response, "prometheus"))

    def promql(self, query: str) -> str:
        return self._prom("query", {"query": query})

    def promql_range(self, query: str, minutes: int, step_seconds: int) -> str:
        end = time.time()
        return self._prom(
            "query_range",
            {"query": query, "start": end - minutes * 60, "end": end, "step": f"{step_seconds}s"},
        )

    # --- Cloud Logging -------------------------------------------------------------------------

    def logs(self, log_filter: str, minutes: int, limit: int) -> str:
        since = rfc3339(datetime.now(UTC) - timedelta(minutes=minutes))
        full_filter = f'timestamp>="{since}"'
        if log_filter.strip():
            full_filter += f" AND ({log_filter})"
        body = {
            "resourceNames": [f"projects/{self._project}"],
            "filter": full_filter,
            "orderBy": "timestamp desc",
            "pageSize": limit,
        }
        data = checked_json(
            self._http.post(f"{LOGGING}/entries:list", json_body=body, timeout=self._timeout),
            "logging",
        )
        entries = data.get("entries") or []
        if not entries:
            return f"no log entries in the last {minutes} min for: {log_filter or '(all)'}"
        lines = [_format_entry(e) for e in entries[:limit]]
        return "\n".join(lines)

    # --- Cloud Trace ---------------------------------------------------------------------------

    def traces(self, trace_filter: str, minutes: int, limit: int) -> str:
        end = datetime.now(UTC)
        params: dict[str, Any] = {
            "view": "COMPLETE",
            "pageSize": limit,
            "startTime": rfc3339(end - timedelta(minutes=minutes)),
            "endTime": rfc3339(end),
            "orderBy": "duration desc",
        }
        if trace_filter.strip():
            params["filter"] = trace_filter
        data = checked_json(
            self._http.get(
                f"{TRACE}/projects/{self._project}/traces", params=params, timeout=self._timeout
            ),
            "trace",
        )
        traces = data.get("traces") or []
        if not traces:
            return f"no traces in the last {minutes} min for: {trace_filter or '(all)'}"
        return "\n".join(_format_trace(t) for t in traces[:limit])


def _format_entry(entry: dict[str, Any]) -> str:
    labels = (entry.get("resource") or {}).get("labels") or {}
    where = "/".join(
        p
        for p in (
            labels.get("namespace_name"),
            labels.get("pod_name"),
            labels.get("container_name"),
        )
        if p
    ) or (entry.get("resource") or {}).get("type", "?")
    if "textPayload" in entry:
        message = str(entry["textPayload"])
    elif isinstance(entry.get("jsonPayload"), dict):
        payload = entry["jsonPayload"]
        message = str(payload.get("message") or payload.get("msg") or json.dumps(payload))
    else:
        message = json.dumps(entry.get("protoPayload") or {})[:300]
    message = message.replace("\n", " ")[:400]
    return f"{entry.get('timestamp', '?')} {entry.get('severity', 'DEFAULT')} [{where}] {message}"


def _span_ms(span: dict[str, Any]) -> float:
    try:
        start = datetime.fromisoformat(str(span["startTime"]).replace("Z", "+00:00"))
        end = datetime.fromisoformat(str(span["endTime"]).replace("Z", "+00:00"))
    except (KeyError, ValueError):
        return 0.0
    return (end - start).total_seconds() * 1000


def _is_error_label(key: str, value: Any) -> bool:
    text = str(value)
    if key in ("error", "/error/message", "/error/name", "otel.status_code") and text != "OK":
        return True
    if key.endswith("status_code") and text.isdigit():
        code = int(text)
        # HTTP: 4xx and 5xx. gRPC (key mentions rpc or grpc): anything but 0 (OK).
        return code != 0 if "rpc" in key else code >= 400
    return False


def _format_trace(trace: dict[str, Any]) -> str:
    spans = trace.get("spans") or []
    if not spans:
        return f"trace {trace.get('traceId', '?')}: no spans"
    root = next((s for s in spans if not s.get("parentSpanId")), spans[0])
    slowest = sorted(spans, key=_span_ms, reverse=True)[:3]
    errors = sorted(
        {
            s.get("name", "?")
            for s in spans
            if any(_is_error_label(k, v) for k, v in (s.get("labels") or {}).items())
        }
    )
    parts = [
        f"trace {trace.get('traceId', '?')}: {root.get('name', '?')} {_span_ms(root):.0f} ms,",
        f"{len(spans)} spans; slowest: "
        + ", ".join(f"{s.get('name', '?')} {_span_ms(s):.0f} ms" for s in slowest),
    ]
    if errors:
        parts.append("; error spans: " + ", ".join(errors[:5]))
    return " ".join(parts)
