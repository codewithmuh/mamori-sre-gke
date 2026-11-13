"""List prices for the cost cross-check (docs/decision-facts.md D8, checked 2026-11-13).

The agent reports `usage.costUsd` itself, summed over its calls, including prompt-cache writes
and reads. The investigation only carries total input and output tokens, so the eval cannot
recompute the exact cost. It checks that the reported cost lies between the cheapest and the
dearest price those token counts could have had:

- lowest: every input token a cache read;
- highest: every input token a 1-hour cache write (2x input) or a long-prompt input, every
  output token at the long-prompt price, plus 10 % for a regional endpoint.

A cost outside that range means the agent's own accounting is wrong, not that tokens were
cached. USD per million tokens, Vertex AI global endpoint (same as the Claude API list price).
"""

from __future__ import annotations

from dataclasses import dataclass

REGIONAL_PREMIUM = 1.10
ONE_HOUR_CACHE_WRITE = 2.0
# Below a tenth of a cent the comparison is rounding noise.
ABSOLUTE_SLACK_USD = 0.001


@dataclass(frozen=True)
class Price:
    input: float
    output: float
    cache_read: float
    long_input: float | None = None
    long_output: float | None = None


PRICES: dict[str, Price] = {
    "claude-opus-5-5": Price(input=4.0, output=20.0, cache_read=0.20),
    # The agent's refusal fallback (#24).
    "claude-opus-5": Price(input=5.0, output=25.0, cache_read=0.50),
    "claude-sonnet-5-5": Price(input=2.0, output=10.0, cache_read=0.10),
    # Haiku 5.5: prompts over 100k tokens cost $0.50 / $2.50. Cache-read price not checked: 0
    # keeps the lower bound safe.
    "claude-haiku-5-5": Price(
        input=0.10, output=0.50, cache_read=0.0, long_input=0.50, long_output=2.50
    ),
}


def normalize_model(model: str) -> str:
    """'publishers/anthropic/models/claude-opus-5-5@...' or 'anthropic.claude-opus-5-5' -> id."""
    name = model.strip().lower().rsplit("/", 1)[-1]
    name = name.split("@", 1)[0]
    return name.removeprefix("anthropic.")


@dataclass(frozen=True)
class CostCheck:
    """`status`: ok, out-of-range, unknown-model or no-usage."""

    status: str
    low_usd: float | None = None
    high_usd: float | None = None


def cost_bounds(model: str, input_tokens: int, output_tokens: int) -> tuple[float, float] | None:
    price = PRICES.get(normalize_model(model))
    if price is None:
        return None
    low = (input_tokens * price.cache_read + output_tokens * price.output) / 1e6
    high_in = max(price.input * ONE_HOUR_CACHE_WRITE, price.long_input or 0.0)
    high_out = max(price.output, price.long_output or 0.0)
    high = (input_tokens * high_in + output_tokens * high_out) / 1e6 * REGIONAL_PREMIUM
    return low, high


def check_cost(
    model: str | None, input_tokens: int | None, output_tokens: int | None, cost_usd: float | None
) -> CostCheck:
    if cost_usd is None or input_tokens is None or output_tokens is None:
        return CostCheck("no-usage")
    if not model:
        return CostCheck("unknown-model")
    bounds = cost_bounds(model, input_tokens, output_tokens)
    if bounds is None:
        return CostCheck("unknown-model")
    low, high = bounds
    in_range = low - ABSOLUTE_SLACK_USD <= cost_usd <= high + ABSOLUTE_SLACK_USD
    return CostCheck("ok" if in_range else "out-of-range", round(low, 6), round(high, 6))
