"""mamori-evals: run, replay and report the agent evaluation (EVALS.md)."""

from __future__ import annotations

import argparse
import signal
import subprocess
import sys
from collections.abc import Callable, Sequence
from datetime import UTC, datetime, timedelta
from pathlib import Path
from types import FrameType

from . import GENERATOR
from .baseline import Baseline, baseline_from_postmortems, load_baseline
from .clients import (
    AgentApi,
    CommandToken,
    ExecutorApi,
    ExecutorLogs,
    GamedayCli,
    JsonHttp,
    PrometheusApi,
    managed_prometheus_url,
    split_command,
)
from .faults import FaultError, describe_faults, load_faults, select_faults
from .jsonutil import DocumentError, read_json, write_json
from .live import MANUAL_MODES, LiveConfig, LiveRunner
from .portforward import Forward, PortForwards
from .records import Recording, RunObservation, parse_recording
from .replay import merge_recordings, results_for
from .report import render_markdown
from .steady import SteadyStateCheck, load_thresholds
from .timeutil import SystemClock, format_seconds, format_time, parse_duration

EXIT_ERROR = 1
EXIT_INTERRUPTED = 130
AGENT_FORWARD = Forward("mamori-agent", "agent", 18080)
EXECUTOR_FORWARD = Forward("mamori-agent", "executor", 18081)


def _log(message: str) -> None:
    print(f"{datetime.now(UTC):%H:%M:%S} {message}", file=sys.stderr, flush=True)


def _duration(text: str) -> timedelta:
    try:
        return parse_duration(text)
    except ValueError as err:
        raise argparse.ArgumentTypeError(str(err)) from err


def _label(text: str) -> tuple[str, str]:
    key, sep, value = text.partition("=")
    if not sep or not key.strip():
        raise argparse.ArgumentTypeError(f"labels look like KEY=VALUE, got {text!r}")
    return key.strip(), value.strip()


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="mamori-evals", description=__doc__)
    parser.add_argument("--version", action="version", version=GENERATOR)
    sub = parser.add_subparsers(dest="command", required=True)

    faults = sub.add_parser("faults", help="list and validate the fault library")
    faults.add_argument("--faults-dir", type=Path, default=Path("chaos/faults"))

    def scoring_options(cmd: argparse.ArgumentParser) -> None:
        cmd.add_argument("--faults-dir", type=Path, default=Path("chaos/faults"))
        cmd.add_argument(
            "--postmortems",
            type=Path,
            default=Path("docs/incidents/postmortems"),
            help="split accuracy by whether a postmortem of the fault existed (skipped if absent)",
        )
        cmd.add_argument(
            "--baseline", type=Path, action="append", default=[], help="a kind: EvalBaseline file"
        )
        cmd.add_argument(
            "--human-baseline",
            action="store_true",
            help="add the human baseline from the postmortems' timelines",
        )
        cmd.add_argument("--out-dir", type=Path, default=Path("evals/results"))

    replay = sub.add_parser("replay", help="score recorded sessions offline")
    replay.add_argument(
        "--recording",
        type=Path,
        action="append",
        required=True,
        help="a session's .recording.json; repeat to score several sessions as one dataset",
    )
    replay.add_argument("--name", help="output file name (default: from the recording)")
    scoring_options(replay)

    report = sub.add_parser("report", help="render a results JSON file as Markdown")
    report.add_argument("results", type=Path)
    report.add_argument("--out", type=Path, help="write here instead of stdout")

    run = sub.add_parser("run", help="live session against the lab (injects real faults)")
    run.add_argument("--faults", required=True, help="ids (comma-separated), 'game-day' or 'all'")
    scoring_options(run)
    prom = run.add_mutually_exclusive_group()
    prom.add_argument("--project-id", help="read the prober SLIs from Managed Prometheus")
    prom.add_argument("--prometheus-url", help="any Prometheus-compatible query API instead")
    run.add_argument(
        "--token-command",
        help="prints a bearer token for the query API "
        "(default with --project-id: gcloud auth print-access-token)",
    )
    run.add_argument("--agent-url", default=AGENT_FORWARD.url)
    run.add_argument("--executor-url", default=EXECUTOR_FORWARD.url)
    run.add_argument(
        "--port-forward", action="store_true", help="kubectl port-forward both Services"
    )
    run.add_argument("--kubectl", default="kubectl")
    run.add_argument("--gameday", default="bin/gameday", help="the gameday CLI (#23)")
    run.add_argument(
        "--executor-logs",
        default="kubectl -n mamori-agent logs deploy/executor -c executor",
        help="command that prints the executor's audit log (gets --since-time)",
    )
    run.add_argument("--slos", type=Path, default=Path("slo/slos.yaml"))
    run.add_argument("--no-latency-check", action="store_true")
    run.add_argument("--steady-window", default="5m")
    run.add_argument("--steady-min-success", type=float, default=0.99)
    defaults = LiveConfig()
    for flag, value in [
        ("--investigation-timeout", defaults.investigation_timeout),
        ("--poll-interval", defaults.poll_interval),
        ("--steady-timeout", defaults.steady_timeout),
        ("--cooldown", defaults.cooldown_min),
        ("--cooldown-timeout", defaults.cooldown_timeout),
        ("--proposal-wait", defaults.proposal_wait),
        ("--clock-skew", defaults.clock_skew),
        ("--budget", defaults.budget),
    ]:
        run.add_argument(flag, type=_duration, default=value, metavar="DURATION")
    run.add_argument("--manual-faults", choices=MANUAL_MODES, default="skip")
    run.add_argument(
        "--label",
        type=_label,
        action="append",
        default=[],
        metavar="KEY=VALUE",
        help="record a condition, e.g. AGENT_POSTMORTEMS_GAMEDAYS=exclude",
    )
    run.add_argument("--dry-run", action="store_true", help="print the plan; touch nothing")
    return parser


