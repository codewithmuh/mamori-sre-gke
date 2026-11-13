"""Build the agent from Settings: credentials, clients, tools, investigator, service."""

from __future__ import annotations

import logging
import os
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Any

import requests

from .config import Settings
from .executor import ExecutorClient
from .http import Http, HttpError, RequestsHttp, Response
from .investigator import Investigator
from .llm import make_caller
from .logs import event
from .service import AgentService, ExclusionHolder
from .slack import SlackNotifier
from .tools import ToolRegistry, build_tools
from .tools.gcp import GoogleReads
from .tools.knowledge import GitHubKnowledge, KnowledgeReads, KnowledgeSource, LocalKnowledge
from .tools.kube import KubeClient, KubeReads

log = logging.getLogger(__name__)

CLOUD_PLATFORM = "https://www.googleapis.com/auth/cloud-platform"


class NoCredentials:
    """Stands in for Google APIs when ADC is missing (a laptop without gcloud login)."""

    def __init__(self, reason: str) -> None:
        self._reason = reason

    def get(self, url: str, **_: Any) -> Response:
        raise HttpError(0, f"no Google credentials: {self._reason}")

    def post(self, url: str, **_: Any) -> Response:
        raise HttpError(0, f"no Google credentials: {self._reason}")


def google_http(settings: Settings) -> tuple[Http, str]:
    """An authorised session for Google REST APIs and the project ID (settings, then ADC)."""
    import google.auth  # noqa: PLC0415
    from google.auth.exceptions import DefaultCredentialsError  # noqa: PLC0415
    from google.auth.transport.requests import AuthorizedSession  # noqa: PLC0415

    try:
        credentials, adc_project = google.auth.default(scopes=[CLOUD_PLATFORM])
    except DefaultCredentialsError as exc:
        event(log, "google_credentials_missing", logging.WARNING, error=str(exc))
        return NoCredentials(str(exc)), settings.resolved_project_id()
    return (
        # google-auth leaves this constructor unannotated.
        RequestsHttp(AuthorizedSession(credentials)),  # type: ignore[no-untyped-call]
        settings.resolved_project_id() or str(adc_project or ""),
    )


def kube_http(settings: Settings) -> Http:
    verify: bool | str = True
    if settings.k8s_api_url.startswith("https://") and os.path.exists(settings.k8s_ca_file):
        verify = settings.k8s_ca_file
    return RequestsHttp(requests.Session(), verify=verify)


@dataclass
class Agent:
    settings: Settings
    project_id: str
    google: Http
    service: AgentService


def build(settings: Settings, overrides: Mapping[str, Any] | None = None) -> Agent:
    overrides = overrides or {}
    google, project = overrides.get("google") or google_http(settings)
    plain: Http = overrides.get("http") or RequestsHttp(requests.Session())
    timeout = settings.tool_timeout_seconds

    knowledge: KnowledgeSource
    if settings.knowledge_source == "local":
        knowledge = LocalKnowledge(settings.repo_dir)
    else:
        knowledge = GitHubKnowledge(plain, settings.github_repo, settings.github_ref, timeout)
    kube = KubeClient(
        overrides.get("kube_http") or kube_http(settings),
        settings.k8s_api_url,
        settings.k8s_token_file,
        timeout,
    )
    exclusions = ExclusionHolder()
    tools = build_tools(
        GoogleReads(google, project, timeout),
        KubeReads(kube, settings.log_namespace_list, settings.hide_fault_labels),
        KnowledgeReads(knowledge, include_gamedays=settings.postmortems_gamedays == "include"),
        exclusions.get,
        settings.chaos_tool,
    )
    registry = ToolRegistry(tools, settings.tool_output_max_chars)
    caller = overrides.get("caller") or make_caller(settings, project)
    executor = (
        ExecutorClient(plain, settings.executor_url, settings.executor_token_file)
        if settings.propose
        else None
    )
    slack = (
        SlackNotifier(plain, settings.slack_bot_token.get_secret_value(), settings.slack_channel)
        if settings.slack
        else None
    )
    service = AgentService(
        settings,
        investigator=Investigator(settings, caller, registry),
        executor=executor,
        slack=slack,
        exclusions=exclusions,
    )
    event(
        log,
        "agent_configured",
        provider=settings.provider,
        model=settings.model,
        fallback_model=settings.fallback_model,
        effort=settings.effort,
        project_id=project,
        tools=registry.names,
        propose=executor is not None,
        slack=bool(slack and slack.enabled),
        knowledge=settings.knowledge_source,
        hide_fault_labels=settings.hide_fault_labels,
        postmortems_gamedays=settings.postmortems_gamedays,
    )
    return Agent(settings, project, google, service)
