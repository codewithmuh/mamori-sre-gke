"""Typed reads from loosely typed JSON and YAML documents.

The agent, the executor and the fault files are written by other components; these helpers
read them tolerantly (missing optional fields are fine) but never guess a wrong type.
"""

from __future__ import annotations

import json
import os
import tempfile
from collections.abc import Mapping, Sequence
from datetime import date, datetime
from pathlib import Path

from .timeutil import format_time, parse_time

type JSONDict = dict[str, object]


class DocumentError(ValueError):
    """A document does not have the shape the eval needs."""


def as_mapping(value: object, what: str) -> Mapping[str, object]:
    if not isinstance(value, Mapping):
        raise DocumentError(f"{what}: expected an object, got {type(value).__name__}")
    return {str(key): item for key, item in value.items()}


def get_str(data: Mapping[str, object], key: str, default: str = "") -> str:
    value = data.get(key)
    if value is None:
        return default
    if isinstance(value, bool) or not isinstance(value, str | int | float):
        raise DocumentError(f"{key}: expected a string, got {type(value).__name__}")
    return str(value)


def get_opt_str(data: Mapping[str, object], key: str) -> str | None:
    value = get_str(data, key)
    return value or None


def get_opt_int(data: Mapping[str, object], key: str) -> int | None:
    value = data.get(key)
    if value is None:
        return None
    if isinstance(value, bool) or not isinstance(value, int | float):
        raise DocumentError(f"{key}: expected a number, got {type(value).__name__}")
    return int(value)


def get_opt_float(data: Mapping[str, object], key: str) -> float | None:
    value = data.get(key)
    if value is None:
        return None
    if isinstance(value, bool) or not isinstance(value, int | float):
        raise DocumentError(f"{key}: expected a number, got {type(value).__name__}")
    return float(value)


def get_bool(data: Mapping[str, object], key: str, default: bool = False) -> bool:
    value = data.get(key)
    if value is None:
        return default
    if not isinstance(value, bool):
        raise DocumentError(f"{key}: expected true or false, got {value!r}")
    return value


def get_list(data: Mapping[str, object], key: str) -> list[object]:
    value = data.get(key)
    if value is None:
        return []
    if isinstance(value, str) or not isinstance(value, Sequence):
        raise DocumentError(f"{key}: expected a list, got {type(value).__name__}")
    return list(value)


def get_str_list(data: Mapping[str, object], key: str) -> tuple[str, ...]:
    items = get_list(data, key)
    for item in items:
        if not isinstance(item, str):
            raise DocumentError(f"{key}: expected a list of strings, got {item!r}")
    return tuple(str(item) for item in items)


def get_opt_mapping(data: Mapping[str, object], key: str) -> Mapping[str, object] | None:
    value = data.get(key)
    if value is None:
        return None
    return as_mapping(value, key)


def get_opt_time(data: Mapping[str, object], key: str) -> datetime | None:
    """RFC3339 string (or a datetime that YAML already parsed) to an aware UTC datetime."""
    value = data.get(key)
    if value is None or value == "":
        return None
    if isinstance(value, datetime):
        return parse_time(value.isoformat())
    if isinstance(value, date):
        raise DocumentError(f"{key}: expected a timestamp, got a date ({value})")
    if not isinstance(value, str):
        raise DocumentError(f"{key}: expected an RFC3339 timestamp, got {value!r}")
    try:
        return parse_time(value)
    except ValueError as err:
        raise DocumentError(f"{key}: {err}") from err


def opt_time_str(moment: datetime | None) -> str | None:
    return format_time(moment) if moment is not None else None


def read_json(path: Path) -> object:
    with path.open(encoding="utf-8") as handle:
        return json.load(handle)


def write_json(path: Path, document: object) -> None:
    """Write atomically, so a crash mid-run never leaves half a results file."""
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            json.dump(document, handle, indent=2, sort_keys=False)
            handle.write("\n")
        os.replace(tmp, path)
    except BaseException:
        Path(tmp).unlink(missing_ok=True)
        raise
