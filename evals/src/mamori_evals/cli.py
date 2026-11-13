"""mamori-evals: run, replay and report the agent evaluation (EVALS.md)."""

from __future__ import annotations

import argparse
import contextlib
import signal
import subprocess
import sys
from collections.abc import Callable, Sequence
from datetime import UTC, datetime, timedelta
from pathlib import Path
from types import FrameType

from . import GENERATOR
from .baseline import (
    Baseline,
    PostmortemIndex,
    baseline_from_postmortems,
    load_baseline,
    postmortem_dates,
    postmortems_on_ref,
)
from .clients import (
    AgentApi,
    AgentLogs,
    ApiError,
    CommandToken,
    ExecutorApi,
    ExecutorLogs,
    GamedayCli,
    JsonHttp,
    PrometheusApi,
    deployment_env,
    managed_prometheus_url,
    split_command,
)
from .conditions import session_conditions
from .faults import FaultError, describe_faults, load_faults, select_faults
from .jsonutil import DocumentError, read_json, write_json
from .live import MANUAL_MODES, LiveConfig, LiveRunner
from .portforward import Forward, PortForwards
from .records import Recording, RunObservation, parse_recording
from .replay import DEFAULT_CLEAN_BURN, merge_recordings, results_for
from .report import render_markdown
from .steady import DEFAULT_MAX_BURN_1H, SteadyStateCheck, load_slo_spec
from .timeutil import SystemClock, format_seconds, format_time, parse_duration

EXIT_ERROR = 1
EXIT_INTERRUPTED = 130
# Local ports for the port-forwards; 18080 is the prober's (make prober-run), 8080 the agent's
# own (make agent-port-forward).
AGENT_LOCAL_PORT = 18181
EXECUTOR_LOCAL_PORT = 18182
POSTMORTEMS_PATH = "docs/incidents/postmortems"


