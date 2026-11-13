# cuj-discovery

A Go command-line tool, run from a laptop, that finds which upstream services each critical user journey (CUJ)
depends on. It breaks one service at a time, runs every journey through the prober, and writes
[`slo/cuj-map.yaml`](../../slo/), which the SLO generator reads. Decision D4 in [PLAN.md](../../PLAN.md); method,
safety and how to read the result: [docs/slo/cuj-discovery.md](../../docs/slo/cuj-discovery.md).

Nothing here has run against a real cluster yet (none exists on 2026-11-13). The tests use fake Kubernetes clients
and a fake prober.

## Use

```sh
make discovery-dry-run      # read-only checks + the plan and every object it would create
make discovery-run          # about 10-30 minutes (estimate); writes slo/cuj-map.yaml
make discovery-cleanup      # only if a run was killed (kill -9, laptop closed) before it could clean up
make discovery-validate     # check slo/cuj-map.yaml against the schema
```

Or directly, from the repo root: `go run -C tools/cuj-discovery . run --out "$PWD/slo/cuj-map.yaml"`.

| Command | What it does |
|---|---|
| `run` | Preflight, baseline, one fault per service, write the map. Exit 0 only when the map was written |
| `run --dry-run` | Preflight (read-only) and print the plan and the manifests; no writes, no prober calls, no map |
| `cleanup [--dry-run]` | Remove every object this tool created in `boutique` and restore `redis-cart`'s replicas. Leaves game-day faults alone and lists them |
| `validate [file]` | Strictly parse a CUJ map and check its rules (default `slo/cuj-map.yaml`) |
| `version` | Print the version |

`run` flags (defaults in brackets):

| Flag | Meaning |
|---|---|
| `--kubeconfig`, `--context` | Cluster to use [`$KUBECONFIG` / `~/.kube/config`, current context]. GKE needs `gke-gcloud-auth-plugin` |
| `--prober-url` | Prober base URL, e.g. after `kubectl -n prober port-forward svc/prober 8080` [none: the tool opens its own port-forward to `prober/prober:8080`] |
| `--cujs`, `--services` | Comma-separated subsets [all four journeys, all ten services] |
| `--runs` | Runs per journey per broken service, 1-100 [20] |
| `--baseline-runs` | Runs per journey before any fault; every one must pass [10] |
| `--recovery-runs` | Runs per journey per recovery check [2] |
| `--threshold` | Failure rate at or above which a service is critical [0.5] |
| `--settle` | Wait after a fault is in place, before measuring [15s] |
| `--recovery-timeout` | How long the shop may take to pass again after each fault [6m] |
| `--timeout` | Limit for the whole run; the fault in place is removed when it fires [90m] |
| `--out` | Output path [`slo/cuj-map.yaml`] |
| `--online-boutique` | Version recorded in the map [`v0.10.7`] |

Logs are JSON lines on stderr (`severity`, `message`, plus fields such as `service`, `cuj`, `failure_rate`,
`failed_steps`, `example_error`). Save them with `2> discovery.log`: the map holds the rates, the log holds the why.
The summary goes to stdout.

## Faults

| Service | Fault | Object |
|---|---|---|
| adservice, cartservice, checkoutservice, currencyservice, emailservice, paymentservice, productcatalogservice, recommendationservice, shippingservice | Istio `VirtualService` on `<svc>.boutique.svc.cluster.local`, `fault.abort.grpcStatus: UNAVAILABLE`, `percentage: 100` | `VirtualService boutique/cuj-discovery-<svc>` |
| redis-cart (plain TCP: Istio's HTTP faults don't apply) | `Deployment boutique/redis-cart` scaled to 0, then back | marker `ConfigMap boutique/cuj-discovery-redis-cart` with the old replica count |

Every object carries `mamori.dev/fault: "true"`, `app.kubernetes.io/managed-by: cuj-discovery`,
`mamori.dev/discovery-run: <run id>` and `mamori.dev/discovery-target: <service>`, plus a
`mamori.dev/description` annotation that says how to remove it.

## Safety

- Changes only namespace `boutique` (hard-coded, no flag).
- Refuses to start if any object in `boutique`, of any kind, is labelled `mamori.dev/fault=true`, if another
  VirtualService already routes a target host, if a target's Service port isn't named `grpc*`/`http*` (Istio would
  treat it as TCP and the abort would silently not apply), or if `redis-cart` is already at 0.
- One fault at a time. Each is removed before the next one, and on error, Ctrl-C (SIGINT/SIGTERM) and `--timeout`;
  removal runs on its own 2-minute context even after the run's context is cancelled. A final sweep deletes anything
  left with this run's label. Further Ctrl-Cs are ignored while it cleans up; only `kill -9` stops it, and then
  `make discovery-cleanup` does the rest.
- Checks after each measurement that the fault was still in place (not removed by the executor, a person, Argo CD
  self-heal or an autoscaler), else the results are discarded and the run stops.
- Waits until every journey passes again before the next fault, so one service isn't blamed for another's damage.
- `cleanup` only ever removes objects with this tool's labels, and only restores a Deployment named in a marker if
  it is the known `redis-cart` target with a sane replica count.

## Develop and test

Go isn't needed on the host; everything runs in pinned containers. From the repo root (the tests also check
`slo/cuj-map.example.yaml`):

```bash
GO_IMAGE=golang:1.27.3-trixie
LINT_IMAGE=golangci/golangci-lint:v2.15.0

docker run --rm -v "$PWD":/repo -w /repo $GO_IMAGE scripts/go-test.sh tools/cuj-discovery
docker run --rm -v "$PWD":/repo -w /repo/tools/cuj-discovery $GO_IMAGE sh -c 'go vet ./... && test -z "$(gofmt -l .)"'
docker run --rm -v "$PWD":/repo -w /repo/tools/cuj-discovery $LINT_IMAGE golangci-lint run ./...
```

`UPDATE_GOLDEN=1` rewrites `testdata/cuj-map.golden.yaml`, the map the end-to-end test expects.

What the tests cover: the full run against a fake cluster and a fake prober that fails journeys according to a
dependency table (map content byte for byte, faults removed, replicas restored, one fault at a time), a dirty baseline,
existing faults of any kind, prober errors, Ctrl-C while `redis-cart` is down, the overall timeout, a fault removed by
someone else mid-measurement, `redis-cart` scaled back mid-measurement, slow and failed recovery, dry run (only `get`
and `list` calls), `cleanup` (dry run, real, nothing to do, bad markers, errors), preflight rules, the prober client
(busy retries, error answers, inconsistent answers), map validation, and the port-forward up to a refused upgrade.
