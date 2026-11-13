"""Time helpers: RFC3339 parsing, durations, and a clock the live runner can fake in tests."""

from __future__ import annotations

import re
import time
from datetime import UTC, datetime, timedelta
from typing import Protocol

# fromisoformat keeps at most microseconds; the executor logs RFC3339Nano.
_EXTRA_DIGITS = re.compile(r"(\.\d{6})\d+")
_DURATION = re.compile(r"^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$")


def parse_time(value: str) -> datetime:
    """Parse an RFC3339 timestamp into an aware UTC datetime."""
    text = value.strip()
    if text[-1:] in ("Z", "z"):
        text = text[:-1] + "+00:00"
    text = _EXTRA_DIGITS.sub(r"\1", text)
    parsed = datetime.fromisoformat(text)
    if parsed.tzinfo is None:
        raise ValueError(f"timestamp has no time zone: {value!r}")
    return parsed.astimezone(UTC)


def format_time(moment: datetime) -> str:
    """RFC3339 in UTC with second precision, the format the agent uses."""
    return moment.astimezone(UTC).isoformat(timespec="seconds").replace("+00:00", "Z")


def parse_duration(value: str | float) -> timedelta:
    """Parse "90s", "10m", "1h30m" or a plain number of seconds."""
    if isinstance(value, int | float):
        if value < 0:
            raise ValueError(f"negative duration: {value}")
        return timedelta(seconds=value)
    text = value.strip()
    if text.isdigit():
        return timedelta(seconds=int(text))
    match = _DURATION.match(text)
    if not text or match is None:
        raise ValueError(f"not a duration (use e.g. 90s, 10m, 1h30m): {value!r}")
    hours, minutes, seconds = (int(part) if part else 0 for part in match.groups())
    return timedelta(hours=hours, minutes=minutes, seconds=seconds)


def go_duration(delta: timedelta) -> str:
    """Render a duration the way Go's time.ParseDuration reads it (the gameday CLI is Go)."""
    total = int(delta.total_seconds())
    hours, rest = divmod(total, 3600)
    minutes, seconds = divmod(rest, 60)
    parts = []
    if hours:
        parts.append(f"{hours}h")
    if minutes:
        parts.append(f"{minutes}m")
    if seconds or not parts:
        parts.append(f"{seconds}s")
    return "".join(parts)


def format_seconds(seconds: float | None) -> str:
    """Short human form for reports: 45s, 4m10s, 1h02m."""
    if seconds is None:
        return "n/a"
    total = round(seconds)
    sign = "-" if total < 0 else ""
    total = abs(total)
    hours, rest = divmod(total, 3600)
    minutes, secs = divmod(rest, 60)
    if hours:
        return f"{sign}{hours}h{minutes:02d}m"
    if minutes:
        return f"{sign}{minutes}m{secs:02d}s"
    return f"{sign}{secs}s"


def seconds_between(start: datetime | None, end: datetime | None) -> float | None:
    if start is None or end is None:
        return None
    return (end - start).total_seconds()


class Clock(Protocol):
    def now(self) -> datetime: ...

    def sleep(self, seconds: float) -> None: ...


class SystemClock:
    def now(self) -> datetime:
        return datetime.now(UTC)

    def sleep(self, seconds: float) -> None:
        if seconds > 0:
            time.sleep(seconds)