def _baselines(args: argparse.Namespace) -> list[Baseline]:
    baselines = [load_baseline(path) for path in args.baseline]
    if args.human_baseline:
        if not args.postmortems.is_dir():
            raise DocumentError(f"--human-baseline: {args.postmortems} does not exist")
        baselines.insert(0, baseline_from_postmortems(args.postmortems))
    return baselines


def _write_outputs(out_dir: Path, name: str, results: dict[str, object]) -> str:
    markdown = render_markdown(results)
    write_json(out_dir / f"{name}.results.json", results)
    (out_dir / f"{name}.md").write_text(markdown, encoding="utf-8")
    _log(f"wrote {out_dir / name}.results.json and {name}.md")
    return markdown


def cmd_faults(args: argparse.Namespace) -> int:
    faults = load_faults(args.faults_dir)
    for line in describe_faults(faults.values()):
        print(line)
    game_days = sum(fault.game_day for fault in faults.values())
    print(f"{len(faults)} faults, {game_days} used in game days: valid for scoring")
    return 0


def cmd_replay(args: argparse.Namespace) -> int:
    faults = load_faults(args.faults_dir)
    paths: list[Path] = args.recording
    recording = merge_recordings([parse_recording(read_json(path)) for path in paths])
    results = results_for(
        recording,
        faults,
        args.postmortems,
        _baselines(args),
        mode="replay",
        generated_at=datetime.now(UTC),
        extra_metadata={
            "recordings": [str(path) for path in paths],
            "faultsDir": str(args.faults_dir),
            "scoredAtCommit": _git_commit(),
        },
    )
    single = paths[0].name.removesuffix(".json").removesuffix(".recording")
    name = args.name or (single if len(paths) == 1 else f"combined-{len(paths)}-sessions")
    print(_write_outputs(args.out_dir, name, results), end="")
    return 0


def cmd_report(args: argparse.Namespace) -> int:
    markdown = render_markdown(read_json(args.results))
    if args.out:
        args.out.write_text(markdown, encoding="utf-8")
    else:
        print(markdown, end="")
    return 0


def _git_commit() -> str | None:
    try:
        done = subprocess.run(
            ["git", "rev-parse", "--short=12", "HEAD"],  # noqa: S607 - git from PATH, read-only
            capture_output=True,
            text=True,
            timeout=10,
            check=False,
        )
    except (OSError, subprocess.SubprocessError):
        return None
    return done.stdout.strip() or None


def _raise_interrupt(signum: int, frame: FrameType | None) -> None:
    raise KeyboardInterrupt


