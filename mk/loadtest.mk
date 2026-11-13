# Capacity test (#29): step load from k6 inside the cluster, then a report from Managed Service for
# Prometheus. Method and limits: docs/capacity/README.md. Needs kubectl pointed at the lab (make up
# does that) and, for the report, Go and `gcloud auth application-default login`.
#
#   make loadtest-run STEPS=10,20,40 STEP_DURATION=5m        prints the run id at the start
#   make loadtest-report RUN_ID=20270112-100000              reads loadtest-results/<run id>/summary.json
#   make loadtest-report FROM=2027-01-12T10:00:00Z TO=2027-01-12T10:40:00Z STEPS=10,20,40 STEP_DURATION=5m
#   make loadtest-clean
#
# Results go to loadtest-results/<run id>/ (gitignored): the k6 log, its summary and the report.

LOADTEST_STEPS ?= 10,20,30,40,50,60,80,100
LOADTEST_STEP_DURATION ?= 5m
# Shares of journeys, not requests: weighted towards the cart journeys so each CUJ gets enough
# requests per step to confirm its SLO (docs/capacity/README.md, "When a step passes").
LOADTEST_MIX ?= browse=40,add-to-cart=10,view-cart=25,checkout=25
# Must include every latency threshold in slo/slos.yaml.
LOADTEST_LATENCY_THRESHOLDS_MS ?= 100,250,500,750,1000,1500,2000,3000
LOADTEST_RESULTS := loadtest-results
LOADTEST_BIN := bin/loadtest
# The ingress gateway's internal port, outside the public rate-limit bucket (#27,
# deploy/resilience/ingress-rate-limit.yaml): capacity runs never meet the limit.
LOADTEST_TARGET_URL ?= http://istio-ingressgateway.istio-ingress.svc.cluster.local:8080
# Opt-in, only for a test of the public path (LOADTEST_TARGET_URL without :8080, inside the limit):
# off = do not enforce the gateway's rate limit during the run, so it finds the shop's knee there.
# keep (default) = leave the limit alone, for example to check that it sheds load at its rate.
LOADTEST_GATEWAY_LIMIT ?= keep

# Envoy runtime switch, per gateway pod, reset when the pod restarts. No Git change, so Argo CD has
# nothing to undo. $(1): 0 = not enforced (still counted), 100 = enforced. It tries every pod, then
# fails if there was no pod or any pod refused. The last command is a plain `[`, which `set -e` sees
# in every bash (macOS's bash 3.2 ignores a failing `[[`).
loadtest-gateway-limit = pods="$$(kubectl -n istio-ingress get pods -l istio=ingressgateway -o name)"; failed=0; \
	for pod in $$pods; do kubectl -n istio-ingress exec "$$pod" -c istio-proxy -- \
	pilot-agent request POST "runtime_modify?ingress_local_rate_limit_enforced=$(1)" >/dev/null || failed=1; done; \
	[ -n "$$pods" ] || failed=1; [ "$$failed" = 0 ]

# STEPS, STEP_DURATION, RUN_ID, FROM and TO count only when given on the command line or in the
# environment, so a variable of the same name in another mk/*.mk file cannot change a run.
loadtest-arg = $(if $(filter command line environment,$(origin $(1))),$(strip $($(1))))
loadtest-steps = $(or $(call loadtest-arg,STEPS),$(LOADTEST_STEPS))
loadtest-step-duration = $(or $(call loadtest-arg,STEP_DURATION),$(LOADTEST_STEP_DURATION))

.PHONY: loadtest-run loadtest-report loadtest-clean loadtest-gateway-limit-on

