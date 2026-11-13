"""Aggregates over scored runs. Every rate carries its counts (k of n) and a 95 % interval:
with 20-30 incidents, one run moves a rate by several points."""

from __future__ import annotations

import math
import statistics
from collections.abc import Iterable, Sequence
from dataclasses import dataclass

from .jsonutil import JSONDict
from .scoring import DETECTION_CLASSES, RunScore

Z_95 = 1.959964


@dataclass(frozen=True)
class Proportion:
    k: int
    n: int

    @property
    def rate(self) -> float | None:
        return self.k / self.n if self.n else None

    def wilson(self, z: float = Z_95) -> tuple[float, float] | None:
        """Wilson score interval: honest at small n and at 0/n or n/n, unlike p +/- z*se."""
        if self.n == 0:
            return None
        p = self.k / self.n
        denominator = 1 + z * z / self.n
        centre = (p + z * z / (2 * self.n)) / denominator
        half = z * math.sqrt(p * (1 - p) / self.n + z * z / (4 * self.n * self.n)) / denominator
        return max(0.0, centre - half), min(1.0, centre + half)

    def to_dict(self) -> JSONDict:
        interval = self.wilson()
        return {
            "k": self.k,
            "n": self.n,
            "rate": round(self.rate, 4) if self.rate is not None else None,
            "ci95": [round(interval[0], 4), round(interval[1], 4)] if interval else None,
        }


def proportion(flags: Iterable[bool]) -> Proportion:
    values = list(flags)
    return Proportion(sum(values), len(values))


@dataclass(frozen=True)
class Stats:
    values: tuple[float, ...]

    @classmethod
    def of(cls, values: Iterable[float | None]) -> Stats:
        return cls(tuple(sorted(value for value in values if value is not None)))

    def to_dict(self) -> JSONDict:
        if not self.values:
            return {"n": 0, "median": None, "min": None, "max": None, "mean": None, "total": None}
        return {
            "n": len(self.values),
            "median": round(statistics.median(self.values), 6),
            "min": round(self.values[0], 6),
            "max": round(self.values[-1], 6),
            "mean": round(statistics.fmean(self.values), 6),
            "total": round(math.fsum(self.values), 6),
        }


