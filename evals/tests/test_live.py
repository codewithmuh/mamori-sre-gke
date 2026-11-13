from __future__ import annotations

import os
import signal
from collections.abc import Callable
from dataclasses import dataclass, field
from datetime import datetime, timedelta
from typing import Any

import pytest

from mamori_evals.clients import FaultRefusedError, GamedayError, ManualFaultError
from mamori_evals.faults import Fault
from mamori_evals.live import LiveConfig, LiveRunner
from mamori_evals.records import (
    AgentEvent,
    AgentStatus,
    ExecutorEvent,
    FaultEnd,
    Investigation,
    Proposal,
    RunObservation,
    parse_agent_event,
    parse_agent_status,
    parse_executor_event,
    parse_proposal,
)
from mamori_evals.steady import SteadyResult

from .conftest import T0, make_fault, make_investigation

STEADY = SteadyResult(True, ())
UNSTEADY = SteadyResult(False, ("checkout: probe success 0.500 < 0.99",))
CONFIG = LiveConfig(
    investigation_timeout=timedelta(minutes=20),
    poll_interval=timedelta(seconds=15),
    steady_timeout=timedelta(minutes=10),
    steady_interval=timedelta(seconds=30),
    cooldown_min=timedelta(minutes=10),
    cooldown_timeout=timedelta(minutes=20),
    budget=timedelta(hours=4),
)


class FakeClock:
    def __init__(self) -> None:
        self.current = T0
        self.slept = 0.0

    def now(self) -> datetime:
        return self.current

    def sleep(self, seconds: float) -> None:
        self.slept += seconds
        self.current += timedelta(seconds=seconds)


@dataclass
class FakeHandle:
    started_at: datetime
    run_id: str | None = "run-1"
    objects: tuple[str, ...] = ("virtualservice/f01-currency-abort",)
    timeline: str | None = "gameday-runs/run-1.json"
    end_at: datetime | None = None
    clock: FakeClock | None = None
    stopped: bool = False

    def ended(self) -> FaultEnd | None:
        if self.end_at is not None and self.clock is not None and self.clock.now() >= self.end_at:
            return FaultEnd("removed-externally", self.end_at)
        return None

    def stop(self, timeout: float = 200.0) -> FaultEnd:
        self.stopped = True
        return self.ended() or FaultEnd("signal", self.clock.now() if self.clock else None)


@dataclass
class FakeGameday:
    clock: FakeClock
    start_errors: dict[str, BaseException] = field(default_factory=dict)
    stop_error: BaseException | None = None
    stop_all_error: BaseException | None = None
    on_stop: Callable[[], None] | None = None
    end_after: timedelta | None = None
    calls: list[str] = field(default_factory=list)
    handles: list[FakeHandle] = field(default_factory=list)

    def start(self, fault_id: str, duration: timedelta) -> FakeHandle:
        self.calls.append(f"start {fault_id} {duration}")
        if fault_id in self.start_errors:
            raise self.start_errors[fault_id]
        end_at = self.clock.now() + self.end_after if self.end_after else None
        handle = FakeHandle(self.clock.now(), end_at=end_at, clock=self.clock)
        self.handles.append(handle)
        return handle

    def stop(self, fault_id: str) -> None:
        self.calls.append(f"stop {fault_id}")
        if self.on_stop:
            self.on_stop()
        if self.stop_error:
            raise self.stop_error

    def stop_all(self) -> None:
        self.calls.append("stop --all")
        if self.stop_all_error:
            raise self.stop_all_error


