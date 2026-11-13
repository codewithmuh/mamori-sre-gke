"""Settings, all from AGENT_* environment variables (see agent/README.md for the full table)."""

from __future__ import annotations

import os
from typing import Literal

from pydantic import Field, SecretStr, field_validator
from pydantic_settings import BaseSettings, SettingsConfigDict

CUJ_IDS: tuple[str, ...] = ("browse", "add-to-cart", "view-cart", "checkout")

# The only namespace the executor acts in (contract, #25).
ACTION_NAMESPACE = "boutique"


def _split(value: str) -> list[str]:
    return [part.strip() for part in value.split(",") if part.strip()]


class Settings(BaseSettings):
    """Every knob has a safe default; the Deployment only sets what differs in the cluster."""

    model_config = SettingsConfigDict(env_prefix="AGENT_", extra="ignore")

    # --- Model (D8) -----------------------------------------------------------------------
    # vertex: Claude on Vertex AI with Application Default Credentials (Workload Identity in
    # the cluster). anthropic: the Anthropic API with ANTHROPIC_API_KEY, for local runs only.
    provider: Literal["vertex", "anthropic"] = "vertex"
    # Google Cloud project for Vertex AI and the read tools. Empty: taken from ADC (the GKE
    # metadata server knows it), so manifests don't need the project ID.
    project_id: str = ""
    # Tokyo has no regional endpoint for the newest Claude models; global is also the cheapest.
    vertex_region: str = "global"
    model: str = "claude-opus-5-5"
    # Client-side refusal fallback (Vertex AI has no server-side `fallbacks`). Empty disables it.
    fallback_model: str = "claude-opus-5"
    effort: Literal["low", "medium", "high", "xhigh", "max"] = "medium"

    # --- Guardrails ---------------------------------------------------------------------------
    max_tokens_per_call: int = Field(default=16000, ge=1024, le=64000)
    max_model_calls: int = Field(default=20, ge=1, le=100)
    max_tool_calls: int = Field(default=25, ge=0, le=200)
    max_total_tokens: int = Field(default=800_000, ge=10_000)
    max_cost_usd: float = Field(default=1.50, gt=0)
    daily_cost_limit_usd: float = Field(default=15.0, gt=0)
    investigation_timeout_seconds: float = Field(default=600, ge=60)
    model_timeout_seconds: float = Field(default=180, ge=10)
    tool_timeout_seconds: float = Field(default=20, ge=1)
    tool_output_max_chars: int = Field(default=6000, ge=500, le=50_000)

    # --- Trigger ------------------------------------------------------------------------------
    pubsub_enabled: bool = True
    subscription: str = "mamori-agent-incidents"
    dedupe_ttl_seconds: int = Field(default=86_400, ge=60)
    queue_size: int = Field(default=10, ge=1, le=100)

    # --- Proposals and Slack ------------------------------------------------------------------
    propose: bool = True
    executor_url: str = "http://executor.mamori-agent.svc:8080"
    # Projected service account token (audience "executor"); the executor checks it with a
    # TokenReview. Missing file: no Authorization header (local runs against AUTH_MODE=none).
    executor_token_file: str = "/var/run/secrets/executor/token"  # noqa: S105 - a path
    slack: bool = True
    slack_bot_token: SecretStr = SecretStr("")
    slack_channel: str = ""

    # --- Read tools ---------------------------------------------------------------------------
    k8s_api_url: str = "https://kubernetes.default.svc"
    k8s_token_file: str = "/var/run/secrets/kubernetes.io/serviceaccount/token"  # noqa: S105
    k8s_ca_file: str = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
    # Pod logs only from these namespaces (RBAC grants pods/log only there, too).
    log_namespaces: str = "boutique,istio-ingress"
    # Knowledge files (CUJ map, SLO spec, postmortems): "github" reads them over HTTPS from the
    # public repo; "local" from repo_dir. Either way only an allow-list of paths, never chaos/.
    knowledge_source: Literal["github", "local"] = "github"
    repo_dir: str = "."
    github_repo: str = "codewithmuh/mamori-sre-gke"
    github_ref: str = "main"
    # include: every postmortem; exclude: drop game-day postmortems (front matter faultId set),
    # for evaluation runs that must not read the answer to a fault they are scored on.
    postmortems_gamedays: Literal["include", "exclude"] = "include"
    # Strip mamori.dev/* labels and annotations from tool output (blind evaluation runs).
    hide_fault_labels: bool = False
    chaos_tool: bool = True

    # --- Service ------------------------------------------------------------------------------
    http_port: int = Field(default=8080, ge=1, le=65535)
    store_size: int = Field(default=200, ge=10, le=5000)
    log_level: str = "INFO"

    @field_validator("model", "fallback_model", "subscription", "slack_channel", mode="before")
    @classmethod
    def _strip(cls, value: object) -> object:
        return value.strip() if isinstance(value, str) else value

    @property
    def log_namespace_list(self) -> list[str]:
        return _split(self.log_namespaces)

    def resolved_project_id(self) -> str:
        """Project from settings, then the usual environment variables; may be empty."""
        return (
            self.project_id
            or os.environ.get("GOOGLE_CLOUD_PROJECT", "")
            or os.environ.get("ANTHROPIC_VERTEX_PROJECT_ID", "")
        )


def in_cluster() -> bool:
    """True inside a Kubernetes pod (the kubelet sets this for every container)."""
    return bool(os.environ.get("KUBERNETES_SERVICE_HOST"))
