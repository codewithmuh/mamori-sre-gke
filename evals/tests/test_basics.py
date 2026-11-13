"""Time helpers, JSON helpers, record parsing, prices."""

from __future__ import annotations

import json
from datetime import UTC, date, datetime, timedelta
from pathlib import Path

import pytest

from mamori_evals.jsonutil import (
    DocumentError,
    as_mapping,
    get_bool,
    get_list,
    get_opt_float,
    get_opt_int,
    get_opt_time,
    get_str,
    get_str_list,
    read_json,
    write_json,
)
from mamori_evals.pricing import check_cost, cost_bounds, normalize_model
from mamori_evals.records import (
    parse_executor_event,
    parse_observation,
    parse_proposal,
    parse_recording,
)
from mamori_evals.timeutil import (
    SystemClock,
    format_seconds,
    format_time,
    go_duration,
    parse_duration,
    parse_time,
    seconds_between,
)

from .conftest import make_investigation


class TestTime:
    def test_parse_time_variants(self) -> None:
        want = datetime(2026, 12, 10, 5, 1, 2, tzinfo=UTC)
        assert parse_time("2026-12-10T05:01:02Z") == want
        assert parse_time("2026-12-10T14:01:02+09:00") == want
        # RFC3339Nano from the executor: extra digits are cut, not rejected.
        assert parse_time("2026-12-10T05:01:02.123456789Z").microsecond == 123456

    def test_parse_time_needs_a_zone(self) -> None:
        with pytest.raises(ValueError, match="time zone"):
            parse_time("2026-12-10T05:01:02")

    @pytest.mark.parametrize(
        ("text", "seconds"),
        [("90s", 90), ("10m", 600), ("1h30m", 5400), ("1h2m3s", 3723), ("45", 45), (12, 12)],
    )
    def test_parse_duration(self, text: str | int, seconds: int) -> None:
        assert parse_duration(text) == timedelta(seconds=seconds)

    @pytest.mark.parametrize("text", ["", "ten minutes", "5d", "-3"])
    def test_parse_duration_rejects(self, text: str) -> None:
        with pytest.raises(ValueError, match="duration"):
            parse_duration(text)

    def test_parse_duration_rejects_negative_numbers(self) -> None:
        with pytest.raises(ValueError, match="negative"):
            parse_duration(-1)

    @pytest.mark.parametrize(
        ("seconds", "text"), [(0, "0s"), (45, "45s"), (1320, "22m"), (5430, "1h30m30s")]
    )
    def test_go_duration(self, seconds: int, text: str) -> None:
        assert go_duration(timedelta(seconds=seconds)) == text

    @pytest.mark.parametrize(
        ("seconds", "text"),
        [(None, "n/a"), (44.6, "45s"), (250, "4m10s"), (3720, "1h02m"), (-90, "-1m30s")],
    )
    def test_format_seconds(self, seconds: float | None, text: str) -> None:
        assert format_seconds(seconds) == text

    def test_format_time_and_between(self) -> None:
        start = datetime(2026, 1, 1, tzinfo=UTC)
        assert format_time(start) == "2026-01-01T00:00:00Z"
        assert seconds_between(start, start + timedelta(minutes=2)) == 120
        assert seconds_between(None, start) is None

    def test_system_clock(self) -> None:
        clock = SystemClock()
        before = clock.now()
        clock.sleep(0)
        assert clock.now() >= before
        assert before.tzinfo is not None


class TestJson:
    def test_getters(self) -> None:
        data = as_mapping(
            {"s": "x", "n": 3, "f": 1.5, "b": True, "l": ["a"], "t": "2026-01-01T00:00:00Z"}, "d"
        )
        assert get_str(data, "s") == "x"
        assert get_str(data, "n") == "3"
        assert get_str(data, "missing", "dflt") == "dflt"
        assert get_opt_int(data, "f") == 1
        assert get_opt_float(data, "n") == 3.0
        assert get_bool(data, "b") is True
        assert get_bool(data, "missing", True) is True
        assert get_str_list(data, "l") == ("a",)
        assert get_list(data, "missing") == []
        assert get_opt_time(data, "t") == datetime(2026, 1, 1, tzinfo=UTC)
        assert get_opt_time(data, "missing") is None

    @pytest.mark.parametrize(
        ("call", "value"),
        [
            (get_str, ["x"]),
            (get_str, True),
            (get_opt_int, "3"),
            (get_opt_int, True),
            (get_opt_float, "1.5"),
            (get_bool, "yes"),
            (get_list, "abc"),
            (get_str_list, [1]),
            (get_opt_time, 5),
            (get_opt_time, date(2026, 1, 1)),
            (get_opt_time, "yesterday"),
        ],
    )
    def test_wrong_types_are_errors(self, call: object, value: object) -> None:
        with pytest.raises(DocumentError):
            call({"k": value}, "k")  # type: ignore[operator]

    def test_yaml_datetimes_are_accepted(self) -> None:
        moment = datetime(2026, 1, 1, 9, tzinfo=UTC)
        assert get_opt_time({"k": moment}, "k") == moment

    def test_as_mapping_rejects_lists(self) -> None:
        with pytest.raises(DocumentError, match="expected an object"):
            as_mapping([], "thing")

    def test_write_json_is_atomic_and_readable(self, tmp_path: Path) -> None:
        target = tmp_path / "deep" / "out.json"
        write_json(target, {"a": 1})
        assert read_json(target) == {"a": 1}
        assert [p.name for p in target.parent.iterdir()] == ["out.json"]

    def test_write_json_cleans_up_on_failure(self, tmp_path: Path) -> None:
        with pytest.raises(TypeError):
            write_json(tmp_path / "bad.json", {"a": object()})
        assert list(tmp_path.iterdir()) == []


