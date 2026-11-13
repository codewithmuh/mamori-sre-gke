from __future__ import annotations

import math
from datetime import UTC, datetime
from pathlib import Path

import pytest

from mamori_evals.baseline import (
    baseline_from_postmortems,
    had_prior_postmortem,
    load_baseline,
    parse_baseline,
    postmortem_dates,
    read_front_matter,
)
from mamori_evals.jsonutil import DocumentError
from mamori_evals.steady import (
    DEFAULT_THRESHOLDS_MS,
    SteadyStateCheck,
    availability_query,
    by_cuj,
    evaluate,
    latency_query,
    load_thresholds,
)

from .conftest import FIXTURES


class TestBaseline:
    def test_front_matter(self) -> None:
        front, body = read_front_matter("---\na: 1\n---\nbody\n")
        assert front == {"a": 1}
        assert body == "body\n"
        assert read_front_matter("no front matter") == (None, "no front matter")
        assert read_front_matter("---\na: 1\nnever closed\n")[0] is None
        assert read_front_matter("---\n---\n")[0] == {}

    def test_parse_baseline_errors(self) -> None:
        with pytest.raises(DocumentError, match="kind"):
            parse_baseline({"kind": "Other"}, "x.yaml")
        with pytest.raises(DocumentError, match="blind"):
            parse_baseline({"kind": "EvalBaseline", "runs": [{"faultId": "F", "blind": 3}]}, "x")
        base = parse_baseline(
            {"kind": "EvalBaseline", "runs": [{"faultId": "F", "blind": "yes"}, {"faultId": "G"}]},
            "x.yaml",
        )
        assert base.name == "x.yaml"
        assert [r.blind for r in base.runs] == [True, None]

    def test_load_baseline_file(self) -> None:
        base = load_baseline(FIXTURES / "baselines" / "synthetic-other-tool.yaml")
        assert base.system == "other"
        assert len(base.runs) == 3
        assert base.runs[0].hypotheses[0].service == "currencyservice"

    def test_postmortem_dates(self, tmp_path: Path) -> None:
        (tmp_path / "a.md").write_text("---\nfaultId: F01-x\ndate: 2026-12-05\n---\n")
        (tmp_path / "b.md").write_text("---\nfaultId: F01-x\ndate: '2026-12-20'\n---\n")
        (tmp_path / "c.md").write_text("---\nfaultId: F02-x\n---\n")
        (tmp_path / "d.md").write_text("# no front matter\n")
        index = postmortem_dates(tmp_path)
        assert [d.isoformat() for d in index["F01-x"]] == ["2026-12-05", "2026-12-20"]
        when = datetime(2026, 12, 10, tzinfo=UTC)
        assert had_prior_postmortem(index, "F01-x", when) is True
        assert had_prior_postmortem(index, "F02-x", when) is False
        assert had_prior_postmortem(None, "F01-x", when) is None
        assert had_prior_postmortem(index, "F01-x", None) is None

    def test_bad_postmortems(self, tmp_path: Path) -> None:
        (tmp_path / "a.md").write_text("---\nfaultId: F01-x\ndate: soon\n---\n")
        with pytest.raises(DocumentError, match="YYYY-MM-DD"):
            postmortem_dates(tmp_path)
        (tmp_path / "a.md").write_text("---\n: [\n---\n")
        with pytest.raises(DocumentError, match="front matter"):
            postmortem_dates(tmp_path)
        (tmp_path / "a.md").write_text("---\nfaultId: F01-x\ntimeline: {faultStart: later}\n---\n")
        with pytest.raises(DocumentError, match="timeline"):
            baseline_from_postmortems(tmp_path)

    def test_yaml_dates_and_datetimes(self, tmp_path: Path) -> None:
        (tmp_path / "a.md").write_text(
            "---\nfaultId: F01-x\ndate: 2026-12-05T10:00:00Z\n"
            "timeline:\n"
            "  faultStart: 2026-12-05T10:00:00Z\n"
            "  rootCauseIdentified: 2026-12-05T10:30:00Z\n"
            "---\n| Blind run | Yes |\n"
        )
        assert postmortem_dates(tmp_path)["F01-x"][0].isoformat() == "2026-12-05"
        (run,) = baseline_from_postmortems(tmp_path).runs
        assert run.blind is True
        assert run.root_cause_at is not None


class FakeProm:
    def __init__(
        self, avail: list[tuple[dict[str, str], float]], p95: list[tuple[dict[str, str], float]]
    ) -> None:
        self.avail, self.p95, self.queries = avail, p95, []

    def query(self, promql: str) -> list[tuple[dict[str, str], float]]:
        self.queries.append(promql)
        return self.avail if "mamori_cuj_probe_total" in promql else self.p95


ALL = ("browse", "add-to-cart", "view-cart", "checkout")


class TestSteady:
    def test_queries(self) -> None:
        assert 'result="success"' in availability_query("5m")
        assert "[5m]" in availability_query("5m")
        assert "histogram_quantile(0.95" in latency_query("10m")
        assert 'source="scheduled"' in latency_query("10m")

    def test_healthy(self) -> None:
        prom = FakeProm([({"cuj": c}, 1.0) for c in ALL], [({"cuj": c}, 0.2) for c in ALL])
        result = SteadyStateCheck(prom).check()
        assert result.ok
        assert result.describe() == "steady"
        assert len(prom.queries) == 2

    def test_reasons(self) -> None:
        result = evaluate(
            {"browse": 0.8, "add-to-cart": 1.0, "view-cart": 1.0},
            {"browse": 0.1, "add-to-cart": 0.9, "view-cart": math.inf},
            ALL,
            0.99,
            DEFAULT_THRESHOLDS_MS,
        )
        assert not result.ok
        text = result.describe()
        assert "browse: probe success 0.800 < 0.99" in text
        assert "add-to-cart: p95 step latency 900 ms > 500 ms" in text
        assert "view-cart: p95 step latency inf ms" in text
        assert "checkout: no scheduled probes" in text
        assert "checkout: no step latency data" in text

    def test_latency_check_can_be_off(self) -> None:
        prom = FakeProm([({"cuj": c}, 1.0) for c in ALL], [])
        assert SteadyStateCheck(prom, check_latency=False).check().ok
        assert len(prom.queries) == 1

    def test_unknown_threshold_only_fails_on_inf(self) -> None:
        assert evaluate({"x": 1.0}, {"x": 9.0}, ["x"], 0.99, {}).ok
        assert "top bucket" in evaluate({"x": 1.0}, {"x": math.inf}, ["x"], 0.99, {}).describe()

    def test_by_cuj_drops_nan_and_unlabelled(self) -> None:
        assert by_cuj([({"cuj": "a"}, math.nan), ({}, 1.0), ({"cuj": "b"}, 0.5)]) == {"b": 0.5}

    def test_thresholds(self, tmp_path: Path) -> None:
        assert load_thresholds(None) == DEFAULT_THRESHOLDS_MS
        assert load_thresholds(tmp_path / "absent.yaml") == DEFAULT_THRESHOLDS_MS
        spec = tmp_path / "slos.yaml"
        spec.write_text(
            "apiVersion: mamori/v1\nkind: SLOSpec\nwindow: 28d\njourneys:\n"
            "  - id: browse\n"
            "    availability: {target: 0.995}\n"
            "    latency: {thresholdMs: 300, target: 0.95}\n"
            "  - {id: checkout, availability: {target: 0.99}}\n"
        )
        thresholds = load_thresholds(spec)
        assert thresholds["browse"] == 300
        assert thresholds["checkout"] == 1000
