"""Steady state and the burn-rate gate, from the prober SLIs in Managed Prometheus.

A fault is only injected into a healthy shop whose error budget isn't still burning from the
last fault; otherwise one run's incident leaks into the next run's score.

Prober metrics (#16), scheduled probes only (on-demand runs are not traffic):
- availability: share of successful probes per journey over the window;
- latency: p95 of the journey's SLO steps (slo/slos.yaml `prober.latencySteps`, #18) against
  the journey's threshold;
- burn rate: the error ratio over 1 h (and 6 h) divided by the journey's error budget
  (1 - target). The page-tier alerts fire at 13.44 over 1 h and 5.6 over 6 h, each with a
  short window (#18). Leftover burn in those long windows makes the next fault's alert fire
  sooner, or lets a fault with no expected impact page anyone.
"""

from __future__ import annotations

import math
import re
from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from pathlib import Path
from typing import Protocol

import yaml

from .faults import CUJS
from .jsonutil import (
    DocumentError,
    as_mapping,
    get_list,
    get_opt_float,
    get_opt_int,
    get_opt_mapping,
    get_str,
    get_str_list,
)

# docs/prd.md starting targets, used when slo/slos.yaml is absent.
DEFAULT_THRESHOLDS_MS: dict[str, float] = {
    "browse": 500.0,
    "add-to-cart": 500.0,
    "view-cart": 500.0,
    "checkout": 1000.0,
}
DEFAULT_TARGETS: dict[str, float] = {
    "browse": 0.995,
    "add-to-cart": 0.995,
    "view-cart": 0.995,
    "checkout": 0.99,
}
# The page tiers' long-window burn thresholds (docs/slo/slo-design.md, #18).
FAST_BURN_1H = 13.44
MEDIUM_BURN_6H = 5.6
# Inject only while every journey's 1-hour burn is below half the fast page threshold.
DEFAULT_MAX_BURN_1H = FAST_BURN_1H / 2
BURN_WINDOWS = ("1h", "6h")
_STEP_NAME = re.compile(r"^[A-Za-z0-9_-]+$")


class QueryApi(Protocol):
    def query(self, promql: str) -> list[tuple[dict[str, str], float]]: ...


@dataclass(frozen=True)
class SloSpec:
    thresholds_ms: Mapping[str, float]
    targets: Mapping[str, float]
    latency_steps: Mapping[str, tuple[str, ...]]


def load_slo_spec(slos_path: Path | None) -> SloSpec:
    """Thresholds, targets and latency steps per journey from slo/slos.yaml; PRD values and
    every step when the file or a field is missing."""
    thresholds = dict(DEFAULT_THRESHOLDS_MS)
    targets = dict(DEFAULT_TARGETS)
    steps: dict[str, tuple[str, ...]] = {}
    if slos_path is None or not slos_path.is_file():
        return SloSpec(thresholds, targets, steps)
    with slos_path.open(encoding="utf-8") as handle:
        spec = as_mapping(yaml.safe_load(handle), "SLO spec")
    for item in get_list(spec, "journeys"):
        journey = as_mapping(item, "journey")
        cuj = get_str(journey, "id")
        latency = get_opt_mapping(journey, "latency")
        threshold = get_opt_int(latency, "thresholdMs") if latency else None
        if threshold:
            thresholds[cuj] = float(threshold)
        availability = get_opt_mapping(journey, "availability")
        target = get_opt_float(availability, "target") if availability else None
        if target is not None:
            targets[cuj] = target
    prober = get_opt_mapping(spec, "prober") or {}
    for cuj, names in (get_opt_mapping(prober, "latencySteps") or {}).items():
        listed = get_str_list({"steps": names}, "steps")
        for name in listed:
            if not _STEP_NAME.match(name):
                raise DocumentError(f"latencySteps.{cuj}: step name {name!r} is not [A-Za-z0-9_-]+")
        steps[cuj] = listed
    return SloSpec(thresholds, targets, steps)


def load_thresholds(slos_path: Path | None) -> dict[str, float]:
    return dict(load_slo_spec(slos_path).thresholds_ms)


def availability_query(window: str) -> str:
    return (
        "sum by (cuj) (increase("
        f'mamori_cuj_probe_total{{source="scheduled",result="success"}}[{window}]))'
        f' / sum by (cuj) (increase(mamori_cuj_probe_total{{source="scheduled"}}[{window}]))'
    )


def latency_query(window: str, cuj: str | None = None, steps: Sequence[str] = ()) -> str:
    """p95 step latency per journey; for one journey's SLO steps when they are given."""
    matchers = ['source="scheduled"']
    if cuj is not None:
        matchers.append(f'cuj="{cuj}"')
    if steps:
        matchers.append(f'step=~"{"|".join(steps)}"')
    return (
        "histogram_quantile(0.95, sum by (cuj, le) "
        f"(rate(mamori_cuj_step_duration_seconds_bucket{{{','.join(matchers)}}}[{window}])))"
    )


