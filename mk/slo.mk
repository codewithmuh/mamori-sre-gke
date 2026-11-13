# SLO generator (tools/slo-gen): slo/slos.yaml + the CUJ map -> recording rules (deploy/slo),
# Cloud Monitoring SLOs and alerts (infra/envs/lab/slo_generated.tf.json), Datadog SLOs and monitors
# (infra/envs/datadog/slo_generated.tf.json) and slo/generated/slo-catalog.md.
# Until cuj-discovery has written slo/cuj-map.yaml, the example map is used (CI does the same).
SLO_CUJ_MAP ?= $(if $(wildcard slo/cuj-map.yaml),slo/cuj-map.yaml,slo/cuj-map.example.yaml)
SLO_GEN := cd tools/slo-gen && go run . -root ../.. -slos slo/slos.yaml -cuj-map $(SLO_CUJ_MAP)

.PHONY: slo-generate slo-check

slo-generate: ## Regenerate the SLO rules, Terraform and catalog from slo/slos.yaml and the CUJ map
	@$(call require-tools,go)
	$(SLO_GEN)

slo-check: ## Fail if the generated SLO files are out of date (as CI does)
	@$(call require-tools,go)
	$(SLO_GEN) -check
