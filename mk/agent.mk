# The incident agent (agent/, docs/agent.md). Included by the Makefile (-include mk/*.mk).
# Local targets need uv 0.13.6 (the dev container has it); cluster targets need kubectl with
# credentials for the lab (make up does that).

AGENT_DIR := agent
AGENT_NAMESPACE := mamori-agent
AGENT_IMAGE ?= mamori-agent:dev
AGENT_PORT ?= 8080

# The agent's Google Cloud grants (infra/bootstrap/agent_identity.tf) need the cluster's workload
# identity pool, which exists only after the first `make up`. Then run
# `TF_VAR_gke_workload_pool_exists=true make bootstrap` (the flag is shared with #19's grants).

.PHONY: agent-check agent-lint agent-test agent-image agent-demo agent-port-forward \
	agent-investigations agent-logs agent-slack-secret

agent-check: agent-lint agent-test ## Agent: ruff, mypy and the tests with the coverage gate (as CI)

agent-lint: ## Agent: ruff format check, ruff lint, mypy --strict
	@$(call require-tools,uv)
	cd $(AGENT_DIR) && uv sync --frozen && uv run ruff format --check src tests && \
		uv run ruff check src tests && uv run mypy

agent-test: ## Agent: pytest with the 80 % coverage gate
	@$(call require-tools,uv)
	cd $(AGENT_DIR) && uv sync --frozen && uv run pytest --cov --cov-fail-under=80

agent-image: ## Agent: build the image locally (context agent/ only)
	@$(call require-tools,docker)
	docker build -t $(AGENT_IMAGE) $(AGENT_DIR)

# Runs one investigation of a sample checkout alert from the laptop: Vertex AI with your ADC
# (or AGENT_PROVIDER=anthropic and ANTHROPIC_API_KEY), and the cluster through `kubectl proxy`.
# Nothing is proposed or posted. Costs one investigation (about $0.40, estimate).
agent-demo: ## Agent: one local investigation of a sample alert (needs kubectl proxy on :8001)
	$(call require-vars,PROJECT_ID)
	@$(call require-tools,uv)
	cd $(AGENT_DIR) && uv sync --frozen && \
		AGENT_PROJECT_ID=$(PROJECT_ID) AGENT_K8S_API_URL=http://127.0.0.1:8001 AGENT_K8S_TOKEN_FILE= \
		AGENT_KNOWLEDGE_SOURCE=local AGENT_REPO_DIR=.. \
		uv run python -m mamori_agent investigate --demo --no-propose --no-slack

agent-port-forward: ## Agent: forward the API to localhost:$(AGENT_PORT)
	@$(call require-tools,kubectl)
	kubectl -n $(AGENT_NAMESPACE) port-forward svc/agent $(AGENT_PORT):8080

agent-investigations: ## Agent: list recent investigations (run agent-port-forward first)
	@$(call require-tools,curl)
	curl -fsS "http://127.0.0.1:$(AGENT_PORT)/v1/investigations?limit=20"

agent-logs: ## Agent: the investigation_completed log lines of the running pod
	@$(call require-tools,kubectl)
	kubectl -n $(AGENT_NAMESPACE) logs deploy/agent --since=24h | grep '"event": "investigation_completed"' || true

# Reads SLACK_BOT_TOKEN and SLACK_CHANNEL from the environment so the token never lands in Git,
# Terraform state or shell history arguments shown by ps.
agent-slack-secret: ## Agent: create or replace Secret agent-slack (SLACK_BOT_TOKEN, SLACK_CHANNEL in env)
	@$(call require-tools,kubectl)
	@test -n "$${SLACK_BOT_TOKEN:-}" && test -n "$${SLACK_CHANNEL:-}" || \
		{ echo "error: export SLACK_BOT_TOKEN (xoxb-...) and SLACK_CHANNEL (channel ID) first" >&2; exit 1; }
	@printf 'bot-token=%s\nchannel=%s\n' "$$SLACK_BOT_TOKEN" "$$SLACK_CHANNEL" | \
		kubectl -n $(AGENT_NAMESPACE) create secret generic agent-slack --from-env-file=/dev/stdin \
		--dry-run=client -o yaml | kubectl apply -f -
	kubectl -n $(AGENT_NAMESPACE) rollout restart deploy/agent
