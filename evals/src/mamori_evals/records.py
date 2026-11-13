"""What the eval observes: investigations (#24), proposals and audit lines (#25), fault runs.

Each record keeps the raw document it was parsed from, so a recording holds exactly what the
APIs returned and can be re-scored later by `mamori-evals replay`.
"""

from __future__ import annotations

from collections.abc import Mapping
from dataclasses import dataclass, field
from datetime import datetime

from .jsonutil import (
    DocumentError,
    JSONDict,
    as_mapping,
    get_list,
    get_opt_float,
    get_opt_int,
    get_opt_mapping,
    get_opt_str,
    get_opt_time,
    get_str,
    get_str_list,
    opt_time_str,
)

# The executor's lifecycle audit events (#25); other log lines (startup, slack) are ignored.
LIFECYCLE_EVENTS = frozenset(
    {"proposal_received", "approved", "rejected", "executed", "blocked", "failed"}
)
# Proposal statuses after which nothing more happens to it.
TERMINAL_STATUSES = frozenset({"rejected", "executed", "failed", "expired", "blocked"})
RECORDING_KIND = "EvalRecording"
API_VERSION = "mamori/v1"


@dataclass(frozen=True)
class Action:
    type: str
    namespace: str | None
    target: str | None
    params: Mapping[str, object] = field(default_factory=dict)

    def describe(self) -> str:
        return self.type if not self.target else f"{self.type} {self.target}"


def parse_action(value: object, what: str) -> Action | None:
    if value is None:
        return None
    data = as_mapping(value, what)
    action_type = get_str(data, "type").strip()
    if not action_type:
        return None
    params = get_opt_mapping(data, "params") or {}
    return Action(
        type=action_type,
        namespace=get_opt_str(data, "namespace"),
        target=get_opt_str(data, "target"),
        params=dict(params),
    )


@dataclass(frozen=True)
class Hypothesis:
    rank: int
    service: str
    summary: str
    confidence: float | None
    evidence: tuple[str, ...]


@dataclass(frozen=True)
class Usage:
    input_tokens: int | None
    output_tokens: int | None
    cost_usd: float | None


@dataclass(frozen=True)
class ProposalError:
    """The executor's 4xx answer, as the agent copies it into the investigation."""

    status: int | None
    reason: str
    error: str


@dataclass(frozen=True)
class Investigation:
    id: str
    incident_id: str | None
    source: str | None
    status: str
    started_at: datetime | None
    finished_at: datetime | None
    cujs: tuple[str, ...]
    severity: str | None
    impact: str
    hypotheses: tuple[Hypothesis, ...]
    proposed_action: Action | None
    proposal_id: str | None
    proposal_error: ProposalError | None
    model: str | None
    usage: Usage
    tool_calls: int | None
    error: str | None
    raw: JSONDict

    @property
    def finished(self) -> bool:
        return self.status in ("completed", "failed") or (
            self.status == "" and self.finished_at is not None
        )


def parse_investigation(document: object) -> Investigation:
    data = as_mapping(document, "investigation")
    inv_id = get_str(data, "id").strip()
    if not inv_id:
        raise DocumentError("investigation has no id")
    hypotheses = []
    for index, item in enumerate(get_list(data, "hypotheses")):
        hyp = as_mapping(item, f"hypotheses[{index}]")
        hypotheses.append(
            Hypothesis(
                rank=get_opt_int(hyp, "rank") or index + 1,
                service=get_str(hyp, "service"),
                summary=get_str(hyp, "summary"),
                confidence=get_opt_float(hyp, "confidence"),
                evidence=tuple(str(e) for e in get_list(hyp, "evidence")),
            )
        )
    usage = get_opt_mapping(data, "usage") or {}
    error_doc = get_opt_mapping(data, "proposalError")
    proposal_error = None
    if error_doc is not None:
        proposal_error = ProposalError(
            status=get_opt_int(error_doc, "status"),
            reason=get_str(error_doc, "reason"),
            error=get_str(error_doc, "error"),
        )
    return Investigation(
        id=inv_id,
        incident_id=get_opt_str(data, "incidentId"),
        source=get_opt_str(data, "source"),
        status=get_str(data, "status"),
        started_at=get_opt_time(data, "startedAt"),
        finished_at=get_opt_time(data, "finishedAt"),
        cujs=get_str_list(data, "cujs"),
        severity=get_opt_str(data, "severity"),
        impact=get_str(data, "impact"),
        hypotheses=tuple(hypotheses),
        proposed_action=parse_action(data.get("proposedAction"), "proposedAction"),
        proposal_id=get_opt_str(data, "proposalId"),
        proposal_error=proposal_error,
        model=get_opt_str(data, "model"),
        usage=Usage(
            input_tokens=get_opt_int(usage, "inputTokens"),
            output_tokens=get_opt_int(usage, "outputTokens"),
            cost_usd=get_opt_float(usage, "costUsd"),
        ),
        tool_calls=get_opt_int(data, "toolCalls"),
        error=get_opt_str(data, "error"),
        raw=dict(data),
    )


@dataclass(frozen=True)
class Proposal:
    proposal_id: str
    status: str
    reason: str | None
    action: Action | None
    raw: JSONDict


