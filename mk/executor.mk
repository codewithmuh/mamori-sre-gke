# Executor (#25): kill switch and day-2 commands. The Makefile includes this file
# (-include mk/*.mk). Runbook: docs/runbooks/executor.md.
#
# The kill switch is key "enabled" of ConfigMap executor-config. The executor reads it before
# accepting a proposal and again right before it runs an approved action. Argo CD ignores this
# key (deploy/apps/executor.yaml), so a change made here sticks.

EXECUTOR_NS ?= mamori-agent
# How far back executor-audit looks in the pod's log (kubectl logs --since).
SINCE ?= 24h

EXECUTOR_EVENTS := proposal_received|approved|rejected|executed|blocked|failed|approval_denied

.PHONY: executor-kill executor-enable executor-status executor-audit executor-slack-secret

executor-kill: ## Executor: kill switch ON. New proposals are refused; approved ones are blocked before they run
	@$(call require-tools,kubectl)
	kubectl -n $(EXECUTOR_NS) patch configmap executor-config --type merge -p '{"data":{"enabled":"false"}}'
	@echo "Executor is OFF. Check: make executor-status. Hard stop as well: kubectl -n $(EXECUTOR_NS) scale deploy/executor --replicas=0"

executor-enable: ## Executor: kill switch OFF. Approved actions may run again (each still needs a Slack approval)
	@$(call require-tools,kubectl)
	@approvers="$$(kubectl -n $(EXECUTOR_NS) get configmap executor-config -o jsonpath='{.data.config\.yaml}' | grep -E '^ *approvers:' || true)"; \
	if [ -z "$$approvers" ] || echo "$$approvers" | grep -q '\[\]'; then \
		echo "warning: executor-config lists no approvers, so every proposal is still refused (set them in Git)" >&2; fi
	@kubectl -n $(EXECUTOR_NS) get secret executor-slack >/dev/null 2>&1 || \
		echo "warning: Secret executor-slack is missing, so nobody can approve (make executor-slack-secret)" >&2
	kubectl -n $(EXECUTOR_NS) patch configmap executor-config --type merge -p '{"data":{"enabled":"true"}}'
	@echo "Executor is ON. Kill it again with: make executor-kill"

executor-status: ## Executor: kill switch, pod, open proposals
	@$(call require-tools,kubectl)
	@echo "enabled: $$(kubectl -n $(EXECUTOR_NS) get configmap executor-config -o jsonpath='{.data.enabled}')"
	@kubectl -n $(EXECUTOR_NS) get pods -l app.kubernetes.io/name=executor
	@state="$$(kubectl -n $(EXECUTOR_NS) get configmap executor-state -o jsonpath='{.data.state\.json}')"; \
	if [ -z "$$state" ]; then echo "no proposals yet"; \
	elif command -v jq >/dev/null 2>&1; then \
		echo "$$state" | jq -r '.proposals[] | select(.status == "pending" or .status == "approved") | [.proposalId, .status, .incidentId, .action.type, .action.target, .expiresAt] | @tsv'; \
	else echo "$$state"; fi

executor-audit: ## Executor: audit events from the running pod's log (SINCE=24h; older ones: Cloud Logging, see runbook)
	@$(call require-tools,kubectl)
	@kubectl -n $(EXECUTOR_NS) logs deploy/executor --since=$(SINCE) | grep -E '"event":"($(EXECUTOR_EVENTS))"' || echo "no audit events in the last $(SINCE)"

# Reads the tokens from the environment; printf is a shell builtin, so they never appear in a
# process list or in the shell history.
executor-slack-secret: ## Executor: create or replace Secret executor-slack from SLACK_APP_TOKEN and SLACK_BOT_TOKEN
	@$(call require-tools,kubectl)
	@[ -n "$${SLACK_APP_TOKEN:-}" ] && [ -n "$${SLACK_BOT_TOKEN:-}" ] || \
		{ echo "error: export SLACK_APP_TOKEN (xapp-...) and SLACK_BOT_TOKEN (xoxb-...) first" >&2; exit 1; }
	@kubectl -n $(EXECUTOR_NS) create secret generic executor-slack \
		--from-file=app-token=<(printf %s "$$SLACK_APP_TOKEN") \
		--from-file=bot-token=<(printf %s "$$SLACK_BOT_TOKEN") \
		--dry-run=client -o yaml | kubectl apply -f -
	kubectl -n $(EXECUTOR_NS) rollout restart deploy/executor
