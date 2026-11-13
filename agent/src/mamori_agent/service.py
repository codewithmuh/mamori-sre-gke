"""Glue: accept incidents, run one investigation at a time, record, propose, notify.

One fault opens several Cloud Monitoring incidents: every journey, signal (prober, mesh), SLI
and burn tier has its own policy, and page and ticket tiers both publish to incident-alerts.
Investigating each would build a paid backlog that outlives the fault. So, at intake:
- only state "open"; a "closed" message is remembered, and a queued investigation whose
  incidents have all closed is cancelled before it starts;
- repeats (renotify) and incident ids seen in the last dedupe_ttl_seconds are skipped;
- only page-tier incidents (policy label severity=page) start an investigation (page_only);
- an incident whose journeys overlap a queued or running investigation that started within
  coalesce_window_seconds joins it (recorded in mergedIncidentIds) instead of starting another;
- the day's model budget: every queued investigation reserves max_cost_usd, so the queue can't
  promise more than daily_cost_limit_usd; the spend is checked again when an investigation
  starts and before every model call, and rebuilt from Cloud Logging when the pod starts;
- the queue is bounded; a full queue drops the incident with a log line.
"""

from __future__ import annotations

import logging
import queue
import threading
import time
import uuid
from collections import OrderedDict
from collections.abc import Callable
from dataclasses import dataclass, field
from datetime import UTC, datetime
from typing import Any

from . import metrics
from .config import Settings
from .executor import ExecutorClient
from .incidents import baseline_impact, baseline_severity
from .investigator import Investigator, Outcome
from .logs import event
from .models import (
    Incident,
    Investigation,
    ProposedAction,
    Status,
    Usage,
    rfc3339,
    utc_now,
)
from .slack import SlackNotifier

log = logging.getLogger(__name__)


@dataclass(frozen=True)
class Accepted:
    accepted: bool
    # investigate|merged|ticket|duplicate|closed|queue_full|daily_budget
    reason: str
    investigation_id: str | None = None


class InvestigationStore:
    """The newest records in memory (bounded). Cloud Logging holds the durable copy."""

    def __init__(self, size: int) -> None:
        self._size = size
        self._items: OrderedDict[str, Investigation] = OrderedDict()
        self._lock = threading.Lock()

    def put(self, inv: Investigation) -> None:
        with self._lock:
            self._items[inv.id] = inv
            self._items.move_to_end(inv.id)
            while len(self._items) > self._size:
                self._items.popitem(last=False)

    def get(self, inv_id: str) -> Investigation | None:
        with self._lock:
            return self._items.get(inv_id)

    def list(self, limit: int, since: str | None = None) -> list[Investigation]:
        with self._lock:
            items = list(self._items.values())
        if since:
            items = [i for i in items if i.started_at >= since]
        items.sort(key=lambda i: (i.started_at, i.id), reverse=True)
        return items[:limit]


class DailySpend:
    """Estimated model spend for the current UTC day, plus what queued work has reserved."""

    def __init__(self, limit: float, today: Callable[[], str]) -> None:
        self._limit = limit
        self._today = today
        self._day = today()
        self._spent = 0.0
        self._reserved = 0.0
        self._lock = threading.Lock()

    def _roll(self) -> None:
        day = self._today()
        if day != self._day:
            self._day, self._spent = day, 0.0
            metrics.DAILY_COST.set(0.0)

    def restore(self, spent: float) -> None:
        """Today's spend from before a restart (read back from the log lines)."""
        with self._lock:
            self._roll()
            self._spent = max(self._spent, spent)
            metrics.DAILY_COST.set(self._spent)

    def try_reserve(self, amount: float) -> bool:
        with self._lock:
            self._roll()
            if self._spent + self._reserved + amount > self._limit:
                return False
            self._reserved += amount
            return True

    def allows(self, run_cost: float) -> bool:
        """Whether a running investigation that has spent run_cost may make another call."""
        with self._lock:
            self._roll()
            return self._spent + run_cost < self._limit

    def settle(self, reserved: float, actual: float) -> None:
        with self._lock:
            self._roll()
            self._reserved = max(0.0, self._reserved - reserved)
            self._spent += actual
            metrics.DAILY_COST.set(self._spent)

    @property
    def spent(self) -> float:
        with self._lock:
            self._roll()
            return self._spent

    @property
    def reserved(self) -> float:
        with self._lock:
            return self._reserved


def _utc_day() -> str:
    return datetime.now(UTC).strftime("%Y-%m-%d")


