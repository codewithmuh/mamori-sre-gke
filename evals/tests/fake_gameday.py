"""Stands in for #23's gameday CLI in tests. The fault id picks the behaviour."""

import json
import signal
import sys
import time


def emit(document: dict[str, object]) -> None:
    print(json.dumps(document), flush=True)


def finished(reason: str) -> None:
    emit(
        {
            "event": "finished",
            "runId": "run-1",
            "finishedAt": "2000-01-01T00:10:00Z",
            "reason": reason,
        }
    )


def main(argv: list[str]) -> int:
    command = argv[0]
    if command == "stop":
        return 1 if argv[1] == "F07-stuck" else 0
    fault = argv[1]
    if fault == "F02-refused":
        print("another fault is applied", flush=True)
        return 2
    if fault == "F03-manual":
        emit(
            {
                "event": "manual",
                "faultId": fault,
                "inject": ["drain the node"],
                "revert": ["uncordon it"],
            }
        )
        return 3
    if fault == "F04-crash":
        return 1
    if fault == "F06-silent":
        time.sleep(30)
        return 0
    print("applying objects", flush=True)

    def on_term(signum: int, frame: object) -> None:
        if fault == "F08-stubborn":
            return
        finished("signal")
        sys.exit(0)

    signal.signal(signal.SIGTERM, on_term)
    emit(
        {
            "event": "started",
            "runId": "run-1",
            "faultId": fault,
            "startedAt": "2000-01-01T00:00:00Z",
            "endsAt": "2000-01-01T00:22:00Z",
            "objects": ["virtualservice/f01-currency-abort"],
            "timeline": "gameday-runs/run-1.json",
        }
    )
    if fault == "F05-quick":
        time.sleep(0.2)
        finished("removed-externally")
        return 0
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        time.sleep(0.05)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
