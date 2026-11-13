"""The investigation loop: model call, read tools, repeat, then a structured final answer.

Guardrails, all from Settings:
- tool calls per investigation (max_tool_calls) and model calls (max_model_calls);
- tokens (max_total_tokens) and estimated cost (max_cost_usd): at 75 % the model is told to
  answer now with tool_choice none; at 100 % no further call is made;
- wall time (investigation_timeout_seconds): the final answer is requested two minutes early,
  and every model and tool call has its own timeout;
- refusals: one client-side retry on the fallback model (Vertex AI has no server-side fallbacks),
  except for reasoning_extraction, which no other model would answer either.

The conversation is append-only: every assistant turn goes back unchanged (thinking blocks
included) and notices are appended, never edited in. That keeps the prompt cache warm and
satisfies the preserved-thinking check of Claude Opus 5.5.
"""

from __future__ import annotations

import json
import logging
import time
from collections.abc import Callable
from dataclasses import dataclass, field
from typing import Any

from pydantic import ValidationError

from . import metrics, prompts
from .config import Settings
from .incidents import baseline_impact, baseline_severity
from .llm import ModelCaller, UsageMeter, regional_premium
from .logs import event
from .models import FINDINGS_SCHEMA, Findings, Incident
from .tools import ToolRegistry

log = logging.getLogger(__name__)

_SOFT_FRACTION = 0.75
_FINAL_ANSWER_RESERVE_SECONDS = 120


class InvestigationError(RuntimeError):
    """The loop ended without a usable answer; `reason` names the guardrail or failure."""

    def __init__(self, reason: str, message: str) -> None:
        super().__init__(message)
        self.reason = reason


@dataclass
class Outcome:
    findings: Findings | None
    meter: UsageMeter
    model: str
    tool_calls: int
    model_calls: int
    error: InvestigationError | None = None
    stops: list[str] = field(default_factory=list)


