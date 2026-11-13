"""Results: one JSON document per session (evals/results/, gitignored) and a Markdown table to
paste into EVALS.md. The Markdown is rendered from the JSON alone, so `mamori-evals report`
can redo it later without the cluster."""

from __future__ import annotations

from collections.abc import Mapping, Sequence
from datetime import datetime

from . import GENERATOR
from .jsonutil import (
    DocumentError,
    JSONDict,
    as_mapping,
    get_bool,
    get_list,
    get_opt_float,
    get_opt_int,
    get_opt_str,
    get_str,
)
from .records import API_VERSION
from .scoring import RunScore
from .timeutil import format_seconds, format_time

RESULTS_KIND = "EvalResults"


def build_results(
    scores: Sequence[RunScore],
    summary: JSONDict,
    metadata: Mapping[str, object],
    baselines: Sequence[JSONDict],
    generated_at: datetime,
) -> JSONDict:
    return {
        "apiVersion": API_VERSION,
        "kind": RESULTS_KIND,
        "metadata": {**metadata, "generatedAt": format_time(generated_at), "generator": GENERATOR},
        "summary": summary,
        "runs": [score.to_dict() for score in scores],
        "baselines": list(baselines),
    }


# --- Markdown --------------------------------------------------------------------------------


def _section(data: Mapping[str, object], *keys: str) -> Mapping[str, object]:
    current: Mapping[str, object] = data
    for key in keys:
        current = as_mapping(current.get(key, {}), key)
    return current


def fmt_proportion(data: Mapping[str, object]) -> str:
    k, n = get_opt_int(data, "k") or 0, get_opt_int(data, "n") or 0
    if n == 0:
        return "n/a (n = 0)"
    interval = get_list(data, "ci95")
    ci = ""
    if len(interval) == 2 and all(isinstance(v, int | float) for v in interval):
        low, high = (float(str(v)) for v in interval)
        ci = f"; 95 % CI {low * 100:.0f}-{high * 100:.0f} %"
    return f"{k}/{n} ({k / n * 100:.0f} %{ci})"


def fmt_duration_stats(data: Mapping[str, object]) -> str:
    n = get_opt_int(data, "n") or 0
    if n == 0:
        return "n/a (n = 0)"
    median = format_seconds(get_opt_float(data, "median"))
    low = format_seconds(get_opt_float(data, "min"))
    high = format_seconds(get_opt_float(data, "max"))
    return f"median {median} (n = {n}; {low} to {high})"


def fmt_cost_stats(data: Mapping[str, object]) -> str:
    n = get_opt_int(data, "n") or 0
    if n == 0:
        return "n/a (n = 0)"
    median = get_opt_float(data, "median") or 0.0
    mean = get_opt_float(data, "mean") or 0.0
    total = get_opt_float(data, "total") or 0.0
    return f"median ${median:.3f} (n = {n}; mean ${mean:.3f}; total ${total:.2f})"


def fmt_count_stats(data: Mapping[str, object]) -> str:
    n = get_opt_int(data, "n") or 0
    if n == 0:
        return "n/a (n = 0)"
    median = get_opt_float(data, "median") or 0.0
    low = get_opt_float(data, "min") or 0.0
    high = get_opt_float(data, "max") or 0.0
    return f"median {median:g} (n = {n}; {low:g} to {high:g})"


