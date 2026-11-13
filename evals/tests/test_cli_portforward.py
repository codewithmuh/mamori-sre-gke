from __future__ import annotations

import json
import socket
import subprocess
import sys
from pathlib import Path
from typing import Any

import pytest

from mamori_evals import cli
from mamori_evals.portforward import Forward, PortForwards, port_open

from .conftest import FIXTURES

FAULTS = str(FIXTURES / "faults")
RECORDING = str(FIXTURES / "recordings" / "synthetic-session.json")


class FakeProcess:
    def __init__(self, exit_code: int | None = None, hang: bool = False) -> None:
        self.code = exit_code
        self.hang = hang
        self.terminated = self.killed = False

    def poll(self) -> int | None:
        return self.code

    def terminate(self) -> None:
        self.terminated = True
        if not self.hang:
            self.code = -15

    def kill(self) -> None:
        self.killed = True
        self.code = -9

    def wait(self, timeout: float | None = None) -> int:
        if self.code is None:
            raise subprocess.TimeoutExpired("kubectl", timeout or 0)
        return self.code


class TestPortForward:
    def test_starts_waits_restarts_and_closes(self) -> None:
        started: list[list[str]] = []
        processes: list[FakeProcess] = []
        ready_calls = {"n": 0}

        def popen(argv: list[str], **kwargs: Any) -> FakeProcess:
            started.append(argv)
            processes.append(FakeProcess())
            return processes[-1]

        def ready(port: int) -> bool:
            ready_calls["n"] += 1
            return ready_calls["n"] % 2 == 0  # every second check succeeds

        forward = Forward("mamori-agent", "agent", 18080)
        with PortForwards([forward], popen=popen, ready=ready, sleep=lambda s: None) as pf:
            assert started == [
                ["kubectl", "-n", "mamori-agent", "port-forward", "svc/agent", "18080:8080"]
            ]
            processes[0].code = 1  # the forward died
            pf.ensure()
            assert len(started) == 2
            pf.ensure()
            assert len(started) == 2
        assert processes[1].terminated
        assert forward.url == "http://127.0.0.1:18080"

    def test_kill_when_terminate_hangs(self) -> None:
        process = FakeProcess(hang=True)
        pf = PortForwards(
            [Forward("ns", "svc", 1)], popen=lambda argv, **kw: process, ready=lambda p: True
        )
        pf.ensure()
        pf.close()
        assert process.killed

    def test_failures(self) -> None:
        dead = PortForwards(
            [Forward("ns", "svc", 1)],
            popen=lambda argv, **kw: FakeProcess(exit_code=1),
            ready=lambda p: False,
        )
        with pytest.raises(RuntimeError, match="exited"), dead:
            pass
        slow = PortForwards(
            [Forward("ns", "svc", 1)],
            popen=lambda argv, **kw: FakeProcess(),
            ready=lambda p: False,
            ready_timeout=0.0,
            sleep=lambda s: None,
        )
        with pytest.raises(RuntimeError, match="not ready"):
            slow.ensure()

    def test_port_open(self) -> None:
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            listener.listen()
            assert port_open(listener.getsockname()[1])
        assert not port_open(9)


