"""The investigation loop: model call, read tools, repeat, then a structured final answer.

Guardrails, all from Settings:
- tool calls per investigation (max_tool_calls) and model calls (max_model_calls);
- tokens (max_total_tokens) and estimated cost (max_cost_usd): at 75 % the model is told to
  answer now; at 100 % no further call is made; the day's spend is checked before every call;
- wall time (investigation_timeout_seconds): the answer is requested two minutes early, the
  deadline is checked between tools, every call's timeout comes from the time left, and one
  short final call is always allowed, even past the deadline;
- model API errors: 429, 529, 5xx, timeouts and connection errors are retried with backoff
  (here, not in the SDK, so retries can't outlast the deadline); others end the run. Either way
  the usage spent so far stays on the record;
- refusals: one client-side retry on the fallback model (Vertex AI has no server-side fallbacks),
  except for reasoning_extraction, which no other model would answer either;
- a response cut off by max_tokens is never acted on: it is retried once with twice the limit,
  then the model is asked for its final answer.

The conversation is append-only: every assistant turn that is kept goes back unchanged (thinking
blocks included) and notices are appended, never edited in. Request parameters stay the same
from call to call (tool_choice stays auto), so the prompt cache stays warm and the
preserved-thinking check of Claude Opus 5.5 passes.
"""

from __future__ import annotations

import json
import logging
import time
from collections.abc import Callable
from dataclasses import dataclass, field
from typing import Any

import anthropic
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
# The one call allowed after the deadline (or with less time left than this) gets this long.
_FINAL_CALL_SECONDS = 60
# Calls allowed once the final answer was requested: the answer, plus one nudge if the model
# still reached for a tool.
_MAX_FINAL_CALLS = 2
_MAX_TOKENS_CEILING = 64_000
_RETRY_STATUSES = frozenset({429, 500, 502, 503, 504, 529})
_MAX_RETRIES = 4
_BACKOFF_SECONDS = 2.0

HARD_STOPS = ("tool_calls", "model_calls", "tokens", "cost", "deadline", "daily_budget")


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


def _classify(exc: Exception) -> tuple[bool, str]:
    """(retryable, short label) for an SDK error."""
    if isinstance(exc, anthropic.APITimeoutError):
        return True, "timeout"
    if isinstance(exc, anthropic.APIConnectionError):
        return True, "connection"
    status = getattr(exc, "status_code", None)
    if isinstance(status, int):
        return status in _RETRY_STATUSES, f"http_{status}"
    return False, type(exc).__name__