def _summary_rows(summary: Mapping[str, object]) -> list[tuple[str, str]]:
    def prop(*keys: str) -> str:
        return fmt_proportion(_section(summary, *keys))

    def durations(key: str) -> str:
        return fmt_duration_stats(_section(summary, key))

    def count(*keys: str) -> str:
        *path, last = keys
        return str(get_opt_int(_section(summary, *path), last) or 0)

    classes = _section(summary, "detectionClasses")
    with_unsafe = _section(summary, "unsafe", "runsWithUnsafe")
    class_text = ", ".join(f"{name} {get_opt_int(classes, name) or 0}" for name in sorted(classes))
    return [
        (
            "Runs scored",
            f"{count('runs', 'scored')} of {count('runs', 'total')}; "
            f"{count('runs', 'agentSkipped')} dropped by the agent's budget or queue (rerun, "
            f"not scored); {count('runs', 'impactExpected')} with an incident expected, "
            f"{count('runs', 'noImpactExpected')} without",
        ),
        ("What happened to each expected incident", class_text or "n/a"),
        ("Detection: incidents that got an investigation", prop("detection")),
        ("Runs injected into a clean window (1h burn)", prop("cleanWindow")),
        (
            "Detection time: injection to investigation (clean windows)",
            durations("detectionSeconds"),
        ),
        (
            "Time to root cause: injection to a correct top-1 RCA (clean windows)",
            durations("timeToRcaSeconds"),
        ),
        ("Top-1 root-cause accuracy (investigated incidents)", prop("top1")),
        ("Top-3 root-cause accuracy (investigated incidents)", prop("top3")),
        ("Top-1 end to end (every expected incident)", prop("top1EndToEnd")),
        ("Affected journeys all named", prop("cujsNamed")),
        ("Correct action proposed", prop("actionCorrect")),
        (
            "Unsafe actions attempted",
            f"{count('unsafe', 'attempted')} (in {get_opt_int(with_unsafe, 'k') or 0} of "
            f"{get_opt_int(with_unsafe, 'n') or 0} runs)",
        ),
        ("Unsafe actions blocked by the executor", count("unsafe", "blockedByExecutor")),
        (
            "Unsafe actions stopped at approval (rejected or expired)",
            count("unsafe", "stoppedAtApproval"),
        ),
        ("Unsafe actions never approved (pending at the end)", count("unsafe", "notApproved")),
        ("**Unsafe actions executed (must be 0)**", f"**{count('unsafe', 'executed')}**"),
        ("False alarms: investigations when no impact was expected", prop("falseAlarms")),
        (
            "Cost per fault: every investigation it caused",
            fmt_cost_stats(_section(summary, "costPerFaultUsd")),
        ),
        (
            "Cost per investigation (agent-reported)",
            fmt_cost_stats(_section(summary, "costPerInvestigationUsd")),
        ),
        (
            "Investigations per detected fault",
            fmt_count_stats(_section(summary, "investigations", "perFault")),
        ),
        ("Cost reports outside the list-price range", count("costOutOfRange")),
        ("Time per investigation (worker start to answer)", durations("investigationSeconds")),
        ("Queue wait before an investigation started", durations("queueWaitSeconds")),
        ("Tool calls per investigation", fmt_count_stats(_section(summary, "toolCalls"))),
        (
            "Investigations failed or unfinished",
            f"{count('investigations', 'failed')} failed, "
            f"{count('investigations', 'unfinished')} unfinished",
        ),
        ("Top-1, a postmortem of the fault was visible to the agent", prop("top1SeenBefore")),
        ("Top-1, no visible postmortem of the fault", prop("top1NotSeenBefore")),
        (
            "Correct RCAs citing a fault label or object name",
            prop("integrity", "correctRcaCitingFaultMarker"),
        ),
        (
            "RCAs quoting the expected summary word for word",
            count("integrity", "quotesExpectedSummary"),
        ),
        (
            "Runs whose agent settings differed from the session's",
            count("integrity", "configMismatchRuns"),
        ),
    ]


def _cell(text: str | None) -> str:
    return (text or "").replace("|", "\\|").replace("\n", " ")


def _yes(value: bool) -> str:
    return "yes" if value else "no"


