"""The outside world for the live runner: the agent and executor APIs, Managed Prometheus,
the gameday CLI and the executor's audit log. Standard library only (urllib, subprocess)."""

from __future__ import annotations

import json
import queue
import shlex
import subprocess
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass, field
from datetime import UTC, datetime, timedelta
from typing import IO

from .jsonutil import DocumentError, as_mapping, get_list, get_opt_str, get_opt_time, get_str
from .records import (
    LIFECYCLE_EVENTS,
    ExecutorEvent,
    FaultEnd,
    Investigation,
    Proposal,
    parse_executor_event,
    parse_investigation,
    parse_proposal,
)
from .timeutil import format_time, go_duration


class ApiError(RuntimeError):
    def __init__(self, message: str, status: int | None = None) -> None:
        super().__init__(message)
        self.status = status


# --- Commands --------------------------------------------------------------------------------


@dataclass(frozen=True)
class CommandResult:
    code: int
    stdout: str
    stderr: str


type CommandRunner = Callable[[Sequence[str], float], CommandResult]


def run_command(argv: Sequence[str], timeout: float) -> CommandResult:
    """Run a command without a shell; argv comes from the operator's own flags."""
    done = subprocess.run(  # noqa: S603 - no shell, operator-supplied argv
        list(argv), capture_output=True, text=True, timeout=timeout, check=False
    )
    return CommandResult(done.returncode, done.stdout, done.stderr)


def split_command(command: str) -> list[str]:
    argv = shlex.split(command)
    if not argv:
        raise ValueError("empty command")
    return argv


class CommandToken:
    """A bearer token printed by a command (default `gcloud auth print-access-token`).

    Access tokens last an hour; a session runs for hours, so the token is fetched again after
    `max_age`.
    """

    def __init__(
        self,
        argv: Sequence[str],
        max_age: timedelta = timedelta(minutes=30),
        runner: CommandRunner = run_command,
        monotonic: Callable[[], float] = time.monotonic,
    ) -> None:
        self._argv = list(argv)
        self._max_age = max_age.total_seconds()
        self._runner = runner
        self._monotonic = monotonic
        self._token: str | None = None
        self._fetched = 0.0

    def __call__(self) -> str:
        now = self._monotonic()
        if self._token is None or now - self._fetched >= self._max_age:
            result = self._runner(self._argv, 60.0)
            token = result.stdout.strip()
            if result.code != 0 or not token:
                raise ApiError(f"token command {self._argv[0]} failed: {result.stderr.strip()}")
            self._token, self._fetched = token, now
        return self._token


# --- HTTP ------------------------------------------------------------------------------------


class JsonHttp:
    def __init__(
        self,
        base_url: str,
        timeout: float = 15.0,
        token: Callable[[], str] | None = None,
    ) -> None:
        scheme = urllib.parse.urlsplit(base_url).scheme
        if scheme not in ("http", "https"):
            raise ValueError(f"URL must be http or https: {base_url!r}")
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout
        self.token = token

    def get(self, path: str, params: Mapping[str, str] | None = None) -> object:
        url = self.base_url + path
        if params:
            url += "?" + urllib.parse.urlencode(params)
        headers = {"Accept": "application/json"}
        if self.token is not None:
            headers["Authorization"] = f"Bearer {self.token()}"
        request = urllib.request.Request(url, headers=headers, method="GET")  # noqa: S310 - scheme checked above
        try:
            with urllib.request.urlopen(request, timeout=self.timeout) as response:  # noqa: S310 - scheme checked above
                body = response.read()
        except urllib.error.HTTPError as err:
            detail = err.read().decode("utf-8", "replace")[:300]
            raise ApiError(f"GET {path}: HTTP {err.code}: {detail}", err.code) from err
        except (urllib.error.URLError, TimeoutError, OSError) as err:
            raise ApiError(f"GET {path}: {err}") from err
        try:
            return json.loads(body)
        except json.JSONDecodeError as err:
            raise ApiError(f"GET {path}: not JSON: {err}") from err


class AgentApi:
    """GET /v1/investigations (#24): {"investigations": [...]}, newest first."""

    def __init__(self, http: JsonHttp) -> None:
        self.http = http

    def investigations_since(self, since: datetime) -> list[Investigation]:
        document = self.http.get(
            "/v1/investigations", {"since": format_time(since), "limit": "200"}
        )
        if isinstance(document, list):
            items: list[object] = list(document)
        else:
            items = get_list(as_mapping(document, "investigations response"), "investigations")
        return [parse_investigation(item) for item in items]