class FakeAgent:
    """Investigations appear `delay` after each injection."""

    def __init__(
        self, clock: FakeClock, delay: timedelta | None = timedelta(minutes=3), **inv: Any
    ) -> None:
        self.clock, self.delay, self.inv = clock, delay, inv
        self.injections: list[datetime] = []
        self.errors: list[BaseException] = []
        # Status answers in turn (the last repeats); None means GET /v1/status is not served.
        self.statuses: list[dict[str, Any] | None] = [{"queued": 0, "running": 0}]
        self.status_calls = 0
        self.extra: list[Investigation] = []

    def status(self) -> AgentStatus | None:
        self.status_calls += 1
        answer = self.statuses[min(self.status_calls - 1, len(self.statuses) - 1)]
        if isinstance(answer, dict) and answer.get("raise"):
            raise RuntimeError("status down")
        return parse_agent_status(answer) if answer is not None else None

    def investigations_since(self, since: datetime) -> list[Investigation]:
        if self.errors:
            raise self.errors.pop(0)
        if self.delay is None:
            return list(self.extra)
        found = list(self.extra)
        for index, injected in enumerate(self.injections):
            start = injected + self.delay
            if self.clock.now() >= start:
                doc: dict[str, Any] = {
                    "id": f"inv-{index}",
                    "incidentId": f"incident-{index}",
                    "startedAt": start.isoformat(),
                    "finishedAt": (start + timedelta(minutes=2)).isoformat(),
                    "proposalId": f"p-{index}",
                }
                doc.update(self.inv)
                found.append(make_investigation(**doc))
        return found


class FakeExecutor:
    def __init__(
        self,
        statuses: list[str] | None = None,
        error: Exception | None = None,
        expires_at: datetime | None = None,
    ) -> None:
        self.statuses = statuses or ["pending"]
        self.error = error
        self.expires_at = expires_at
        self.calls = 0

    def proposal(self, proposal_id: str) -> Proposal | None:
        self.calls += 1
        if self.error:
            raise self.error
        status = self.statuses[min(self.calls - 1, len(self.statuses) - 1)]
        if not status:
            return None
        doc: dict[str, Any] = {"proposalId": proposal_id, "status": status}
        if self.expires_at is not None:
            doc["expiresAt"] = self.expires_at.isoformat()
        return parse_proposal(doc)


class FakeLogs:
    def __init__(
        self, events: list[dict[str, Any]] | None = None, error: Exception | None = None
    ) -> None:
        self.events = events or []
        self.error = error

    def events_since(self, since: datetime) -> list[ExecutorEvent]:
        if self.error:
            raise self.error
        return [parse_executor_event(e) for e in self.events]


class FakeSteady:
    def __init__(
        self, results: Callable[[int], SteadyResult | BaseException] = lambda n: STEADY
    ) -> None:
        self.results = results
        self.calls = 0
        self.gates: list[bool] = []

    def check(self, burn_gate: bool = False) -> SteadyResult:
        self.calls += 1
        self.gates.append(burn_gate)
        result = self.results(self.calls)
        if isinstance(result, BaseException):
            raise result
        if burn_gate:
            burn = {"1h": {"browse": 0.2}, "6h": {"browse": 0.1}}
            return SteadyResult(result.ok, result.reasons, burn=burn)
        return result


class FakeAgentLogs:
    def __init__(self, events: list[dict[str, Any]] | None = None, error: bool = False) -> None:
        self.events = events or []
        self.error = error

    def events_since(self, since: datetime) -> list[AgentEvent]:
        if self.error:
            raise RuntimeError("agent logs forbidden")
        return [parse_agent_event(e) for e in self.events]


@dataclass
class Harness:
    faults: list[Fault]
    config: LiveConfig = CONFIG
    clock: FakeClock = field(default_factory=FakeClock)
    agent_delay: timedelta | None = timedelta(minutes=3)
    agent_fields: dict[str, Any] = field(default_factory=dict)
    executor: FakeExecutor = field(default_factory=FakeExecutor)
    logs: FakeLogs = field(default_factory=FakeLogs)
    steady: FakeSteady = field(default_factory=FakeSteady)
    agent_logs: FakeAgentLogs | None = None
    ask: Callable[[str], str] | None = None
    logged: list[str] = field(default_factory=list)
    updates: int = 0
    keepalives: int = 0

    def __post_init__(self) -> None:
        self.gameday = FakeGameday(self.clock)
        self.agent = FakeAgent(self.clock, self.agent_delay, **self.agent_fields)
        original = self.gameday.start

        def start(fault_id: str, duration: timedelta) -> FakeHandle:
            handle = original(fault_id, duration)
            self.agent.injections.append(handle.started_at)
            return handle

        self.gameday.start = start  # type: ignore[method-assign]

    def runner(self) -> LiveRunner:
        def update(runs: list[RunObservation]) -> None:
            self.updates += 1

        def keepalive() -> None:
            self.keepalives += 1

        return LiveRunner(
            self.faults,
            self.config,
            self.clock,
            self.gameday,
            self.agent,
            self.executor,
            self.logs,
            self.steady,
            log=self.logged.append,
            on_update=update,
            ask=self.ask,
            keepalive=keepalive,
            agent_logs=self.agent_logs,
        )


