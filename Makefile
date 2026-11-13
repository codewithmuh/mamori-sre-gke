# One entry point for the lab. Settings come from the environment, the make command line, or a
# gitignored .env (template: .env.example). Run `make` to list the targets.
SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

CONFIG_VARS := PROJECT_ID ZONE TF_STATE_BUCKET FRONTEND_HOSTNAME BILLING_ACCOUNT_ID

# Precedence: make command line, then the environment, then .env, then the defaults below.
# Make would let .env override the environment, so non-empty environment values are restored.
$(foreach v,$(CONFIG_VARS),$(if $(filter environment,$(origin $(v))),$(eval saved.$(v) := $$($(v)))))
-include .env
$(foreach v,$(CONFIG_VARS),$(if $(strip $(saved.$(v))),$(eval $(v) := $$(saved.$(v)))))
export $(CONFIG_VARS)

# Defaults for settings left empty.
ZONE := $(or $(strip $(ZONE)),asia-northeast1-a)
FRONTEND_HOSTNAME := $(or $(strip $(FRONTEND_HOSTNAME)),mamori.codewithmuh.com)
TF_STATE_BUCKET := $(or $(strip $(TF_STATE_BUCKET)),$(if $(strip $(PROJECT_ID)),$(strip $(PROJECT_ID))-tfstate))

# Terraform reads its variables from TF_VAR_*. A root that does not declare one ignores it.
ifneq ($(strip $(PROJECT_ID)),)
export TF_VAR_project_id := $(strip $(PROJECT_ID))
endif
export TF_VAR_zone := $(ZONE)
export TF_VAR_frontend_hostname := $(FRONTEND_HOSTNAME)
ifneq ($(strip $(BILLING_ACCOUNT_ID)),)
export TF_VAR_billing_account_id := $(strip $(BILLING_ACCOUNT_ID))
endif

BOOTSTRAP_DIR := infra/bootstrap
LAB_DIR := infra/envs/lab
CLUSTER := mamori
NODE_POOL := spot-pool
NODE_COUNT := 3
# Argo CD Helm chart (app v3.5.6). Argo CD does not manage itself, so this is its only pin.
ARGOCD_CHART_VERSION := 10.11.6
GO_MODULES := $(patsubst %/go.mod,%,$(wildcard tools/*/go.mod))
# Forked upstream services: tests only, no lint and no coverage gate (as in CI).
SERVICE_MODULES := $(patsubst %/go.mod,%,$(wildcard services/*/go.mod))

# Tools each lint part needs, only once the code it checks exists.
TF_LINT_TOOLS := $(if $(wildcard infra),terraform tflint)
MANIFEST_LINT_TOOLS := $(if $(wildcard deploy/validate.sh),helm kubectl)
GO_LINT_TOOLS := $(if $(GO_MODULES),golangci-lint)

# Extra flags for `terraform apply` in `make up`, e.g. TF_APPLY_ARGS=-auto-approve.
TF_APPLY_ARGS ?=

# --- Helpers ---------------------------------------------------------------------------------

# $(call require-vars,A B): stop before anything runs when a setting is empty.
require-vars = $(foreach v,$(1),$(if $(strip $($(v))),,$(error $(v) is not set. Put it in .env (see .env.example) or run: make $@ $(v)=<value>)))

# $(call require-tools,a b): fail with one clear message listing every missing command.
require-tools = missing=""; for t in $(1); do command -v "$$t" >/dev/null 2>&1 || missing="$$missing $$t"; done; \
	if [ -n "$$missing" ]; then echo "error: make $@ needs:$$missing (not on PATH; the dev container has them all)" >&2; exit 1; fi

# Terraform uses Application Default Credentials; the gcloud CLI uses its own login.
require-gcloud-login = gcloud auth application-default print-access-token >/dev/null 2>&1 || \
	{ echo "error: no Application Default Credentials. Run: gcloud auth application-default login" >&2; exit 1; }; \
	gcloud auth print-access-token >/dev/null 2>&1 || \
	{ echo "error: gcloud is not logged in. Run: gcloud auth login" >&2; exit 1; }

# `make down` asks once, up front; YES=1 skips the question.
confirm-down = if [ "$(YES)" != 1 ]; then \
	read -r -p "This destroys the lab in $(PROJECT_ID). Type the project ID to go on: " answer; \
	[ "$$answer" = "$(PROJECT_ID)" ] || { echo "Aborted: nothing was changed." >&2; exit 1; }; fi

lab-init = terraform -chdir=$(LAB_DIR) init -input=false -reconfigure \
	-backend-config="bucket=$(TF_STATE_BUCKET)" -backend-config="prefix=lab"

# Point kubectl at the cluster with the command Terraform outputs, instead of rebuilding it here.
lab-credentials = eval "$$(terraform -chdir=$(LAB_DIR) output -raw get_credentials_command)"