class ExecutorApi:
    """GET /v1/proposals/{id} (#25). Refused proposals are not stored: 404."""

    def __init__(self, http: JsonHttp) -> None:
        self.http = http

    def proposal(self, proposal_id: str) -> Proposal | None:
        path = "/v1/proposals/" + urllib.parse.quote(proposal_id, safe="")
        try:
            return parse_proposal(self.http.get(path))
        except ApiError as err:
            if err.status == 404:
                return None
            raise


def managed_prometheus_url(project_id: str) -> str:
    """Managed Service for Prometheus query API (needs roles/monitoring.viewer)."""
    project = urllib.parse.quote(project_id, safe="")
    return f"https://monitoring.googleapis.com/v1/projects/{project}/location/global/prometheus"


class PrometheusApi:
    def __init__(self, http: JsonHttp) -> None:
        self.http = http

    def query(self, promql: str) -> list[tuple[dict[str, str], float]]:
        """Instant query; returns (labels, value) per series of a vector result."""
        document = as_mapping(self.http.get("/api/v1/query", {"query": promql}), "query")
        if get_str(document, "status") != "success":
            raise ApiError(f"query failed: {get_str(document, 'error')}")
        data = as_mapping(document.get("data"), "data")
        series = []
        for item in get_list(data, "result"):
            entry = as_mapping(item, "result")
            labels = {
                key: str(value) for key, value in as_mapping(entry.get("metric"), "metric").items()
            }
            value = get_list(entry, "value")
            if len(value) != 2:
                raise DocumentError(f"unexpected sample {value!r}")
            series.append((labels, float(str(value[1]))))
        return series


# --- Executor audit log ----------------------------------------------------------------------


def parse_executor_log(text: str) -> list[ExecutorEvent]:
    """The executor's lifecycle lines (one JSON object per line); everything else is skipped."""
    events = []
    for line in text.splitlines():
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            document = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(document, dict) and document.get("event") in LIFECYCLE_EVENTS:
            events.append(parse_executor_event(document))
    return events


class ExecutorLogs:
    def __init__(self, argv: Sequence[str], runner: CommandRunner = run_command) -> None:
        self.argv = list(argv)
        self.runner = runner

    def events_since(self, since: datetime) -> list[ExecutorEvent]:
        result = self.runner([*self.argv, f"--since-time={format_time(since)}"], 120.0)
        if result.code != 0:
            raise ApiError(f"executor logs: {result.stderr.strip()[:300]}")
        return parse_executor_log(result.stdout)


# --- gameday CLI (#23) -----------------------------------------------------------------------


# On SIGTERM the gameday CLI waits up to 3 minutes for its objects to go (Chaos Mesh recovers
# the pods first, behind a finalizer).
STOP_TIMEOUT = 200.0


class GamedayError(RuntimeError):
    def __init__(self, message: str, output: str = "") -> None:
        super().__init__(message)
        self.output = output


class FaultRefusedError(GamedayError):
    """Exit 2: usage error or a safety check (another fault is still applied)."""


class ManualFaultError(GamedayError):
    """Exit 3: the fault needs a manual procedure; `output` holds the steps."""


@dataclass
class FaultProcess:
    """A running `gameday start -o json`: it holds the fault in the foreground until its
    duration ends, it gets a signal, or something else deletes the objects."""

    process: subprocess.Popen[str]
    lines: queue.Queue[str | None]
    run_id: str | None
    started_at: datetime
    objects: tuple[str, ...]
    timeline: str | None
    output: list[str] = field(default_factory=list)
    end: FaultEnd | None = None
    eof: bool = False

    def _drain_to_eof(self, timeout: float = 5.0) -> None:
        """After exit, read what the pipe still holds (the 'finished' line comes last)."""
        deadline = time.monotonic() + timeout
        while not self.eof and time.monotonic() < deadline:
            try:
                line = self.lines.get(timeout=0.2)
            except queue.Empty:
                continue
            if line is None:
                self.eof = True
                return
            event = _json_line(line)
            if event is not None and event.get("event") == "finished":
                self.end = FaultEnd(
                    reason=get_str(event, "reason"), at=get_opt_time(event, "finishedAt")
                )
            else:
                self.output.append(line)

    def _final(self) -> FaultEnd:
        self._drain_to_eof()
        return self.end or FaultEnd(reason=f"exit {self.process.returncode}", at=None)

    def ended(self) -> FaultEnd | None:
        """Non-blocking: how the fault ended, once the process has exited."""
        if self.process.poll() is None:
            return None
        return self._final()

    def stop(self, timeout: float = STOP_TIMEOUT) -> FaultEnd:
        """SIGTERM (the CLI deletes its objects and waits until they are gone), then SIGKILL if
        it hangs; `gameday stop <id>` afterwards does the same cleanup."""
        if self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=timeout)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=10)
        return self._final()


