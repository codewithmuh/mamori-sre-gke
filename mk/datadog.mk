# The 14-day Datadog window (docs/datadog-window.md): the Agent in the cluster, and the Terraform
# root infra/envs/datadog (synthetics, monitors, SLOs, Google Cloud integration). Included by the
# Makefile, so PROJECT_ID, ZONE, TF_STATE_BUCKET, CLUSTER and the require-* helpers come from there.
#
# Keys come only from the environment: export DD_API_KEY and DD_APP_KEY in your shell (from your
# password manager), never in .env or on the make command line. Recipes pass them to curl on stdin
# and to kubectl through a pipe, so they don't show up in a process list or in make's output.

DATADOG_DIR := infra/envs/datadog
DATADOG_APP := deploy/optional/datadog/application.yaml
DATADOG_VALUES := deploy/optional/datadog/values.yaml
DATADOG_NS := datadog
# AP1 is Datadog's site in Japan. Must equal datadog.site in $(DATADOG_VALUES).
DD_SITE ?= ap1.datadoghq.com

datadog-kubectl = kubectl --context=gke_$(PROJECT_ID)_$(ZONE)_$(or $(CLUSTER),mamori)

datadog-require-api-key = : "$${DD_API_KEY:?export DD_API_KEY (a Datadog API key for $(DD_SITE)) in your shell first}"
datadog-require-keys = $(datadog-require-api-key); \
	: "$${DD_APP_KEY:?export DD_APP_KEY (a Datadog application key for $(DD_SITE)) in your shell first}"

# curl against Datadog's API with both keys as headers read from stdin (-H @-).
datadog-curl = printf 'DD-API-KEY: %s\nDD-APPLICATION-KEY: %s\n' "$$DD_API_KEY" "$$DD_APP_KEY" | curl -sS -H @-

# $(call datadog-delegate-sh,get|create) prints Datadog's delegate service account, the account
# that impersonates ours (one per Datadog organisation). "create" makes it when the organisation
# has none yet, as the provider would on its first apply; "get" never changes anything.
define datadog-delegate-sh
url="https://api.$(DD_SITE)/api/v2/integration/gcp/sts_delegate"; out="$$(mktemp)"; \
code="$$($(datadog-curl) -o "$$out" -w '%{http_code}' "$$url")"; \
if [ "$$code" = 404 ] && [ "$(1)" = create ]; then \
	code="$$($(datadog-curl) -X POST -H 'Content-Type: application/json' -d '{}' -o "$$out" -w '%{http_code}' "$$url")"; \