class Investigator:
    def __init__(
        self,
        settings: Settings,
        call_model: ModelCaller,
        tools: ToolRegistry,
        clock: Callable[[], float] = time.monotonic,
        sleep: Callable[[float], None] = time.sleep,
    ) -> None:
        self._s = settings
        self._call = call_model
        self._tools = tools
        self._clock = clock
        self._sleep = sleep
        self._tool_schemas = tools.schemas()

    def run(self, incident: Incident, budget: Callable[[float], bool] | None = None) -> Outcome:
        """Investigate. `budget(run_cost)` says whether the day's spend allows another call."""
        meter = UsageMeter(regional_premium=regional_premium(self._s))
        state = _LoopState(
            model=self._s.model,
            deadline=self._clock() + self._s.investigation_timeout_seconds,
            max_tokens=self._s.max_tokens_per_call,
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
            outcome.findings = self._loop(messages, state, meter, outcome, budget)
        except InvestigationError as exc:
            outcome.error = exc
            if exc.reason in HARD_STOPS:
                metrics.GUARDRAIL_STOPS.labels(exc.reason).inc()
        except Exception as exc:
            log.exception("investigation loop crashed", extra={"event": "loop_crashed"})
            outcome.error = InvestigationError("crash", f"{type(exc).__name__}: {exc}")
        outcome.model = state.served_by or state.model
        outcome.stops = state.soft_stops
        return outcome

    # ------------------------------------------------------------------------------------------

    def _guard(
        self,
        state: _LoopState,
        meter: UsageMeter,
        outcome: Outcome,
        budget: Callable[[float], bool] | None,
    ) -> None:
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
        if budget is not None and not budget(meter.cost_usd):
            raise InvestigationError("daily_budget", "the day's model budget is used up")
        if state.final and state.final_calls >= _MAX_FINAL_CALLS:
            raise InvestigationError("no_answer", "no final answer after it was requested")
        now = self._clock()
        if now >= state.deadline:
            if state.final and state.final_calls >= 1:
                raise InvestigationError("deadline", "investigation timeout reached")
            state.request_final("deadline")  # one short final call, even past the deadline
        if not state.final:
            reason = None
            if meter.cost_usd >= _SOFT_FRACTION * s.max_cost_usd:
                reason = "cost"
            elif meter.total_tokens >= _SOFT_FRACTION * s.max_total_tokens:
                reason = "tokens"
            elif outcome.model_calls >= s.max_model_calls - 1:
                reason = "model_calls"
            elif now >= state.deadline - _FINAL_ANSWER_RESERVE_SECONDS:
                reason = "deadline"
            if reason:
                state.request_final(reason)

    def _params(self, messages: list[dict[str, Any]], state: _LoopState) -> dict[str, Any]:
        # The same parameters on every call except max_tokens after a cut-off: tool_choice stays
        # auto even for the final answer (changing it would invalidate the prompt cache); the
        # appended notice asks for the answer instead.
        return {
            "model": state.model,
            "max_tokens": state.max_tokens,
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

    def _call_with_retry(self, params: dict[str, Any], state: _LoopState) -> Any:
        """One model call; transient API errors are retried while the deadline allows."""
        attempt = 0
        while True:
            remaining = state.deadline - self._clock()
            floor = _FINAL_CALL_SECONDS if state.final else 5.0
            timeout = min(self._s.model_timeout_seconds, max(remaining, floor))
            try:
                return self._call(params, timeout=timeout)
            except anthropic.APIError as exc:
                retryable, label = _classify(exc)
                metrics.MODEL_ERRORS.labels(state.model, label).inc()
                delay = _BACKOFF_SECONDS * 2**attempt
                if (
                    not retryable
                    or attempt >= _MAX_RETRIES
                    or self._clock() + delay >= state.deadline
                ):
                    raise InvestigationError(
                        "model_error", f"model API error ({label}): {exc}"
                    ) from exc
                event(
                    log,
                    "model_retry",
                    logging.WARNING,
                    model=state.model,
                    error=label,
                    attempt=attempt + 1,
                    delay_s=delay,
                )
                self._sleep(delay)
                attempt += 1

    def _loop(
        self,
        messages: list[dict[str, Any]],
        state: _LoopState,
        meter: UsageMeter,
        outcome: Outcome,
        budget: Callable[[float], bool] | None,
    ) -> Findings:
        answer_retries = 0
        while True:
            self._guard(state, meter, outcome, budget)
            if state.pending_notice:
                # Reached when the previous turn left no tool results to carry the notice.
                messages.append({"role": "user", "content": state.take_notice()})
            was_final = state.final
            response = self._call_with_retry(self._params(messages, state), state)
            outcome.model_calls += 1
            if was_final:
                state.final_calls += 1
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
            tool_uses = [b for b in content if getattr(b, "type", "") == "tool_use"]

            if stop == "max_tokens" and (tool_uses or not state.bumped):
                # A cut-off turn may hold a truncated tool call: never run it, never replay it.
                if not state.bumped:
                    state.max_tokens = min(_MAX_TOKENS_CEILING, state.max_tokens * 2)
                    state.bumped = True
                    event(log, "max_tokens_retry", logging.WARNING, max_tokens=state.max_tokens)
                else:
                    state.request_final("max_tokens", prompts.CUT_OFF_NOTICE)
                continue

            messages.append({"role": "assistant", "content": content})

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

            messages.append({"role": "user", "content": self._run_tools(tool_uses, state, outcome)})
            # Check the soft limits now, so the notice travels with these results.
            self._guard(state, meter, outcome, budget)
            if state.pending_notice:
                messages[-1]["content"].append({"type": "text", "text": state.take_notice()})
            elif state.final:
                # Tools were asked for after the final notice: say it again, briefly.
                messages[-1]["content"].append({"type": "text", "text": prompts.FINAL_REMINDER})

    def _run_tools(
        self, tool_uses: list[Any], state: _LoopState, outcome: Outcome
    ) -> list[dict[str, Any]]:
        results: list[dict[str, Any]] = []
        for block in tool_uses:
            if not state.final:
                if outcome.tool_calls >= self._s.max_tool_calls:
                    state.request_final("tool_calls")
                elif self._clock() >= state.deadline - _FINAL_ANSWER_RESERVE_SECONDS:
                    state.request_final("deadline")
            if state.final:
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
        return results

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
    max_tokens: int
    final: bool = False
    final_calls: int = 0
    bumped: bool = False
    fell_back: bool = False
    served_by: str = ""
    pending_notice: str = ""
    soft_stops: list[str] = field(default_factory=list)

    def request_final(self, reason: str, notice: str = prompts.FINAL_NOTICE) -> None:
        if reason not in ("bad_answer", "max_tokens"):
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