# Order: the relay must be up before k6 sends to it; the old Job must go (Jobs are immutable). The
# Job's settings are set on the rendered manifest with `kubectl set env`, so the repo stays as is.
# With LOADTEST_GATEWAY_LIMIT=off the gateway's rate limit goes back on when this command ends, however
# it ends: the EXIT trap is set before the first pod is switched, so a partial switch is undone too.
loadtest-run: ## Step-load the shop from inside the cluster: STEPS=10,20,40 STEP_DURATION=5m
	@$(call require-tools,kubectl)
	@set -euo pipefail; \
	run_id="$(or $(call loadtest-arg,RUN_ID),$$(date -u +%Y%m%d-%H%M%S))"; \
	case "$$run_id" in *[!A-Za-z0-9._-]*) echo "error: RUN_ID may only hold letters, digits, '.', '_' and '-'" >&2; exit 1 ;; esac; \
	out="$(LOADTEST_RESULTS)/$$run_id"; \
	mkdir -p "$$out"; \
	printf 'RUN_ID=%s\nSTEPS=%s\nSTEP_DURATION=%s\nMIX=%s\nLATENCY_THRESHOLDS_MS=%s\nTARGET_URL=%s\nTARGET_HOST=%s\nGATEWAY_LIMIT=%s\n' \
		"$$run_id" "$(loadtest-steps)" "$(loadtest-step-duration)" "$(LOADTEST_MIX)" \
		"$(LOADTEST_LATENCY_THRESHOLDS_MS)" "$(LOADTEST_TARGET_URL)" "$(FRONTEND_HOSTNAME)" "$(LOADTEST_GATEWAY_LIMIT)" >"$$out/run.env"; \
	echo "--- run $$run_id: $(loadtest-steps) RPS, $(loadtest-step-duration) per step (results in $$out)"; \
	kubectl apply -k deploy/loadtest; \
	kubectl -n loadtest rollout status deployment/k6-metrics --timeout=3m; \
	kubectl -n loadtest delete job k6 --ignore-not-found --wait=true; \
	if [ "$(LOADTEST_GATEWAY_LIMIT)" = off ]; then \
		trap 'if { $(call loadtest-gateway-limit,100); }; then echo "--- ingress gateway rate limit: enforced again"; \
			else echo "warning: could not re-enable the gateway rate limit on every pod; run make loadtest-gateway-limit-on" >&2; fi' EXIT; \
		echo "--- ingress gateway rate limit: not enforced during the run (LOADTEST_GATEWAY_LIMIT=off)"; \
		$(call loadtest-gateway-limit,0); \
	fi; \
	kubectl kustomize deploy/loadtest/run \
		| kubectl set env --local -f - -c k6 -o yaml \
			RUN_ID="$$run_id" STEPS="$(loadtest-steps)" STEP_DURATION="$(loadtest-step-duration)" \
			MIX="$(LOADTEST_MIX)" LATENCY_THRESHOLDS_MS="$(LOADTEST_LATENCY_THRESHOLDS_MS)" \
			TARGET_URL="$(LOADTEST_TARGET_URL)" TARGET_HOST="$(FRONTEND_HOSTNAME)" \
		| kubectl create -f -; \
	echo "--- following k6. Ctrl-C ends this command, not the Job (and re-enables the gateway limit if it was turned off)."; \
	kubectl -n loadtest logs -f job/k6 --pod-running-timeout=5m \
		| awk '/^MAMORI_K6_SUMMARY_BEGIN$$/ { skip = 1 } !skip { print } /^MAMORI_K6_SUMMARY_END$$/ { skip = 0 }' || true; \
	while :; do \
		finished="$$(kubectl -n loadtest get job k6 -o jsonpath='{.status.succeeded}{.status.failed}')"; \
		[ -z "$$finished" ] || break; \
		sleep 15; \
	done; \
	kubectl -n loadtest logs job/k6 >"$$out/k6.log"; \
	if [ "$$(kubectl -n loadtest get job k6 -o jsonpath='{.status.succeeded}')" != 1 ]; then \
		echo "error: the k6 Job failed; see $$out/k6.log and kubectl -n loadtest describe job k6" >&2; exit 1; \
	fi; \
	awk '/^MAMORI_K6_SUMMARY_END$$/ { f = 0 } f { print } /^MAMORI_K6_SUMMARY_BEGIN$$/ { f = 1 }' "$$out/k6.log" >"$$out/summary.json"; \
	[ -s "$$out/summary.json" ] || { echo "error: no k6 summary in $$out/k6.log" >&2; exit 1; }; \
	echo; \
	echo "Done. Give Managed Service for Prometheus a minute, then: make loadtest-report RUN_ID=$$run_id"; \
	echo "Afterwards: make loadtest-clean (the relay keeps running and being scraped until then)"

loadtest-report: ## Capacity report from Managed Prometheus: RUN_ID=<id>, or FROM= TO= STEPS= STEP_DURATION=
	$(call require-vars,PROJECT_ID)
	@$(call require-tools,go)
	go -C tools/loadtest build -o "$(CURDIR)/$(LOADTEST_BIN)" .
	@set -euo pipefail; \
	run_id="$(call loadtest-arg,RUN_ID)"; \
	summary="$(LOADTEST_RESULTS)/$$run_id/summary.json"; \
	args=(--project "$(PROJECT_ID)" --slos slo/slos.yaml); \
	if [ -n "$$run_id" ] && [ -f "$$summary" ]; then \
		args+=(--summary "$$summary"); \
	else \
		[ -n "$(call loadtest-arg,FROM)" ] || { echo "error: give RUN_ID=<id> of a run in $(LOADTEST_RESULTS)/, or FROM=<RFC 3339 start of step 1>" >&2; exit 1; }; \
		args+=(--steps "$(loadtest-steps)" --step-duration "$(loadtest-step-duration)"); \
	fi; \
	[ -z "$$run_id" ] || args+=(--run-id "$$run_id"); \
	[ -z "$(call loadtest-arg,FROM)" ] || args+=(--from "$(call loadtest-arg,FROM)"); \
	[ -z "$(call loadtest-arg,TO)" ] || args+=(--to "$(call loadtest-arg,TO)"); \
	[ -z "$(call loadtest-arg,STEPS)" ] || args+=(--steps "$(call loadtest-arg,STEPS)"); \
	[ -z "$(call loadtest-arg,STEP_DURATION)" ] || args+=(--step-duration "$(call loadtest-arg,STEP_DURATION)"); \
	out="$(LOADTEST_RESULTS)/$${run_id:-manual-$$(date -u +%Y%m%d-%H%M%S)}"; \
	mkdir -p "$$out"; \
	"$(LOADTEST_BIN)" report "$${args[@]}" --out "$$out/report.md" --json-out "$$out/report.json"; \
	cat "$$out/report.md"; \
	echo; echo "Saved to $$out/report.md and report.json. Template for the write-up: docs/capacity/report-template.md"

loadtest-clean: ## Delete the load-test namespace: the k6 Job and the metrics relay
	@$(call require-tools,kubectl)
	kubectl delete namespace loadtest --ignore-not-found --wait=true

loadtest-gateway-limit-on: ## Enforce the ingress gateway's rate limit again (if an interrupted run left it off)
	@$(call require-tools,kubectl)
	$(call loadtest-gateway-limit,100)