def fault(n: int, **overrides: Any) -> Fault:
    return make_fault(id=f"F0{n}-fault-{n}", **overrides)


def test_happy_path_two_faults() -> None:
    h = Harness([fault(1), fault(2)])
    h.logs.events = [
        {"event": "proposal_received", "proposalId": "p-0", "investigationId": "inv-0"},
        {"event": "proposal_received", "proposalId": "x", "investigationId": "inv-other"},
    ]
    runner = h.runner()
    runs = runner.run()
    assert [r.status for r in runs] == ["completed", "completed"]
    first = runs[0]
    assert first.injected_at == T0 + timedelta(seconds=30)  # two steady checks, 30 s apart
    assert first.investigation is not None
    assert first.investigation.id == "inv-0"
    assert first.proposal is not None
    assert first.proposal.status == "pending"
    assert [e.proposal_id for e in first.executor_events] == ["p-0", "x"]
    assert [inv.id for inv in first.window_investigations or ()] == ["inv-0"]
    assert first.pre_burn == {"1h": {"browse": 0.2}, "6h": {"browse": 0.1}}
    assert first.agent_status_before == {"queued": 0, "running": 0}
    assert True in h.steady.gates
    assert False in h.steady.gates
    assert first.fault_end is not None
    assert first.fault_end.reason == "signal"
    assert first.recovered_at is not None
    assert first.fault_run_id == "run-1"
    assert "gameday timeline: gameday-runs/run-1.json" in first.notes
    assert runs[1].investigation is not None
    assert runs[1].investigation.id == "inv-1"
    assert h.gameday.calls[0] == "start F01-fault-1 0:22:00"
    assert "stop F01-fault-1" in h.gameday.calls
    assert "stop F02-fault-2" in h.gameday.calls
    assert all(handle.stopped for handle in h.gameday.handles)
    assert h.updates >= 4
    assert h.keepalives > 0
    assert not runner.interrupted
    assert runner.abort_reason is None


def test_no_investigation() -> None:
    h = Harness([fault(1)], agent_delay=None)
    (run,) = h.runner().run()
    assert run.status == "completed"
    assert run.investigation is None
    assert "no investigation within 20m00s" in run.notes
    assert h.gameday.handles[0].stopped


def test_running_investigation_at_timeout_is_kept() -> None:
    h = Harness([fault(1)], agent_fields={"status": "running", "finishedAt": None})
    (run,) = h.runner().run()
    assert run.investigation is not None
    assert run.investigation.status == "running"


def test_not_steady_before_the_first_fault() -> None:
    h = Harness([fault(1), fault(2)], steady=FakeSteady(lambda n: UNSTEADY))
    runner = h.runner()
    runs = runner.run()
    assert [r.status for r in runs] == ["skipped", "skipped"]
    assert runs[0].reason is not None
    assert "probe success" in runs[0].reason
    assert runs[1].reason == "shop not ready before F01-fault-1"
    assert h.gameday.calls == []


def test_budget_skips_what_does_not_fit() -> None:
    config = LiveConfig(budget=timedelta(minutes=50), steady_timeout=timedelta(minutes=10))
    h = Harness([fault(1), fault(2)], config=config)
    runs = h.runner().run()
    assert runs[0].status == "completed"
    assert (runs[1].status, runs[1].reason) == ("skipped", "time budget used up")


