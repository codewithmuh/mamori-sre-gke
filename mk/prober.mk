# CUJ prober (tools/prober, deploy/prober). Included by the Makefile; uses its require-tools.
# The prober has no public endpoint: these targets reach it through a kubectl port-forward.

PROBER_NAMESPACE ?= prober
PROBER_LOCAL_PORT ?= 18080
CUJ ?= browse
RUNS ?= 5

# Recipe prefix: open a port-forward to the prober, close it when the recipe's shell exits, and
# wait (up to 10 s) until the prober answers through it.
prober-port-forward = kubectl -n $(PROBER_NAMESPACE) port-forward svc/prober $(PROBER_LOCAL_PORT):8080 >/dev/null & pf=$$!; \
	trap 'kill $$pf 2>/dev/null || true' EXIT; \
	for _ in $$(seq 1 50); do \
		curl -fsS -o /dev/null --max-time 1 http://127.0.0.1:$(PROBER_LOCAL_PORT)/readyz 2>/dev/null && break; sleep 0.2; \
	done;

.PHONY: prober-run prober-metrics

# The call can take minutes when steps time out (up to about 21 minutes for 100 checkouts), so
# curl waits up to 25 minutes.
prober-run: ## Run a journey on demand through the prober: make prober-run CUJ=checkout RUNS=5
	@$(call require-tools,kubectl curl)
	@case "$(CUJ)" in browse|add-to-cart|view-cart|checkout) ;; \
		*) echo "error: CUJ must be browse, add-to-cart, view-cart or checkout" >&2; exit 2 ;; esac
	@case "$(RUNS)" in ''|*[!0-9]*) echo "error: RUNS must be a number from 1 to 100" >&2; exit 2 ;; esac
	@$(prober-port-forward) \
	curl -sS --fail-with-body --max-time 1500 -X POST -H 'Content-Type: application/json' \
		-d '{"cuj":"$(CUJ)","runs":$(RUNS)}' http://127.0.0.1:$(PROBER_LOCAL_PORT)/v1/run \
		| { if command -v jq >/dev/null; then jq .; else cat; echo; fi; }

prober-metrics: ## Show the prober's journey and step counters
	@$(call require-tools,kubectl curl)
	@$(prober-port-forward) \
	curl -fsS --max-time 10 http://127.0.0.1:$(PROBER_LOCAL_PORT)/metrics | grep -E '^mamori_cuj_(probe|step)_total'