@dataclass(frozen=True)
class SteadyResult:
    ok: bool
    reasons: tuple[str, ...]
    availability: Mapping[str, float] = field(default_factory=dict)
    p95_ms: Mapping[str, float] = field(default_factory=dict)
    burn: Mapping[str, Mapping[str, float]] = field(default_factory=dict)

    def describe(self) -> str:
        return "steady" if self.ok else "not steady: " + "; ".join(self.reasons)


def by_cuj(series: Sequence[tuple[dict[str, str], float]]) -> dict[str, float]:
    """NaN (no probes in the window) counts as missing."""
    return {
        labels["cuj"]: value
        for labels, value in series
        if "cuj" in labels and not math.isnan(value)
    }


def burn_rates(availability: Mapping[str, float], targets: Mapping[str, float]) -> dict[str, float]:
    """Error ratio divided by the error budget, per journey with data."""
    rates = {}
    for cuj, ratio in availability.items():
        budget = 1.0 - targets.get(cuj, DEFAULT_TARGETS.get(cuj, 0.995))
        if budget > 0:
            rates[cuj] = round(max(0.0, 1.0 - ratio) / budget, 4)
    return rates


def evaluate(
    availability: Mapping[str, float],
    p95_seconds: Mapping[str, float] | None,
    cujs: Sequence[str],
    min_success: float,
    thresholds_ms: Mapping[str, float],
    burn_1h: Mapping[str, float] | None = None,
    max_burn_1h: float | None = None,
) -> SteadyResult:
    reasons = []
    p95_ms = {cuj: value * 1000 for cuj, value in (p95_seconds or {}).items()}
    for cuj in cujs:
        ratio = availability.get(cuj)
        if ratio is None:
            reasons.append(f"{cuj}: no scheduled probes in the window")
        elif ratio < min_success:
            reasons.append(f"{cuj}: probe success {ratio:.3f} < {min_success}")
        if p95_seconds is not None:
            latency = p95_ms.get(cuj)
            limit = thresholds_ms.get(cuj)
            if latency is None:
                reasons.append(f"{cuj}: no step latency data")
            elif math.isinf(latency) or (limit is not None and latency > limit):
                bound = f"{limit:.0f} ms" if limit is not None else "the histogram's top bucket"
                reasons.append(f"{cuj}: p95 step latency {latency:.0f} ms > {bound}")
        if max_burn_1h is not None and burn_1h is not None:
            burn = burn_1h.get(cuj)
            if burn is None:
                reasons.append(f"{cuj}: no 1h burn rate")
            elif burn >= max_burn_1h:
                reasons.append(f"{cuj}: 1h burn rate {burn:.2f} >= {max_burn_1h:.2f}")
    return SteadyResult(not reasons, tuple(reasons), dict(availability), p95_ms)


class SteadyStateCheck:
    def __init__(
        self,
        prometheus: QueryApi,
        window: str = "5m",
        min_success: float = 0.99,
        thresholds_ms: Mapping[str, float] | None = None,
        check_latency: bool = True,
        cujs: Sequence[str] = CUJS,
        spec: SloSpec | None = None,
        max_burn_1h: float | None = DEFAULT_MAX_BURN_1H,
    ) -> None:
        self.prometheus = prometheus
        self.window = window
        self.min_success = min_success
        self.spec = spec or SloSpec(dict(DEFAULT_THRESHOLDS_MS), dict(DEFAULT_TARGETS), {})
        self.thresholds_ms = dict(thresholds_ms or self.spec.thresholds_ms)
        self.check_latency = check_latency
        self.cujs = tuple(cujs)
        self.max_burn_1h = max_burn_1h

    def _p95(self) -> dict[str, float]:
        if not self.spec.latency_steps:
            return by_cuj(self.prometheus.query(latency_query(self.window)))
        p95: dict[str, float] = {}
        for cuj in self.cujs:
            steps = self.spec.latency_steps.get(cuj, ())
            p95.update(by_cuj(self.prometheus.query(latency_query(self.window, cuj, steps))))
        return p95

    def burn(self) -> dict[str, dict[str, float]]:
        return {
            window: burn_rates(
                by_cuj(self.prometheus.query(availability_query(window))), self.spec.targets
            )
            for window in BURN_WINDOWS
        }

    def check(self, burn_gate: bool = False) -> SteadyResult:
        """Healthy now; with burn_gate, also no leftover 1-hour burn (before an injection)."""
        availability = by_cuj(self.prometheus.query(availability_query(self.window)))
        p95 = self._p95() if self.check_latency else None
        burn = self.burn() if burn_gate else {}
        result = evaluate(
            availability,
            p95,
            self.cujs,
            self.min_success,
            self.thresholds_ms,
            burn.get("1h"),
            self.max_burn_1h if burn_gate else None,
        )
        return SteadyResult(result.ok, result.reasons, result.availability, result.p95_ms, burn)
