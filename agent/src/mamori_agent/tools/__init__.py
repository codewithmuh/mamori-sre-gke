"""The read-only tools the model may call, with their input schemas and output caps.

Every tool: validates its input with pydantic (the model's JSON is untrusted too), runs one or a
few GETs with a timeout, redacts secrets, caps the text, and wraps it as untrusted data.
"""

from __future__ import annotations

import logging
from collections.abc import Callable
from dataclasses import dataclass
from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field, ValidationError

from mamori_agent import metrics
from mamori_agent.http import HttpError
from mamori_agent.safety import cap, redact, wrap_untrusted

from .gcp import GoogleReads
from .knowledge import ForbiddenPath, KnowledgeReads
from .kube import KubeReads

log = logging.getLogger(__name__)

NAMESPACE_PATTERN = r"^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$"
NAME_PATTERN = r"^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$"


class _Input(BaseModel):
    model_config = ConfigDict(extra="forbid")


class PromQLInput(_Input):
    query: str = Field(min_length=1, max_length=2000, description="A PromQL expression.")


class PromQLRangeInput(_Input):
    query: str = Field(min_length=1, max_length=2000, description="A PromQL expression.")
    minutes: int = Field(default=30, ge=1, le=360, description="Look back this many minutes.")
    step_seconds: int = Field(default=60, ge=15, le=3600, description="Resolution.")


class LogsInput(_Input):
    filter: str = Field(
        default="",
        max_length=2000,
        description=(
            'Cloud Logging query, e.g. resource.type="k8s_container" '
            'resource.labels.namespace_name="boutique" '
            'resource.labels.container_name="server" severity>=WARNING'
        ),
    )
    minutes: int = Field(default=30, ge=1, le=360)
    limit: int = Field(default=40, ge=1, le=100)


class TracesInput(_Input):
    filter: str = Field(
        default="",
        max_length=500,
        description="Cloud Trace filter, e.g. root:/cart/checkout or latency:500ms; empty = all.",
    )
    minutes: int = Field(default=30, ge=1, le=360)
    limit: int = Field(default=10, ge=1, le=20)


class NamespaceInput(_Input):
    namespace: str = Field(default="boutique", pattern=NAMESPACE_PATTERN)


class PodsInput(NamespaceInput):
    label_selector: str = Field(default="", max_length=300, description="e.g. app=currencyservice")


class EventsInput(NamespaceInput):
    object_name: str = Field(
        default="", max_length=253, description="Only events about this object."
    )
    minutes: int = Field(default=60, ge=1, le=1440)
    warnings_only: bool = False
    limit: int = Field(default=40, ge=1, le=100)


class DeploymentsInput(NamespaceInput):
    name: str = Field(
        default="", max_length=253, description="One deployment in detail (env, images)."
    )


class RolloutInput(NamespaceInput):
    deployment: str = Field(pattern=NAME_PATTERN)


class PodLogsInput(NamespaceInput):
    pod: str = Field(
        min_length=1,
        max_length=253,
        description="Pod name, or a deployment name for its first pod.",
    )
    container: str = Field(default="", max_length=63, description="e.g. server or istio-proxy")
    tail_lines: int = Field(default=100, ge=1, le=300)
    previous: bool = Field(default=False, description="Logs of the previous (crashed) container.")


class NoInput(_Input):
    pass


class ArgoInput(_Input):
    name: str = Field(default="", max_length=253, description="One Application in detail.")


class PastIncidentsInput(_Input):
    query: str = Field(default="", max_length=300, description="Service names or symptoms.")
    id: str = Field(default="", max_length=120, description="Read one postmortem in full.")


@dataclass(frozen=True)
class Tool:
    name: str
    description: str
    input_model: type[_Input]
    run: Callable[[Any], str]
    source: str

    def schema(self) -> dict[str, Any]:
        input_schema = self.input_model.model_json_schema()
        input_schema.pop("title", None)
        return {"name": self.name, "description": self.description, "input_schema": input_schema}


@dataclass(frozen=True)
class ToolOutcome:
    text: str
    is_error: bool
    result: Literal["ok", "error", "invalid", "unknown"]


class ToolRegistry:
    def __init__(self, tools: list[Tool], max_chars: int) -> None:
        self._tools = {t.name: t for t in tools}
        self._max_chars = max_chars

    @property
    def names(self) -> list[str]:
        return list(self._tools)

    def schemas(self) -> list[dict[str, Any]]:
        # Stable order and content keep the prompt-cache prefix identical across calls.
        return [self._tools[name].schema() for name in sorted(self._tools)]

    def run(self, name: str, raw_input: Any) -> ToolOutcome:
        tool = self._tools.get(name)
        if tool is None:
            metrics.TOOL_CALLS.labels("unknown", "unknown").inc()
            return ToolOutcome(f"unknown tool {name!r}", True, "unknown")
        try:
            args = tool.input_model.model_validate(raw_input or {})
        except ValidationError as exc:
            metrics.TOOL_CALLS.labels(name, "invalid").inc()
            problems = "; ".join(
                f"{'.'.join(map(str, e['loc']))}: {e['msg']}" for e in exc.errors()
            )
            return ToolOutcome(f"invalid input: {problems}", True, "invalid")
        try:
            text = tool.run(args)
            result: Literal["ok", "error"] = "ok"
        except (HttpError, ForbiddenPath, OSError, ValueError, KeyError, TypeError) as exc:
            # Errors go back to the model as data, so it can adapt (wrong label, no access...).
            log.info("tool failed", extra={"event": "tool_failed", "tool": name, "error": str(exc)})
            text, result = f"ERROR: {exc}", "error"
        metrics.TOOL_CALLS.labels(name, result).inc()
        body = wrap_untrusted(tool.source, cap(redact(text), self._max_chars))
        return ToolOutcome(body, result == "error", result)


