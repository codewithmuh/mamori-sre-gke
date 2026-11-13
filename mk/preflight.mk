# First-run checks (scripts/preflight.sh, docs/runbooks/first-run.md, #15). Included by the Makefile,
# which exports PROJECT_ID, ZONE, BILLING_ACCOUNT_ID, FRONTEND_HOSTNAME and TF_STATE_BUCKET from
# .env with its defaults. Read-only: the script changes nothing, here or in Google Cloud or GitHub.
#   make preflight PHASE=before-bootstrap   before `make bootstrap`
#   make preflight                          before `make up` (PHASE=before-up, the default)
#   make preflight PHASE=after-up           after `make up`, the DNS records and the second bootstrap
# The script exits 1 on a FAIL line and 2 on a bad PHASE; make then stops with an error.

PHASE ?= before-up

.PHONY: preflight

preflight: ## Read-only checks before the next step: make preflight PHASE=before-bootstrap|before-up|after-up
	@scripts/preflight.sh --phase "$(PHASE)"
