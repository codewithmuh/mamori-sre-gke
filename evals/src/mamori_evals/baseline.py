"""Baselines to compare the agent with, scored by the same rules.

- Human: the game-day postmortems (#20). Their optional front-matter `timeline` gives
  faultStart, detected and rootCauseIdentified; the body's key-facts table says whether the
  game day was blind. A responder who knew which fault ran finds the cause fast, so blind and
  non-blind runs are reported apart.
- Another tool (e.g. HolmesGPT on the same faults): a baseline file, `kind: EvalBaseline`.
"""

from __future__ import annotations

import re
from collections.abc import Mapping
from dataclasses import dataclass
from datetime import UTC, date, datetime, time
from pathlib import Path

import yaml

from .aggregate import Stats, proportion
from .clients import CommandRunner, run_command
from .faults import Fault
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
)
from .records import Hypothesis
from .scoring import accepted_answers, expected_rank
from .timeutil import parse_time, seconds_between

BASELINE_KIND = "EvalBaseline"
_BLIND_ROW = re.compile(r"^\|\s*blind\b[^|]*\|\s*(yes|no)\b", re.IGNORECASE | re.MULTILINE)


@dataclass(frozen=True)
class BaselineRun:
    fault_id: str
    fault_start: datetime | None
    detected_at: datetime | None
    root_cause_at: datetime | None
    hypotheses: tuple[Hypothesis, ...]
    cost_usd: float | None
    blind: bool | None
    source: str | None


@dataclass(frozen=True)
class Baseline:
    name: str
    system: str
    source: str
    runs: tuple[BaselineRun, ...]


def _opt_bool(data: Mapping[str, object], key: str) -> bool | None:
    value = data.get(key)
    if value is None:
        return None
    if isinstance(value, bool):
        return value
    if isinstance(value, str) and value.strip().lower() in ("yes", "no", "true", "false"):
        return value.strip().lower() in ("yes", "true")
    raise DocumentError(f"{key}: expected yes/no, got {value!r}")


def parse_baseline(document: object, origin: str) -> Baseline:
    try:
        data = as_mapping(document, "baseline")
        if get_str(data, "kind") != BASELINE_KIND:
            raise DocumentError(f"kind is {data.get('kind')!r}, want {BASELINE_KIND}")
        runs = []
        for index, item in enumerate(get_list(data, "runs")):
            run = as_mapping(item, f"runs[{index}]")
            hypotheses = []
            for position, raw in enumerate(get_list(run, "hypotheses"), start=1):
                hyp = as_mapping(raw, f"runs[{index}].hypotheses")
                hypotheses.append(
                    Hypothesis(
                        rank=get_opt_int(hyp, "rank") or position,
                        service=get_str(hyp, "service"),
                        summary=get_str(hyp, "summary"),
                        confidence=None,
                        evidence=(),
                    )
                )
            runs.append(
                BaselineRun(
                    fault_id=get_str(run, "faultId"),
                    fault_start=get_opt_time(run, "faultStart"),
                    detected_at=get_opt_time(run, "detectedAt"),
                    root_cause_at=get_opt_time(run, "rootCauseAt"),
                    hypotheses=tuple(hypotheses),
                    cost_usd=get_opt_float(run, "costUsd"),
                    blind=_opt_bool(run, "blind"),
                    source=get_opt_str(run, "source"),
                )
            )
        return Baseline(
            name=get_str(data, "name", origin),
            system=get_str(data, "system", "other"),
            source=get_str(data, "source", origin),
            runs=tuple(runs),
        )
    except (DocumentError, ValueError) as err:
        raise DocumentError(f"{origin}: {err}") from err


def load_baseline(path: Path) -> Baseline:
    with path.open(encoding="utf-8") as handle:
        return parse_baseline(yaml.safe_load(handle), str(path))


def read_front_matter(text: str) -> tuple[Mapping[str, object] | None, str]:
    """YAML between the first two '---' lines, and the body after it."""
    lines = text.splitlines(keepends=True)
    if not lines or lines[0].strip() != "---":
        return None, text
    for index in range(1, len(lines)):
        if lines[index].strip() == "---":
            front = yaml.safe_load("".join(lines[1:index]))
            body = "".join(lines[index + 1 :])
            return (as_mapping(front, "front matter") if front is not None else {}), body
    return None, text


def _postmortems(directory: Path) -> list[tuple[Path, Mapping[str, object], str]]:
    found = []
    for path in sorted(directory.glob("*.md")):
        try:
            front, body = read_front_matter(path.read_text(encoding="utf-8"))
        except (yaml.YAMLError, DocumentError) as err:
            raise DocumentError(f"{path}: front matter: {err}") from err
        if front is not None:
            found.append((path, front, body))
    return found


