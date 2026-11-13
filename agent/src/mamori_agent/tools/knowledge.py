"""Read tools on the repository's knowledge files: the CUJ map, the SLO spec, past postmortems.

Only an allow-list of paths can be read, from the public repository over HTTPS (in the cluster)
or from a local checkout (laptop runs). Nothing under chaos/ is reachable: chaos/faults/ holds
the expected answers of the evaluation (contract, "Evaluation integrity"), and neither the image
nor any mount contains it. docs/incidents/ outside postmortems/ (game-day plans name their
target services) is refused too.
"""

from __future__ import annotations

import re
import threading
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Protocol

import yaml

from mamori_agent.http import Http, HttpError, checked_json

CUJ_MAP = "slo/cuj-map.yaml"
SLO_SPEC = "slo/slos.yaml"
POSTMORTEM_DIR = "docs/incidents/postmortems"
_ALLOWED_FILES = frozenset({CUJ_MAP, SLO_SPEC})
_POSTMORTEM_FILE = re.compile(r"^\d{4}-\d{2}-\d{2}-[a-z0-9][a-z0-9-]{0,80}\.md$")
_CACHE_SECONDS = 600


class ForbiddenPath(PermissionError):
    pass


def check_path(path: str) -> str:
    """Return the path if it's on the allow-list; raise ForbiddenPath otherwise."""
    if path in _ALLOWED_FILES:
        return path
    directory, _, name = path.rpartition("/")
    if directory == POSTMORTEM_DIR and _POSTMORTEM_FILE.match(name):
        return path
    raise ForbiddenPath(f"not readable by the agent: {path}")


class KnowledgeSource(Protocol):
    def read(self, path: str) -> str | None: ...
    def list_postmortems(self) -> list[str]: ...


class LocalKnowledge:
    """A local checkout of the repository (AGENT_KNOWLEDGE_SOURCE=local)."""

    def __init__(self, repo_dir: str) -> None:
        self._root = Path(repo_dir).resolve()

    def read(self, path: str) -> str | None:
        target = (self._root / check_path(path)).resolve()
        if not target.is_relative_to(self._root) or not target.is_file():
            return None
        return target.read_text(encoding="utf-8")

    def list_postmortems(self) -> list[str]:
        directory = self._root / POSTMORTEM_DIR
        if not directory.is_dir():
            return []
        return sorted(p.name for p in directory.iterdir() if _POSTMORTEM_FILE.match(p.name))


class GitHubKnowledge:
    """The public repository on GitHub, cached for ten minutes per file.

    raw.githubusercontent.com serves file content; the contents API lists the postmortem
    directory (60 unauthenticated requests per hour per IP is plenty with the cache).
    """

    def __init__(self, http: Http, repo: str, ref: str, timeout: float) -> None:
        self._http = http
        self._raw = f"https://raw.githubusercontent.com/{repo}/{ref}"
        self._api = f"https://api.github.com/repos/{repo}/contents"
        self._ref = ref
        self._timeout = timeout
        self._cache: dict[str, tuple[float, Any]] = {}
        self._lock = threading.Lock()

    def _cached(self, key: str) -> Any:
        with self._lock:
            hit = self._cache.get(key)
        if hit and time.monotonic() - hit[0] < _CACHE_SECONDS:
            return hit[1]
        return None

    def _store(self, key: str, value: Any) -> None:
        with self._lock:
            self._cache[key] = (time.monotonic(), value)

    def read(self, path: str) -> str | None:
        check_path(path)
        cached = self._cached(path)
        if cached is not None:
            return str(cached) if cached != "" else None
        response = self._http.get(f"{self._raw}/{path}", timeout=self._timeout)
        if response.status == 404:
            self._store(path, "")
            return None
        if response.status >= 400:
            raise HttpError(response.status, f"github raw {path}")
        self._store(path, response.text)
        return response.text

    def list_postmortems(self) -> list[str]:
        cached = self._cached("__list__")
        if cached is not None:
            return list(cached)
        response = self._http.get(
            f"{self._api}/{POSTMORTEM_DIR}",
            params={"ref": self._ref},
            headers={"Accept": "application/vnd.github+json"},
            timeout=self._timeout,
        )
        if response.status == 404:
            names: list[str] = []
        else:
            entries = checked_json(response, "github contents")
            names = sorted(
                str(e.get("name"))
                for e in entries
                if isinstance(e, dict) and _POSTMORTEM_FILE.match(str(e.get("name", "")))
            )
        self._store("__list__", names)
        return names


