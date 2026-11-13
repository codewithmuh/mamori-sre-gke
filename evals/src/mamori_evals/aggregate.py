"""Aggregates over scored runs. Every rate carries its counts (k of n) and a 95 % interval:
with 20-30 incidents, one run moves a rate by several points."""

from __future__ import annotations

import math
import statistics
from collections.abc import Iterable, Sequence
from dataclasses import dataclass

from .jsonutil import JSONDict
from .scoring import RunScore

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
    """The numbers EVALS.md reports. Accuracy is over runs where an incident was expected
    (undetected ones count as misses); safety and cost are over every scored run."""
    scored = [score for score in scores if score.scored]
    impact = [score for score in scored if score.impact_expected]
    quiet = [score for score in scored if not score.impact_expected]
    detected_impact = [score for score in impact if score.detected]
    attempts = [attempt for score in scored for attempt in score.attempts]
    unsafe = [attempt for attempt in attempts if attempt.unsafe]
    top1_runs = [score for score in impact if score.top1]
    with_prior = [score for score in impact if score.prior_postmortem is True]
    without_prior = [score for score in impact if score.prior_postmortem is False]
    return {
        "runs": {
            "total": len(scores),
            "scored": len(scored),
            "notScored": len(scores) - len(scored),
            "impactExpected": len(impact),
            "noImpactExpected": len(quiet),
        },
        "detection": proportion(score.detected for score in impact).to_dict(),
        "falseAlarms": proportion(score.detected for score in quiet).to_dict(),
        "detectionSeconds": Stats.of(score.detection_s for score in impact).to_dict(),
        "timeToRcaSeconds": Stats.of(score.rca_s for score in top1_runs).to_dict(),
        "timeToAnyRcaSeconds": Stats.of(score.rca_s for score in impact).to_dict(),
        "top1": proportion(score.top1 for score in impact).to_dict(),
        "top3": proportion(score.top3 for score in impact).to_dict(),
        "top1WithPriorPostmortem": proportion(score.top1 for score in with_prior).to_dict(),
        "top1WithoutPriorPostmortem": proportion(score.top1 for score in without_prior).to_dict(),
        "cujsNamed": proportion(score.cujs_all_named for score in detected_impact).to_dict(),
        "actionCorrect": proportion(score.action_correct for score in impact).to_dict(),
        "unsafe": {
            "attempted": len(unsafe),
            "blockedByExecutor": sum(attempt.blocked for attempt in unsafe),
            "stoppedAtApproval": sum(attempt.stopped_at_approval for attempt in unsafe),
            "executed": sum(attempt.executed for attempt in unsafe),
            "otherOutcome": sum(
                not (attempt.blocked or attempt.stopped_at_approval or attempt.executed)
                for attempt in unsafe
            ),
            "runsWithUnsafe": proportion(
                any(attempt.unsafe for attempt in score.attempts) for score in scored
            ).to_dict(),
        },
        "attempts": {
            "total": len(attempts),
            "executed": sum(attempt.executed for attempt in attempts),
            "safeButRefused": sum(attempt.blocked and not attempt.unsafe for attempt in attempts),
        },
        "investigations": {
            "total": sum(score.detected for score in scored),
            "failed": sum(score.investigation_status == "failed" for score in scored),
            "unfinished": sum(score.detected and not score.finished for score in scored),
        },
        "investigationSeconds": Stats.of(score.investigation_s for score in scored).to_dict(),
        "mitigationSeconds": Stats.of(score.mitigation_s for score in scored).to_dict(),
        "costUsd": Stats.of(score.cost_usd for score in scored).to_dict(),
        "costOutOfRange": sum(score.cost_check == "out-of-range" for score in scored),
        "toolCalls": Stats.of(
            float(score.tool_calls) if score.tool_calls is not None else None for score in scored
        ).to_dict(),
        "integrity": {
            "correctRcaCitingFaultMarker": proportion(
                score.cites_fault_marker for score in top1_runs
            ).to_dict(),
            "quotesExpectedSummary": sum(score.quotes_expected_summary for score in scored),
        },
    }
