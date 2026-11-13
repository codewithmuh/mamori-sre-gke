"""Steady state: every journey's prober SLIs are healthy, read from Managed Prometheus.

A fault is only injected into a healthy shop, and the next one only once the shop has
recovered; otherwise one run's incident leaks into the next run's score.

Prober metrics (#16), scheduled probes only (on-demand runs are not traffic):
- availability: share of successful probes per journey over the window;
- latency: p95 of the probe's request steps per journey, against the journey's SLO threshold
  (slo/slos.yaml from #18, else the PRD's starting values).
"""

from __future__ import annotations

import math
from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from pathlib import Path
from typing import Protocol

import yaml

from .faults import CUJS
from .jsonutil import as_mapping, get_list, get_opt_int, get_opt_mapping, get_str

# docs/prd.md starting targets: 500 ms, checkout 1,000 ms.
DEFAULT_THRESHOLDS_MS: dict[str, float] = {
    "browse": 500.0,
    "add-to-cart": 500.0,
    "view-cart": 500.0,
    "checkout": 1000.0,
}


class QueryApi(Protocol):
    def query(self, promql: str) -> list[tuple[dict[str, str], float]]: ...


def availability_query(window: str) -> str:
    return (
        "sum by (cuj) (increase("
        f'mamori_cuj_probe_total{{source="scheduled",result="success"}}[{window}]))'
        f' / sum by (cuj) (increase(mamori_cuj_probe_total{{source="scheduled"}}[{window}]))'
    )


def latency_query(window: str) -> str:
    return (
        "histogram_quantile(0.95, sum by (cuj, le) "
        f'(rate(mamori_cuj_step_duration_seconds_bucket{{source="scheduled"}}[{window}])))'
    )


def load_thresholds(slos_path: Path | None) -> dict[str, float]:
    """Latency thresholds per journey from the SLO spec, falling back to the PRD values."""
    thresholds = dict(DEFAULT_THRESHOLDS_MS)
    if slos_path is None or not slos_path.is_file():
        return thresholds
    with slos_path.open(encoding="utf-8") as handle:
        spec = as_mapping(yaml.safe_load(handle), "SLO spec")
    for item in get_list(spec, "journeys"):
        journey = as_mapping(item, "journey")
        latency = get_opt_mapping(journey, "latency")
        threshold = get_opt_int(latency, "thresholdMs") if latency else None
        if threshold:
            thresholds[get_str(journey, "id")] = float(threshold)
    return thresholds


@dataclass(frozen=True)
class SteadyResult:
    ok: bool
    reasons: tuple[str, ...]
    availability: Mapping[str, float] = field(default_factory=dict)
    p95_ms: Mapping[str, float] = field(default_factory=dict)

    def describe(self) -> str:
        return "steady" if self.ok else "not steady: " + "; ".join(self.reasons)


def by_cuj(series: Sequence[tuple[dict[str, str], float]]) -> dict[str, float]:
    """NaN (no probes in the window) counts as missing."""
    return {
        labels["cuj"]: value
        for labels, value in series
        if "cuj" in labels and not math.isnan(value)
    }


def evaluate(
    availability: Mapping[str, float],
    p95_seconds: Mapping[str, float] | None,
    cujs: Sequence[str],
    min_success: float,
    thresholds_ms: Mapping[str, float],
) -> SteadyResult:
    reasons = []
    p95_ms = {cuj: value * 1000 for cuj, value in (p95_seconds or {}).items()}
    for cuj in cujs:
        ratio = availability.get(cuj)
        if ratio is None:
            reasons.append(f"{cuj}: no scheduled probes in the window")
        elif ratio < min_success:
            reasons.append(f"{cuj}: probe success {ratio:.3f} < {min_success}")
        if p95_seconds is None:
            continue
        latency = p95_ms.get(cuj)
        limit = thresholds_ms.get(cuj)
        if latency is None:
            reasons.append(f"{cuj}: no step latency data")
        elif math.isinf(latency) or (limit is not None and latency > limit):
            bound = f"{limit:.0f} ms" if limit is not None else "the histogram's top bucket"
            reasons.append(f"{cuj}: p95 step latency {latency:.0f} ms > {bound}")
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
    ) -> None:
        self.prometheus = prometheus
        self.window = window
        self.min_success = min_success
        self.thresholds_ms = dict(thresholds_ms or DEFAULT_THRESHOLDS_MS)
        self.check_latency = check_latency
        self.cujs = tuple(cujs)

    def check(self) -> SteadyResult:
        availability = by_cuj(self.prometheus.query(availability_query(self.window)))
        p95 = (
            by_cuj(self.prometheus.query(latency_query(self.window)))
            if self.check_latency
            else None
        )
        return evaluate(availability, p95, self.cujs, self.min_success, self.thresholds_ms)
