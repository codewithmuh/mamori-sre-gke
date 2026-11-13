"""Data shapes: the parsed incident, the model's findings, and the investigation record.

The record's JSON (camelCase) is the contract shared with the eval runner (#26): the fields of
the contract's schema, plus three additive ones (status, error, proposalError).
"""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import UTC, datetime
from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field, field_validator
from pydantic.alias_generators import to_camel

from .config import ACTION_NAMESPACE, CUJ_IDS

Severity = Literal["SEV1", "SEV2", "SEV3", "SEV4"]
Source = Literal["cloud-monitoring", "alertmanager", "manual"]
ActionType = Literal["restart", "scale", "rollback", "remove-fault", "none"]
Status = Literal["running", "completed", "failed"]

SEVERITIES: tuple[str, ...] = ("SEV1", "SEV2", "SEV3", "SEV4")


def utc_now() -> datetime:
    return datetime.now(UTC)


def rfc3339(moment: datetime) -> str:
    """UTC, second precision, Z suffix: 2026-12-10T05:01:02Z."""
    return moment.astimezone(UTC).replace(microsecond=0).isoformat().replace("+00:00", "Z")


@dataclass
class Incident:
    """One alert, from whichever source, reduced to what triage and the prompt need."""

    incident_id: str
    source: Source
    state: Literal["open", "closed"]
    started_at: datetime
    summary: str
    policy_name: str = ""
    condition_name: str = ""
    # Cloud Monitoring severity (CRITICAL, ERROR, WARNING, "no severity") or Alertmanager's label.
    alert_severity: str = ""
    labels: dict[str, str] = field(default_factory=dict)
    cujs: list[str] = field(default_factory=list)
    # From the slo-gen policy labels (#18): burn fast|medium|slow|none, sli, signal, alert.
    burn: str = ""
    sli: str = ""
    signal: str = ""
    alert_kind: str = ""
    url: str = ""
    documentation: str = ""
    renotify: bool = False
    # Evaluation runs only: postmortems with these faultIds are hidden from this investigation.
    exclude_fault_ids: list[str] = field(default_factory=list)


class _Camel(BaseModel):
    model_config = ConfigDict(alias_generator=to_camel, populate_by_name=True, extra="ignore")


class Hypothesis(_Camel):
    rank: int
    service: str
    summary: str
    confidence: float
    evidence: list[str] = Field(default_factory=list)

    @field_validator("confidence", mode="after")
    @classmethod
    def _clamp(cls, value: float) -> float:
        # Structured outputs can't bound numbers; clamp instead of rejecting a whole answer.
        return round(min(1.0, max(0.0, value)), 2)


class ProposedAction(_Camel):
    type: ActionType
    namespace: str = ACTION_NAMESPACE
    target: str = ""
    params: dict[str, Any] = Field(default_factory=dict)


class Usage(_Camel):
    input_tokens: int = 0
    output_tokens: int = 0
    cost_usd: float = 0.0


class Investigation(_Camel):
    id: str
    incident_id: str
    source: Source
    started_at: str
    finished_at: str | None = None
    cujs: list[str] = Field(default_factory=list)
    severity: Severity = "SEV3"
    impact: str = ""
    hypotheses: list[Hypothesis] = Field(default_factory=list)
    proposed_action: ProposedAction | None = None
    proposal_id: str | None = None
    model: str = ""
    usage: Usage = Field(default_factory=Usage)
    tool_calls: int = 0
    # Additive fields (not in the contract schema).
    status: Status = "running"
    error: str | None = None
    proposal_error: dict[str, Any] | None = None

    def to_json(self) -> dict[str, Any]:
        return self.model_dump(by_alias=True, mode="json")


class FindingsAction(BaseModel):
    """The action as the model writes it (flat, so the output schema stays simple)."""

    model_config = ConfigDict(extra="ignore")

    type: ActionType
    namespace: str
    target: str
    replicas: int | None = None
    revision: str | None = None
    rationale: str = ""

    def to_proposed(self) -> ProposedAction:
        params: dict[str, Any] = {}
        if self.type == "scale" and self.replicas is not None:
            params["replicas"] = self.replicas
        if self.type == "rollback" and self.revision:
            params["revision"] = self.revision
        return ProposedAction(
            type=self.type,
            namespace=self.namespace,
            target=self.target.strip(),
            params=params,
        )


class Findings(BaseModel):
    """The model's final answer, validated after the structured-output JSON is parsed."""

    model_config = ConfigDict(extra="ignore")

    summary: str
    cujs: list[str]
    severity: Severity
    impact: str
    hypotheses: list[Hypothesis]
    proposed_action: FindingsAction = Field(alias="proposedAction")
    related_postmortems: list[str] = Field(default_factory=list, alias="relatedPostmortems")

    def normalised(self) -> Findings:
        """Known CUJs only, hypotheses ranked 1..n (best first, at most five)."""
        cujs = [c for c in dict.fromkeys(self.cujs) if c in CUJ_IDS]
        ranked = sorted(self.hypotheses, key=lambda h: (h.rank, -h.confidence))[:5]
        hypotheses = [h.model_copy(update={"rank": i}) for i, h in enumerate(ranked, start=1)]
        return self.model_copy(update={"cujs": cujs, "hypotheses": hypotheses})


# JSON schema for output_config.format. Structured outputs need additionalProperties: false
# and support no numeric bounds, so confidence is checked by pydantic afterwards.
_HYPOTHESIS_SCHEMA: dict[str, Any] = {
    "type": "object",
    "properties": {
        "rank": {"type": "integer", "description": "1 = most likely"},
        "service": {"type": "string", "description": "Deployment name, e.g. currencyservice"},
        "summary": {"type": "string"},
        "confidence": {"type": "number", "description": "0.0 to 1.0"},
        "evidence": {"type": "array", "items": {"type": "string"}},
    },
    "required": ["rank", "service", "summary", "confidence", "evidence"],
    "additionalProperties": False,
}

FINDINGS_SCHEMA: dict[str, Any] = {
    "type": "object",
    "properties": {
        "summary": {"type": "string", "description": "Two to four sentences for the on-call."},
        "cujs": {"type": "array", "items": {"type": "string", "enum": list(CUJ_IDS)}},
        "severity": {"type": "string", "enum": list(SEVERITIES)},
        "impact": {"type": "string", "description": "What shoppers see, with numbers."},
        "hypotheses": {"type": "array", "items": _HYPOTHESIS_SCHEMA},
        "proposedAction": {
            "type": "object",
            "properties": {
                "type": {
                    "type": "string",
                    "enum": ["restart", "scale", "rollback", "remove-fault", "none"],
                },
                "namespace": {"type": "string"},
                "target": {"type": "string", "description": "<kind>/<name>, e.g. deployment/x"},
                "replicas": {"anyOf": [{"type": "integer"}, {"type": "null"}]},
                "revision": {"anyOf": [{"type": "string"}, {"type": "null"}]},
                "rationale": {"type": "string"},
            },
            "required": ["type", "namespace", "target", "replicas", "revision", "rationale"],
            "additionalProperties": False,
        },
        "relatedPostmortems": {"type": "array", "items": {"type": "string"}},
    },
    "required": [
        "summary",
        "cujs",
        "severity",
        "impact",
        "hypotheses",
        "proposedAction",
        "relatedPostmortems",
    ],
    "additionalProperties": False,
}