def parse_proposal(document: object) -> Proposal:
    data = as_mapping(document, "proposal")
    return Proposal(
        proposal_id=get_str(data, "proposalId"),
        status=get_str(data, "status"),
        reason=get_opt_str(data, "reason"),
        action=parse_action(data.get("action"), "action"),
        raw=dict(data),
    )


@dataclass(frozen=True)
class ExecutorEvent:
    time: datetime | None
    event: str
    proposal_id: str | None
    incident_id: str | None
    investigation_id: str | None
    action: Action | None
    status: str | None
    reason: str | None
    raw: JSONDict


def parse_executor_event(document: object) -> ExecutorEvent:
    data = as_mapping(document, "executor event")
    return ExecutorEvent(
        time=get_opt_time(data, "time"),
        event=get_str(data, "event"),
        proposal_id=get_opt_str(data, "proposalId"),
        incident_id=get_opt_str(data, "incidentId"),
        investigation_id=get_opt_str(data, "investigationId"),
        action=parse_action(data.get("action"), "action"),
        status=get_opt_str(data, "status"),
        reason=get_opt_str(data, "reason"),
        raw=dict(data),
    )


@dataclass(frozen=True)
class FaultEnd:
    """How the gameday process ended: duration, signal (the runner) or removed-externally."""

    reason: str
    at: datetime | None


@dataclass
class RunObservation:
    """One fault run, as observed. `status`: completed, skipped, aborted or error."""

    fault_id: str
    status: str
    reason: str | None = None
    injected_at: datetime | None = None
    stopped_at: datetime | None = None
    recovered_at: datetime | None = None
    fault_run_id: str | None = None
    fault_objects: tuple[str, ...] = ()
    fault_end: FaultEnd | None = None
    investigation: Investigation | None = None
    proposal: Proposal | None = None
    executor_events: tuple[ExecutorEvent, ...] = ()
    notes: list[str] = field(default_factory=list)

    def to_dict(self) -> JSONDict:
        return {
            "faultId": self.fault_id,
            "status": self.status,
            "reason": self.reason,
            "injectedAt": opt_time_str(self.injected_at),
            "stoppedAt": opt_time_str(self.stopped_at),
            "recoveredAt": opt_time_str(self.recovered_at),
            "faultRunId": self.fault_run_id,
            "faultObjects": list(self.fault_objects),
            "faultEnd": (
                {"reason": self.fault_end.reason, "at": opt_time_str(self.fault_end.at)}
                if self.fault_end
                else None
            ),
            "investigation": self.investigation.raw if self.investigation else None,
            "proposal": self.proposal.raw if self.proposal else None,
            "executorEvents": [event.raw for event in self.executor_events],
            "notes": list(self.notes),
        }


def parse_observation(document: object) -> RunObservation:
    data = as_mapping(document, "run")
    fault_id = get_str(data, "faultId")
    if not fault_id:
        raise DocumentError("run has no faultId")
    end_doc = get_opt_mapping(data, "faultEnd")
    investigation = data.get("investigation")
    proposal = data.get("proposal")
    return RunObservation(
        fault_id=fault_id,
        status=get_str(data, "status", "completed"),
        reason=get_opt_str(data, "reason"),
        injected_at=get_opt_time(data, "injectedAt"),
        stopped_at=get_opt_time(data, "stoppedAt"),
        recovered_at=get_opt_time(data, "recoveredAt"),
        fault_run_id=get_opt_str(data, "faultRunId"),
        fault_objects=get_str_list(data, "faultObjects"),
        fault_end=(
            FaultEnd(reason=get_str(end_doc, "reason"), at=get_opt_time(end_doc, "at"))
            if end_doc is not None
            else None
        ),
        investigation=parse_investigation(investigation) if investigation is not None else None,
        proposal=parse_proposal(proposal) if proposal is not None else None,
        executor_events=tuple(
            parse_executor_event(event) for event in get_list(data, "executorEvents")
        ),
        notes=[str(note) for note in get_list(data, "notes")],
    )


@dataclass
class Recording:
    """A session of runs plus where it came from. `synthetic` marks fixtures, never lab data."""

    metadata: JSONDict
    runs: list[RunObservation]

    @property
    def synthetic(self) -> bool:
        return self.metadata.get("synthetic") is True

    def to_dict(self) -> JSONDict:
        return {
            "apiVersion": API_VERSION,
            "kind": RECORDING_KIND,
            "metadata": self.metadata,
            "runs": [run.to_dict() for run in self.runs],
        }


def parse_recording(document: object) -> Recording:
    data = as_mapping(document, "recording")
    if get_str(data, "kind") != RECORDING_KIND:
        raise DocumentError(f"not a recording: kind is {data.get('kind')!r}, want {RECORDING_KIND}")
    metadata = get_opt_mapping(data, "metadata") or {}
    runs = []
    for index, item in enumerate(get_list(data, "runs")):
        try:
            runs.append(parse_observation(item))
        except DocumentError as err:
            raise DocumentError(f"runs[{index}]: {err}") from err
    return Recording(metadata=dict(metadata), runs=runs)