@dataclass
class Postmortem:
    id: str
    meta: dict[str, Any]
    body: str
    fault_id: str | None = None
    services: list[str] = field(default_factory=list)


def parse_postmortem(name: str, text: str) -> Postmortem:
    meta: dict[str, Any] = {}
    body = text
    end = text.find("\n---", 3) if text.startswith("---") else -1
    if end != -1:
        try:
            loaded = yaml.safe_load(text[3:end])
        except yaml.YAMLError:
            loaded = None
        if isinstance(loaded, dict):
            meta = loaded
        # Everything after the closing "---" line.
        body = text[end + 4 :].partition("\n")[2]
    services = meta.get("services") or []
    fault = meta.get("faultId")
    return Postmortem(
        id=str(meta.get("id") or name.removesuffix(".md")),
        meta=meta,
        body=body.strip(),
        fault_id=str(fault) if fault else None,
        services=[str(s) for s in services] if isinstance(services, list) else [],
    )


class KnowledgeReads:
    def __init__(self, source: KnowledgeSource, include_gamedays: bool) -> None:
        self._source = source
        self._include_gamedays = include_gamedays

    def cuj_map(self) -> str:
        text = self._source.read(CUJ_MAP)
        return text or f"{CUJ_MAP} not found (the CUJ discovery run has not been committed yet)"

    def slo_spec(self) -> str:
        text = self._source.read(SLO_SPEC)
        return text or f"{SLO_SPEC} not found"

    def _postmortems(self, exclude_fault_ids: list[str]) -> list[Postmortem]:
        found = []
        for name in self._source.list_postmortems():
            text = self._source.read(f"{POSTMORTEM_DIR}/{name}")
            if not text:
                continue
            pm = parse_postmortem(name, text)
            if pm.fault_id and (not self._include_gamedays or pm.fault_id in exclude_fault_ids):
                continue
            found.append(pm)
        return found

    def past_incidents(self, query: str, postmortem_id: str, exclude_fault_ids: list[str]) -> str:
        postmortems = self._postmortems(exclude_fault_ids)
        if postmortem_id:
            for pm in postmortems:
                if pm.id == postmortem_id:
                    return (
                        f"postmortem {pm.id}\n{yaml.safe_dump(pm.meta, sort_keys=True)}\n{pm.body}"
                    )
            return f"no postmortem with id {postmortem_id}"
        if not postmortems:
            return "no postmortems yet"
        terms = [t for t in re.split(r"[^a-z0-9-]+", query.lower()) if len(t) > 2]

        def score(pm: Postmortem) -> int:
            haystack = " ".join(
                [
                    " ".join(pm.services),
                    str(pm.meta.get("rootCause", "")),
                    " ".join(str(c) for c in pm.meta.get("cujs") or []),
                    pm.body[:4000],
                ]
            ).lower()
            return sum(haystack.count(t) for t in terms)

        ranked = sorted(postmortems, key=lambda p: (score(p), p.id), reverse=True)
        if terms:
            ranked = [p for p in ranked if score(p) > 0] or ranked[:3]
        lines = []
        for pm in ranked[:8]:
            lines.append(
                f"{pm.id}: severity={pm.meta.get('severity', '?')} "
                f"services={','.join(pm.services) or '-'} "
                f"cujs={','.join(str(c) for c in pm.meta.get('cujs') or []) or '-'} "
                f"rootCause={str(pm.meta.get('rootCause', '?'))[:240]}"
            )
        lines.append("(read one in full with past_incidents id=<id>)")
        return "\n".join(lines)
