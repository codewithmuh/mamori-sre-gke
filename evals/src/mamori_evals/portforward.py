"""kubectl port-forward to the agent and executor Services for the length of a session.

Both APIs are ClusterIP only, behind NetworkPolicies; port-forward enters the pod's network
namespace and needs only `pods/portforward` RBAC (#24, #25). A forward dies when its pod is
replaced, so `ensure()` restarts any that exited.
"""

from __future__ import annotations

import socket
import subprocess
import time
from collections.abc import Callable, Sequence
from dataclasses import dataclass
from types import TracebackType


@dataclass(frozen=True)
class Forward:
    namespace: str
    service: str
    local_port: int
    remote_port: int = 8080

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self.local_port}"


def port_open(port: int, host: str = "127.0.0.1") -> bool:
    try:
        with socket.create_connection((host, port), timeout=0.5):
            return True
    except OSError:
        return False


type Popen = Callable[..., subprocess.Popen[bytes]]


class PortForwards:
    def __init__(
        self,
        forwards: Sequence[Forward],
        kubectl: Sequence[str] = ("kubectl",),
        ready_timeout: float = 30.0,
        popen: Popen = subprocess.Popen,
        ready: Callable[[int], bool] = port_open,
        sleep: Callable[[float], None] = time.sleep,
    ) -> None:
        self.forwards = list(forwards)
        self.kubectl = list(kubectl)
        self.ready_timeout = ready_timeout
        self.popen = popen
        self.ready = ready
        self.sleep = sleep
        self.processes: dict[Forward, subprocess.Popen[bytes]] = {}

    def _start(self, forward: Forward) -> None:
        argv = [
            *self.kubectl,
            "-n",
            forward.namespace,
            "port-forward",
            f"svc/{forward.service}",
            f"{forward.local_port}:{forward.remote_port}",
        ]
        self.processes[forward] = self.popen(argv, stdout=subprocess.DEVNULL)
        deadline = time.monotonic() + self.ready_timeout
        while not self.ready(forward.local_port):
            code = self.processes[forward].poll()
            if code is not None:
                raise RuntimeError(f"port-forward to svc/{forward.service} exited ({code})")
            if time.monotonic() > deadline:
                raise RuntimeError(f"port-forward to svc/{forward.service} not ready")
            self.sleep(0.2)

    def ensure(self) -> None:
        """Restart any forward whose kubectl process has exited."""
        for forward in self.forwards:
            process = self.processes.get(forward)
            if process is None or process.poll() is not None:
                self._start(forward)

    def close(self) -> None:
        for process in self.processes.values():
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
        self.processes.clear()

    def __enter__(self) -> PortForwards:
        try:
            self.ensure()
        except BaseException:
            self.close()
            raise
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        tb: TracebackType | None,
    ) -> None:
        self.close()
