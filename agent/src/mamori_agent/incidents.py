"""Turn alert payloads into Incidents, and a deterministic first triage.

Sources:
- cloud-monitoring: the Pub/Sub message of a Cloud Monitoring notification channel,
  {"incident": {...}, "version": "1.2"}. Treat the version as data, not a promise.
- alertmanager: one alert of an Alertmanager webhook (kept from ai-sre-investigator).
- manual: POST /v1/investigations or `python -m mamori_agent investigate`.

The slo-gen policies (#18) carry user labels cuj, signal (prober|mesh), sli (availability|latency),
alert (burn-rate|no-data), burn (fast|medium|slow|none) and severity (page|ticket).
"""

from __future__ import annotations

import json
import re
from datetime import UTC, datetime
from typing import Any

from .config import CUJ_IDS
from .models import Incident, Severity

_CUJ_IN_TEXT = re.compile(r"\bCUJ\s+(" + "|".join(re.escape(c) for c in CUJ_IDS) + r")\b", re.I)
_MAX_TEXT = 4000


class InvalidIncident(ValueError):
    """The payload is not an incident this agent understands."""


def _str(value: Any, limit: int = _MAX_TEXT) -> str:
    if value is None:
        return ""
    text = value if isinstance(value, str) else json.dumps(value, sort_keys=True)
    return text[:limit]


def _labels(value: Any) -> dict[str, str]:
    if not isinstance(value, dict):
        return {}
    return {str(k)[:63]: _str(v, 256) for k, v in value.items()}


def _cujs_from(labels: dict[str, str], *texts: str) -> list[str]:
    found: list[str] = []
    label = labels.get("cuj", "").strip().lower()
    if label in CUJ_IDS:
        found.append(label)
    for text in texts:
        for match in _CUJ_IN_TEXT.finditer(text or ""):
            cuj = match.group(1).lower()
            if cuj not in found:
                found.append(cuj)
    return found


def _epoch(value: Any) -> datetime:
    try:
        return datetime.fromtimestamp(float(value), tz=UTC)
    except (TypeError, ValueError, OverflowError) as exc:
        raise InvalidIncident(f"started_at is not a Unix time: {value!r}") from exc


def _rfc3339(value: Any) -> datetime:
    if not isinstance(value, str) or not value:
        return datetime.now(UTC)
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise InvalidIncident(f"not an RFC3339 time: {value!r}") from exc
    return parsed if parsed.tzinfo else parsed.replace(tzinfo=UTC)


def parse_cloud_monitoring(payload: dict[str, Any]) -> Incident:
    incident = payload.get("incident")
    if not isinstance(incident, dict):
        raise InvalidIncident("no 'incident' object")
    incident_id = _str(incident.get("incident_id"), 128)
    if not incident_id:
        raise InvalidIncident("incident_id missing")
    state = _str(incident.get("state")).lower()
    if state not in ("open", "closed"):
        raise InvalidIncident(f"unknown state {state!r}")

    labels = _labels(incident.get("policy_user_labels"))
    documentation = incident.get("documentation") or {}
    doc_subject = _str(documentation.get("subject")) if isinstance(documentation, dict) else ""
    doc_content = _str(documentation.get("content")) if isinstance(documentation, dict) else ""
    policy_name = _str(incident.get("policy_name"), 256)
    condition_name = _str(incident.get("condition_name"), 256)
    return Incident(
        incident_id=incident_id,
        source="cloud-monitoring",
        state="open" if state == "open" else "closed",
        started_at=_epoch(incident.get("started_at")),
        summary=_str(incident.get("summary"), 1000),
        policy_name=policy_name,
        condition_name=condition_name,
        alert_severity=_str(incident.get("severity"), 32),
        labels=labels,
        cujs=_cujs_from(labels, policy_name, condition_name, doc_subject),
        burn=labels.get("burn", ""),
        sli=labels.get("sli", ""),
        signal=labels.get("signal", ""),
        alert_kind=labels.get("alert", ""),
        url=_str(incident.get("url"), 512),
        documentation="\n".join(p for p in (doc_subject, doc_content) if p),
        renotify=bool(incident.get("renotify", False)),
    )