.PHONY: help bootstrap up down resume edge-off edge-on plan fmt lint lint-tools lint-shell \
	lint-terraform lint-manifests lint-go test check scan clean

# --- Targets ---------------------------------------------------------------------------------

help: ## Show the available commands
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(firstword $(MAKEFILE_LIST)) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

# The GitHub environment comes first: GitHub would create it unprotected the first time a
# workflow names it, and it is what guards tf-apply.
bootstrap: ## Once per project: GitHub env lab, state bucket, CI identities, APIs; prints GitHub settings
	$(call require-vars,PROJECT_ID BILLING_ACCOUNT_ID)
	@$(call require-tools,gcloud terraform gh)
	@$(require-gcloud-login)
	scripts/github-lab-env.sh
	terraform -chdir=$(BOOTSTRAP_DIR) init -input=false
	terraform -chdir=$(BOOTSTRAP_DIR) apply -input=false
	@scripts/print-repo-vars.sh

up: ## Build the lab from nothing: infra, Argo CD, the shop; prints the URL and DNS records
	$(call require-vars,PROJECT_ID TF_STATE_BUCKET BILLING_ACCOUNT_ID)
	@$(call require-tools,gcloud gke-gcloud-auth-plugin terraform kubectl helm)
	@$(require-gcloud-login)
	$(lab-init)
	terraform -chdir=$(LAB_DIR) apply -input=false $(TF_APPLY_ARGS)
	$(lab-credentials)
	helm upgrade --install argocd argo-cd --repo https://argoproj.github.io/argo-helm \
		--version $(ARGOCD_CHART_VERSION) --namespace argocd --create-namespace \
		--values deploy/bootstrap/argocd-values.yaml --wait --timeout 10m
	kubectl apply -f deploy/bootstrap/root-app.yaml
	scripts/wait-for-shop.sh
	@echo
	@echo "Shop: https://$(FRONTEND_HOSTNAME)  (load balancer IP: $$(terraform -chdir=$(LAB_DIR) output -raw frontend_ip))"
	@echo "HTTPS works once these DNS records exist where codewithmuh.com is hosted and the certificate is active:"
	@terraform -chdir=$(LAB_DIR) output -raw dns_records_to_add 2>/dev/null || terraform -chdir=$(LAB_DIR) output dns_records_to_add

# Order matters. The cluster's own controllers built the load balancer, so it must go while the
# cluster still runs: destroying the cluster first would leave it behind, still billed.
#   1. delete-shop.sh: the Argo CD root app (cascades to every app; skipped when no node runs),
#      Gateway mamori-frontend and the istio-ingress Services behind it, without waiting
#   2. wait-for-lb-gone.sh: wait until the GKE controllers have released the load balancer, and
#      clear GKE-made firewall rules that would block deleting the VPC
#   3. terraform destroy, then the leftover check
down: ## Delete the shop and its load balancer, destroy the lab, then fail if anything billable is left
	$(call require-vars,PROJECT_ID TF_STATE_BUCKET BILLING_ACCOUNT_ID)
	@$(call require-tools,gcloud gke-gcloud-auth-plugin terraform kubectl)
	@$(require-gcloud-login)
	@$(confirm-down)
	$(lab-init)
	if terraform -chdir=$(LAB_DIR) output -raw get_credentials_command >/dev/null 2>&1 && $(lab-credentials); then \
		scripts/delete-shop.sh; \
	else \
		echo "No reachable cluster in the Terraform state; skipping the in-cluster cleanup."; \
	fi
	scripts/wait-for-lb-gone.sh "$$(terraform -chdir=$(LAB_DIR) output -raw frontend_ip 2>/dev/null || true)"
	terraform -chdir=$(LAB_DIR) destroy -input=false -auto-approve
	scripts/leftover-check.sh

# The spend guard fires again on the next budget alert while spend is still over the threshold:
# raise the budget (or the guard's THRESHOLD) first.
resume: ## Scale node pool spot-pool back to 3 nodes after the spend guard set it to 0
	$(call require-vars,PROJECT_ID ZONE)
	@$(call require-tools,gcloud)
	gcloud container clusters resize $(CLUSTER) --node-pool=$(NODE_POOL) --num-nodes=$(NODE_COUNT) \
		--zone=$(ZONE) --project=$(PROJECT_ID) --quiet