fi; \
if [ "$$code" = 404 ]; then echo "error: no Datadog delegate yet; run: make datadog-delegate" >&2; rm -f "$$out"; exit 1; fi; \
if [ "$$code" != 200 ]; then echo "error: Datadog answered HTTP $$code for $$url: $$(cat "$$out")" >&2; rm -f "$$out"; exit 1; fi; \
email="$$(sed -n 's/.*"delegate_account_email" *: *"\([^"]*\)".*/\1/p' "$$out")"; rm -f "$$out"; \
[ -n "$$email" ] || { echo "error: no delegate_account_email in Datadog's answer" >&2; exit 1; }; \
echo "$$email"
endef

datadog-init = terraform -chdir=$(DATADOG_DIR) init -input=false -reconfigure \
	-backend-config="bucket=$(TF_STATE_BUCKET)" -backend-config="prefix=datadog"

.PHONY: datadog-delegate datadog-plan datadog-apply datadog-destroy datadog-on datadog-off

datadog-delegate: ## Print Datadog's delegate service account (creates it once per Datadog org)
	@$(datadog-require-keys)
	@$(call require-tools,curl)
	@$(call datadog-delegate-sh,create)

datadog-plan: ## Show what datadog-apply would change in Datadog and Google Cloud
	$(call require-vars,PROJECT_ID TF_STATE_BUCKET)
	@$(call require-tools,gcloud terraform curl)
	@$(require-gcloud-login)
	@$(datadog-require-keys)
	$(datadog-init)
	@delegate="$$($(call datadog-delegate-sh,get))" || exit 1; \
	TF_VAR_datadog_site="$(DD_SITE)" TF_VAR_datadog_delegate_email="$$delegate" \
		terraform -chdir=$(DATADOG_DIR) plan -input=false

datadog-apply: ## Create the window's synthetics, monitors, SLOs and Google Cloud integration
	$(call require-vars,PROJECT_ID TF_STATE_BUCKET)
	@$(call require-tools,gcloud terraform curl)
	@$(require-gcloud-login)
	@$(datadog-require-keys)
	$(datadog-init)
	@delegate="$$($(call datadog-delegate-sh,create))" || exit 1; \
	TF_VAR_datadog_site="$(DD_SITE)" TF_VAR_datadog_delegate_email="$$delegate" \
		terraform -chdir=$(DATADOG_DIR) apply -input=false $(TF_APPLY_ARGS)

# End of the window, before the trial ends: removes everything datadog-apply made, including the
# Google service account and its role bindings.
datadog-destroy: ## Delete everything datadog-apply made (end of the window)
	$(call require-vars,PROJECT_ID TF_STATE_BUCKET)
	@$(call require-tools,gcloud terraform curl)
	@$(require-gcloud-login)
	@$(datadog-require-keys)
	$(datadog-init)
	@delegate="$$($(call datadog-delegate-sh,get))" || exit 1; \
	TF_VAR_datadog_site="$(DD_SITE)" TF_VAR_datadog_delegate_email="$$delegate" \
		terraform -chdir=$(DATADOG_DIR) destroy -input=false

# The Application's values come from main on GitHub, so this works once deploy/optional/datadog is
# merged. The cluster-agent token is made once and kept, so re-running doesn't restart the Agents.
datadog-on: ## Install the Datadog Agent: Secrets from DD_API_KEY, then the Argo CD app
	$(call require-vars,PROJECT_ID ZONE)
	@$(call require-tools,gcloud gke-gcloud-auth-plugin kubectl openssl)
	@$(datadog-require-api-key)
	@grep -qxF '  site: $(DD_SITE)' $(DATADOG_VALUES) || \
		{ echo "error: datadog.site in $(DATADOG_VALUES) is not $(DD_SITE); make them agree first" >&2; exit 1; }
	gcloud container clusters get-credentials $(or $(CLUSTER),mamori) --zone $(ZONE) --project $(PROJECT_ID)
	$(datadog-kubectl) create namespace $(DATADOG_NS) --dry-run=client -o yaml | $(datadog-kubectl) apply -f -
	@echo "Secret $(DATADOG_NS)/datadog-secret (api-key from DD_API_KEY)"
	@printf '%s' "$$DD_API_KEY" | $(datadog-kubectl) -n $(DATADOG_NS) create secret generic datadog-secret \
		--from-file=api-key=/dev/stdin --dry-run=client -o yaml | \
		$(datadog-kubectl) apply --server-side --field-manager=make-datadog-on -f - >/dev/null
	@if ! $(datadog-kubectl) -n $(DATADOG_NS) get secret datadog-cluster-agent-token >/dev/null 2>&1; then \
		echo "Secret $(DATADOG_NS)/datadog-cluster-agent-token (new random token)"; \
		openssl rand -hex 32 | tr -d '\n' | $(datadog-kubectl) -n $(DATADOG_NS) create secret generic \
			datadog-cluster-agent-token --from-file=token=/dev/stdin >/dev/null; \
	fi
	$(datadog-kubectl) apply -f $(DATADOG_APP)
	$(datadog-kubectl) -n argocd wait applications.argoproj.io/datadog \
		--for=jsonpath='{.status.health.status}'=Healthy --timeout=10m
	$(datadog-kubectl) -n $(DATADOG_NS) rollout status daemonset/datadog --timeout=10m
	$(datadog-kubectl) -n $(DATADOG_NS) rollout status deployment/datadog-cluster-agent --timeout=5m

# The Application's finalizer deletes everything it deployed first; then the Secrets and the
# namespace, which Argo CD did not create.
datadog-off: ## Remove the Datadog Agent, its Secrets and namespace from the cluster
	$(call require-vars,PROJECT_ID ZONE)
	@$(call require-tools,gcloud gke-gcloud-auth-plugin kubectl)
	gcloud container clusters get-credentials $(or $(CLUSTER),mamori) --zone $(ZONE) --project $(PROJECT_ID)
	$(datadog-kubectl) -n argocd delete applications.argoproj.io datadog --ignore-not-found --wait --timeout=10m
	$(datadog-kubectl) -n $(DATADOG_NS) delete secret datadog-secret datadog-cluster-agent-token --ignore-not-found
	$(datadog-kubectl) delete namespace $(DATADOG_NS) --ignore-not-found --wait --timeout=5m
