"""Live runner: inject one fault at a time into the lab and record what the agent does.

Per fault: wait for steady state, `gameday start`, wait for the matching investigation, read
the proposal, stop the fault, read the executor's audit lines, cool down until steady again.
The fault is stopped in every case (error, timeout, Ctrl-C, budget): the gameday process gets
SIGTERM, then `gameday stop <id>` runs, then `gameday stop --all` if that failed. The gameday
CLI also ends the fault on its own after `fault_duration`, in case this process dies.

The runner never sends the agent anything: investigations start from real alerts, and the
expected answers stay in this process.
"""

from __future__ import annotations

import contextlib
import signal
import threading
from collections.abc import Callable, Iterator, Sequence
from dataclasses import dataclass
from datetime import datetime, timedelta
from typing import Protocol

from .clients import FaultRefusedError, ManualFaultError
from .faults import Fault
from .records import (
    TERMINAL_STATUSES,
    ExecutorEvent,
    FaultEnd,
    Investigation,
    Proposal,
    RunObservation,
)
from .scoring import events_for, match_investigation
from .steady import SteadyResult
from .timeutil import Clock, format_seconds

MANUAL_MODES = ("skip", "prompt")


@dataclass(frozen=True)
class LiveConfig:
    investigation_timeout: timedelta = timedelta(minutes=20)
    poll_interval: timedelta = timedelta(seconds=15)
    steady_timeout: timedelta = timedelta(minutes=10)
    steady_interval: timedelta = timedelta(seconds=30)
    steady_checks: int = 2
    cooldown_min: timedelta = timedelta(minutes=10)
    cooldown_timeout: timedelta = timedelta(minutes=20)
    proposal_wait: timedelta = timedelta(0)
    clock_skew: timedelta = timedelta(seconds=30)
    budget: timedelta = timedelta(hours=4)
    # The gameday CLI holds a fault for at most 30 minutes (#23).
    max_fault_duration: timedelta = timedelta(minutes=30)
    manual_faults: str = "skip"

    @property
    def fault_duration(self) -> timedelta:
        """Long enough for the agent and the approval wait, and no longer: the gameday CLI
        removes the fault by itself after this, even if the runner has died."""
        wanted = self.investigation_timeout + self.proposal_wait + timedelta(minutes=2)
        return min(wanted, self.max_fault_duration)

    @property
    def max_run_time(self) -> timedelta:
        """Worst case for one fault, for planning a session."""
        return self.steady_timeout + self.fault_duration + self.cooldown_min + self.cooldown_timeout


class RunningFault(Protocol):
    @property
    def run_id(self) -> str | None: ...

    @property
    def started_at(self) -> datetime: ...

    @property
    def objects(self) -> tuple[str, ...]: ...

    @property
    def timeline(self) -> str | None: ...

    def ended(self) -> FaultEnd | None: ...

    def stop(self, timeout: float = ...) -> FaultEnd: ...


class Gameday(Protocol):
    def start(self, fault_id: str, duration: timedelta) -> RunningFault: ...

    def stop(self, fault_id: str) -> None: ...

    def stop_all(self) -> None: ...


class InvestigationSource(Protocol):
    def investigations_since(self, since: datetime) -> list[Investigation]: ...


class ProposalSource(Protocol):
    def proposal(self, proposal_id: str) -> Proposal | None: ...


class ExecutorEventSource(Protocol):
    def events_since(self, since: datetime) -> list[ExecutorEvent]: ...


class SteadySource(Protocol):
    def check(self) -> SteadyResult: ...


class BudgetExceededError(Exception):
    """The session's time budget ran out."""