def _log(message: str) -> None:
    # A closed terminal must not break the cleanup that follows a SIGHUP.
    with contextlib.suppress(OSError, ValueError):
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
            default=Path(POSTMORTEMS_PATH),
            help="game-day postmortems, for the human baseline and the 'seen before?' split",
        )
        cmd.add_argument(
            "--postmortems-ref",
            default="origin/main",
            help="git ref the agent reads postmortems from; '' uses the files' front-matter dates",
        )
        cmd.add_argument(
            "--baseline", type=Path, action="append", default=[], help="a kind: EvalBaseline file"
        )
        cmd.add_argument(
            "--human-baseline",
            action="store_true",
            help="add the human baseline from the postmortems' timelines",
        )
        cmd.add_argument(
            "--clean-burn-1h",
            type=float,
            default=DEFAULT_CLEAN_BURN,
            help="a run is in a clean window when every journey's 1h burn rate was below this",
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
    run.add_argument("--agent-local-port", type=int, default=AGENT_LOCAL_PORT)
    run.add_argument("--executor-local-port", type=int, default=EXECUTOR_LOCAL_PORT)
    run.add_argument("--agent-url", help="default: the agent's local port")
    run.add_argument("--executor-url", help="default: the executor's local port")
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
    run.add_argument(
        "--agent-logs",
        default="kubectl -n mamori-agent logs deploy/agent -c agent",
        help="command that prints the agent's log (gets --since-time)",
    )
    run.add_argument(
        "--agent-deployment",
        default="kubectl -n mamori-agent get deployment agent -o json",
        help="command that prints the agent Deployment, to check the run conditions",
    )
    run.add_argument(
        "--no-verify-conditions",
        action="store_true",
        help="don't check --label values against the agent (development only)",
    )
    run.add_argument("--slos", type=Path, default=Path("slo/slos.yaml"))
    run.add_argument("--no-latency-check", action="store_true")
    run.add_argument("--steady-window", default="5m")
    run.add_argument("--steady-min-success", type=float, default=0.99)
    run.add_argument(
        "--max-burn-1h",
        type=float,
        default=DEFAULT_MAX_BURN_1H,
        help="inject only while every journey's 1h burn rate is below this (page at 13.44)",
    )
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
        ("--reread-wait", defaults.reread_wait),
    ]:
        run.add_argument(flag, type=_duration, default=value, metavar="DURATION")
    run.add_argument(
        "--min-budget-investigations", type=int, default=defaults.min_budget_investigations
    )
    run.add_argument("--manual-faults", choices=MANUAL_MODES, default="skip")
    run.add_argument(
        "--label",
        type=_label,
        action="append",
        default=[],
        metavar="KEY=VALUE",
        help="a condition, e.g. APPROVALS=none; AGENT_* ones are checked against the agent",
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


def _postmortem_index(args: argparse.Namespace) -> tuple[PostmortemIndex | None, str]:
    """When each game-day postmortem reached the branch the agent reads; front-matter dates
    when git can't say; nothing when there are no postmortems."""
    if args.postmortems_ref:
        try:
            return (
                postmortems_on_ref(Path("."), args.postmortems_ref, POSTMORTEMS_PATH),
                f"git {args.postmortems_ref}",
            )
        except (DocumentError, OSError, subprocess.SubprocessError) as err:
            _log(f"postmortems on {args.postmortems_ref} not read ({err}); using their dates")
    if args.postmortems.is_dir():
        return postmortem_dates(args.postmortems), "front-matter dates (approximate)"
    return None, "none"


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
    index, index_source = _postmortem_index(args)
    results = results_for(
        recording,
        faults,
        index,
        _baselines(args),
        mode="replay",
        generated_at=datetime.now(UTC),
        extra_metadata={
            "recordings": [str(path) for path in paths],
            "faultsDir": str(args.faults_dir),
            "scoredAtCommit": _git_commit(),
            "postmortemIndex": index_source,
        },
        clean_burn=args.clean_burn_1h,
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
        min_budget_investigations=args.min_budget_investigations,
        reread_wait=args.reread_wait,
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
        check_latency=not args.no_latency_check,
        spec=load_slo_spec(args.slos),
        max_burn_1h=args.max_burn_1h,
    )
    agent_forward = Forward("mamori-agent", "agent", args.agent_local_port)
    executor_forward = Forward("mamori-agent", "executor", args.executor_local_port)
    agent = AgentApi(JsonHttp(args.agent_url or agent_forward.url))
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
            "maxBurn1h": args.max_burn_1h,
        },
    }
    recording_path = args.out_dir / f"{session_id}.recording.json"

    def save(runs: list[RunObservation]) -> None:
        write_json(recording_path, Recording(metadata, list(runs)).to_dict())

    forwards = PortForwards(
        [agent_forward, executor_forward] if args.port_forward else [],
        kubectl=split_command(args.kubectl),
    )
    previous = {
        sig: signal.signal(sig, _raise_interrupt) for sig in (signal.SIGTERM, signal.SIGHUP)
    }
    ask: Callable[[str], str] | None = input if sys.stdin.isatty() else None
    try:
        with forwards:
            if not args.no_verify_conditions:
                status = agent.status()
                metadata["labels"] = session_conditions(
                    dict(args.label),
                    deployment_env(split_command(args.agent_deployment), "agent"),
                    status.agent_config if status else None,
                )
                _log(f"conditions: {metadata['labels']}")
            runner = LiveRunner(
                selected,
                config,
                SystemClock(),
                GamedayCli(split_command(args.gameday)),
                agent,
                ExecutorApi(JsonHttp(args.executor_url or executor_forward.url)),
                ExecutorLogs(split_command(args.executor_logs)),
                steady,
                log=_log,
                on_update=save,
                ask=ask,
                keepalive=forwards.ensure if args.port_forward else None,
                agent_logs=AgentLogs(split_command(args.agent_logs)),
            )
            runs = runner.run()
    finally:
        for sig, handler in previous.items():
            signal.signal(sig, handler)
    metadata["finishedAt"] = format_time(datetime.now(UTC))
    recording = Recording(metadata, runs)
    write_json(recording_path, recording.to_dict())
    _log(f"wrote {recording_path}")
    index, index_source = _postmortem_index(args)
    results = results_for(
        recording,
        faults,
        index,
        baselines,
        mode="live",
        generated_at=datetime.now(UTC),
        extra_metadata={
            "recording": str(recording_path),
            "faultsDir": str(args.faults_dir),
            "postmortemIndex": index_source,
        },
        clean_burn=args.clean_burn_1h,
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
    except (FaultError, DocumentError, ApiError, ValueError, OSError) as err:
        print(f"error: {err}", file=sys.stderr)
        return EXIT_ERROR
    except KeyboardInterrupt:
        print("interrupted", file=sys.stderr)
        return EXIT_INTERRUPTED


if __name__ == "__main__":  # pragma: no cover
    sys.exit(main())