class Investigator:
    def __init__(
        self,
        settings: Settings,
        call_model: ModelCaller,
        tools: ToolRegistry,
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        self._s = settings
        self._call = call_model
        self._tools = tools
        self._clock = clock
        self._tool_schemas = tools.schemas()

    def run(self, incident: Incident) -> Outcome:
        meter = UsageMeter(regional_premium=regional_premium(self._s))
        state = _LoopState(
            model=self._s.model, deadline=self._clock() + self._s.investigation_timeout_seconds
        )
        outcome = Outcome(
            findings=None, meter=meter, model=self._s.model, tool_calls=0, model_calls=0
        )
        messages: list[dict[str, Any]] = [
            {
                "role": "user",
                "content": prompts.incident_message(
                    incident, baseline_severity(incident), baseline_impact(incident)
                ),
            }
        ]
        try:
            outcome.findings = self._loop(messages, state, meter, outcome)
        except InvestigationError as exc:
            outcome.error = exc
            if exc.reason in ("tool_calls", "model_calls", "tokens", "cost", "deadline"):
                metrics.GUARDRAIL_STOPS.labels(exc.reason).inc()
        outcome.model = state.served_by or state.model
        outcome.stops = state.soft_stops
        return outcome

    # ------------------------------------------------------------------------------------------

    def _guard(self, state: _LoopState, meter: UsageMeter, outcome: Outcome) -> None:
        """Hard limits before a model call; soft limits switch to the final answer."""
        s = self._s
        if outcome.model_calls >= s.max_model_calls:
            raise InvestigationError(
                "model_calls", f"stopped after {outcome.model_calls} model calls"
            )
        if meter.cost_usd >= s.max_cost_usd:
            raise InvestigationError("cost", f"cost cap ${s.max_cost_usd:.2f} reached")
        if meter.total_tokens >= s.max_total_tokens:
            raise InvestigationError("tokens", f"token cap {s.max_total_tokens} reached")
        if self._clock() >= state.deadline:
            raise InvestigationError("deadline", "investigation timeout reached")
        if not state.final:
            reason = None
            if meter.cost_usd >= _SOFT_FRACTION * s.max_cost_usd:
                reason = "cost"
            elif meter.total_tokens >= _SOFT_FRACTION * s.max_total_tokens:
                reason = "tokens"
            elif outcome.model_calls >= s.max_model_calls - 1:
                reason = "model_calls"
            elif self._clock() >= state.deadline - _FINAL_ANSWER_RESERVE_SECONDS:
                reason = "deadline"
            if reason:
                state.request_final(reason)

    def _params(self, messages: list[dict[str, Any]], state: _LoopState) -> dict[str, Any]:
        params: dict[str, Any] = {
            "model": state.model,
            "max_tokens": self._s.max_tokens_per_call,
            "system": prompts.SYSTEM_PROMPT,
            "tools": self._tool_schemas,
            "messages": messages,
            # Opus 5.5 always thinks adaptively; effort is the control (its default is medium,
            # set explicitly so a model switch can't change it silently).
            "thinking": {"type": "adaptive"},
            "output_config": {
                "effort": self._s.effort,
                "format": {"type": "json_schema", "schema": FINDINGS_SCHEMA},
            },
            # Automatic caching: the breakpoint moves to the newest block each turn, so every
            # call reads the earlier turns from cache.
            "cache_control": {"type": "ephemeral"},
        }
        if state.final:
            params["tool_choice"] = {"type": "none"}
        return params

    def _loop(
        self,
        messages: list[dict[str, Any]],
        state: _LoopState,
        meter: UsageMeter,
        outcome: Outcome,
    ) -> Findings:
        answer_retries = 0
        while True:
            self._guard(state, meter, outcome)
            if state.pending_notice:
                # Only reached when the previous turn ended without tool calls (a bad answer);
                # otherwise the notice rides along with the tool results.
                messages.append({"role": "user", "content": state.take_notice()})
            remaining = max(5.0, state.deadline - self._clock())
            response = self._call(
                self._params(messages, state), timeout=min(self._s.model_timeout_seconds, remaining)
            )
            outcome.model_calls += 1
            served_by = str(getattr(response, "model", "") or state.model)
            cost = meter.add(served_by, getattr(response, "usage", None))
            stop = str(getattr(response, "stop_reason", ""))
            metrics.MODEL_CALLS.labels(served_by, stop).inc()
            log.debug(
                "model call",
                extra={
                    "event": "model_call",
                    "model": served_by,
                    "stop_reason": stop,
                    "cost_usd": round(cost, 5),
                },
            )

            if stop == "refusal":
                self._on_refusal(response, state, served_by)
                continue  # same messages, now on the fallback model
            state.served_by = served_by

            content = list(getattr(response, "content", []) or [])
            messages.append({"role": "assistant", "content": content})
            tool_uses = [b for b in content if getattr(b, "type", "") == "tool_use"]

            if not tool_uses:
                if stop == "pause_turn":
                    continue
                try:
                    return _parse_findings(content, stop)
                except InvestigationError as exc:
                    answer_retries += 1
                    if answer_retries > 1:
                        raise
                    state.request_final("bad_answer", prompts.RETRY_NOTICE.format(problem=exc))
                    continue

            results: list[dict[str, Any]] = []
            for block in tool_uses:
                if state.final or outcome.tool_calls >= self._s.max_tool_calls:
                    if not state.final:
                        state.request_final("tool_calls")
                    results.append(
                        {
                            "type": "tool_result",
                            "tool_use_id": block.id,
                            "content": "Not run: the tool budget is used up.",
                            "is_error": True,
                        }
                    )
                    metrics.TOOL_CALLS.labels(str(block.name), "skipped").inc()
                    continue
                outcome.tool_calls += 1
                result = self._tools.run(str(block.name), getattr(block, "input", {}))
                results.append(
                    {
                        "type": "tool_result",
                        "tool_use_id": block.id,
                        "content": result.text,
                        "is_error": result.is_error,
                    }
                )
            if not state.final and outcome.tool_calls >= self._s.max_tool_calls:
                state.request_final("tool_calls")
            # Check the soft limits now, so the notice travels with these results.
            self._guard(state, meter, outcome)
            if state.pending_notice:
                results.append({"type": "text", "text": state.take_notice()})
            messages.append({"role": "user", "content": results})

    def _on_refusal(self, response: Any, state: _LoopState, model: str) -> None:
        details = getattr(response, "stop_details", None)
        category = str(getattr(details, "category", None) or "none")
        metrics.REFUSALS.labels(model, category).inc()
        fallback = self._s.fallback_model
        if (
            fallback
            and not state.fell_back
            and category != "reasoning_extraction"
            and fallback != model
        ):
            event(
                log,
                "model_refusal_fallback",
                logging.WARNING,
                model=model,
                category=category,
                fallback=fallback,
            )
            state.model = fallback
            state.fell_back = True
            return
        raise InvestigationError("refusal", f"the model declined (category {category})")


@dataclass
class _LoopState:
    model: str
    deadline: float
    final: bool = False
    fell_back: bool = False
    served_by: str = ""
    pending_notice: str = ""
    soft_stops: list[str] = field(default_factory=list)

    def request_final(self, reason: str, notice: str = prompts.FINAL_NOTICE) -> None:
        if reason != "bad_answer":
            if self.final:
                return
            self.soft_stops.append(reason)
            metrics.GUARDRAIL_STOPS.labels(reason).inc()
        self.final = True
        self.pending_notice = notice

    def take_notice(self) -> str:
        notice, self.pending_notice = self.pending_notice, ""
        return notice


def _parse_findings(content: list[Any], stop: str) -> Findings:
    text = "".join(
        getattr(b, "text", "") for b in content if getattr(b, "type", "") == "text"
    ).strip()
    if not text:
        raise InvestigationError("no_answer", f"the model returned no text (stop_reason {stop})")
    try:
        data = json.loads(text)
    except json.JSONDecodeError as exc:
        hint = " (cut off by max_tokens)" if stop == "max_tokens" else ""
        raise InvestigationError("bad_answer", f"answer is not JSON{hint}: {exc.msg}") from exc
    try:
        return Findings.model_validate(data).normalised()
    except ValidationError as exc:
        first = exc.errors()[0]
        where = ".".join(str(p) for p in first["loc"])
        raise InvestigationError(
            "bad_answer", f"answer does not match the schema at {where}: {first['msg']}"
        ) from exc
