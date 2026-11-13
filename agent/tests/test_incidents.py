from __future__ import annotations

import base64
import json

import pytest

from conftest import cm_payload
from mamori_agent.incidents import (
    InvalidIncident,
    baseline_impact,
    baseline_severity,
    parse_alertmanager,
    parse_any,
    parse_cloud_monitoring,
    parse_manual,
    parse_pubsub_data,
)


def test_cloud_monitoring_open_incident_reads_slo_gen_labels() -> None:
    inc = parse_cloud_monitoring(cm_payload())
    assert inc.incident_id == "0.abc123"
    assert inc.source == "cloud-monitoring"
    assert inc.state == "open"
    assert inc.cujs == ["checkout"]
    assert (inc.burn, inc.sli, inc.signal, inc.alert_kind) == (
        "fast",
        "availability",
        "mesh",
        "burn-rate",
    )
    assert inc.alert_severity == "CRITICAL"
    assert inc.started_at.year == 2025
    assert "cuj-checkout.md" in inc.documentation
    assert inc.url.startswith("https://console.cloud.google.com")


def test_cloud_monitoring_cuj_from_text_when_label_missing() -> None:
    payload = cm_payload(policy_user_labels={}, policy_name="CUJ view-cart: prober no data")
    inc = parse_cloud_monitoring(payload)
    assert inc.cujs == ["view-cart", "checkout"]  # name, then documentation subject


def test_cloud_monitoring_closed_and_renotify() -> None:
    closed = parse_cloud_monitoring(cm_payload(state="closed", ended_at=1_765_000_600))
    assert closed.state == "closed"
    again = parse_cloud_monitoring(cm_payload(renotify=True))
    assert again.renotify is True


@pytest.mark.parametrize(
    ("payload", "message"),
    [
        ({"version": "1.2"}, "no 'incident'"),
        (cm_payload(incident_id=""), "incident_id"),
        (cm_payload(state="acknowledged"), "unknown state"),
        (cm_payload(started_at="yesterday"), "Unix time"),
    ],
)
def test_cloud_monitoring_invalid(payload: dict, message: str) -> None:
    with pytest.raises(InvalidIncident, match=message):
        parse_cloud_monitoring(payload)


def test_cloud_monitoring_odd_types_are_tolerated() -> None:
    inc = parse_cloud_monitoring(cm_payload(documentation="not an object", policy_user_labels=[1]))
    assert inc.documentation == ""
    assert inc.labels == {}


def test_pubsub_data() -> None:
    inc = parse_pubsub_data(json.dumps(cm_payload()).encode())
    assert inc.incident_id == "0.abc123"
    with pytest.raises(InvalidIncident, match="not JSON"):
        parse_pubsub_data(b"\xff\xfe")
    with pytest.raises(InvalidIncident, match="not a JSON object"):
        parse_pubsub_data(b"[1]")
    assert base64.b64encode(b"x")  # keeps the import honest for readers


def test_alertmanager_alert() -> None:
    inc = parse_alertmanager(
        {
            "status": "firing",
            "labels": {"alertname": "CUJ browse burn", "severity": "critical", "cuj": "browse"},
            "annotations": {
                "summary": "browse failing",
                "runbook_url": "docs/runbooks/cuj-browse.md",
            },
            "startsAt": "2026-12-10T05:01:02Z",
            "fingerprint": "abc",
        }
    )
    assert (inc.incident_id, inc.source, inc.state, inc.cujs) == (
        "abc",
        "alertmanager",
        "open",
        ["browse"],
    )
    assert inc.started_at.isoformat() == "2026-12-10T05:01:02+00:00"
    resolved = parse_alertmanager(
        {"status": "resolved", "labels": {"alertname": "x"}, "startsAt": "2026-12-10T05:01:02"}
    )
    assert resolved.state == "closed"
    assert resolved.incident_id.startswith("x-")
    with pytest.raises(InvalidIncident):
        parse_alertmanager({"labels": {}})
    with pytest.raises(InvalidIncident):
        parse_alertmanager({"labels": {"alertname": "x"}, "startsAt": "not a time"})


def test_manual_body() -> None:
    inc = parse_manual(
        {
            "incidentId": "eval-1",
            "summary": "CUJ add-to-cart failing",
            "excludeFaultIds": ["F01-currency-abort"],
        }
    )
    assert inc.cujs == ["add-to-cart"]
    assert inc.exclude_fault_ids == ["F01-currency-abort"]
    explicit = parse_manual({"incidentId": "e", "summary": "s", "cujs": ["browse", "browse"]})
    assert explicit.cujs == ["browse"]
    for bad in (
        {"summary": "s"},
        {"incidentId": "e", "summary": "s", "cujs": ["shopping"]},
        {"incidentId": "e", "summary": "s", "excludeFaultIds": "F01"},
    ):
        with pytest.raises(InvalidIncident):
            parse_manual(bad)


def test_parse_any_dispatch() -> None:
    assert parse_any(cm_payload()).source == "cloud-monitoring"
    assert (
        parse_any({"labels": {"alertname": "a"}, "startsAt": "2026-01-01T00:00:00Z"}).source
        == "alertmanager"
    )
    assert parse_any({"incidentId": "m", "summary": "s"}).source == "manual"


@pytest.mark.parametrize(
    ("labels", "severity", "expected"),
    [
        ({"burn": "fast"}, "CRITICAL", "SEV2"),
        ({"burn": "medium"}, "CRITICAL", "SEV2"),
        ({"burn": "slow"}, "WARNING", "SEV3"),
        ({"alert": "no-data", "burn": "none"}, "CRITICAL", "SEV3"),
        ({}, "CRITICAL", "SEV2"),
        ({}, "no severity", "SEV3"),
    ],
)
def test_baseline_severity(labels: dict, severity: str, expected: str) -> None:
    inc = parse_cloud_monitoring(cm_payload(policy_user_labels=labels, severity=severity))
    assert baseline_severity(inc) == expected


def test_baseline_impact() -> None:
    assert baseline_impact(parse_cloud_monitoring(cm_payload())).startswith(
        "checkout: mesh availability error budget fast burn"
    )
    nodata = parse_cloud_monitoring(
        cm_payload(
            policy_user_labels={"cuj": "browse", "alert": "no-data", "signal": "prober"},
            policy_name="CUJ browse: prober no data",
            documentation={},
        )
    )
    assert baseline_impact(nodata) == "Monitoring blind: no prober data for browse."
    unknown = parse_manual({"incidentId": "m", "summary": "something broke"})
    assert "unknown journey" in baseline_impact(unknown)