def test_budget_runs_out_while_waiting() -> None:
    # Room for one run at the start (1 + 20 + 1 min), but the approval wait is longer.
    config = LiveConfig(
        budget=timedelta(minutes=22),
        steady_timeout=timedelta(minutes=1),
        cooldown_min=timedelta(minutes=1),
        proposal_wait=timedelta(minutes=30),
    )
    h = Harness([fault(1)], config=config)
    (run,) = h.runner().run()
    assert (run.status, run.reason) == ("aborted", "time budget used up")
    assert h.gameday.handles[0].stopped


def test_ctrl_c_stops_the_fault_and_the_session() -> None:
    h = Harness([fault(1), fault(2)])
    h.agent.errors = [KeyboardInterrupt()]
    runner = h.runner()
    runs = runner.run()
    assert (runs[0].status, runs[0].reason) == ("aborted", "interrupted")
    assert h.gameday.handles[0].stopped
    assert "stop F01-fault-1" in h.gameday.calls
    assert (runs[1].status, runs[1].reason) == ("skipped", "interrupted")
    assert runner.interrupted


def test_refused_start_ends_the_session() -> None:
    h = Harness([fault(1), fault(2)])
    h.gameday.start_errors["F01-fault-1"] = FaultRefusedError(
        "refused (exit 2)", "a fault is applied"
    )
    runs = h.runner().run()
    assert runs[0].status == "error"
    assert "a fault is applied" in (runs[0].reason or "")
    assert "stop F01-fault-1" in h.gameday.calls  # stopped anyway
    assert runs[1].reason == "gameday refused F01-fault-1"


def test_other_start_errors_continue() -> None:
    h = Harness([fault(1), fault(2)])
    h.gameday.start_errors["F01-fault-1"] = GamedayError("exit 1")
    runs = h.runner().run()
    assert runs[0].status == "error"
    assert runs[1].status == "completed"


def test_manual_fault_is_skipped_without_a_prompt() -> None:
    h = Harness([fault(1)])
    h.gameday.start_errors["F01-fault-1"] = ManualFaultError("exit 3", "drain the node")
    (run,) = h.runner().run()
    assert (run.status, run.reason) == ("skipped", "needs a manual procedure (gameday exit 3)")
    assert h.gameday.calls == ["start F01-fault-1 0:22:00"]


def test_manual_fault_with_prompt() -> None:
    questions: list[str] = []
    h = Harness(
        [fault(1)],
        config=LiveConfig(manual_faults="prompt"),
        ask=lambda q: questions.append(q) or "",
    )
    h.gameday.start_errors["F01-fault-1"] = ManualFaultError("exit 3", "1. drain the node")
    # The operator presses Enter right after the two steady checks (30 s).
    h.agent.injections.append(T0 + timedelta(seconds=30))
    (run,) = h.runner().run()
    assert run.status == "completed"
    assert "injected by hand" in run.notes
    assert run.injected_at == T0 + timedelta(seconds=30)
    assert run.investigation is not None
    assert len(questions) == 2
    assert "Undo F01-fault-1" in questions[1]
    assert "1. drain the node" in h.logged


def test_stop_falls_back_to_stop_all() -> None:
    h = Harness([fault(1)])
    h.gameday.stop_error = GamedayError("stop failed")
    runner = h.runner()
    (run,) = runner.run()
    assert "stop --all" in h.gameday.calls
    assert runner.abort_reason is None
    assert run.recovered_at is not None


def test_stop_failure_ends_the_session_loudly() -> None:
    h = Harness([fault(1), fault(2)])
    h.gameday.stop_error = GamedayError("stop failed")
    h.gameday.stop_all_error = GamedayError("stop --all failed")
    runner = h.runner()
    runs = runner.run()
    assert any("stop --all failed" in note for note in runs[0].notes)
    assert runs[1].reason == "could not confirm that F01-fault-1 was removed"
    assert any(line.startswith("WARNING") for line in h.logged)


