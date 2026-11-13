"""A two-method HTTP interface, so every external call can be faked in tests.

The agent only ever sends GET, plus POST to four places: Pub/Sub pull and acknowledge, the
executor's proposal endpoint, Slack's chat.postMessage, and Cloud Logging's entries:list (a read
that takes its query as a POST body). There is no PUT, PATCH or DELETE anywhere in the agent.
"""

from __future__ import annotations

import json
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Any, Protocol

import requests


@dataclass(frozen=True)
class Response:
    status: int
    body: bytes

    def json(self) -> Any:
        return json.loads(self.body or b"null")

    @property
    def text(self) -> str:
        return self.body.decode("utf-8", errors="replace")


class HttpError(RuntimeError):
    """A failed call: status 0 means no HTTP answer (DNS, TLS, timeout, refused)."""

    def __init__(self, status: int, message: str) -> None:
        super().__init__(f"HTTP {status}: {message}" if status else message)
        self.status = status
        self.message = message


class Http(Protocol):
    def get(
        self,
        url: str,
        *,
        params: Mapping[str, Any] | None = None,
        headers: Mapping[str, str] | None = None,
        timeout: float,
    ) -> Response: ...

    def post(
        self,
        url: str,
        *,
        json_body: Any = None,
        headers: Mapping[str, str] | None = None,
        timeout: float,
    ) -> Response: ...


class RequestsHttp:
    """Http over a requests.Session (or google-auth's AuthorizedSession, which subclasses it)."""

    def __init__(self, session: requests.Session, verify: bool | str = True) -> None:
        self._session = session
        self._verify = verify

    def get(
        self,
        url: str,
        *,
        params: Mapping[str, Any] | None = None,
        headers: Mapping[str, str] | None = None,
        timeout: float,
    ) -> Response:
        try:
            r = self._session.get(
                url,
                params=params,
                headers=dict(headers or {}),
                timeout=timeout,
                verify=self._verify,
            )
        except requests.RequestException as exc:
            raise HttpError(0, f"{type(exc).__name__}: {exc}") from exc
        return Response(r.status_code, r.content)

    def post(
        self,
        url: str,
        *,
        json_body: Any = None,
        headers: Mapping[str, str] | None = None,
        timeout: float,
    ) -> Response:
        try:
            r = self._session.post(
                url,
                json=json_body,
                headers=dict(headers or {}),
                timeout=timeout,
                verify=self._verify,
            )
        except requests.RequestException as exc:
            raise HttpError(0, f"{type(exc).__name__}: {exc}") from exc
        return Response(r.status_code, r.content)


def checked_json(response: Response, what: str) -> Any:
    """The JSON body of a 2xx answer; HttpError with a short reason otherwise."""
    if response.status >= 400:
        detail = response.text.strip().replace("\n", " ")[:300]
        raise HttpError(response.status, f"{what}: {detail}")
    try:
        return response.json()
    except ValueError as exc:
        raise HttpError(response.status, f"{what}: answer is not JSON") from exc