class TestCli:
    def test_faults(self, capsys: pytest.CaptureFixture[str]) -> None:
        assert cli.main(["faults", "--faults-dir", FAULTS]) == 0
        out = capsys.readouterr().out
        assert "8 faults, 5 used in game days: valid for scoring" in out

    def test_faults_invalid(self, tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
        assert cli.main(["faults", "--faults-dir", str(tmp_path)]) == cli.EXIT_ERROR
        assert "no *.yaml fault files" in capsys.readouterr().err

    def test_replay_and_report(self, tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
        args = [
            "replay",
            "--recording",
            RECORDING,
            "--faults-dir",
            FAULTS,
            "--postmortems",
            str(FIXTURES / "postmortems"),
            "--human-baseline",
            "--baseline",
            str(FIXTURES / "baselines" / "synthetic-other-tool.yaml"),
            "--out-dir",
            str(tmp_path),
            "--postmortems-ref",
            "",
        ]
        assert cli.main(args) == 0
        out = capsys.readouterr().out
        assert "Synthetic replay" in out
        results = tmp_path / "synthetic-session.results.json"
        assert json.loads(results.read_text())["summary"]["top1"]["k"] == 4
        assert (tmp_path / "synthetic-session.md").read_text() == out
        assert cli.main(["report", str(results)]) == 0
        assert capsys.readouterr().out == out
        target = tmp_path / "again.md"
        assert cli.main(["report", str(results), "--out", str(target)]) == 0
        assert target.read_text() == out

    def test_replay_several_sessions(
        self, tmp_path: Path, capsys: pytest.CaptureFixture[str]
    ) -> None:
        args = [
            "replay",
            "--recording",
            RECORDING,
            "--recording",
            RECORDING,
            "--faults-dir",
            FAULTS,
        ]
        more = ["--out-dir", str(tmp_path), "--postmortems", str(tmp_path), "--postmortems-ref", ""]
        assert cli.main([*args, *more]) == 0
        results = json.loads((tmp_path / "combined-2-sessions.results.json").read_text())
        assert results["summary"]["runs"]["total"] == 22
        assert results["metadata"]["recordings"] == [RECORDING, RECORDING]
        capsys.readouterr()

    def test_human_baseline_needs_postmortems(
        self, tmp_path: Path, capsys: pytest.CaptureFixture[str]
    ) -> None:
        args = [
            "replay",
            "--recording",
            RECORDING,
            "--faults-dir",
            FAULTS,
            "--human-baseline",
            "--postmortems",
            str(tmp_path / "none"),
            "--out-dir",
            str(tmp_path),
        ]
        assert cli.main(args) == cli.EXIT_ERROR
        assert "--human-baseline" in capsys.readouterr().err

    def test_run_dry_run(self, capsys: pytest.CaptureFixture[str]) -> None:
        args = [
            "run",
            "--faults",
            "game-day",
            "--faults-dir",
            FAULTS,
            "--dry-run",
            "--budget",
            "6h",
        ]
        assert cli.main(args) == 0
        err = capsys.readouterr().err
        assert "5 faults" in err
        assert "dry run: nothing injected" in err
        assert "each fault held at most 22m00s" in err

    def test_run_needs_a_metrics_source(self, capsys: pytest.CaptureFixture[str]) -> None:
        args = ["run", "--faults", "F91-synthetic-currency-abort", "--faults-dir", FAULTS]
        assert cli.main(args) == cli.EXIT_ERROR
        assert "--project-id" in capsys.readouterr().err

    def test_bad_arguments(self) -> None:
        with pytest.raises(SystemExit):
            cli.main(["run", "--faults", "x", "--budget", "forever"])
        with pytest.raises(SystemExit):
            cli.main(["run", "--faults", "x", "--label", "novalue"])

    def test_module_entry_point(self) -> None:
        done = subprocess.run(
            [sys.executable, "-m", "mamori_evals", "--version"],
            capture_output=True,
            text=True,
            check=False,
        )
        assert done.returncode == 0
        assert "mamori-evals/" in done.stdout


def test_run_live_with_fakes(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    """cmd_run wiring, with the runner replaced: files land where they should."""
    from mamori_evals.records import RunObservation

    class StubRunner:
        def __init__(self, faults: list[Any], *args: Any, **kwargs: Any) -> None:
            self.faults, self.on_update = faults, kwargs["on_update"]
            self.abort_reason, self.interrupted = "interrupted", True

        def run(self) -> list[RunObservation]:
            runs = [RunObservation(f.id, "skipped", reason="interrupted") for f in self.faults]
            self.on_update(runs)
            return runs

    monkeypatch.setattr(cli, "LiveRunner", StubRunner)
    args = [
        "run",
        "--faults",
        "F91-synthetic-currency-abort",
        "--faults-dir",
        FAULTS,
        "--prometheus-url",
        "http://127.0.0.1:9090",
        "--out-dir",
        str(tmp_path),
        "--label",
        "AGENT_POSTMORTEMS_GAMEDAYS=exclude",
        "--postmortems",
        str(tmp_path / "none"),
        "--postmortems-ref",
        "",
        "--no-verify-conditions",
    ]
    assert cli.main(args) == cli.EXIT_INTERRUPTED
    recordings = list(tmp_path.glob("*.recording.json"))
    assert len(recordings) == 1
    recording = json.loads(recordings[0].read_text())
    assert recording["metadata"]["labels"] == {"AGENT_POSTMORTEMS_GAMEDAYS": "exclude"}
    assert recording["metadata"]["synthetic"] is False
    assert "finishedAt" in recording["metadata"]
    assert len(list(tmp_path.glob("*.results.json"))) == 1
    assert "session ended early: interrupted" in capsys.readouterr().err


def test_run_checks_the_conditions(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    from mamori_evals.records import RunObservation

    class StubRunner:
        def __init__(self, faults: list[Any], *args: Any, **kwargs: Any) -> None:
            self.faults = faults
            self.abort_reason, self.interrupted = None, False

        def run(self) -> list[RunObservation]:
            return [RunObservation(f.id, "skipped", reason="test") for f in self.faults]

    env = {"AGENT_POSTMORTEMS_GAMEDAYS": "exclude", "AGENT_DAILY_COST_LIMIT_USD": "40"}
    monkeypatch.setattr(cli, "LiveRunner", StubRunner)
    monkeypatch.setattr(cli, "deployment_env", lambda argv, container: env)
    monkeypatch.setattr(cli.AgentApi, "status", lambda self: None)
    base = [
        "run",
        "--faults",
        "F91-synthetic-currency-abort",
        "--faults-dir",
        FAULTS,
        "--prometheus-url",
        "http://127.0.0.1:9090",
        "--out-dir",
        str(tmp_path),
        "--postmortems",
        str(tmp_path / "none"),
        "--postmortems-ref",
        "",
    ]
    assert cli.main([*base, "--label", "AGENT_POSTMORTEMS_GAMEDAYS=exclude"]) == 0
    (recording,) = tmp_path.glob("*.recording.json")
    assert json.loads(recording.read_text())["metadata"]["labels"] == env
    capsys.readouterr()
    assert cli.main([*base, "--label", "AGENT_POSTMORTEMS_GAMEDAYS=include"]) == cli.EXIT_ERROR
    assert "you typed include" in capsys.readouterr().err


def test_replay_falls_back_to_postmortem_dates(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    args = [
        "replay",
        "--recording",
        RECORDING,
        "--faults-dir",
        FAULTS,
        "--postmortems",
        str(FIXTURES / "postmortems"),
        "--postmortems-ref",
        "no-such-ref-for-evals-tests",
        "--out-dir",
        str(tmp_path),
    ]
    assert cli.main(args) == 0
    results = json.loads((tmp_path / "synthetic-session.results.json").read_text())
    assert results["metadata"]["postmortemIndex"] == "front-matter dates (approximate)"
    assert "using their dates" in capsys.readouterr().err
