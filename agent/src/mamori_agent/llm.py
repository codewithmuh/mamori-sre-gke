"""The model client (Vertex AI by default), list prices and usage metering.

Prices are USD per million tokens, Claude list prices checked 2026-11-13 on
platform.claude.com/docs/en/about-claude/pricing. Vertex AI's global endpoint charges the same;
its multi-region and regional endpoints add 10 % (docs, "Global, multi-region, and regional
endpoints"), which `regional_premium` covers. Costs here are estimates for guardrails and the
evaluation; the Google Cloud bill is the measured number.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Protocol

from . import metrics
from .config import Settings, in_cluster


@dataclass(frozen=True)
class Price:
    input: float
    cache_write: float  # 5-minute cache writes, the only kind the agent uses
    cache_read: float
    output: float


PRICES: dict[str, Price] = {
    "claude-opus-5-5": Price(4.00, 5.00, 0.20, 20.00),
    "claude-opus-5": Price(5.00, 6.25, 0.50, 25.00),
    "claude-opus-4-8": Price(5.00, 6.25, 0.50, 25.00),
    "claude-sonnet-5-5": Price(2.00, 2.50, 0.10, 10.00),
    # Prompts up to 100k tokens; the agent's prompts stay below that (max_total_tokens is per run).
    "claude-haiku-5-5": Price(0.10, 0.125, 0.01, 0.50),
}

# An unknown model is priced like the most expensive one above, so guardrails err on the safe side.
_FALLBACK_PRICE = Price(5.00, 6.25, 0.50, 25.00)


def price_for(model: str) -> Price:
    return PRICES.get(model, _FALLBACK_PRICE)


@dataclass
class UsageMeter:
    """Running totals over every model call of one investigation."""

    regional_premium: float = 1.0
    input_tokens: int = 0
    cache_write_tokens: int = 0
    cache_read_tokens: int = 0
    output_tokens: int = 0
    cost_usd: float = 0.0
    by_model: dict[str, float] = field(default_factory=dict)

    def add(self, model: str, usage: Any) -> float:
        """Add one response's usage; returns that call's cost."""
        fresh = int(getattr(usage, "input_tokens", 0) or 0)
        written = int(getattr(usage, "cache_creation_input_tokens", 0) or 0)
        read = int(getattr(usage, "cache_read_input_tokens", 0) or 0)
        out = int(getattr(usage, "output_tokens", 0) or 0)
        price = price_for(model)
        cost = (
            (
                fresh * price.input
                + written * price.cache_write
                + read * price.cache_read
                + out * price.output
            )
            / 1_000_000
            * self.regional_premium
        )
        self.input_tokens += fresh
        self.cache_write_tokens += written
        self.cache_read_tokens += read
        self.output_tokens += out
        self.cost_usd += cost
        self.by_model[model] = self.by_model.get(model, 0.0) + cost
        for kind, count in (
            ("input", fresh),
            ("cache_write", written),
            ("cache_read", read),
            ("output", out),
        ):
            metrics.TOKENS.labels(model, kind).inc(count)
        metrics.COST.labels(model).inc(cost)
        return cost

    @property
    def total_input_tokens(self) -> int:
        """All input the model read: uncached, written to cache and read from cache."""
        return self.input_tokens + self.cache_write_tokens + self.cache_read_tokens

    @property
    def total_tokens(self) -> int:
        return self.total_input_tokens + self.output_tokens


class ModelCaller(Protocol):
    """One Messages API call: the request parameters in, the SDK's Message out."""

    def __call__(self, params: dict[str, Any], timeout: float) -> Any: ...


class ConfigError(RuntimeError):
    """The settings can't produce a working client."""


def make_caller(settings: Settings, project_id: str) -> ModelCaller:
    """Build the SDK client once; return a function that calls messages.create on it."""
    client: Any
    if settings.provider == "vertex":
        from anthropic import AnthropicVertex  # noqa: PLC0415 - heavy import, only when serving

        if not project_id:
            raise ConfigError(
                "Vertex AI needs a project: set AGENT_PROJECT_ID or ADC with a project"
            )
        # Credentials: Application Default Credentials. In GKE that is the pod's Kubernetes
        # service account through Workload Identity; no key exists anywhere.
        client = AnthropicVertex(
            project_id=project_id,
            region=settings.vertex_region,
            timeout=settings.model_timeout_seconds,
            max_retries=2,
        )
    else:
        if in_cluster():
            # D8: no API key in the cluster. The fallback exists for laptops only.
            raise ConfigError(
                "AGENT_PROVIDER=anthropic is for local runs; use vertex in the cluster"
            )
        from anthropic import Anthropic  # noqa: PLC0415

        client = Anthropic(timeout=settings.model_timeout_seconds, max_retries=2)

    def call(params: dict[str, Any], timeout: float) -> Any:
        return client.messages.create(**params, timeout=timeout)

    return call


def regional_premium(settings: Settings) -> float:
    if settings.provider == "vertex" and settings.vertex_region != "global":
        return 1.10
    return 1.0