# Edge cost switch. Cloud Armor and load balancer logging are billed per request, denied ones
# included, and the spend guard can't stop that (it only scales nodes). edge-off turns off Argo CD
# auto-sync for root and edge (so self-heal can't bring the Gateway back), deletes Gateway
# mamori-frontend and waits until the load balancer is gone. The static IP, certificate and
# Cloud Armor policy stay (Terraform owns them), so DNS needs no change. edge-on re-applies the
# root app, which turns auto-sync back on; Argo CD restores the edge app and the Gateway.
edge-off: ## Stop public traffic costs: delete the Gateway and wait until its load balancer is gone
	$(call require-vars,PROJECT_ID TF_STATE_BUCKET)
	@$(call require-tools,gcloud gke-gcloud-auth-plugin terraform kubectl)
	@$(require-gcloud-login)
	$(lab-init)
	$(lab-credentials)
	kubectl -n argocd patch applications.argoproj.io root --type merge -p '{"spec":{"syncPolicy":{"automated":null}}}'
	kubectl -n argocd patch applications.argoproj.io edge --type merge -p '{"spec":{"syncPolicy":{"automated":null}}}'
	kubectl delete gateways.gateway.networking.k8s.io --all-namespaces --field-selector metadata.name=mamori-frontend --wait=false
	scripts/wait-for-lb-gone.sh "$$(terraform -chdir=$(LAB_DIR) output -raw frontend_ip)"
	@echo "Edge is off: no load balancer, no public traffic. 'make edge-on' brings it back."

edge-on: ## Bring the public edge back after edge-off (Argo CD re-syncs the Gateway)
	$(call require-vars,PROJECT_ID TF_STATE_BUCKET)
	@$(call require-tools,gcloud gke-gcloud-auth-plugin terraform kubectl)
	@$(require-gcloud-login)
	$(lab-init)
	$(lab-credentials)
	kubectl apply -f deploy/bootstrap/root-app.yaml
	scripts/wait-for-shop.sh
	@echo "Edge is on: https://$(FRONTEND_HOSTNAME)"

plan: ## Show what `make up` would change in the lab (terraform plan)
	$(call require-vars,PROJECT_ID TF_STATE_BUCKET BILLING_ACCOUNT_ID)
	@$(call require-tools,gcloud terraform)
	@$(require-gcloud-login)
	$(lab-init)
	terraform -chdir=$(LAB_DIR) plan -input=false

fmt: ## Format Terraform and Go code
	@$(call require-tools,$(if $(wildcard infra),terraform) $(if $(GO_MODULES),gofmt))
	@if [ -d infra ]; then terraform fmt -recursive infra; fi
	@for mod in $(GO_MODULES); do gofmt -l -w "$$mod"; done

# The lint-* parts also run on their own; CI calls lint-terraform and lint-manifests.
lint: lint-tools lint-shell lint-terraform lint-manifests lint-go ## Lint shell scripts, Terraform, Kubernetes manifests and Go (as CI does)

lint-tools:
	@$(call require-tools,shellcheck $(TF_LINT_TOOLS) $(MANIFEST_LINT_TOOLS) $(GO_LINT_TOOLS))

lint-shell:
	@$(call require-tools,shellcheck)
	shellcheck scripts/*.sh

lint-terraform:
	@$(call require-tools,$(TF_LINT_TOOLS))
	scripts/lint-terraform.sh

# deploy/validate.sh (owned with the manifests) renders every app and runs kubeconform; it takes
# the Argo CD chart version from here so the pin lives in one place.
lint-manifests:
	@$(call require-tools,$(MANIFEST_LINT_TOOLS))
	@if [ -f deploy/validate.sh ]; then \
		echo "ARGOCD_CHART_VERSION=$(ARGOCD_CHART_VERSION) bash deploy/validate.sh"; \
		ARGOCD_CHART_VERSION=$(ARGOCD_CHART_VERSION) bash deploy/validate.sh; \
	else \
		echo "No deploy/validate.sh yet; skipping the manifest checks."; \
	fi

lint-go:
	@$(call require-tools,$(GO_LINT_TOOLS))
	@for mod in $(GO_MODULES); do echo "golangci-lint $$mod"; (cd "$$mod" && golangci-lint run ./...); done

test: ## Run the Go tests with the race detector, tools/ with the 80% coverage gate (same as CI)
	@$(if $(GO_MODULES)$(SERVICE_MODULES),$(call require-tools,go),echo "No Go modules under tools/ or services/ yet.")
	@for mod in $(GO_MODULES); do scripts/go-test.sh "$$mod"; done
	@for mod in $(SERVICE_MODULES); do echo "go test -race $$mod"; (cd "$$mod" && go test -race ./...); done

check: lint test ## Everything CI checks before merge

scan: ## Secret and vulnerability scans (needs gitleaks and trivy)
	gitleaks git --redact --no-banner .
	trivy fs --scanners vuln,misconfig,secret --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 .

clean: ## Remove local Terraform caches, build zips and test output (never Terraform state)
	@if [ -d infra ]; then find infra -type d \( -name .terraform -o -name .build \) -prune -exec rm -rf {} +; fi
	@if [ -d infra ]; then find infra -type f \( -name '*.tfplan' -o -name plan.txt \) -delete; fi
	rm -f $(addsuffix /coverage.out,$(GO_MODULES))
