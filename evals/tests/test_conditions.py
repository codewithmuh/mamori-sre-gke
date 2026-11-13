from __future__ import annotations

import pytest

from mamori_evals.conditions import session_conditions
from mamori_evals.jsonutil import DocumentError

ENV = {
    "AGENT_POSTMORTEMS_GAMEDAYS": "exclude",
    "AGENT_HIDE_FAULT_LABELS": "false",
    "AGENT_DAILY_COST_LIMIT_USD": "40",
    "AGENT_SUBSCRIPTION": "mamori-agent-incidents",
}


def test_labels_come_from_the_deployment() -> None:
    labels = session_conditions({"APPROVALS": "none"}, ENV)
    assert labels == {
        "APPROVALS": "none",
        "AGENT_POSTMORTEMS_GAMEDAYS": "exclude",
        "AGENT_HIDE_FAULT_LABELS": "false",
        "AGENT_DAILY_COST_LIMIT_USD": "40",
    }


def test_typed_labels_must_agree() -> None:
    assert session_conditions({"AGENT_HIDE_FAULT_LABELS": "False"}, ENV)
    with pytest.raises(DocumentError, match="you typed include, the agent Deployment has exclude"):
        session_conditions({"AGENT_POSTMORTEMS_GAMEDAYS": "include"}, ENV)
    with pytest.raises(DocumentError, match="AGENT_CHAOS_TOOL: not set on the agent Deployment"):
        session_conditions({"AGENT_CHAOS_TOOL": "true"}, ENV)


def test_the_running_agent_must_match_its_deployment() -> None:
    live = {"postmortemsGamedays": "exclude", "dailyCostLimitUsd": 40.0, "hideFaultLabels": False}
    assert session_conditions({}, ENV, live)
    with pytest.raises(DocumentError, match="not rolled out yet"):
        session_conditions({}, ENV, {**live, "postmortemsGamedays": "include"})