def build_tools(
    google: GoogleReads,
    kube: KubeReads,
    knowledge: KnowledgeReads,
    exclude_fault_ids: Callable[[], list[str]],
    chaos_tool: bool,
) -> list[Tool]:
    """The tool list. `exclude_fault_ids` reads the current investigation's eval filter."""
    tools = [
        Tool(
            "promql_query",
            "Instant PromQL query on Managed Service for Prometheus (Istio mesh metrics such as "
            "istio_requests_total and istio_request_duration_milliseconds_bucket, prober metrics "
            "mamori_cuj_probe_total, SLO recording rules mamori:cuj_*). Read-only.",
            PromQLInput,
            lambda a: google.promql(a.query),
            "prometheus",
        ),
        Tool(
            "promql_range",
            "Range PromQL query over the last N minutes, to see when a signal changed. Read-only.",
            PromQLRangeInput,
            lambda a: google.promql_range(a.query, a.minutes, a.step_seconds),
            "prometheus",
        ),
        Tool(
            "logs_search",
            "Search Cloud Logging (container logs of every namespace, Kubernetes events, audit "
            "logs). Newest first. Read-only.",
            LogsInput,
            lambda a: google.logs(a.filter, a.minutes, a.limit),
            "cloud-logging",
        ),
        Tool(
            "traces_search",
            "List the slowest Cloud Trace traces in a window, with slowest and failing spans. "
            "Empty if the services don't export traces. Read-only.",
            TracesInput,
            lambda a: google.traces(a.filter, a.minutes, a.limit),
            "cloud-trace",
        ),
        Tool(
            "k8s_pods",
            "Pods in a namespace: readiness, phase, restarts, last termination reason "
            "(OOMKilled, Error), node.",
            PodsInput,
            lambda a: kube.pods(a.namespace, a.label_selector),
            "kubernetes",
        ),
        Tool(
            "k8s_events",
            "Kubernetes events in a namespace, newest first (scheduling, probes, kills, pulls).",
            EventsInput,
            lambda a: kube.events(a.namespace, a.object_name, a.minutes, a.warnings_only, a.limit),
            "kubernetes",
        ),
        Tool(
            "k8s_deployments",
            "Deployments: desired/ready replicas, revision, images, conditions; with name: env, "
            "args and resources of each container.",
            DeploymentsInput,
            lambda a: kube.deployments(a.namespace, a.name),
            "kubernetes",
        ),
        Tool(
            "k8s_rollout_history",
            "Revisions of one deployment (its ReplicaSets), newest first, with what changed "
            "between revisions (images, env, args, resources).",
            RolloutInput,
            lambda a: kube.rollout_history(a.namespace, a.deployment),
            "kubernetes",
        ),
        Tool(
            "k8s_pod_logs",
            "Recent log lines of one pod (namespaces boutique and istio-ingress only).",
            PodLogsInput,
            lambda a: kube.pod_logs(a.namespace, a.pod, a.container, a.tail_lines, a.previous),
            "pod-logs",
        ),
        Tool(
            "k8s_nodes",
            "Nodes: readiness, Spot, age (a young node often means a preemption), conditions.",
            NoInput,
            lambda a: kube.nodes(),
            "kubernetes",
        ),
        Tool(
            "argocd_applications",
            "Argo CD Applications: sync and health status, deployed Git revisions and when.",
            ArgoInput,
            lambda a: kube.argocd_applications(a.name),
            "kubernetes",
        ),
        Tool(
            "istio_config",
            "Istio objects in a namespace: VirtualServices (routes, fault injection, timeouts, "
            "retries), DestinationRules, Sidecars, AuthorizationPolicies, with labels and spec.",
            NamespaceInput,
            lambda a: kube.mesh_config(a.namespace),
            "kubernetes",
        ),
        Tool(
            "cuj_map",
            "Which upstream services each critical user journey depends on (critical vs "
            "non-critical), from the fault-injection discovery run.",
            NoInput,
            lambda a: knowledge.cuj_map(),
            "repository",
        ),
        Tool(
            "slo_spec",
            "SLO targets per journey (availability and latency, 28-day window).",
            NoInput,
            lambda a: knowledge.slo_spec(),
            "repository",
        ),
        Tool(
            "past_incidents",
            "Search past postmortems (has this happened before?), or read one by id.",
            PastIncidentsInput,
            lambda a: knowledge.past_incidents(a.query, a.id, exclude_fault_ids()),
            "postmortems",
        ),
    ]
    if chaos_tool:
        tools.append(
            Tool(
                "chaos_experiments",
                "Chaos Mesh experiments (PodChaos, NetworkChaos, StressChaos) in a namespace, "
                "with phase, selector and spec.",
                NamespaceInput,
                lambda a: kube.chaos_experiments(a.namespace),
                "kubernetes",
            )
        )
    return tools
