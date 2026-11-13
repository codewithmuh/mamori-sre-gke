# Agent evaluation (#26, EVALS.md). Included by the Makefile; uses its require-vars and
# require-tools helpers. evals-run injects real faults into the lab, one at a time.

EVALS := uv run --project evals --locked mamori-evals
# Comma-separated fault ids, game-day, or all.
FAULTS ?=
EVALS_BUDGET ?= 6h
# Conditions typed for the session, space-separated KEY=VALUE, e.g. APPROVALS=none. AGENT_*
# switches are read from the agent Deployment and checked against these (see EVALS.md).
EVALS_LABELS ?=
# Local ports for the agent and executor port-forwards (18080 is the prober's).
EVALS_AGENT_PORT ?= 18181
EVALS_EXECUTOR_PORT ?= 18182
# Extra flags for `mamori-evals run`, e.g. --dry-run or --investigation-timeout 30m.
EVALS_ARGS ?=
# Built by `make gameday-build` (#23).
EVALS_GAMEDAY ?= bin/gameday
# Recordings to re-score; empty means the synthetic fixtures (a self-test of the scoring).
RECORDING ?=
# A results file for evals-report.
RESULTS ?=

POSTMORTEMS_DIR := docs/incidents/postmortems
EVALS_HUMAN_BASELINE = $(if $(wildcard $(POSTMORTEMS_DIR)/*.md),--human-baseline)

.PHONY: evals-faults evals-run evals-replay evals-report evals-check

evals-faults: ## List the fault library and check it has what the eval scoring needs
	@$(call require-tools,uv)
	$(EVALS) faults --faults-dir chaos/faults

evals-run: ## Live eval: FAULTS=<id,...>|game-day|all, one at a time (injects faults; see EVALS.md)
	$(call require-vars,PROJECT_ID FAULTS)
	@$(call require-tools,uv kubectl gcloud)
	@test -x "$(EVALS_GAMEDAY)" || { echo "error: $(EVALS_GAMEDAY) not found. Run: make gameday-build" >&2; exit 1; }
	$(EVALS) run --faults "$(FAULTS)" --project-id "$(PROJECT_ID)" --budget "$(EVALS_BUDGET)" \
		--gameday "$(EVALS_GAMEDAY)" --port-forward $(EVALS_HUMAN_BASELINE) \
		--agent-local-port $(EVALS_AGENT_PORT) --executor-local-port $(EVALS_EXECUTOR_PORT) \
		$(foreach label,$(EVALS_LABELS),--label "$(label)") $(EVALS_ARGS)

ifeq ($(strip $(RECORDING)),)
evals-replay: EVALS_REPLAY_ARGS = --recording evals/fixtures/recordings/synthetic-session.json \
	--faults-dir evals/fixtures/faults --postmortems evals/fixtures/postmortems --human-baseline \
	--baseline evals/fixtures/baselines/synthetic-other-tool.yaml
else
evals-replay: EVALS_REPLAY_ARGS = $(foreach file,$(RECORDING),--recording "$(file)") \
	--faults-dir chaos/faults $(EVALS_HUMAN_BASELINE)
endif
evals-replay: ## Re-score RECORDING="<file> ..." offline as one dataset (default: the synthetic fixtures)
	@$(call require-tools,uv)
	$(EVALS) replay $(EVALS_REPLAY_ARGS)

evals-report: ## Print the Markdown table of RESULTS=evals/results/<session>.results.json
	$(call require-vars,RESULTS)
	@$(call require-tools,uv)
	$(EVALS) report "$(RESULTS)"

evals-check: ## Lint, type-check and test the eval harness (same as CI)
	@$(call require-tools,uv)
	cd evals && uv sync --locked && uv run --locked ruff format --check . && \
		uv run --locked ruff check . && uv run --locked mypy && uv run --locked pytest