def _json_line(line: str) -> Mapping[str, object] | None:
    text = line.strip()
    if not text.startswith("{"):
        return None
    try:
        document = json.loads(text)
    except json.JSONDecodeError:
        return None
    return as_mapping(document, "gameday output") if isinstance(document, dict) else None


def _manual_steps(event: Mapping[str, object]) -> str:
    """Exit 3 with -o json: {"event":"manual","faultId","inject":[...],"revert":[...]}."""
    lines = [f"{get_str(event, 'faultId')} is applied by hand."]
    for title, key in (("To inject", "inject"), ("To revert", "revert")):
        lines.append(f"{title}:")
        lines += [f"  {n}. {step}" for n, step in enumerate(get_list(event, key), start=1)]
    return "\n".join(lines)


def _pump(stream: IO[str], lines: queue.Queue[str | None]) -> None:
    for line in stream:
        lines.put(line.rstrip("\n"))
    lines.put(None)


type Popen = Callable[..., subprocess.Popen[str]]


class GamedayCli:
    def __init__(
        self,
        argv: Sequence[str],
        runner: CommandRunner = run_command,
        popen: Popen = subprocess.Popen,
        start_timeout: float = 180.0,
    ) -> None:
        self.argv = list(argv)
        self.runner = runner
        self.popen = popen
        self.start_timeout = start_timeout

    def start(self, fault_id: str, duration: timedelta) -> FaultProcess:
        argv = [*self.argv, "start", fault_id, "--duration", go_duration(duration), "-o", "json"]
        process = self.popen(argv, stdout=subprocess.PIPE, text=True, bufsize=1)
        if process.stdout is None:
            raise GamedayError("gameday start: no stdout pipe")
        lines: queue.Queue[str | None] = queue.Queue()
        threading.Thread(target=_pump, args=(process.stdout, lines), daemon=True).start()
        output: list[str] = []
        deadline = time.monotonic() + self.start_timeout
        while True:
            try:
                line = lines.get(timeout=0.5)
            except queue.Empty:
                if time.monotonic() > deadline:
                    process.terminate()
                    process.wait(timeout=60)
                    raise GamedayError(
                        f"gameday start {fault_id}: no 'started' line in {self.start_timeout:.0f}s",
                        "\n".join(output),
                    ) from None
                continue
            if line is None:
                code = process.wait(timeout=60)
                text = "\n".join(output)
                if code == 2:
                    raise FaultRefusedError(f"gameday start {fault_id} refused (exit 2)", text)
                if code == 3:
                    raise ManualFaultError(f"{fault_id} needs a manual procedure (exit 3)", text)
                raise GamedayError(f"gameday start {fault_id} ended with exit {code}", text)
            event = _json_line(line)
            if event is not None and event.get("event") == "manual":
                output.append(_manual_steps(event))
                continue
            if event is None or event.get("event") != "started":
                output.append(line)
                continue
            started = get_opt_time(event, "startedAt")
            return FaultProcess(
                process=process,
                lines=lines,
                run_id=get_opt_str(event, "runId"),
                started_at=started or datetime.now(UTC),
                objects=tuple(str(obj) for obj in get_list(event, "objects")),
                timeline=get_opt_str(event, "timeline"),
                output=output,
            )

    def stop(self, fault_id: str) -> None:
        self._run("stop", fault_id)

    def stop_all(self) -> None:
        self._run("stop", "--all")

    def _run(self, *args: str) -> None:
        result = self.runner([*self.argv, *args], 180.0)
        if result.code != 0:
            raise GamedayError(
                f"gameday {' '.join(args)} failed (exit {result.code}): {result.stderr.strip()}",
                result.stdout,
            )
