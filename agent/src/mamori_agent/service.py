"""Glue: accept incidents, run one investigation at a time, record, propose, notify.

Accepting an incident (from Pub/Sub or the API):
- only state "open"; closed incidents and repeats (renotify) are skipped;
- deduplicated on (source, incident id) for dedupe_ttl_seconds;
- refused once the day's estimated model spend reaches daily_cost_limit_usd (an alert storm
  must not become a bill);
- queued (bounded); a full queue drops the incident with a log line.
"""

from __future__ import annotations

import logging
import queue
import threading
import time
import uuid
from collections import OrderedDict
from collections.abc import Callable
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Any

from . import metrics
from .config import Settings
from .executor import ExecutorClient
from .incidents import baseline_impact, baseline_severity
from .investigator import Investigator, Outcome
from .logs import event
from .models import Incident, Investigation, ProposedAction, Usage, rfc3339, utc_now
from .slack import SlackNotifier

log = logging.getLogger(__name__)


@dataclass(frozen=True)
class Accepted:
    accepted: bool
    reason: str  # investigate|duplicate|closed|ignored|queue_full|daily_budget
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
    def __init__(self, limit: float, today: Callable[[], str]) -> None:
        self._limit = limit
        self._today = today
        self._day = today()
        self._spent = 0.0
        self._lock = threading.Lock()

    def _roll(self) -> None:
        day = self._today()
        if day != self._day:
            self._day, self._spent = day, 0.0

    def exhausted(self) -> bool:
        with self._lock:
            self._roll()
            return self._spent >= self._limit

    def add(self, cost: float) -> None:
        with self._lock:
            self._roll()
            self._spent += cost
            metrics.DAILY_COST.set(self._spent)


def _utc_day() -> str:
    return datetime.now(UTC).strftime("%Y-%m-%d")


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
        self._queue: queue.Queue[tuple[Incident, Investigation]] = queue.Queue(settings.queue_size)
        self._seen: dict[str, float] = {}
        self._seen_lock = threading.Lock()

    # --- intake --------------------------------------------------------------------------------

    def _duplicate(self, key: str) -> bool:
        now = self._clock()
        with self._seen_lock:
            for k in [k for k, t in self._seen.items() if now - t > self._s.dedupe_ttl_seconds]:
                del self._seen[k]
            if key in self._seen:
                return True
            self._seen[key] = now
            return False

    def submit(self, incident: Incident) -> Accepted:
        reason = self._triage_intake(incident)
        metrics.INCIDENTS.labels(incident.source, reason).inc()
        if reason != "investigate":
            event(
                log,
                "incident_skipped",
                incident_id=incident.incident_id,
                source=incident.source,
                reason=reason,
            )
            return Accepted(False, reason)
        inv = Investigation(
            id=f"inv-{uuid.uuid4().hex[:12]}",
            incident_id=incident.incident_id,
            source=incident.source,
            started_at=rfc3339(utc_now()),
            cujs=incident.cujs,
            severity=baseline_severity(incident),
            impact=baseline_impact(incident),
            model=self._s.model,
        )
        try:
            self._queue.put_nowait((incident, inv))
        except queue.Full:
            metrics.INCIDENTS.labels(incident.source, "queue_full").inc()
            event(
                log,
                "incident_dropped",
                logging.WARNING,
                incident_id=incident.incident_id,
                reason="queue_full",
            )
            return Accepted(False, "queue_full")
        self.store.put(inv)
        metrics.QUEUE_DEPTH.set(self._queue.qsize())
        event(
            log,
            "incident_accepted",
            incident_id=incident.incident_id,
            source=incident.source,
            investigation_id=inv.id,
            cujs=incident.cujs,
        )
        return Accepted(True, "investigate", inv.id)

    def _triage_intake(self, incident: Incident) -> str:
        if incident.state != "open":
            return "closed"
        if incident.renotify:
            return "duplicate"
        if self.spend.exhausted():
            metrics.GUARDRAIL_STOPS.labels("daily_budget").inc()
            return "daily_budget"
        if self._duplicate(f"{incident.source}:{incident.incident_id}"):
            return "duplicate"
        return "investigate"

    # --- worker --------------------------------------------------------------------------------

    def run_worker(self, stop: threading.Event) -> None:
        while not stop.is_set():
            try:
                incident, inv = self._queue.get(timeout=1.0)
            except queue.Empty:
                continue
            metrics.QUEUE_DEPTH.set(self._queue.qsize())
            try:
                self.investigate(incident, inv)
            finally:
                self._queue.task_done()

    def investigate(self, incident: Incident, inv: Investigation | None = None) -> Investigation:
        """Run one investigation to its final record (also used by the CLI)."""
        if inv is None:
            inv = Investigation(
                id=f"inv-{uuid.uuid4().hex[:12]}",
                incident_id=incident.incident_id,
                source=incident.source,
                started_at=rfc3339(utc_now()),
                cujs=incident.cujs,
                severity=baseline_severity(incident),
                impact=baseline_impact(incident),
                model=self._s.model,
            )
            self.store.put(inv)
        started = self._clock()
        metrics.IN_PROGRESS.inc()
        self._exclusions.set(incident.exclude_fault_ids)
        outcome: Outcome | None = None
        crash: str | None = None
        try:
            outcome = self._investigator.run(incident)
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
            self._propose(record, incident, outcome.findings.proposed_action.rationale)
        record.finished_at = rfc3339(utc_now())
        self.store.put(record)
        self.spend.add(record.usage.cost_usd)
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
