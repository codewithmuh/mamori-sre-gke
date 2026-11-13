"""The fault library (chaos/faults/*.yaml, owned by #23): the dataset and its expected answers.

Only the eval reads `expected`. The agent must never see these files (EVALS.md, "Integrity").
"""

from __future__ import annotations

import re
from collections.abc import Iterable, Mapping, Sequence
from dataclasses import dataclass
from datetime import timedelta
from pathlib import Path

import yaml

from .jsonutil import DocumentError, as_mapping, get_bool, get_opt_str, get_str, get_str_list
from .timeutil import parse_duration

FAULT_ID = re.compile(r"^F[0-9]{2}-[a-z0-9-]+$")
CATEGORIES = frozenset(
    {
        "istio-abort",
        "istio-delay",
        "pod-failure",
        "network",
        "stress",
        "bad-deploy",
        "config",
        "node",
    }
)
ACTION_TYPES = frozenset({"restart", "scale", "rollback", "remove-fault", "none"})
CUJS = ("browse", "add-to-cart", "view-cart", "checkout")
# Names the agent can give as a hypothesis' service: the shop's workloads (contract), plus
# `node` for node-level causes (#24). A root cause outside this set could never score.
AGENT_SERVICE_NAMES = frozenset(
    {
        "adservice",
        "cartservice",
        "checkoutservice",
        "currencyservice",
        "emailservice",
        "paymentservice",
        "productcatalogservice",
        "recommendationservice",
        "shippingservice",
        "redis-cart",
        "frontend",
        "loadgenerator",
        "node",
    }
)


class FaultError(ValueError):
    """A fault file is missing something the scoring needs."""


@dataclass(frozen=True)
class ExpectedAction:
    type: str
    target: str | None = None

    def describe(self) -> str:
        return self.type if not self.target else f"{self.type} {self.target}"


@dataclass(frozen=True)
class Fault:
    id: str
    title: str
    category: str
    manifest: str
    target_namespace: str
    target_kind: str
    target_name: str
    duration: timedelta
    game_day: bool
    cujs: tuple[str, ...]
    root_cause_service: str
    root_cause_summary: str
    # Other answers that count as right (#23's rootCause.acceptableServices), e.g. the service
    # a node failure took down.
    acceptable_services: tuple[str, ...]
    correct_action: ExpectedAction
    unsafe_actions: tuple[str, ...]
    path: str

    @property
    def impact_expected(self) -> bool:
        """No expected CUJs: nobody should be paged, so an investigation is a false alarm."""
        return bool(self.cujs)


def parse_fault(document: object, path: str) -> Fault:
    try:
        data = as_mapping(document, "fault")
        fault_id = get_str(data, "id")
        if not FAULT_ID.match(fault_id):
            raise FaultError(f"id {fault_id!r} does not match {FAULT_ID.pattern}")
        category = get_str(data, "category")
        if category not in CATEGORIES:
            raise FaultError(f"category {category!r} is not one of {sorted(CATEGORIES)}")
        target = as_mapping(data.get("target"), "target")
        expected = as_mapping(data.get("expected"), "expected")
        cujs = get_str_list(expected, "cujs")
        unknown = sorted(set(cujs) - set(CUJS))
        if unknown:
            raise FaultError(f"expected.cujs has unknown journeys {unknown}; known: {list(CUJS)}")
        root = as_mapping(expected.get("rootCause"), "expected.rootCause")
        service = get_str(root, "service").strip()
        if not service:
            raise FaultError("expected.rootCause.service is empty")
        acceptable = tuple(name.strip() for name in get_str_list(root, "acceptableServices"))
        for name in (service, *acceptable):
            if name.lower() not in AGENT_SERVICE_NAMES:
                raise FaultError(
                    f"root cause {name!r} is not a name the agent can give; "
                    f"use one of {sorted(AGENT_SERVICE_NAMES)}"
                )
        action = as_mapping(expected.get("correctAction"), "expected.correctAction")
        action_type = get_str(action, "type")
        if action_type not in ACTION_TYPES:
            raise FaultError(
                f"expected.correctAction.type {action_type!r} is not one of {sorted(ACTION_TYPES)}"
            )
        action_target = get_opt_str(action, "target")
        if action_type != "none" and not action_target:
            raise FaultError(f"expected.correctAction.target is required for {action_type!r}")
        unsafe = get_str_list(expected, "unsafeActions")
        for pattern in unsafe:
            if not pattern.strip():
                raise FaultError("expected.unsafeActions has an empty entry")
        return Fault(
            id=fault_id,
            title=get_str(data, "title"),
            category=category,
            manifest=get_str(data, "manifest"),
            target_namespace=get_str(target, "namespace"),
            target_kind=get_str(target, "kind"),
            target_name=get_str(target, "name"),
            duration=parse_duration(get_str(data, "duration", "10m")),
            game_day=get_bool(data, "gameDay"),
            cujs=tuple(sorted(cujs, key=CUJS.index)),
            root_cause_service=service,
            root_cause_summary=get_str(root, "summary"),
            acceptable_services=acceptable,
            correct_action=ExpectedAction(action_type, action_target),
            unsafe_actions=tuple(pattern.strip() for pattern in unsafe),
            path=path,
        )
    except (DocumentError, ValueError) as err:
        raise FaultError(f"{path}: {err}") from err


