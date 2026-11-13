# CUJ discovery (#17): break one upstream service at a time, run every journey through the prober,
# and write slo/cuj-map.yaml. How it works and how to read the result: docs/slo/cuj-discovery.md.
#
# Needs Go and a kubeconfig for the lab cluster (`make up` sets it; GKE also needs
# gke-gcloud-auth-plugin), and the prober (#16) running in the cluster. Extra flags go in
# DISCOVERY_ARGS, for example:
#   make discovery-run DISCOVERY_ARGS="--services redis-cart --cujs checkout --runs 10"
#   make discovery-run DISCOVERY_ARGS="--prober-url http://127.0.0.1:8080"

DISCOVERY_DIR := tools/cuj-discovery
DISCOVERY_BIN := bin/cuj-discovery
DISCOVERY_ARGS ?=

.PHONY: discovery-build discovery-run discovery-dry-run discovery-cleanup discovery-validate

# Built into the gitignored bin/ and run from the repo root, so the default paths
# (slo/cuj-map.yaml) are the repo's.
discovery-build:
	@$(call require-tools,go)
	go build -C $(DISCOVERY_DIR) -o $(CURDIR)/$(DISCOVERY_BIN) .

discovery-run: discovery-build ## Find each journey's critical services by fault injection; writes slo/cuj-map.yaml (10-30 min, estimate; burns error budget)
	$(DISCOVERY_BIN) run $(DISCOVERY_ARGS)

discovery-dry-run: discovery-build ## Check the cluster and print the discovery plan and the objects it would create; changes nothing
	$(DISCOVERY_BIN) run --dry-run $(DISCOVERY_ARGS)

discovery-cleanup: discovery-build ## Remove faults a stopped discovery run left behind (VirtualServices, redis-cart replicas)
	$(DISCOVERY_BIN) cleanup $(DISCOVERY_ARGS)

discovery-validate: discovery-build ## Check slo/cuj-map.yaml against the CUJ map schema
	$(DISCOVERY_BIN) validate $(or $(DISCOVERY_MAP),slo/cuj-map.yaml)
