# agent

The lab's AI incident agent (PLAN.md section 4, decisions D8, D9 and D13). When a user-journey
SLO alert fires, it works out which journeys are hurt and how badly, ranks the likely root causes
with evidence, and proposes at most one action. It never changes anything: the proposal goes to
the executor (#25), which asks a human in Slack and acts with its own narrow permissions.

How it works, the guardrails, the cost model and the account upgrade it needs are in
[docs/agent.md](../docs/agent.md). This page is for working on the code.

## Layout

```
agent/
├── pyproject.toml, uv.lock     exact pins (uv 0.13.6, Python 3.13)
├── Dockerfile                  distroless, non-root, read-only root filesystem; context agent/ only
├── src/mamori_agent/
│   ├── cli.py, __main__.py     serve | investigate | parse-incident
│   ├── app.py                  builds everything from Settings
│   ├── config.py               AGENT_* settings
│   ├── pubsub.py               pull trigger on mamori-agent-incidents
│   ├── incidents.py            Cloud Monitoring / Alertmanager / manual payloads, first triage
│   ├── service.py              intake (dedupe, daily budget, queue), worker, record, proposal, Slack
│   ├── investigator.py         the model loop and its guardrails
│   ├── prompts.py              system prompt and incident message
│   ├── llm.py                  Vertex AI or Anthropic client, list prices, usage meter
│   ├── models.py               incident, findings schema, investigation record (the contract)
│   ├── tools/                  read-only tools: gcp.py, kube.py, knowledge.py, registry
│   ├── safety.py               untrusted-data fencing, redaction, label stripping, Slack escaping
│   ├── executor.py, slack.py   the two outputs
│   ├── server.py, metrics.py, logs.py
└── tests/                      fakes for every external call
```

## Develop

```sh
cd agent
uv sync --frozen
uv run ruff format --check src tests && uv run ruff check src tests
uv run mypy
uv run pytest --cov            # 80 % gate, as CI (.github/workflows/agent.yml)
```

Or `make agent-check` from the repository root. No network is needed for the tests.

Updating dependencies: move `exclude-newer` in pyproject.toml to two weeks before today, change
the pins, `uv lock`, and run the checks.

## Run one investigation from a laptop

```sh
kubectl proxy &                                    # the agent's Kubernetes reads go through it
gcloud auth application-default login              # Vertex AI and the Google read APIs
cd agent
AGENT_PROJECT_ID=<project> AGENT_K8S_API_URL=http://127.0.0.1:8001 AGENT_K8S_TOKEN_FILE= \
AGENT_KNOWLEDGE_SOURCE=local AGENT_REPO_DIR=.. \
  uv run python -m mamori_agent investigate --demo --no-propose --no-slack
```

`--incident-file x.json` takes a Cloud Monitoring Pub/Sub message, an Alertmanager alert or a
manual body instead of the demo alert. Without Vertex AI access, `AGENT_PROVIDER=anthropic` with
`ANTHROPIC_API_KEY` set uses the Anthropic API; that path refuses to start inside a pod (D8: no
API key in the cluster). Each run costs about $0.30 to $0.40 (estimate, docs/agent.md).

## Settings

All environment variables, all optional. The Deployment (`deploy/agent/deployment.yaml`) sets
the cluster values.

| Variable | Default | What |
|---|---|---|
| `AGENT_PROVIDER` | `vertex` | `vertex` (Workload Identity / ADC) or `anthropic` (API key, local only) |
| `AGENT_PROJECT_ID` | from ADC | Google Cloud project for Vertex AI and the read APIs |
| `AGENT_VERTEX_REGION` | `global` | Vertex AI endpoint; Tokyo has no regional endpoint for the newest models, and the others cost 10 % more |
| `AGENT_MODEL` | `claude-opus-5-5` | model id (Vertex AI uses the bare id) |
| `AGENT_FALLBACK_MODEL` | `claude-opus-5` | retried once on a refusal; empty disables |
| `AGENT_EFFORT` | `medium` | `low` to `max`; set explicitly because the default differs per model |
| `AGENT_MAX_TOOL_CALLS` | `25` | per investigation |
| `AGENT_MAX_MODEL_CALLS` | `20` | per investigation |
| `AGENT_MAX_TOTAL_TOKENS` | `800000` | input (incl. cache) + output, per investigation |
| `AGENT_MAX_COST_USD` | `1.50` | per investigation, at list prices |
| `AGENT_DAILY_COST_LIMIT_USD` | `15` | new incidents are refused for the rest of the UTC day |
| `AGENT_INVESTIGATION_TIMEOUT_SECONDS` | `600` | wall time; the answer is requested 120 s before |
| `AGENT_MODEL_TIMEOUT_SECONDS` / `AGENT_TOOL_TIMEOUT_SECONDS` | `180` / `20` | per call |
| `AGENT_TOOL_OUTPUT_MAX_CHARS` | `6000` | per tool result |
| `AGENT_MAX_TOKENS_PER_CALL` | `16000` | thinking plus answer |
| `AGENT_PUBSUB_ENABLED`, `AGENT_SUBSCRIPTION` | `true`, `mamori-agent-incidents` | the trigger |
| `AGENT_DEDUPE_TTL_SECONDS`, `AGENT_QUEUE_SIZE` | `86400`, `10` | intake |
| `AGENT_PROPOSE`, `AGENT_EXECUTOR_URL`, `AGENT_EXECUTOR_TOKEN_FILE` | `true`, `http://executor.mamori-agent.svc:8080`, `/var/run/secrets/executor/token` | the proposal |
| `AGENT_SLACK`, `AGENT_SLACK_BOT_TOKEN`, `AGENT_SLACK_CHANNEL` | `true`, empty, empty | the summary; skipped while token or channel is empty |
| `AGENT_K8S_API_URL`, `AGENT_K8S_TOKEN_FILE`, `AGENT_K8S_CA_FILE` | in-cluster values | the Kubernetes API |
| `AGENT_LOG_NAMESPACES` | `boutique,istio-ingress` | where pod logs may be read |
| `AGENT_KNOWLEDGE_SOURCE`, `AGENT_REPO_DIR`, `AGENT_GITHUB_REPO`, `AGENT_GITHUB_REF` | `github`, `.`, `codewithmuh/mamori-sre-gke`, `main` | CUJ map, SLO spec, postmortems |
| `AGENT_POSTMORTEMS_GAMEDAYS` | `include` | `exclude` hides postmortems that have a `faultId` (evaluation runs) |
| `AGENT_HIDE_FAULT_LABELS` | `false` | strip `mamori.dev/*` labels from tool output (blind runs) |
| `AGENT_CHAOS_TOOL` | `true` | the Chaos Mesh read tool |
| `AGENT_HTTP_PORT`, `AGENT_STORE_SIZE`, `AGENT_LOG_LEVEL` | `8080`, `200`, `INFO` | service |

## What came over from ai-sre-investigator

D13 moves that project in as `agent/`. It ran on kind with a local Prometheus, Loki and a
breakable demo app, called Claude with an API key and could run a `kubectl` fix itself after
`--approve`. Kept, rewritten for GKE: the tool-use loop (now with caps, a deadline, a refusal
fallback and structured output), the read-only tool set and its output caps, the PromQL
instant/range summaries, the recent-deploys check (now rollout history with diffs and Argo CD
history), the Alertmanager payload parser, the Slack Block Kit summary (now escaped), the demo
alert and the CLI.

Dropped, and why:

| Source | Why it's gone |
|---|---|
| `deploy/demo-app/` (payments app, PrometheusRule, ServiceMonitor), `deploy/kind-cluster.yaml`, `deploy/monitoring/` (kube-prometheus-stack, loki-stack values) | Online Boutique, GKE, Managed Prometheus, Cloud Logging and the game-day faults (#23) replace the kind lab |
| `scripts/` (setup, break, loadgen, demo, port-forward, teardown) | kind lab scripts; `make up` and the game-day CLI replace them |
| `agent/integrations/loki.py` | Loki is replaced by Cloud Logging (`logs_search`) |
| `agent/integrations/alertmanager.py` (list firing alerts) | there is no Alertmanager; alerts arrive on Pub/Sub. The payload parser is kept in `incidents.py` |
| `agent/integrations/kubernetes.py` (kubectl subprocess with a verb allow-list) | no kubectl in a distroless image; a GET-only API client plus read-only RBAC is a stronger limit than filtering kubectl arguments |
| `cli.py remediate --approve` and `.last_remediation.json` | the agent must not act (D9); the executor does, after a human approves |
| `server/main.py` (FastAPI Alertmanager webhook) | the trigger is a Pub/Sub pull, so nothing calls into the cluster; the small API runs on the standard library (fewer dependencies) |
| `report.py` console rendering (rich) | output is the JSON record; rich is no longer a dependency |
| `requirements.txt` ranges, `.env.example`, `Makefile`, `.gitignore` | exact pins in `pyproject.toml` + `uv.lock`; settings table above; `mk/agent.mk`; the repository's `.gitignore` |
| `docs/DEMO_SCRIPT.md`, README claims | a video script for the kind demo; not part of this lab |
| JSON-in-a-code-fence answer parsing | replaced by structured outputs (`output_config.format`) |
