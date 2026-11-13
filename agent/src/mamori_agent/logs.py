"""JSON lines on stdout. Cloud Logging reads `severity` and `message` from each object."""

from __future__ import annotations

import json
import logging
import sys
from typing import Any

from .models import rfc3339, utc_now

_RESERVED = set(vars(logging.makeLogRecord({})))


class JsonFormatter(logging.Formatter):
    def format(self, record: logging.LogRecord) -> str:
        line: dict[str, Any] = {
            "time": rfc3339(utc_now()),
            "severity": record.levelname,
            "message": record.getMessage(),
            "logger": record.name,
        }
        for key, value in vars(record).items():
            if key not in _RESERVED and not key.startswith("_"):
                line[key] = value
        if record.exc_info and record.exc_info[0] is not None:
            line["exception"] = f"{record.exc_info[0].__name__}: {record.exc_info[1]}"
        return json.dumps(line, default=str, ensure_ascii=False)


def setup(level: str = "INFO") -> None:
    handler = logging.StreamHandler(sys.stdout)
    handler.setFormatter(JsonFormatter())
    root = logging.getLogger()
    root.handlers[:] = [handler]
    root.setLevel(level.upper())
    # Library chatter (connection pools, retries) stays out of the incident log.
    for noisy in ("urllib3", "httpx2", "httpcore2", "anthropic", "google"):
        logging.getLogger(noisy).setLevel(logging.WARNING)


def event(logger: logging.Logger, name: str, level: int = logging.INFO, **fields: Any) -> None:
    """One structured line with `event=<name>` and the given fields at the top level."""
    logger.log(level, name.replace("_", " "), extra={"event": name, **fields})