class TestRecords:
    def test_investigation_defaults(self) -> None:
        inv = make_investigation(hypotheses=[{"service": "a"}, {"service": "b"}], status="")
        assert [h.rank for h in inv.hypotheses] == [1, 2]
        assert inv.finished  # no status field, but finishedAt is set

    def test_running_investigation_is_not_finished(self) -> None:
        assert not make_investigation(status="running", finishedAt=None).finished

    def test_investigation_needs_an_id(self) -> None:
        with pytest.raises(DocumentError, match="no id"):
            make_investigation(id="")

    def test_proposal_error_and_empty_action(self) -> None:
        inv = make_investigation(
            proposedAction={"type": ""},
            proposalId=None,
            proposalError={"status": 403, "error": "no", "reason": "not-allowed"},
        )
        assert inv.proposed_action is None
        assert inv.proposal_error is not None
        assert inv.proposal_error.reason == "not-allowed"

    def test_proposal_and_event(self) -> None:
        proposal = parse_proposal({"proposalId": "p", "status": "pending", "action": None})
        assert proposal.action is None
        event = parse_executor_event(
            {"time": "2026-01-01T00:00:00.5Z", "event": "blocked", "reason": "kill-switch"}
        )
        assert event.reason == "kill-switch"
        assert event.time is not None

    def test_observation_round_trip(self) -> None:
        inv = make_investigation()
        document = {
            "faultId": "F01-currency-abort",
            "injectedAt": "2000-01-01T00:00:00Z",
            "faultEnd": {"reason": "signal", "at": "2000-01-01T00:10:00Z"},
            "investigation": inv.raw,
            "proposal": {"proposalId": "p-1", "status": "pending"},
            "executorEvents": [{"event": "proposal_received", "proposalId": "p-1"}],
            "notes": ["x"],
        }
        observation = parse_observation(document)
        assert observation.status == "completed"
        again = parse_observation(json.loads(json.dumps(observation.to_dict())))
        assert again.to_dict() == observation.to_dict()

    def test_observation_needs_fault_id(self) -> None:
        with pytest.raises(DocumentError, match="faultId"):
            parse_observation({"status": "completed"})

    def test_recording_kind_and_bad_runs(self) -> None:
        with pytest.raises(DocumentError, match="not a recording"):
            parse_recording({"kind": "EvalResults"})
        with pytest.raises(DocumentError, match=r"runs\[0\]"):
            parse_recording({"kind": "EvalRecording", "runs": [{"status": "x"}]})
        assert not parse_recording({"kind": "EvalRecording"}).synthetic


class TestPricing:
    @pytest.mark.parametrize(
        "model",
        [
            "claude-opus-5-5",
            "CLAUDE-OPUS-5-5",
            "publishers/anthropic/models/claude-opus-5-5@20260101",
            "anthropic.claude-opus-5-5",
        ],
    )
    def test_normalize_model(self, model: str) -> None:
        assert normalize_model(model) == "claude-opus-5-5"

    def test_bounds(self) -> None:
        low, high = cost_bounds("claude-opus-5-5", 1_000_000, 100_000) or (0.0, 0.0)
        assert low == pytest.approx(0.20 + 2.0)
        assert high == pytest.approx((8.0 + 2.0) * 1.1)
        assert cost_bounds("gpt-x", 1, 1) is None

    def test_haiku_long_prompt_raises_the_ceiling(self) -> None:
        _, high = cost_bounds("claude-haiku-5-5", 1_000_000, 1_000_000) or (0.0, 0.0)
        assert high == pytest.approx((0.50 + 2.50) * 1.1)

    def test_check_cost(self) -> None:
        assert check_cost("claude-opus-5-5", 100_000, 5_000, 0.5).status == "ok"
        assert check_cost("claude-opus-5-5", 100_000, 5_000, 5.0).status == "out-of-range"
        assert check_cost("claude-opus-5-5", 100_000, 5_000, 0.001).status == "out-of-range"
        assert check_cost("mystery", 1, 1, 1.0).status == "unknown-model"
        assert check_cost(None, 1, 1, 1.0).status == "unknown-model"
        assert check_cost("claude-opus-5-5", None, 1, 1.0).status == "no-usage"