def parse_alertmanager(alert: dict[str, Any]) -> Incident:
    labels = _labels(alert.get("labels"))
    if not labels.get("alertname"):
        raise InvalidIncident("alertname label missing")
    annotations = _labels(alert.get("annotations"))
    fingerprint = _str(alert.get("fingerprint"), 128)
    started = _rfc3339(alert.get("startsAt"))
    incident_id = fingerprint or f"{labels['alertname']}-{int(started.timestamp())}"
    summary = annotations.get("summary") or annotations.get("description", "")
    return Incident(
        incident_id=incident_id,
        source="alertmanager",
        state="closed" if alert.get("status") == "resolved" else "open",
        started_at=started,
        summary=summary,
        policy_name=labels["alertname"],
        alert_severity=labels.get("severity", ""),
        labels=labels,
        cujs=_cujs_from(labels, labels["alertname"], summary),
        burn=labels.get("burn", ""),
        sli=labels.get("sli", ""),
        signal=labels.get("signal", ""),
        alert_kind=labels.get("alert", ""),
        documentation=annotations.get("runbook_url", ""),
    )


def parse_manual(body: dict[str, Any]) -> Incident:
    incident_id = _str(body.get("incidentId"), 128).strip()
    summary = _str(body.get("summary"), 2000).strip()
    if not incident_id or not summary:
        raise InvalidIncident("incidentId and summary are required")
    cujs_in = body.get("cujs") or []
    if not isinstance(cujs_in, list) or any(c not in CUJ_IDS for c in cujs_in):
        raise InvalidIncident(f"cujs must be a list drawn from {', '.join(CUJ_IDS)}")
    exclude = body.get("excludeFaultIds") or []
    if not isinstance(exclude, list) or not all(isinstance(x, str) for x in exclude):
        raise InvalidIncident("excludeFaultIds must be a list of strings")
    return Incident(
        incident_id=incident_id,
        source="manual",
        state="open",
        started_at=_rfc3339(body.get("startedAt")),
        summary=summary,
        cujs=list(dict.fromkeys(cujs_in)) or _cujs_from({}, summary),
        exclude_fault_ids=[x[:64] for x in exclude[:50]],
    )


def parse_any(body: dict[str, Any]) -> Incident:
    """POST /v1/investigations body: manual, a Cloud Monitoring message or an Alertmanager alert."""
    if "incident" in body:
        return parse_cloud_monitoring(body)
    if "labels" in body and "startsAt" in body:
        return parse_alertmanager(body)
    return parse_manual(body)


def parse_pubsub_data(data: bytes) -> Incident:
    try:
        payload = json.loads(data)
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise InvalidIncident("message data is not JSON") from exc
    if not isinstance(payload, dict):
        raise InvalidIncident("message data is not a JSON object")
    return parse_cloud_monitoring(payload)


# --- Deterministic triage -------------------------------------------------------------------
# The model refines these, but they hold even when the model call fails. Severity levels are
# docs/incidents/severity.md (#20): SEV1 a CUJ hard down, SEV2 a fast burn below that, SEV3 slow
# burn / non-critical part missing / monitoring blind, SEV4 no shopper impact.


def baseline_severity(incident: Incident) -> Severity:
    if incident.alert_kind == "no-data":
        return "SEV3"
    if incident.burn in ("fast", "medium"):
        return "SEV2"
    if incident.burn == "slow":
        return "SEV3"
    if incident.alert_severity.upper() in ("CRITICAL", "PAGE"):
        return "SEV2"
    return "SEV3"


def baseline_impact(incident: Incident) -> str:
    journeys = ", ".join(incident.cujs) if incident.cujs else "unknown journey"
    if incident.alert_kind == "no-data":
        return f"Monitoring blind: no {incident.signal or 'SLO'} data for {journeys}."
    what = " ".join(p for p in (incident.signal, incident.sli) if p) or "SLO"
    burn = f"{incident.burn} burn" if incident.burn and incident.burn != "none" else "alert"
    return f"{journeys}: {what} error budget {burn} ({incident.policy_name or incident.summary})."
