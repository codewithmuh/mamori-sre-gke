"""Prometheus metrics on /metrics, scraped by Managed Prometheus.

The PodMonitoring is deploy/agent/podmonitoring.yaml.
"""

from __future__ import annotations

from prometheus_client import Counter, Gauge, Histogram

INCIDENTS = Counter(
    "mamori_agent_incidents_total",
    "Incident notifications received, by what the agent did with them.",
    ["source", "action"],  # investigate|duplicate|closed|ignored|invalid|queue_full|daily_budget
)
INVESTIGATIONS = Counter(
    "mamori_agent_investigations_total",
    "Investigations finished, by outcome.",
    ["source", "outcome"],  # completed|failed
)
DURATION = Histogram(
    "mamori_agent_investigation_duration_seconds",
    "Wall time from start to finished record.",
    ["source"],
    buckets=(15, 30, 60, 90, 120, 180, 240, 300, 450, 600, 900),
)
TOKENS = Counter(
    "mamori_agent_tokens_total",
    "Tokens billed by the model API.",
    ["model", "kind"],  # input|cache_write|cache_read|output
)
COST = Counter("mamori_agent_cost_usd_total", "Estimated model cost at list prices.", ["model"])
MODEL_CALLS = Counter(
    "mamori_agent_model_calls_total",
    "Model API calls, by stop reason.",
    ["model", "stop_reason"],
)
TOOL_CALLS = Counter(
    "mamori_agent_tool_calls_total",
    "Read tool calls, by result.",
    ["tool", "result"],  # ok|error|invalid|unknown|skipped
)
GUARDRAIL_STOPS = Counter(
    "mamori_agent_guardrail_stops_total",
    "Times a guardrail ended tool use early or stopped an investigation.",
    ["guardrail"],  # tool_calls|model_calls|tokens|cost|deadline|daily_budget
)
REFUSALS = Counter("mamori_agent_refusals_total", "Model refusals.", ["model", "category"])
PROPOSALS = Counter(
    "mamori_agent_proposals_total",
    "Proposals sent to the executor, by result.",
    ["result"],  # accepted|rejected|error|skipped|invalid
)
SLACK_POSTS = Counter("mamori_agent_slack_posts_total", "Slack summaries.", ["result"])
QUEUE_DEPTH = Gauge("mamori_agent_queue_depth", "Incidents waiting for the worker.")
IN_PROGRESS = Gauge("mamori_agent_investigations_in_progress", "Investigations running now.")
DAILY_COST = Gauge("mamori_agent_daily_cost_usd", "Estimated model cost so far today (UTC).")