def test_api_errors_are_noted() -> None:
    h = Harness(
        [fault(1)],
        executor=FakeExecutor(error=RuntimeError("executor down")),
        logs=FakeLogs(error=RuntimeError("forbidden")),
    )
    h.agent.errors = [RuntimeError("agent down"), RuntimeError("agent down")]
    (run,) = h.runner().run()
    assert run.notes.count("agent API error: agent down") == 1
    assert "executor API error: executor down" in run.notes
    assert "executor audit log not read: forbidden" in run.notes
    assert run.investigation is not None


def test_proposal_wait_follows_the_status() -> None:
    config = LiveConfig(proposal_wait=timedelta(minutes=5))
    executor = FakeExecutor(["pending", "approved", "executed"])
    h = Harness([fault(1)], config=config, executor=executor)
    (run,) = h.runner().run()
    assert run.proposal is not None
    assert run.proposal.status == "executed"
    assert executor.calls == 4  # three while waiting, one more at the end of the session


def test_refused_proposal_reads_as_none() -> None:
    h = Harness([fault(1)], executor=FakeExecutor([""]))
    (run,) = h.runner().run()
    assert run.proposal is None


def test_fault_ending_early_is_noted() -> None:
    h = Harness([fault(1)], agent_delay=timedelta(minutes=8))
    h.gameday.end_after = timedelta(minutes=5)
    (run,) = h.runner().run()
    assert run.fault_end is not None
    assert run.fault_end.reason == "removed-externally"
    assert "fault ended before the investigation: removed-externally" in run.notes


def test_no_recovery_ends_the_session() -> None:
    steady = FakeSteady(lambda n: STEADY if n <= 2 else UNSTEADY)
    h = Harness([fault(1), fault(2)], steady=steady)
    runs = h.runner().run()
    assert runs[0].recovered_at is None
    assert any(note.startswith("after the run: not steady") for note in runs[0].notes)
    assert runs[1].reason == "shop did not recover after F01-fault-1"


def test_steady_query_errors_count_as_not_steady() -> None:
    steady = FakeSteady(lambda n: RuntimeError("prometheus down"))
    h = Harness([fault(1)], steady=steady)
    (run,) = h.runner().run()
    assert run.status == "skipped"
    assert "prometheus down" in (run.reason or "")


@pytest.mark.parametrize(
    ("error", "reason"), [(KeyboardInterrupt(), "interrupted"), (None, "time budget used up")]
)
def test_cooldown_interruptions(error: BaseException | None, reason: str) -> None:
    if error is None:
        config = LiveConfig(budget=timedelta(minutes=35), steady_timeout=timedelta(minutes=1))
        h = Harness([fault(1), fault(2)], config=config)
    else:
        h = Harness([fault(1), fault(2)], steady=FakeSteady(lambda n: STEADY if n <= 2 else error))
    runner = h.runner()
    runs = runner.run()
    assert runs[0].status == "completed"
    assert runs[1].reason == reason


def test_executor_lines_without_an_investigation_are_kept() -> None:
    h = Harness(
        [fault(1)], agent_delay=None, logs=FakeLogs([{"event": "blocked", "proposalId": "x"}])
    )
    (run,) = h.runner().run()
    assert [e.proposal_id for e in run.executor_events] == ["x"]


@pytest.mark.parametrize("sig", [signal.SIGINT, signal.SIGHUP])
def test_signals_wait_until_the_fault_is_gone(sig: signal.Signals) -> None:
    h = Harness([fault(1), fault(2)])
    h.gameday.on_stop = lambda: os.kill(os.getpid(), sig)
    runner = h.runner()
    runs = runner.run()
    assert runs[0].status == "completed"
    assert h.gameday.handles[0].stopped
    assert runner.interrupted
    assert runs[1].reason == "interrupted"
    assert signal.getsignal(signal.SIGINT) is signal.default_int_handler