def _key(incident: Incident) -> str:
    return f"{incident.source}:{incident.incident_id}"


@dataclass
class _Work:
    incident: Incident
    inv_id: str
    keys: list[str]
    cujs: set[str]
    accepted_at: float
    reserved: float
    running: bool = False
    merged: list[str] = field(default_factory=list)


class AgentService:
    def __init__(
        self,
        settings: Settings,
        *,
        investigator: Investigator,
        executor: ExecutorClient | None,
        slack: SlackNotifier | None,
        exclusions: ExclusionHolder,
        clock: Callable[[], float] = time.monotonic,
        today: Callable[[], str] = _utc_day,
    ) -> None:
        self._s = settings
        self._investigator = investigator
        self._executor = executor
        self._slack = slack
        self._exclusions = exclusions
        self._clock = clock
        self.store = InvestigationStore(settings.store_size)
        self.spend = DailySpend(settings.daily_cost_limit_usd, today)
        self._queue: queue.Queue[str] = queue.Queue(settings.queue_size)
        self._active: dict[str, _Work] = {}  # queued or running, by investigation id
        self._seen: dict[str, float] = {}
        self._closed: dict[str, float] = {}
        self._seq = 0
        self._lock = threading.Lock()

    # --- intake --------------------------------------------------------------------------------

    def _new_record(self, incident: Incident, status: Status = "queued") -> Investigation:
        self._seq += 1
        return Investigation(
            id=f"inv-{uuid.uuid4().hex[:12]}",
            seq=self._seq,
            status=status,
            incident_id=incident.incident_id,
            source=incident.source,
            started_at=rfc3339(utc_now()),
            cujs=incident.cujs,
            severity=baseline_severity(incident),
            impact=baseline_impact(incident),
            model=self._s.model,
            agent_config=self._s.effective(),
        )

    def status(self) -> dict[str, Any]:
        """GET /v1/status: whether the agent is busy, and the day's budget."""
        with self._lock:
            running = sum(1 for w in self._active.values() if w.running)
            queued = len(self._active) - running
        return {
            "queued": queued,
            "running": running,
            "dailySpentUsd": round(self.spend.spent, 4),
            "dailyReservedUsd": round(self.spend.reserved, 4),
            "agentConfig": self._s.effective(),
        }

    def _expire(self, table: dict[str, float], now: float) -> None:
        for k in [k for k, t in table.items() if now - t > self._s.dedupe_ttl_seconds]:
            del table[k]

    def submit(self, incident: Incident) -> Accepted:
        result = self._intake(incident)
        metrics.INCIDENTS.labels(incident.source, result.reason).inc()
        if result.reason == "investigate":
            metrics.QUEUE_DEPTH.set(self._queue.qsize())
            event(
                log,
                "incident_accepted",
                incident_id=incident.incident_id,
                source=incident.source,
                investigation_id=result.investigation_id,
                cujs=incident.cujs,
                policy_severity=incident.labels.get("severity"),
            )
        else:
            level = (
                logging.WARNING if result.reason in ("queue_full", "daily_budget") else logging.INFO
            )
            event(
                log,
                "incident_merged" if result.reason == "merged" else "incident_skipped",
                level,
                incident_id=incident.incident_id,
                source=incident.source,
                reason=result.reason,
                investigation_id=result.investigation_id,
                cujs=incident.cujs,
                policy_severity=incident.labels.get("severity"),
            )
            if result.reason == "daily_budget":
                metrics.GUARDRAIL_STOPS.labels("daily_budget").inc()
        return result

    def _intake(self, incident: Incident) -> Accepted:
        key, now = _key(incident), self._clock()
        with self._lock:
            self._expire(self._seen, now)
            self._expire(self._closed, now)
            if incident.state != "open":
                self._closed[key] = now
                return Accepted(False, "closed")
            if incident.renotify or key in self._seen:
                return Accepted(False, "duplicate")
            # slo-gen's policy label (#18): severity=page|ticket. Policies without it pass.
            tier = incident.labels.get("severity")
            if (
                self._s.page_only
                and incident.source == "cloud-monitoring"
                and tier is not None
                and tier != "page"
            ):
                return Accepted(False, "ticket")
            self._seen[key] = now
            work = self._coalesce_target(incident, now)
            if work is not None:
                self._merge(work, incident, key)
                return Accepted(True, "merged", work.inv_id)
            if not self.spend.try_reserve(self._s.max_cost_usd):
                return Accepted(False, "daily_budget")
            inv = self._new_record(incident)
            try:
                self._queue.put_nowait(inv.id)
            except queue.Full:
                # Not acknowledged (cli._on_message): Pub/Sub delivers it again later, and it
                # must not count as seen then.
                self.spend.settle(self._s.max_cost_usd, 0.0)
                self._seen.pop(key, None)
                return Accepted(False, "queue_full")
            self._active[inv.id] = _Work(
                incident, inv.id, [key], set(incident.cujs), now, self._s.max_cost_usd
            )
            self.store.put(inv)
            return Accepted(True, "investigate", inv.id)

    def _coalesce_target(self, incident: Incident, now: float) -> _Work | None:
        """The newest queued or running investigation this incident belongs to, if any."""
        if incident.source == "manual":
            return None  # an explicit request always gets its own run
        candidates = [
            w
            for w in self._active.values()
            if w.incident.source != "manual"
            and now - w.accepted_at < self._s.coalesce_window_seconds
            and (not incident.cujs or not w.cujs or w.cujs & set(incident.cujs))
        ]
        # Newest first; on a tie the later intake (dict order) wins.
        return max(reversed(candidates), key=lambda w: w.accepted_at, default=None)

    def _merge(self, work: _Work, incident: Incident, key: str) -> None:
        work.keys.append(key)
        work.merged.append(incident.incident_id)
        work.cujs |= set(incident.cujs)
        inv = self.store.get(work.inv_id)
        if inv is None:
            return
        update: dict[str, Any] = {"merged_incident_ids": list(work.merged)}
        if not work.running:
            # Not started yet: widen the first triage to the union of what has fired.
            update["cujs"] = sorted(work.cujs)
            update["severity"] = min(inv.severity, baseline_severity(incident))
        self.store.put(inv.model_copy(update=update))

    # --- worker --------------------------------------------------------------------------------

    def run_worker(self, stop: threading.Event) -> None:
        while not stop.is_set():
            try:
                inv_id = self._queue.get(timeout=1.0)
            except queue.Empty:
                continue
            metrics.QUEUE_DEPTH.set(self._queue.qsize())
            try:
                self._process(inv_id)
            except Exception as exc:
                # Nothing may kill the worker thread: record the failure and go on.
                log.exception(
                    "worker failed", extra={"event": "worker_failed", "investigation_id": inv_id}
                )
                self._finish_failed(inv_id, f"{type(exc).__name__}: {exc}")
            finally:
                self._queue.task_done()

    def _process(self, inv_id: str) -> None:
        with self._lock:
            work = self._active.get(inv_id)
            if work is None:
                return
            if all(k in self._closed for k in work.keys):
                self._cancel(work, "every incident closed before the investigation started")
                return
            if not self.spend.allows(0.0):
                self._cancel(work, "the day's model budget is used up")
                return
            work.running = True
            inv = (self.store.get(inv_id) or self._new_record(work.incident)).model_copy(
                update={"id": inv_id, "status": "running", "run_started_at": rfc3339(utc_now())}
            )
            self.store.put(inv)
        self.investigate(work.incident, inv, work.reserved)

    def _cancel(self, work: _Work, reason: str) -> None:
        """Called with the lock held."""
        self._active.pop(work.inv_id, None)
        self.spend.settle(work.reserved, 0.0)
        inv = self.store.get(work.inv_id) or self._new_record(work.incident)
        record = inv.model_copy(
            update={"status": "cancelled", "error": reason, "finished_at": rfc3339(utc_now())}
        )
        self.store.put(record)
        metrics.INVESTIGATIONS.labels(record.source, "cancelled").inc()
        event(
            log,
            "investigation_cancelled",
            investigation_id=work.inv_id,
            incident_ids=[work.incident.incident_id, *work.merged],
            reason=reason,
        )

    def _finish_failed(self, inv_id: str, error: str) -> None:
        with self._lock:
            work = self._active.pop(inv_id, None)
            if work is not None:
                self.spend.settle(work.reserved, 0.0)
            inv = self.store.get(inv_id)
            if inv is not None and inv.status in ("queued", "running"):
                self.store.put(
                    inv.model_copy(
                        update={
                            "status": "failed",
                            "error": error,
                            "finished_at": rfc3339(utc_now()),
                        }
                    )
                )

    def investigate(
        self, incident: Incident, inv: Investigation | None = None, reserved: float = 0.0
    ) -> Investigation:
        """Run one investigation to its final record (the worker, and the CLI directly)."""
        if inv is None:
            with self._lock:
                inv = self._new_record(incident, status="running")
            inv.run_started_at = inv.started_at
            self.store.put(inv)
        started = self._clock()
        metrics.IN_PROGRESS.inc()
        self._exclusions.set(incident.exclude_fault_ids)
        outcome: Outcome | None = None
        crash: str | None = None
        try:
            outcome = self._investigator.run(incident, budget=self.spend.allows)
        except Exception as exc:
            log.exception(
                "investigation crashed",
                extra={"event": "investigation_crashed", "investigation_id": inv.id},
            )
            crash = f"{type(exc).__name__}: {exc}"
        finally:
            self._exclusions.set([])
            metrics.IN_PROGRESS.dec()
        record = self._record(inv, outcome, crash)
        summary = outcome.findings.summary if outcome and outcome.findings else (record.error or "")
        if (
            outcome
            and outcome.findings
            and record.proposed_action
            and self._s.propose
            and self._executor
        ):
            try:
                self._propose(record, incident, outcome.findings.proposed_action.rationale)
            except Exception as exc:
                log.exception("proposal failed", extra={"event": "proposal_failed"})
                metrics.PROPOSALS.labels("error").inc()
                record.proposal_error = {
                    "status": None,
                    "error": f"{type(exc).__name__}: {exc}",
                    "reason": "agent-error",
                }
        record.finished_at = rfc3339(utc_now())
        with self._lock:
            # Incidents may have joined while it ran; then it leaves the active set.
            work = self._active.pop(inv.id, None)
            if work is not None:
                record.merged_incident_ids = list(work.merged)
            self.store.put(record)
        self.spend.settle(reserved, record.usage.cost_usd)
        metrics.INVESTIGATIONS.labels(record.source, record.status).inc()
        metrics.DURATION.labels(record.source).observe(self._clock() - started)
        # The contract's log line: one per investigation, the record's fields at the top level.
        event(
            log,
            "investigation_completed",
            logging.INFO if record.status == "completed" else logging.ERROR,
            **record.to_json(),
        )
        if self._slack and self._s.slack:
            result = self._slack.post(record, incident, summary)
            metrics.SLACK_POSTS.labels(result).inc()
        return record

    def _record(
        self, inv: Investigation, outcome: Outcome | None, crash: str | None
    ) -> Investigation:
        if outcome is None:
            return inv.model_copy(update={"status": "failed", "error": crash})
        usage = Usage(
            input_tokens=outcome.meter.total_input_tokens,
            output_tokens=outcome.meter.output_tokens,
            cost_usd=round(outcome.meter.cost_usd, 4),
        )
        update: dict[str, Any] = {
            "model": outcome.model,
            "usage": usage,
            "tool_calls": outcome.tool_calls,
        }
        if outcome.findings is None:
            error = outcome.error
            message = f"{error.reason}: {error}" if error else "no answer"
            update |= {"status": "failed", "error": message}
            return inv.model_copy(update=update)
        f = outcome.findings
        update |= {
            "status": "completed",
            "cujs": f.cujs or inv.cujs,
            "severity": f.severity,
            "impact": f.impact,
            "hypotheses": f.hypotheses,
            "proposed_action": f.proposed_action.to_proposed(),
            "error": None,
        }
        return inv.model_copy(update=update)

    def _propose(self, record: Investigation, incident: Incident, rationale: str) -> None:
        action: ProposedAction | None = record.proposed_action
        if self._executor is None or action is None:
            return
        result = self._executor.propose(incident.incident_id, record.id, action, rationale)
        metrics.PROPOSALS.labels(result.result).inc()
        record.proposal_id = result.proposal_id
        record.proposal_error = result.error if result.result != "skipped" else None
        event(
            log,
            "proposal_sent" if result.proposal_id else "proposal_not_sent",
            investigation_id=record.id,
            incident_id=incident.incident_id,
            result=result.result,
            proposal_id=result.proposal_id,
            error=result.error,
        )


class ExclusionHolder:
    """The current investigation's excluded fault ids, read by the past_incidents tool.

    One worker runs one investigation at a time, so a single slot is enough.
    """

    def __init__(self) -> None:
        self._ids: list[str] = []
        self._lock = threading.Lock()

    def set(self, ids: list[str]) -> None:
        with self._lock:
            self._ids = list(ids)

    def get(self) -> list[str]:
        with self._lock:
            return list(self._ids)
