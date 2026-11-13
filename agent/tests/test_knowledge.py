from __future__ import annotations

from pathlib import Path

import pytest

from conftest import FakeHttp, ok, text
from mamori_agent.http import HttpError
from mamori_agent.tools.knowledge import (
    ForbiddenPath,
    GitHubKnowledge,
    KnowledgeReads,
    LocalKnowledge,
    check_path,
    parse_postmortem,
)

PM_REAL = """---
id: "2026-12-01-redis-oom"
date: "2026-12-01"
faultId: null
services: [redis-cart, cartservice]
rootCause: "redis-cart ran out of memory"
severity: "SEV2"
cujs: [add-to-cart]
---
# Postmortem
Cart writes failed while redis-cart restarted.
"""

PM_GAMEDAY = """---
id: "2026-12-05-currency-abort"
date: "2026-12-05"
faultId: "F01-currency-abort"
services: [currencyservice]
rootCause: "Istio abort on currencyservice"
severity: "SEV1"
---
Game day one.
"""


@pytest.mark.parametrize(
    "path",
    [
        "chaos/faults/F01-currency-abort.yaml",
        "docs/incidents/game-days.md",
        "docs/incidents/postmortems/README.md",
        "docs/incidents/postmortems/../game-days.md",
        "docs/incidents/postmortem-template.md",
        "slo/../chaos/faults/x.yaml",
        "/etc/passwd",
    ],
)
def test_forbidden_paths(path: str) -> None:
    with pytest.raises(ForbiddenPath):
        check_path(path)


def test_allowed_paths() -> None:
    assert check_path("slo/cuj-map.yaml") == "slo/cuj-map.yaml"
    assert check_path("docs/incidents/postmortems/2026-12-01-redis-oom.md")


def _repo(tmp_path: Path) -> Path:
    (tmp_path / "slo").mkdir()
    (tmp_path / "slo" / "cuj-map.yaml").write_text("journeys: []\n")
    pm = tmp_path / "docs" / "incidents" / "postmortems"
    pm.mkdir(parents=True)
    (pm / "2026-12-01-redis-oom.md").write_text(PM_REAL)
    (pm / "2026-12-05-currency-abort.md").write_text(PM_GAMEDAY)
    (pm / "README.md").write_text("index")
    (tmp_path / "chaos" / "faults").mkdir(parents=True)
    (tmp_path / "chaos" / "faults" / "F01.yaml").write_text("answer")
    return tmp_path


def test_local_knowledge(tmp_path: Path) -> None:
    src = LocalKnowledge(str(_repo(tmp_path)))
    assert src.read("slo/cuj-map.yaml") == "journeys: []\n"
    assert src.read("slo/slos.yaml") is None
    assert src.list_postmortems() == ["2026-12-01-redis-oom.md", "2026-12-05-currency-abort.md"]
    with pytest.raises(ForbiddenPath):
        src.read("chaos/faults/F01.yaml")
    assert LocalKnowledge(str(tmp_path / "nowhere")).list_postmortems() == []


def test_parse_postmortem() -> None:
    pm = parse_postmortem("2026-12-01-redis-oom.md", PM_REAL)
    assert pm.id == "2026-12-01-redis-oom"
    assert pm.fault_id is None
    assert pm.services == ["redis-cart", "cartservice"]
    assert pm.body.startswith("# Postmortem")
    plain = parse_postmortem("2026-01-01-x.md", "no front matter")
    assert (plain.id, plain.body, plain.meta) == ("2026-01-01-x", "no front matter", {})
    broken = parse_postmortem("2026-01-01-y.md", "---\n: [\n---\nbody")
    assert broken.meta == {} and broken.body == "body"


def test_past_incidents_search_read_and_gameday_filters(tmp_path: Path) -> None:
    src = LocalKnowledge(str(_repo(tmp_path)))
    reads = KnowledgeReads(src, include_gamedays=True)
    out = reads.past_incidents("currencyservice errors", "", [])
    assert out.splitlines()[0].startswith("2026-12-05-currency-abort: severity=SEV1")
    full = reads.past_incidents("", "2026-12-01-redis-oom", [])
    assert "Cart writes failed" in full and "rootCause: redis-cart ran out of memory" in full
    assert reads.past_incidents("", "nope", []) == "no postmortem with id nope"
    # Unknown terms: fall back to the newest few.
    assert "2026-12-05-currency-abort" in reads.past_incidents("zzzz", "", [])
    # Per-run exclusion of a fault id, and the global game-day switch.
    excluded = reads.past_incidents("currencyservice", "", ["F01-currency-abort"])
    assert "currency-abort" not in excluded
    no_gamedays = KnowledgeReads(src, include_gamedays=False).past_incidents("", "", [])
    assert "currency-abort" not in no_gamedays and "redis-oom" in no_gamedays
    empty = KnowledgeReads(LocalKnowledge(str(tmp_path / "x")), True)
    assert empty.past_incidents("", "", []) == "no postmortems yet"
    assert "not found" in empty.cuj_map() and "not found" in empty.slo_spec()
    assert reads.cuj_map() == "journeys: []\n"


def test_github_knowledge_reads_and_caches() -> None:
    listing = [
        {"name": "2026-12-01-redis-oom.md"},
        {"name": "README.md"},
        {"name": "../../chaos"},
        "junk",
    ]
    http = FakeHttp(
        {
            "api.github.com": ok(listing),
            "postmortems/2026-12-01-redis-oom.md": text(PM_REAL),
            "slo/slos.yaml": text("missing", 404),
            "slo/cuj-map.yaml": text("journeys: []"),
        }
    )
    gh = GitHubKnowledge(http, "codewithmuh/mamori-sre-gke", "main", 5)
    assert gh.list_postmortems() == ["2026-12-01-redis-oom.md"]
    assert gh.list_postmortems() == ["2026-12-01-redis-oom.md"]
    assert gh.read("slo/cuj-map.yaml") == "journeys: []"
    assert gh.read("slo/cuj-map.yaml") == "journeys: []"
    assert gh.read("slo/slos.yaml") is None
    assert gh.read("slo/slos.yaml") is None
    urls = [c["url"] for c in http.calls]
    assert (
        urls.count(
            "https://raw.githubusercontent.com/codewithmuh/mamori-sre-gke/main/slo/cuj-map.yaml"
        )
        == 1
    )
    assert sum("api.github.com" in u for u in urls) == 1
    with pytest.raises(ForbiddenPath):
        gh.read("chaos/faults/F01.yaml")
    assert not any("chaos" in u for u in urls)


def test_github_knowledge_errors() -> None:
    gh = GitHubKnowledge(
        FakeHttp(
            {
                "raw.githubusercontent.com": text("rate limited", 429),
                "api.github.com": text("x", 404),
            }
        ),
        "o/r",
        "main",
        5,
    )
    with pytest.raises(HttpError):
        gh.read("slo/cuj-map.yaml")
    assert gh.list_postmortems() == []
