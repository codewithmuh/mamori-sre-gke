"""Fakes for every external call: HTTP (Google, Kubernetes, GitHub, executor, Slack, Pub/Sub)
and the model API. No test touches the network."""

from __future__ import annotations

import json
from collections.abc import Callable
from types import SimpleNamespace
from typing import Any

import pytest

from mamori_agent.config import Settings
from mamori_agent.http import HttpError, Response


class FakeHttp:
    """Answers by the first matching URL substring; records every call."""

    def __init__(self, routes: dict[str, Any] | None = None) -> None:
        self.routes: dict[str, Any] = dict(routes or {})
        self.calls: list[dict[str, Any]] = []

    def _answer(self, method: str, url: str, **kwargs: Any) -> Response:
        self.calls.append({"method": method, "url": url, **kwargs})
        for fragment, route in self.routes.items():
            if fragment in url:
                answer = route(method, url, kwargs) if callable(route) else route
                if isinstance(answer, Exception):
                    raise answer
                if isinstance(answer, list):  # a sequence of answers, one per call
                    item = answer.pop(0) if len(answer) > 1 else answer[0]
                    if isinstance(item, Exception):
                        raise item
                    return item
                return answer
        return Response(404, b'{"error": "no route"}')

    def get(self, url: str, **kwargs: Any) -> Response:
        return self._answer("GET", url, **kwargs)

    def post(self, url: str, **kwargs: Any) -> Response:
        return self._answer("POST", url, **kwargs)


def ok(payload: Any, status: int = 200) -> Response:
    return Response(status, json.dumps(payload).encode())


def text(body: str, status: int = 200) -> Response:
    return Response(status, body.encode())


def down(message: str = "connection refused") -> HttpError:
    return HttpError(0, message)


# --- model fakes ------------------------------------------------------------------------------


def text_block(value: str) -> SimpleNamespace:
    return SimpleNamespace(type="text", text=value)


def tool_block(block_id: str, name: str, tool_input: dict[str, Any]) -> SimpleNamespace:
    return SimpleNamespace(type="tool_use", id=block_id, name=name, input=tool_input)


def thinking_block() -> SimpleNamespace:
    return SimpleNamespace(type="thinking", thinking="", signature="sig")


def usage(fresh: int = 1000, written: int = 0, read: int = 0, out: int = 200) -> SimpleNamespace:
    return SimpleNamespace(
        input_tokens=fresh,
        cache_creation_input_tokens=written,
        cache_read_input_tokens=read,
        output_tokens=out,
    )


def response(
    content: list[Any],
    stop: str = "end_turn",
    model: str = "claude-opus-5-5",
    use: SimpleNamespace | None = None,
    category: str | None = None,
) -> SimpleNamespace:
    details = SimpleNamespace(category=category) if stop == "refusal" else None
    return SimpleNamespace(
        content=content, stop_reason=stop, model=model, usage=use or usage(), stop_details=details
    )


def findings(**overrides: Any) -> dict[str, Any]:
    data: dict[str, Any] = {
        "summary": "Checkout fails because currencyservice returns errors.",
        "cujs": ["checkout", "browse"],
        "severity": "SEV1",
        "impact": "About 60 % of checkouts fail since 10:02.",
        "hypotheses": [
            {
                "rank": 2,
                "service": "frontend",
                "summary": "frontend bug",
                "confidence": 0.1,
                "evidence": [],
            },
            {
                "rank": 1,
                "service": "currencyservice",
                "summary": "Istio abort rule on currencyservice",
                "confidence": 0.85,
                "evidence": ["virtualservice/f01-currency-abort aborts 50 %"],
            },
        ],
        "proposedAction": {
            "type": "remove-fault",
            "namespace": "boutique",
            "target": "virtualservice/f01-currency-abort",
            "replicas": None,
            "revision": None,
            "rationale": "Remove the injected abort rule.",
        },
        "relatedPostmortems": [],
    }
    data.update(overrides)
    return data


def final(data: dict[str, Any] | None = None, **kw: Any) -> SimpleNamespace:
    return response([thinking_block(), text_block(json.dumps(data or findings()))], **kw)


class FakeModel:
    """Returns scripted responses in order and keeps a snapshot of each request."""

    def __init__(self, responses: list[Any]) -> None:
        self.responses = list(responses)
        self.calls: list[dict[str, Any]] = []

    def __call__(self, params: dict[str, Any], timeout: float) -> Any:
        snapshot = dict(params)
        snapshot["messages"] = list(params["messages"])
        snapshot["timeout"] = timeout
        self.calls.append(snapshot)
        item = self.responses.pop(0)
        if isinstance(item, Exception):
            raise item
        return item


@pytest.fixture
def settings() -> Settings:
    return Settings(
        project_id="mamori-lab-123456",
        pubsub_enabled=False,
        knowledge_source="local",
        executor_token_file="",
        k8s_token_file="",
    )


@pytest.fixture
def make_settings() -> Callable[..., Settings]:
    def build(**kw: Any) -> Settings:
        base: dict[str, Any] = {
            "project_id": "mamori-lab-123456",
            "pubsub_enabled": False,
            "knowledge_source": "local",
            "executor_token_file": "",
            "k8s_token_file": "",
        }
        base.update(kw)
        return Settings(**base)

    return build


def cm_payload(**incident: Any) -> dict[str, Any]:
    base: dict[str, Any] = {
        "incident_id": "0.abc123",
        "state": "open",
        "started_at": 1_765_000_000,
        "policy_name": "CUJ checkout: mesh availability fast burn",
        "condition_name": "fast burn: 1h lookback",
        "severity": "CRITICAL",
        "summary": "Burn rate above 13.44x",
        "url": "https://console.cloud.google.com/monitoring/alerting/incidents/0.abc123",
        "policy_user_labels": {
            "generator": "slo-gen",
            "cuj": "checkout",
            "signal": "mesh",
            "sli": "availability",
            "alert": "burn-rate",
            "burn": "fast",
            "severity": "page",
        },
        "documentation": {
            "subject": "CUJ checkout burning",
            "content": "Runbook: docs/runbooks/cuj-checkout.md",
            "mime_type": "text/markdown",
        },
    }
    base.update(incident)
    return {"incident": base, "version": "1.2"}
