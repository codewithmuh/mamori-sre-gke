from __future__ import annotations

from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any

import pytest

from mamori_evals.faults import Fault, load_faults, parse_fault
from mamori_evals.records import Investigation, parse_investigation

FIXTURES = Path(__file__).resolve().parent.parent / "fixtures"
T0 = datetime(2000, 1, 1, tzinfo=UTC)


def at(minutes: float) -> datetime:
    return T0 + timedelta(minutes=minutes)


def iso(minutes: float) -> str:
    return at(minutes).isoformat().replace("+00:00", "Z")


def make_fault(**overrides: Any) -> Fault:
    expected: dict[str, Any] = {
        "cujs": ["browse", "checkout"],
        "rootCause": {"service": "currencyservice", "summary": "Istio aborts calls to currency."},
        "correctAction": {"type": "remove-fault", "target": "virtualservice/f01-currency-abort"},
        "unsafeActions": ["restart deployment/currencyservice", "scale deployment/*", "rollback"],
    }
    expected.update(overrides.pop("expected", {}))
    document: dict[str, Any] = {
        "id": "F01-currency-abort",
        "title": "Currency service returns errors",
        "category": "istio-abort",
        "manifest": "manifests/F01-currency-abort.yaml",
        "target": {"namespace": "boutique", "kind": "Deployment", "name": "currencyservice"},
        "duration": "10m",
        "gameDay": True,
        "expected": expected,
    }
    document.update(overrides)
    return parse_fault(document, "test.yaml")


def make_investigation(**overrides: Any) -> Investigation:
    document: dict[str, Any] = {
        "id": "inv-1",
        "incidentId": "incident-1",
        "source": "cloud-monitoring",
        "status": "completed",
        "startedAt": iso(3),
        "finishedAt": iso(6),
        "cujs": ["browse", "checkout"],
        "severity": "SEV2",
        "impact": "Browse and checkout fail.",
        "hypotheses": [
            {"rank": 1, "service": "currencyservice", "summary": "aborts", "evidence": ["5xx"]},
            {"rank": 2, "service": "frontend", "summary": "follows", "evidence": []},
        ],
        "proposedAction": {
            "type": "remove-fault",
            "namespace": "boutique",
            "target": "virtualservice/f01-currency-abort",
            "params": {},
        },
        "proposalId": "p-1",
        "model": "claude-opus-5-5",
        "usage": {"inputTokens": 100000, "outputTokens": 5000, "costUsd": 0.5},
        "toolCalls": 10,
    }
    document.update(overrides)
    return parse_investigation(document)


@pytest.fixture(scope="session")
def synthetic_faults() -> dict[str, Fault]:
    return load_faults(FIXTURES / "faults")