def baseline_from_postmortems(directory: Path) -> Baseline:
    """Human baseline: game-day postmortems that carry a faultId and a timeline."""
    runs = []
    for path, front, body in _postmortems(directory):
        fault_id = get_opt_str(front, "faultId")
        timeline = get_opt_mapping(front, "timeline")
        if not fault_id or timeline is None:
            continue
        blind_match = _BLIND_ROW.search(body)
        try:
            runs.append(
                BaselineRun(
                    fault_id=fault_id,
                    fault_start=get_opt_time(timeline, "faultStart"),
                    detected_at=get_opt_time(timeline, "detected"),
                    root_cause_at=get_opt_time(timeline, "rootCauseIdentified"),
                    hypotheses=(),
                    cost_usd=None,
                    blind=(blind_match.group(1).lower() == "yes") if blind_match else None,
                    source=path.name,
                )
            )
        except DocumentError as err:
            raise DocumentError(f"{path}: timeline: {err}") from err
    return Baseline(
        name="Human responder (game-day postmortems)",
        system="human",
        source=str(directory),
        runs=tuple(runs),
    )


type PostmortemIndex = dict[str, list[datetime]]


def postmortem_dates(directory: Path) -> PostmortemIndex:
    """faultId -> when each postmortem about it existed, from the front matter's `date`.

    An approximation: a postmortem is merged some time after its incident. Prefer
    postmortems_on_ref, which knows when each file reached the branch the agent reads.
    """
    index: PostmortemIndex = {}
    for path, front, _ in _postmortems(directory):
        fault_id = get_opt_str(front, "faultId")
        raw = front.get("date")
        if not fault_id or raw is None:
            continue
        if isinstance(raw, datetime):
            when = raw.date()
        elif isinstance(raw, date):
            when = raw
        else:
            try:
                when = date.fromisoformat(str(raw).strip())
            except ValueError as err:
                raise DocumentError(f"{path}: date {raw!r} is not YYYY-MM-DD") from err
        index.setdefault(fault_id, []).append(datetime.combine(when, time.min, UTC))
    return index


def postmortems_on_ref(
    repo: Path,
    ref: str,
    path: str = "docs/incidents/postmortems",
    runner: CommandRunner = run_command,
) -> PostmortemIndex:
    """faultId -> when each game-day postmortem landed on `ref` (the agent reads
    postmortems from the repository's main branch, #24)."""

    def git(*args: str) -> str:
        result = runner(["git", "-C", str(repo), *args], 60.0)
        if result.code != 0:
            raise DocumentError(f"git {args[0]} {ref}: {result.stderr.strip()[:200]}")
        return result.stdout

    index: PostmortemIndex = {}
    for name in git("ls-tree", "-r", "--name-only", ref, "--", path).split():
        if not name.endswith(".md"):
            continue
        front, _ = read_front_matter(git("show", f"{ref}:{name}"))
        fault_id = get_opt_str(front, "faultId") if front else None
        if not fault_id:
            continue
        added = [
            parse_time(line)
            for line in git("log", "--diff-filter=A", "--format=%cI", ref, "--", name).split()
        ]
        if added:
            index.setdefault(fault_id, []).append(min(added))
    return index


def prior_postmortem(
    index: PostmortemIndex | None,
    fault_id: str,
    when: datetime | None,
    gamedays_setting: object = None,
) -> str | None:
    """Whether the agent could have read a postmortem of this fault: `seen`, `hidden` (one
    existed, but AGENT_POSTMORTEMS_GAMEDAYS=exclude hid it), `none`, or None if unknown."""
    if index is None or when is None:
        return None
    if not any(landed <= when for landed in index.get(fault_id, ())):
        return "none"
    if str(gamedays_setting).strip().lower() == "exclude":
        return "hidden"
    return "seen"


def summarize_baseline(baseline: Baseline, faults: Mapping[str, Fault]) -> JSONDict:
    runs = [run for run in baseline.runs if run.fault_id in faults]
    ranked_runs = [run for run in runs if run.hypotheses]
    ranks = {
        id(run): expected_rank(run.hypotheses, accepted_answers(faults[run.fault_id]))
        for run in ranked_runs
    }
    rows: list[JSONDict] = []
    for run in runs:
        rank = ranks.get(id(run))
        rows.append(
            {
                "faultId": run.fault_id,
                "detectionSeconds": seconds_between(run.fault_start, run.detected_at),
                "rcaSeconds": seconds_between(run.fault_start, run.root_cause_at),
                "top1": rank == 1 if run.hypotheses else None,
                "blind": run.blind,
                "costUsd": run.cost_usd,
                "source": run.source,
            }
        )
    return {
        "name": baseline.name,
        "system": baseline.system,
        "source": baseline.source,
        "runs": len(runs),
        "unknownFaults": sorted({run.fault_id for run in baseline.runs} - set(faults)),
        "blindRuns": sum(run.blind is True for run in runs),
        "detectionSeconds": Stats.of(
            seconds_between(run.fault_start, run.detected_at) for run in runs
        ).to_dict(),
        "timeToRcaSeconds": Stats.of(
            seconds_between(run.fault_start, run.root_cause_at) for run in runs
        ).to_dict(),
        "timeToRcaBlindSeconds": Stats.of(
            seconds_between(run.fault_start, run.root_cause_at) for run in runs if run.blind
        ).to_dict(),
        "top1": proportion(ranks[id(run)] == 1 for run in ranked_runs).to_dict(),
        "top3": proportion(
            (rank := ranks[id(run)]) is not None and rank <= 3 for run in ranked_runs
        ).to_dict(),
        "costUsd": Stats.of(run.cost_usd for run in runs).to_dict(),
        "perRun": rows,
    }
