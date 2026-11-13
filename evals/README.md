# evals

The evaluation harness for the incident agent: it injects faults from the fault library into the lab one at a time,
records what the agent and the executor did, and scores it. The method, the metric definitions and the results are in
[EVALS.md](../EVALS.md); this file is about the code. See R8 and P2 in [PLAN.md](../PLAN.md).

Python 3.13, one runtime dependency (PyYAML); HTTP and processes use the standard library.

## Commands

From the repository root (the `mk/evals.mk` targets wrap these):

```sh
uv run --project evals mamori-evals faults                # list chaos/faults and check what scoring needs
uv run --project evals mamori-evals run --faults game-day --project-id "$PROJECT_ID" --port-forward \
    --label AGENT_POSTMORTEMS_GAMEDAYS=exclude            # live session: injects real faults
uv run --project evals mamori-evals replay --recording evals/results/<session>.recording.json
uv run --project evals mamori-evals report evals/results/<session>.results.json
```

`run --dry-run` prints the plan and touches nothing. `run --help` lists every timeout.

## Layout

| Module | What it does |
|---|---|
| `faults.py` | Reads `chaos/faults/*.yaml` (#23) and checks the fields scoring needs |
| `records.py` | Parses what the eval observes: investigations (#24), proposals and audit lines (#25), runs |
| `scoring.py` | Pure functions: one run + its fault -> scores; matching an investigation to a run |
| `aggregate.py` | Rates as k/n with a Wilson 95 % interval; durations as median, range and n |
| `pricing.py` | List prices, for the check that the agent's reported cost is possible |
| `baseline.py` | Human baseline from game-day postmortems (#20); baseline files for other tools |
| `live.py` | The live runner: steady state, inject, wait, read, stop, cool down |
| `clients.py` | Agent and executor APIs, Managed Prometheus, the gameday CLI, the executor's audit log |
| `steady.py` | Steady state from the prober SLIs (#16) |
| `replay.py`, `report.py`, `cli.py` | Offline scoring, the results JSON and Markdown, the command line |

## Files

A live session writes three files to `evals/results/` (gitignored), named after the session's start time:

- `<session>.recording.json` (`kind: EvalRecording`): the raw observations, written again after every run so a
  crash keeps what was done. It holds the investigations and audit lines exactly as the APIs returned them.
- `<session>.results.json` (`kind: EvalResults`): per-run scores, the summary and the baselines.
- `<session>.md`: the Markdown table for `EVALS.md`.

`replay` re-scores recordings with the current code, so a scoring fix never needs the cluster again. Several
`--recording` flags make one dataset; it refuses to mix synthetic fixtures with lab sessions, or sessions run under
different `--label` conditions.

A baseline file for another tool (for example HolmesGPT on the same faults), passed with `--baseline`:

```yaml
apiVersion: mamori/v1
kind: EvalBaseline
name: HolmesGPT 0.46.0
system: holmesgpt
source: where the runs came from
runs:
  - faultId: F01-currency-abort
    faultStart: "2026-12-10T05:00:00Z"
    detectedAt: "2026-12-10T05:03:00Z"      # optional
    rootCauseAt: "2026-12-10T05:09:00Z"     # when its answer was ready
    hypotheses: [{rank: 1, service: currencyservice}]
    costUsd: 0.31                           # optional
    blind: true                             # optional
```

## Fixtures

[`fixtures/`](fixtures/README.md) holds **synthetic** faults, one recorded session, postmortems and a baseline file.
They exist to test the scoring before the lab runs; nothing derived from them is a result.

## Development

```sh
cd evals
uv sync --locked
uv run ruff format --check . && uv run ruff check . && uv run mypy && uv run pytest   # what CI runs
```

Tests need no cluster: the live runner is tested with fakes, the gameday CLI with a stand-in script
(`tests/fake_gameday.py`), the APIs with a local HTTP server. Coverage must stay at 80 % or more.