def test_config() -> None:
    assert LiveConfig().fault_duration == timedelta(minutes=22)
    assert LiveConfig(investigation_timeout=timedelta(hours=1)).fault_duration == timedelta(
        minutes=30
    )
    assert LiveConfig().max_run_time == timedelta(minutes=127)
    assert LiveConfig().room_needed == timedelta(minutes=105)
    with pytest.raises(ValueError, match="manual_faults"):
        Harness([fault(1)], config=LiveConfig(manual_faults="yolo")).runner()


def test_waits_for_the_agent_to_be_idle() -> None:
    h = Harness([fault(1)])
    h.agent.statuses = [
        {"queued": 1, "running": 1},
        {"queued": 0, "running": 1},
        {"queued": 0, "running": 0},
    ]
    (run,) = h.runner().run()
    assert run.status == "completed"
    assert "waiting for the agent to finish 2 investigation(s)" in h.logged
    assert run.injected_at == T0 + timedelta(seconds=60)  # 2 polls, then 2 steady checks


@pytest.mark.parametrize(
    ("statuses", "reason"),
    [
        ([{"queued": 1, "running": 0}], "the agent is still busy with earlier investigations"),
        ([{"raise": True}], "the agent's status could not be read"),
    ],
)
def test_a_busy_agent_ends_the_session(statuses: list[dict[str, Any]], reason: str) -> None:
    h = Harness([fault(1), fault(2)])
    h.agent.statuses = statuses
    runs = h.runner().run()
    assert [(r.status, r.reason) for r in runs] == [("skipped", reason)] * 2
    assert h.gameday.calls == []


def test_without_a_status_endpoint_busy_records_count() -> None:
    h = Harness([fault(1)])
    h.agent.statuses = [None]
    h.agent.extra = [make_investigation(id="old", status="queued", finishedAt=None)]
    (run,) = h.runner().run()
    assert run.reason == "the agent is still busy with earlier investigations"


def test_too_little_model_budget_ends_the_session() -> None:
    h = Harness([fault(1)])
    h.agent.statuses = [
        {
            "queued": 0,
            "running": 0,
            "dailySpentUsd": 14.0,
            "dailyReservedUsd": 0.0,
            "agentConfig": {"dailyCostLimitUsd": 15, "maxCostUsd": 1.5},
        }
    ]
    (run,) = h.runner().run()
    assert run.status == "skipped"
    assert "raise AGENT_DAILY_COST_LIMIT_USD" in (run.reason or "")


def test_agent_intake_lines_are_recorded() -> None:
    line = {"time": (T0 + timedelta(minutes=2)).isoformat(), "event": "incident_skipped"}
    old = {"time": (T0 - timedelta(hours=1)).isoformat(), "event": "incident_skipped"}
    h = Harness([fault(1)], agent_logs=FakeAgentLogs([{**line, "reason": "ticket"}, old]))
    (run,) = h.runner().run()
    assert run.agent_events is not None
    assert [e.reason for e in run.agent_events] == ["ticket"]
    broken = Harness([fault(1)], agent_logs=FakeAgentLogs(error=True))
    (run,) = broken.runner().run()
    assert run.agent_events is None
    assert "agent intake log not read: agent logs forbidden" in run.notes


def test_pending_proposals_are_read_again_after_they_expire() -> None:
    executor = FakeExecutor(["pending", "expired"], expires_at=T0 + timedelta(minutes=40))
    h = Harness([fault(1)], executor=executor)
    (run,) = h.runner().run()
    assert run.proposal is not None
    assert run.proposal.status == "expired"
    assert h.clock.now() >= T0 + timedelta(minutes=40)
    assert any(line.startswith("waiting") and "expire" in line for line in h.logged)


def test_the_reread_tolerates_errors() -> None:
    executor = FakeExecutor(["pending"])
    h = Harness([fault(1)], executor=executor)
    runner = h.runner()
    original = executor.proposal

    def flaky(proposal_id: str) -> Proposal | None:
        if executor.calls >= 1:
            executor.calls += 1
            raise RuntimeError("gone")
        return original(proposal_id)

    executor.proposal = flaky  # type: ignore[method-assign]
    (run,) = runner.run()
    assert "proposal not read again: gone" in run.notes
