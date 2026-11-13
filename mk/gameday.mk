# Game days: the fault library (chaos/faults) and the gameday CLI (tools/gameday).
# Included by the Makefile (`-include mk/*.mk`). How to run a game day safely: docs/game-days.md.

GAMEDAY := bin/gameday
GAMEDAY_GO_IMAGE := golang:1.27.3-trixie
# Extra flags for every gameday command, e.g. GAMEDAY_ARGS="--context gke_mamori-lab-123456_asia-northeast1-a_mamori"
GAMEDAY_ARGS ?=
# Overrides the fault's own duration for gameday-start (max 60m), e.g. DURATION=60m.
DURATION ?=

.PHONY: gameday-build gameday-list gameday-start gameday-stop gameday-stop-all gameday-status \
	gameday-validate gameday-test gameday-schemas

gameday-build: ## Build bin/gameday (local Go, or Go in Docker cross-compiled for this machine)
	@mkdir -p bin
	@if command -v go >/dev/null 2>&1; then \
		go -C tools/gameday build -trimpath -o ../../$(GAMEDAY) . ; \
	else \
		os=$$(uname -s | tr '[:upper:]' '[:lower:]'); arch=$$(uname -m); \
		case "$$arch" in x86_64) arch=amd64;; aarch64) arch=arm64;; esac; \
		docker run --rm -v "$$PWD":/src -w /src/tools/gameday -e CGO_ENABLED=0 -e GOOS=$$os -e GOARCH=$$arch \
			$(GAMEDAY_GO_IMAGE) go build -trimpath -buildvcs=false -o /src/$(GAMEDAY) . ; \
	fi
	@echo "built $(GAMEDAY)"

gameday-list: gameday-build ## List the fault library (ids, categories, game-day faults, manual ones)
	$(GAMEDAY) list $(GAMEDAY_ARGS)

gameday-start: gameday-build ## Run one fault until its duration ends: make gameday-start FAULT=F01-currency-abort [DURATION=10m]
	$(if $(strip $(FAULT)),,$(error FAULT is not set, e.g. make gameday-start FAULT=F01-currency-abort))
	$(GAMEDAY) start $(FAULT) $(if $(strip $(DURATION)),--duration $(DURATION)) $(GAMEDAY_ARGS)

gameday-stop: gameday-build ## Remove one fault's objects now: make gameday-stop FAULT=F01-currency-abort
	$(if $(strip $(FAULT)),,$(error FAULT is not set, e.g. make gameday-stop FAULT=F01-currency-abort))
	$(GAMEDAY) stop $(FAULT) $(GAMEDAY_ARGS)

gameday-stop-all: gameday-build ## Remove every fault object in namespace boutique (the big red button)
	$(GAMEDAY) stop --all $(GAMEDAY_ARGS)

gameday-status: gameday-build ## Show the fault objects in the cluster now, and any that outlived their run
	$(GAMEDAY) status $(GAMEDAY_ARGS)

gameday-validate: ## Check every fault manifest offline (kubeconform -strict with Istio and Chaos Mesh schemas)
	bash chaos/validate.sh

gameday-test: ## Go tests of tools/gameday, including the fault library checks (race detector, 80% coverage)
	@if command -v go >/dev/null 2>&1; then \
		scripts/go-test.sh tools/gameday; \
	else \
		docker run --rm -v "$$PWD":/src -w /src $(GAMEDAY_GO_IMAGE) scripts/go-test.sh tools/gameday; \
	fi

gameday-schemas: ## Regenerate chaos/schemas from the Chaos Mesh chart pinned in deploy/apps/chaos-mesh.yaml
	bash chaos/schemas/generate.sh