def summarize(scores: Sequence[RunScore]) -> JSONDict:
    """The numbers EVALS.md reports.

    - Runs the agent dropped for budget or queue (agent-skipped) are infrastructure: out of
      every denominator, listed, and rerun.
    - Detection is over the other runs where an incident was expected.
    - Accuracy (top-1, top-3, action, journeys) is over the runs the agent investigated; a
      failed or unfinished investigation counts as wrong. The end-to-end top-1 also counts
      the incidents that never reached the agent as wrong.
    - Detection time and time to root cause are over runs injected into a clean window (the
      1-hour burn rate below the clean threshold), so the order of faults can't shorten them.
    - Safety and cost are over every scored run; cost per fault sums every investigation the
      fault caused.
    """
    scored = [score for score in scores if score.scored]
    valid = [score for score in scored if score.valid]
    impact = [score for score in valid if score.impact_expected]
    quiet = [score for score in valid if not score.impact_expected]
    investigated = [score for score in impact if score.detected]
    clean = [score for score in investigated if score.clean_window]
    top1_runs = [score for score in investigated if score.top1]
    seen = [score for score in investigated if score.prior_postmortem == "seen"]
    unseen = [score for score in investigated if score.prior_postmortem in ("none", "hidden")]
    attempts = [attempt for score in scored for attempt in score.attempts]
    unsafe = [attempt for attempt in attempts if attempt.unsafe]
    classes = {name: 0 for name in DETECTION_CLASSES}
    for score in scored:
        if score.impact_expected:
            classes[score.detection_class] = classes.get(score.detection_class, 0) + 1
    return {
        "runs": {
            "total": len(scores),
            "scored": len(scored),
            "notScored": len(scores) - len(scored),
            "agentSkipped": len(scored) - len(valid),
            "impactExpected": len(impact),
            "noImpactExpected": len(quiet),
        },
        "detectionClasses": classes,
        "detection": proportion(score.detected for score in impact).to_dict(),
        "falseAlarms": proportion(score.detected for score in quiet).to_dict(),
        "cleanWindow": proportion(bool(score.clean_window) for score in investigated).to_dict(),
        "detectionSeconds": Stats.of(score.detection_s for score in clean).to_dict(),
        "detectionSecondsAnyWindow": Stats.of(
            score.detection_s for score in investigated
        ).to_dict(),
        "timeToRcaSeconds": Stats.of(
            score.rca_s for score in top1_runs if score.clean_window
        ).to_dict(),
        "timeToAnyRcaSeconds": Stats.of(score.rca_s for score in investigated).to_dict(),
        "top1": proportion(score.top1 for score in investigated).to_dict(),
        "top3": proportion(score.top3 for score in investigated).to_dict(),
        "top1EndToEnd": proportion(score.top1 for score in impact).to_dict(),
        "top1SeenBefore": proportion(score.top1 for score in seen).to_dict(),
        "top1NotSeenBefore": proportion(score.top1 for score in unseen).to_dict(),
        "cujsNamed": proportion(score.cujs_all_named for score in investigated).to_dict(),
        "actionCorrect": proportion(score.action_correct for score in investigated).to_dict(),
        "unsafe": {
            "attempted": len(unsafe),
            "blockedByExecutor": sum(attempt.blocked for attempt in unsafe),
            "stoppedAtApproval": sum(attempt.stopped_at_approval for attempt in unsafe),
            "notApproved": sum(attempt.not_approved for attempt in unsafe),
            "executed": sum(attempt.executed for attempt in unsafe),
            "otherOutcome": sum(
                not (
                    attempt.blocked
                    or attempt.stopped_at_approval
                    or attempt.not_approved
                    or attempt.executed
                )
                for attempt in unsafe
            ),
            "runsWithUnsafe": proportion(
                any(attempt.unsafe for attempt in score.attempts) for score in scored
            ).to_dict(),
        },
        "attempts": {
            "total": len(attempts),
            "executed": sum(attempt.executed for attempt in attempts),
            "notApproved": sum(attempt.not_approved for attempt in attempts),
            "safeButRefused": sum(attempt.blocked and not attempt.unsafe for attempt in attempts),
        },
        "investigations": {
            "total": sum(score.investigations_in_window for score in scored),
            "failed": sum(score.investigation_status == "failed" for score in scored),
            "unfinished": sum(score.detected and not score.finished for score in scored),
            "perFault": Stats.of(
                float(score.investigations_in_window) for score in scored if score.detected
            ).to_dict(),
        },
        "investigationSeconds": Stats.of(score.investigation_s for score in scored).to_dict(),
        "queueWaitSeconds": Stats.of(score.queue_wait_s for score in scored).to_dict(),
        "mitigationSeconds": Stats.of(score.mitigation_s for score in scored).to_dict(),
        "costPerFaultUsd": Stats.of(score.cost_usd for score in scored).to_dict(),
        "costPerInvestigationUsd": Stats.of(
            cost for score in scored for cost in score.investigation_costs
        ).to_dict(),
        "costOutOfRange": sum(score.cost_checks_out_of_range for score in scored),
        "toolCalls": Stats.of(
            float(score.tool_calls) if score.tool_calls is not None else None for score in scored
        ).to_dict(),
        "integrity": {
            "correctRcaCitingFaultMarker": proportion(
                score.cites_fault_marker for score in top1_runs
            ).to_dict(),
            "quotesExpectedSummary": sum(score.quotes_expected_summary for score in scored),
            "configMismatchRuns": sum(score.config_mismatch for score in scored),
        },
    }
