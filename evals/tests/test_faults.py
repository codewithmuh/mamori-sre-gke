from __future__ import annotations

from pathlib import Path
from typing import Any

import pytest

from mamori_evals.faults import (
    FaultError,
    describe_faults,
    load_fault_file,
    load_faults,
    select_faults,
)

from .conftest import FIXTURES, make_fault


def test_fixture_library_loads(synthetic_faults: dict[str, Any]) -> None:
    assert len(synthetic_faults) == 8
    fault = synthetic_faults["F91-synthetic-currency-abort"]
    assert fault.cujs == ("browse", "view-cart", "checkout")
    assert fault.correct_action.describe() == (
        "remove-fault virtualservice/f91-synthetic-currency-abort"
    )
    assert not synthetic_faults["F96-synthetic-recommendation-cpu-stress"].impact_expected
    node = synthetic_faults["F95-synthetic-node-preemption"]
    assert node.root_cause_service == "node"
    assert node.acceptable_services == ("frontend",)


def test_cujs_are_put_in_journey_order() -> None:
    fault = make_fault(expected={"cujs": ["checkout", "browse"]})
    assert fault.cujs == ("browse", "checkout")


@pytest.mark.parametrize(
    ("overrides", "message"),
    [
        ({"id": "F1-short"}, "does not match"),
        ({"category": "meteor"}, "category"),
        ({"expected": {"cujs": ["search"]}}, "unknown journeys"),
        ({"expected": {"rootCause": {"service": " "}}}, "service is empty"),
        ({"expected": {"rootCause": {"service": "gke-node"}}}, "not a name the agent can give"),
        (
            {"expected": {"rootCause": {"service": "cartservice", "acceptableServices": ["db"]}}},
            "'db' is not a name",
        ),
        ({"expected": {"correctAction": {"type": "delete"}}}, "correctAction.type"),
        ({"expected": {"correctAction": {"type": "restart"}}}, "target is required"),
        ({"expected": {"unsafeActions": [" "]}}, "empty entry"),
        ({"duration": "soon"}, "duration"),
        ({"target": "currencyservice"}, "target"),
    ],
)
def test_invalid_faults(overrides: dict[str, Any], message: str) -> None:
    with pytest.raises(FaultError, match=message):
        make_fault(**overrides)


def test_none_action_needs_no_target() -> None:
    fault = make_fault(expected={"correctAction": {"type": "none"}})
    assert fault.correct_action.describe() == "none"


def test_load_faults_reports_every_problem(tmp_path: Path) -> None:
    good = (FIXTURES / "faults" / "F91-synthetic-currency-abort.yaml").read_text()
    (tmp_path / "F91-synthetic-currency-abort.yaml").write_text(good)
    (tmp_path / "F01-wrong-name.yaml").write_text(good)
    (tmp_path / "F02-broken.yaml").write_text("id: [unclosed")
    (tmp_path / "F03-bad.yaml").write_text("id: nope\n")
    with pytest.raises(FaultError) as err:
        load_faults(tmp_path)
    text = str(err.value)
    assert "file name should be" in text
    assert "duplicate id" in text
    assert "not valid YAML" in text
    assert "F03-bad.yaml" in text


def test_load_faults_needs_files(tmp_path: Path) -> None:
    with pytest.raises(FaultError, match="no such directory"):
        load_faults(tmp_path / "missing")
    with pytest.raises(FaultError, match=r"no \*\.yaml"):
        load_faults(tmp_path)


def test_load_fault_file() -> None:
    fault = load_fault_file(FIXTURES / "faults" / "F93-synthetic-redis-cart-pod-kill.yaml")
    assert fault.correct_action.type == "none"


def test_select(synthetic_faults: dict[str, Any]) -> None:
    assert len(select_faults(synthetic_faults, "all")) == 8
    assert len(select_faults(synthetic_faults, "game-day")) == 5
    chosen = select_faults(
        synthetic_faults, "F93-synthetic-redis-cart-pod-kill, F91-synthetic-currency-abort"
    )
    assert [f.id for f in chosen] == [
        "F93-synthetic-redis-cart-pod-kill",
        "F91-synthetic-currency-abort",
    ]


@pytest.mark.parametrize(
    ("selection", "message"),
    [
        ("", "no faults selected"),
        ("F00-nope", "unknown fault ids"),
        ("F91-synthetic-currency-abort,F91-synthetic-currency-abort", "listed twice"),
    ],
)
def test_select_errors(synthetic_faults: dict[str, Any], selection: str, message: str) -> None:
    with pytest.raises(FaultError, match=message):
        select_faults(synthetic_faults, selection)


def test_select_game_day_needs_one() -> None:
    fault = make_fault(gameDay=False)
    with pytest.raises(FaultError, match="gameDay"):
        select_faults({fault.id: fault}, "game-day")


def test_describe_hides_the_answers(synthetic_faults: dict[str, Any]) -> None:
    text = "\n".join(describe_faults(synthetic_faults.values()))
    assert "F96-synthetic-recommendation-cpu-stress" in text
    assert "no impact expected" in text
    assert "currencyservice" not in text
    assert "remove-fault" not in text