def cmd_run(args: argparse.Namespace) -> int:
    faults = load_faults(args.faults_dir)
    selected = select_faults(faults, args.faults)
    config = LiveConfig(
        investigation_timeout=args.investigation_timeout,
        poll_interval=args.poll_interval,
        steady_timeout=args.steady_timeout,
        cooldown_min=args.cooldown,
        cooldown_timeout=args.cooldown_timeout,
        proposal_wait=args.proposal_wait,
        clock_skew=args.clock_skew,
        budget=args.budget,
        manual_faults=args.manual_faults,
    )
    worst = config.max_run_time * len(selected)
    _log(
        f"{len(selected)} faults: {', '.join(fault.id for fault in selected)}. Budget "
        f"{format_seconds(config.budget.total_seconds())}; worst case "
        f"{format_seconds(worst.total_seconds())} "
        f"({format_seconds(config.max_run_time.total_seconds())} each); "
        f"each fault held at most {format_seconds(config.fault_duration.total_seconds())}."
    )
    if args.dry_run:
        _log("dry run: nothing injected")
        return 0
    if not (args.project_id or args.prometheus_url):
        raise DocumentError("give --project-id (Managed Prometheus) or --prometheus-url")
    if args.manual_faults == "prompt" and not sys.stdin.isatty():
        raise DocumentError("--manual-faults prompt needs a terminal")
    # Read the baselines now: a bad file should fail before any fault, not after hours.
    baselines = _baselines(args)

    token_command = args.token_command or (
        "gcloud auth print-access-token" if args.project_id else None
    )
    token = CommandToken(split_command(token_command)) if token_command else None
    prom_url = args.prometheus_url or managed_prometheus_url(args.project_id)
    steady = SteadyStateCheck(
        PrometheusApi(JsonHttp(prom_url, token=token)),
        window=args.steady_window,
        min_success=args.steady_min_success,
        thresholds_ms=load_thresholds(args.slos),
        check_latency=not args.no_latency_check,
    )
    session = datetime.now(UTC)
    session_id = session.strftime("%Y%m%dT%H%M%SZ")
    metadata: dict[str, object] = {
        "session": session_id,
        "startedAt": format_time(session),
        "synthetic": False,
        "faults": [fault.id for fault in selected],
        "labels": dict(args.label),
        "gitCommit": _git_commit(),
        "config": {
            "investigationTimeout": args.investigation_timeout.total_seconds(),
            "cooldown": args.cooldown.total_seconds(),
            "proposalWait": args.proposal_wait.total_seconds(),
            "clockSkew": args.clock_skew.total_seconds(),
            "steadyWindow": args.steady_window,
            "steadyMinSuccess": args.steady_min_success,
            "latencyCheck": not args.no_latency_check,
        },
    }
    recording_path = args.out_dir / f"{session_id}.recording.json"

    def save(runs: list[RunObservation]) -> None:
        write_json(recording_path, Recording(metadata, list(runs)).to_dict())

    forwards = PortForwards(
        [AGENT_FORWARD, EXECUTOR_FORWARD] if args.port_forward else [],
        kubectl=split_command(args.kubectl),
    )
    previous_term = signal.signal(signal.SIGTERM, _raise_interrupt)
    ask: Callable[[str], str] | None = input if sys.stdin.isatty() else None
    try:
        with forwards:
            runner = LiveRunner(
                selected,
                config,
                SystemClock(),
                GamedayCli(split_command(args.gameday)),
                AgentApi(JsonHttp(args.agent_url)),
                ExecutorApi(JsonHttp(args.executor_url)),
                ExecutorLogs(split_command(args.executor_logs)),
                steady,
                log=_log,
                on_update=save,
                ask=ask,
                keepalive=forwards.ensure if args.port_forward else None,
            )
            runs = runner.run()
    finally:
        signal.signal(signal.SIGTERM, previous_term)
    metadata["finishedAt"] = format_time(datetime.now(UTC))
    recording = Recording(metadata, runs)
    write_json(recording_path, recording.to_dict())
    _log(f"wrote {recording_path}")
    results = results_for(
        recording,
        faults,
        args.postmortems,
        baselines,
        mode="live",
        generated_at=datetime.now(UTC),
        extra_metadata={"recording": str(recording_path), "faultsDir": str(args.faults_dir)},
    )
    print(_write_outputs(args.out_dir, session_id, results), end="")
    if runner.abort_reason:
        _log(f"session ended early: {runner.abort_reason}")
    return EXIT_INTERRUPTED if runner.interrupted else 0


COMMANDS: dict[str, Callable[[argparse.Namespace], int]] = {
    "faults": cmd_faults,
    "replay": cmd_replay,
    "report": cmd_report,
    "run": cmd_run,
}


def main(argv: Sequence[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    try:
        return COMMANDS[args.command](args)
    except (FaultError, DocumentError, ValueError, OSError) as err:
        print(f"error: {err}", file=sys.stderr)
        return EXIT_ERROR
    except KeyboardInterrupt:
        print("interrupted", file=sys.stderr)
        return EXIT_INTERRUPTED


if __name__ == "__main__":  # pragma: no cover
    sys.exit(main())