class LiveRunner:
    def __init__(
        self,
        faults: Sequence[Fault],
        config: LiveConfig,
        clock: Clock,
        gameday: Gameday,
        agent: InvestigationSource,
        executor: ProposalSource,
        executor_logs: ExecutorEventSource,
        steady: SteadySource,
        log: Callable[[str], None],
        on_update: Callable[[list[RunObservation]], None] | None = None,
        ask: Callable[[str], str] | None = None,
        keepalive: Callable[[], None] | None = None,
    ) -> None:
        if config.manual_faults not in MANUAL_MODES:
            raise ValueError(f"manual_faults must be one of {MANUAL_MODES}")
        self.faults = list(faults)
        self.config = config
        self.clock = clock
        self.gameday = gameday
        self.agent = agent
        self.executor = executor
        self.executor_logs = executor_logs
        self.steady = steady
        self.log = log
        self.on_update = on_update
        self.ask = ask
        self.keepalive = keepalive
        self.deadline = clock.now() + config.budget
        self.interrupted = False
        self.abort_reason: str | None = None
        self.used_investigations: set[str] = set()
        self._signal_pending = False

    # --- session -------------------------------------------------------------------------

    def run(self) -> list[RunObservation]:
        self.deadline = self.clock.now() + self.config.budget
        results: list[RunObservation] = []
        for fault in self.faults:
            if self.abort_reason is None:
                self._check_session_room()
            if self.abort_reason is not None:
                results.append(RunObservation(fault.id, "skipped", reason=self.abort_reason))
                self._update(results)
                continue
            before = self._guarded(lambda: self._wait_steady(self.config.steady_timeout))
            if before is None:
                results.append(RunObservation(fault.id, "skipped", reason=self.abort_reason))
                self._update(results)
                continue
            if not before.ok:
                self.abort_reason = f"shop not steady before {fault.id}"
                results.append(RunObservation(fault.id, "skipped", reason=before.describe()))
                self._update(results)
                continue
            observation = self._run_one(fault)
            results.append(observation)
            self._update(results)
            if self._signal_pending:
                self.interrupted = True
                self.abort_reason = self.abort_reason or "interrupted"
            if observation.injected_at is None or self.abort_reason is not None:
                continue
            cooldown = format_seconds(self.config.cooldown_min.total_seconds())
            self.log(f"{fault.id}: cooling down for at least {cooldown}")
            after = self._guarded(self._cool_down)
            if after is not None:
                if after.ok:
                    observation.recovered_at = self.clock.now()
                else:
                    self.abort_reason = f"shop did not recover after {fault.id}"
                    observation.notes.append(f"after the run: {after.describe()}")
            self._update(results)
        return results

    def _check_session_room(self) -> None:
        needed = self.config.investigation_timeout + self.config.cooldown_min
        if self.deadline - self.clock.now() < needed:
            self.abort_reason = "time budget used up"

    def _guarded(self, step: Callable[[], SteadyResult]) -> SteadyResult | None:
        """A wait with no fault applied: budget and Ctrl-C end the session cleanly."""
        try:
            return step()
        except BudgetExceededError:
            self.abort_reason = "time budget used up"
        except KeyboardInterrupt:
            self.interrupted = True
            self.abort_reason = "interrupted"
        return None

    def _update(self, results: list[RunObservation]) -> None:
        if self.on_update is not None:
            self.on_update(results)

    # --- one fault -----------------------------------------------------------------------

    def _run_one(self, fault: Fault) -> RunObservation:
        observation = RunObservation(fault.id, "completed")
        handle: RunningFault | None = None
        manual = False
        try:
            try:
                self.log(f"{fault.id}: injecting")
                handle = self.gameday.start(fault.id, self.config.fault_duration)
            except ManualFaultError as err:
                if self.config.manual_faults != "prompt" or self.ask is None:
                    observation.status = "skipped"
                    observation.reason = "needs a manual procedure (gameday exit 3)"
                    return observation
                manual = True
                self._inject_by_hand(fault, err, observation, self.ask)
            if handle is not None:
                observation.injected_at = handle.started_at
                observation.fault_run_id = handle.run_id
                observation.fault_objects = handle.objects
                if handle.timeline:
                    observation.notes.append(f"gameday timeline: {handle.timeline}")
            self._observe(fault, observation, handle)
        except KeyboardInterrupt:
            self.interrupted = True
            self.abort_reason = "interrupted"
            observation.status, observation.reason = "aborted", "interrupted"
        except BudgetExceededError:
            self.abort_reason = "time budget used up"
            observation.status, observation.reason = "aborted", "time budget used up"
        except FaultRefusedError as err:
            # Exit 2: another fault is still applied, or a usage error. Nothing more is safe.
            self.abort_reason = f"gameday refused {fault.id}"
            observation.status = "error"
            observation.reason = f"{err} {err.output.strip()[-300:]}".strip()
        except Exception as err:  # a failed run must still stop the fault
            observation.status, observation.reason = "error", f"{type(err).__name__}: {err}"
        finally:
            if observation.status != "skipped":
                self._stop(fault, handle, observation, manual)
        self._collect_executor_events(observation)
        return observation

    def _inject_by_hand(
        self,
        fault: Fault,
        err: ManualFaultError,
        observation: RunObservation,
        ask: Callable[[str], str],
    ) -> None:
        if err.output.strip():
            self.log(err.output.strip())
        ask(
            f"{fault.id} is applied by hand: follow the steps above, "
            "then press Enter the moment it is in effect"
        )
        observation.injected_at = self.clock.now()
        observation.notes.append("injected by hand")

    def _observe(
        self, fault: Fault, observation: RunObservation, handle: RunningFault | None
    ) -> None:
        injected_at = observation.injected_at
        if injected_at is None:
            raise RuntimeError("no injection time")
        deadline = self.clock.now() + self.config.investigation_timeout
        self.log(f"{fault.id}: injected at {injected_at:%H:%M:%S}Z; waiting for the agent")
        investigation = self._wait_investigation(fault, observation, injected_at, handle, deadline)
        observation.investigation = investigation
        if investigation is None:
            observation.notes.append(
                "no investigation within "
                + format_seconds(self.config.investigation_timeout.total_seconds())
            )
            self.log(f"{fault.id}: no investigation")
            return
        self.used_investigations.add(investigation.id)
        self.log(f"{fault.id}: investigation {investigation.id} ({investigation.status or 'n/a'})")
        if investigation.proposal_id:
            observation.proposal = self._read_proposal(investigation.proposal_id, observation)

    def _wait_investigation(
        self,
        fault: Fault,
        observation: RunObservation,
        injected_at: datetime,
        handle: RunningFault | None,
        deadline: datetime,
    ) -> Investigation | None:
        best: Investigation | None = None
        ended_noted = False
        while True:
            self._check_budget()
            if self.keepalive is not None:
                self.keepalive()
            try:
                candidates = self.agent.investigations_since(injected_at - self.config.clock_skew)
            except Exception as err:  # keep polling; the API may come back
                _note_once(observation, f"agent API error: {err}")
            else:
                match = match_investigation(
                    candidates,
                    injected_at,
                    deadline,
                    fault.cujs,
                    self.config.clock_skew,
                    self.used_investigations,
                )
                if match is not None:
                    best = match
                    if match.finished:
                        return match
            if handle is not None and not ended_noted:
                end = handle.ended()
                if end is not None:
                    ended_noted = True
                    observation.fault_end = end
                    observation.notes.append(f"fault ended before the investigation: {end.reason}")
            now = self.clock.now()
            if now >= deadline:
                return best
            self._sleep(min(self.config.poll_interval, deadline - now).total_seconds())

    def _read_proposal(self, proposal_id: str, observation: RunObservation) -> Proposal | None:
        deadline = self.clock.now() + self.config.proposal_wait
        latest: Proposal | None = None
        while True:
            try:
                latest = self.executor.proposal(proposal_id)
            except Exception as err:  # recorded; scoring falls back to the audit log
                _note_once(observation, f"executor API error: {err}")
            now = self.clock.now()
            if latest is None or latest.status in TERMINAL_STATUSES or now >= deadline:
                return latest
            self._sleep(min(self.config.poll_interval, deadline - now).total_seconds())

    def _stop(
        self,
        fault: Fault,
        handle: RunningFault | None,
        observation: RunObservation,
        manual: bool,
    ) -> None:
        problems: list[str] = []
        with self._signals_deferred():
            if handle is not None:
                try:
                    end = handle.stop()
                    observation.fault_end = observation.fault_end or end
                except Exception as err:  # fall through to gameday stop
                    problems.append(f"stopping the gameday process: {err}")
            try:
                self.gameday.stop(fault.id)
            except Exception as err:  # try the wider stop next
                problems.append(f"gameday stop {fault.id}: {err}")
                try:
                    self.gameday.stop_all()
                except Exception as err_all:  # reported below
                    problems.append(f"gameday stop --all: {err_all}")
                else:
                    problems.clear()
            if manual and self.ask is not None:
                self.ask(f"Undo {fault.id} by hand now (see the steps above), then press Enter")
        observation.stopped_at = self.clock.now()
        if problems:
            observation.notes.extend(problems)
            self.abort_reason = f"could not confirm that {fault.id} was removed"
            self.log(
                f"WARNING: could not confirm that {fault.id} was removed. "
                "Check now with `gameday status` and `gameday stop --all`."
            )
        else:
            self.log(f"{fault.id}: stopped")

    def _collect_executor_events(self, observation: RunObservation) -> None:
        if observation.injected_at is None:
            return
        try:
            events = self.executor_logs.events_since(
                observation.injected_at - self.config.clock_skew
            )
        except Exception as err:  # scoring still has the proposal and the 4xx
            observation.notes.append(f"executor audit log not read: {err}")
            return
        investigation = observation.investigation
        if investigation is None:
            if events:
                observation.notes.append(
                    f"{len(events)} executor audit lines in the window, "
                    "none tied to an investigation"
                )
            return
        observation.executor_events = tuple(events_for(investigation, events))

    # --- waiting -------------------------------------------------------------------------

    def _cool_down(self) -> SteadyResult:
        self._sleep(self.config.cooldown_min.total_seconds())
        return self._wait_steady(self.config.cooldown_timeout)

    def _wait_steady(self, timeout: timedelta) -> SteadyResult:
        deadline = self.clock.now() + timeout
        streak = 0
        while True:
            self._check_budget()
            try:
                result = self.steady.check()
            except Exception as err:  # a failed query is "not steady yet"
                result = SteadyResult(False, (f"steady-state query failed: {err}",))
            streak = streak + 1 if result.ok else 0
            now = self.clock.now()
            if streak >= self.config.steady_checks or now >= deadline:
                return result
            self._sleep(min(self.config.steady_interval, deadline - now).total_seconds())

    def _check_budget(self) -> None:
        if self.clock.now() >= self.deadline:
            raise BudgetExceededError

    def _sleep(self, seconds: float) -> None:
        left = (self.deadline - self.clock.now()).total_seconds()
        if left <= 0:
            raise BudgetExceededError
        self.clock.sleep(max(0.0, min(seconds, left)))

    @contextlib.contextmanager
    def _signals_deferred(self) -> Iterator[None]:
        """While the fault is being removed, Ctrl-C and SIGTERM wait until it is gone."""
        if threading.current_thread() is not threading.main_thread():
            yield
            return

        def remember(signum: int, frame: object) -> None:
            self._signal_pending = True

        previous = {sig: signal.signal(sig, remember) for sig in (signal.SIGINT, signal.SIGTERM)}
        try:
            yield
        finally:
            for sig, handler in previous.items():
                signal.signal(sig, handler)


def _note_once(observation: RunObservation, note: str) -> None:
    if note not in observation.notes:
        observation.notes.append(note)