def load_fault_file(path: Path) -> Fault:
    with path.open(encoding="utf-8") as handle:
        return parse_fault(yaml.safe_load(handle), str(path))


def load_faults(directory: Path) -> dict[str, Fault]:
    """Every chaos/faults/*.yaml (top level only: manifests live in a subdirectory)."""
    if not directory.is_dir():
        raise FaultError(f"{directory}: no such directory")
    faults: dict[str, Fault] = {}
    errors: list[str] = []
    for path in sorted(directory.glob("*.yaml")):
        try:
            fault = load_fault_file(path)
        except FaultError as err:
            errors.append(str(err))
            continue
        except yaml.YAMLError as err:
            errors.append(f"{path}: not valid YAML: {err}")
            continue
        if path.stem != fault.id:
            errors.append(f"{path}: file name should be {fault.id}.yaml")
        if fault.id in faults:
            errors.append(f"{path}: duplicate id {fault.id} (also in {faults[fault.id].path})")
        faults[fault.id] = fault
    if errors:
        raise FaultError("invalid fault library:\n  " + "\n  ".join(errors))
    if not faults:
        raise FaultError(f"{directory}: no *.yaml fault files")
    return faults


def select_faults(faults: Mapping[str, Fault], selection: str) -> list[Fault]:
    """FAULTS=all, FAULTS=game-day, or a comma-separated list of ids (run in the order given)."""
    wanted = selection.strip()
    if wanted == "all":
        return [faults[key] for key in sorted(faults)]
    if wanted == "game-day":
        chosen = [faults[key] for key in sorted(faults) if faults[key].game_day]
        if not chosen:
            raise FaultError("no fault has gameDay: true")
        return chosen
    ids = [part.strip() for part in wanted.split(",") if part.strip()]
    if not ids:
        raise FaultError("no faults selected (use ids, 'game-day' or 'all')")
    missing = [fault_id for fault_id in ids if fault_id not in faults]
    if missing:
        raise FaultError(f"unknown fault ids: {', '.join(missing)}")
    if len(set(ids)) != len(ids):
        raise FaultError("a fault id is listed twice; run it twice in separate sessions instead")
    return [faults[fault_id] for fault_id in ids]


def describe_faults(faults: Iterable[Fault]) -> Sequence[str]:
    """One line per fault for `mamori-evals faults`; root causes and actions stay out of it."""
    lines = []
    for fault in faults:
        game_day = "game day" if fault.game_day else ""
        impact = ",".join(fault.cujs) if fault.cujs else "(no impact expected)"
        lines.append(f"{fault.id:<36} {fault.category:<12} {impact:<36} {game_day}".rstrip())
    return lines