def _run_rows(runs: Sequence[object]) -> list[str]:
    rows = []
    for item in runs:
        run = as_mapping(item, "run")
        fault = get_str(run, "faultId")
        if not get_bool(run, "scored"):
            reason = get_opt_str(run, "reason") or ""
            rows.append(f"| {fault} | {get_str(run, 'status')}: {_cell(reason)} | | | | | | | | |")
            continue
        rank = get_opt_int(run, "expectedRank")
        top = f"{_yes(rank == 1)} (rank {rank})" if rank else "no"
        if not get_bool(run, "impactExpected"):
            top = "no impact expected"
        attempts = [as_mapping(a, "attempt") for a in get_list(run, "attempts")]
        unsafe = [a for a in attempts if get_bool(a, "unsafe")]
        unsafe_text = (
            ", ".join(f"{get_str(a, 'action')}: {get_str(a, 'outcome')}" for a in unsafe) or "none"
        )
        cost = get_opt_float(run, "costUsd")
        burn = get_opt_float(run, "preBurn1h")
        clean = run.get("cleanWindow")
        burn_text = "n/a" if burn is None else f"{burn:.2f}" + (" (clean)" if clean else "")
        rows.append(
            "| "
            + " | ".join(
                [
                    fault,
                    get_str(run, "detectionClass", "n/a"),
                    burn_text,
                    format_seconds(get_opt_float(run, "detectionSeconds")),
                    format_seconds(get_opt_float(run, "rcaSeconds")),
                    _cell(top),
                    _cell(get_opt_str(run, "proposedAction") or "none"),
                    _yes(get_bool(run, "actionCorrect")),
                    _cell(unsafe_text),
                    f"${cost:.3f}" if cost is not None else "n/a",
                ]
            )
            + " |"
        )
    return rows


def _baseline_rows(baselines: Sequence[object]) -> list[str]:
    rows = []
    for item in baselines:
        base = as_mapping(item, "baseline")
        rows.append(
            "| "
            + " | ".join(
                [
                    _cell(get_str(base, "name")),
                    str(get_opt_int(base, "runs") or 0),
                    str(get_opt_int(base, "blindRuns") or 0),
                    fmt_duration_stats(_section(base, "detectionSeconds")),
                    fmt_duration_stats(_section(base, "timeToRcaSeconds")),
                    fmt_duration_stats(_section(base, "timeToRcaBlindSeconds")),
                    fmt_proportion(_section(base, "top1")),
                ]
            )
            + " |"
        )
    return rows


def render_markdown(results: object) -> str:
    doc = as_mapping(results, "results")
    if get_str(doc, "kind") != RESULTS_KIND:
        raise DocumentError(f"not a results file: kind is {doc.get('kind')!r}")
    meta = _section(doc, "metadata")
    summary = _section(doc, "summary")
    synthetic = get_bool(meta, "synthetic")
    lines = []
    if synthetic:
        lines += [
            "> **Synthetic replay.** These numbers come from the made-up fixtures in",
            "> `evals/fixtures/`. They test the scoring, not the agent. Never quote them.",
            "",
        ]
    lines += [
        f"Mode: {get_str(meta, 'mode', 'unknown')}. Generated {get_str(meta, 'generatedAt')} "
        f"by {get_str(meta, 'generator')}.",
    ]
    session = [get_opt_str(meta, key) for key in ("startedAt", "finishedAt")]
    if all(session):
        lines.append(f"Session: {session[0]} to {session[1]} (UTC).")
    labels = meta.get("labels")
    if isinstance(labels, Mapping) and labels:
        rendered = ", ".join(f"`{key}={value}`" for key, value in sorted(labels.items()))
        lines.append(f"Conditions: {rendered}.")
    approvals = str(labels.get("APPROVALS", "none")) if isinstance(labels, Mapping) else "none"
    if approvals != "on-call":
        lines += [
            "",
            f"> `APPROVALS={approvals}`: nobody approved proposals, so nothing could be executed",
            '> and "executed = 0" holds by construction. It is not evidence for the guardrails;',
            "> don't cite it as such. Blocked-by-executor counts are.",
        ]
    lines += ["", "| Metric | Result |", "|---|---|"]
    lines += [f"| {name} | {value} |" for name, value in _summary_rows(summary)]
    lines += [
        "",
        "Per run:",
        "",
        "| Fault | Reached the agent | 1h burn before | Detection | Time to RCA | Top-1 "
        "| Proposed action | Correct action | Unsafe attempts | Cost of the fault |",
        "|---|---|---|---|---|---|---|---|---|---|",
    ]
    lines += _run_rows(get_list(doc, "runs"))
    baselines = get_list(doc, "baselines")
    if baselines:
        lines += [
            "",
            "Baselines (scored by the same rules):",
            "",
            "| Baseline | Runs | Blind runs | Detection | Time to root cause | Time to root cause "
            "(blind only) | Top-1 |",
            "|---|---|---|---|---|---|---|",
        ]
        lines += _baseline_rows(baselines)
    return "\n".join(lines) + "\n"
