# loadtest

A Go CLI that turns a k6 step-load run into a capacity report: for each step it reads the four
journeys' SLIs from Managed Service for Prometheus, finds the highest step at which every SLO in
`slo/slos.yaml` still held, and prices that rate per 1,000 requests per second. The method, what limits
the result and how to read the report are in [docs/capacity/README.md](../../docs/capacity/README.md);
the load itself comes from [deploy/loadtest](../../deploy/loadtest/).

It runs on a laptop, not in the cluster, so it has no Dockerfile. `make loadtest-report` builds it into
`bin/loadtest` and runs it.

## Usage

```sh
# After `make loadtest-run`, which saves the run's k6 summary (start time, steps, run id):
loadtest report --project mamori-lab-123456 --summary loadtest-results/20270112-100000/summary.json

# Without a summary, give the schedule exactly as the run used it:
loadtest report --project mamori-lab-123456 --run-id 20270112-100000 \
  --from 2027-01-12T10:00:05Z --to 2027-01-12T10:46:00Z --steps 10,20,40,60 --step-duration 5m

# Any Prometheus-compatible API, without authentication (for example a local Prometheus):
loadtest report --prometheus-url http://localhost:9090 --summary .../summary.json --format json
```

| Flag | Default | Meaning |
|---|---|---|
| `--project` | | Query this project's Managed Service for Prometheus with Application Default Credentials (`gcloud auth application-default login`). Needs `roles/monitoring.viewer` |
| `--prometheus-url` | | Query this Prometheus HTTP API instead, without authentication. Give exactly one of the two |
| `--summary` | | k6 summary JSON of the run; supplies `--from`, `--to`, `--steps`, `--step-duration` and `--run-id` |
| `--from`, `--to` | from the summary; `--to` else now | RFC 3339. Step 1 starts at `--from`; steps not finished by `--to` are `not-run` |
| `--steps`, `--step-duration` | from the summary | As in the run: rising requests per second, and each step's length |
| `--run-id` | from the summary | Count only metrics with this `run` label |
| `--slos` | `slo/slos.yaml` | The SLO spec (kind `SLOSpec`) with each journey's targets |
| `--config` | built in | YAML file overriding [`config.yaml`](config.yaml) |
| `--format`, `--out`, `--json-out` | markdown, stdout | Report format and file; `--json-out` also writes the JSON |
| `--timeout` | 5m | Time limit for all queries |

Exit codes: 0 report written (whatever it found), 1 error, 2 bad command line. Logs are JSON lines on
stderr (`severity`, `message`), the report goes to stdout or `--out`.

## What it does

1. Reads [`config.yaml`](config.yaml) (built in; `--config` overrides any field), `slo/slos.yaml` and
   the schedule. Fails early when the run did not count requests under an SLO's latency threshold.
2. Lays the steps end to end from the start time. Each step's **measured window** is the step minus
   the warm-up (60 s), e.g. 240 s of a 5-minute step.
3. Per step, runs instant queries at the step's end over the window:

   ```promql
   sum by (cuj) (increase(k6_mamori_cuj_requests_total{run="<id>"}[240s]))
   sum by (cuj) (increase(k6_mamori_cuj_good_total{run="<id>"}[240s]))
   sum by (cuj, threshold_ms) (increase(k6_mamori_cuj_fast_total{run="<id>"}[240s]))
   ```

   and k6's 429 answers from `k6_http_reqs_total` (the run met the gateway's rate limit). For context
   only: `k6_dropped_iterations_total`, p95 from `k6_http_req_duration_milliseconds`, k6's CPU and the
   nodes' CPU from free GKE system metrics, the node count, and the frontend's request rate from
   `istio_requests_total`. A failed context query costs only that number.
4. Judges each SLI on its Wilson score interval (`confidenceZ`, one-sided 95 %): **met** when the
   lower end reaches the target, **missed** when the upper end is below it, **undecided** otherwise.
5. Decides each step: **pass** (every SLI met, at least 100 requests per journey, at least 95 % of the
   planned rate delivered, no 429s), **fail** (an SLI missed), **inconclusive** (nothing missed, but
   something undecided or too few requests), **invalid** (any 429, k6 behind the plan, or no data),
   **not-run**.
6. The capacity is the measured rate of the highest passing step before the first fail, invalid or
   not-run step; inconclusive steps are passed over. Cost per 1K RPS = the node pool's hourly price ×
   1000 ÷ that rate, plus an estimate without the load generator's CPU share. The suggested gateway
   rate limit is 90 % of the capacity, rounded down.

## Settings (`config.yaml`)

| Key | Default | Meaning |
|---|---|---|
| `cluster` | `mamori` | Fills `${cluster}` in the context queries |
| `warmup` | `60s` | Unmeasured start of every step |
| `minDeliveredRatio` | `0.95` | Below this share of the planned rate a step is invalid |
| `confidenceZ` | `1.645` | z of the Wilson interval each SLI is judged on (one-sided 95 %) |
| `minRequestsPerJourney` | `100` | Fewer requests for a journey in a step: inconclusive |
| `generatorCPULimit` | `1` | k6's CPU limit; a warning above 80 % of it |
| `rateLimitShare` | `0.9` | Share of the capacity suggested as the ingress gateway's rate limit (deploy/resilience/ingress-rate-limit.yaml) |
| `metrics.*` | `k6_mamori_cuj_*_total`, `k6_http_reqs_total`; labels `cuj`, `threshold_ms`, `run`, `status` | Names k6 and the relay produce |
| `context.*` | PromQL with `${range}`, `${cluster}` | Informational per-step queries; empty skips one |
| `cost.*` | 3 × e2-standard-2 Spot, $0.051584/h, 30 GB pd-standard at $0.052/GB-month, 730 h, 1.93 allocatable cores per node | The price model; Tokyo list prices of 2026-11-13 |

## Develop and test

Go isn't needed on the host; everything runs in pinned containers. From this directory:

```sh
GO_IMAGE=golang:1.27.3-trixie
LINT_IMAGE=golangci/golangci-lint:v2.15.0

docker run --rm -v "$PWD":/src -w /src $GO_IMAGE \
  sh -c 'go test -race -covermode=atomic -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1'
docker run --rm -v "$PWD":/src -w /src $GO_IMAGE sh -c 'go vet ./... && test -z "$(gofmt -l .)"'
docker run --rm -v "$PWD":/src -w /src $LINT_IMAGE golangci-lint run ./...
docker run --rm -v "$PWD":/src -w /src $GO_IMAGE go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
```

The tests need no network or credentials. A fake Prometheus (`fake_test.go`) answers every query from
[`testdata/run-fixture.json`](testdata/run-fixture.json): a five-step run where step 1 is inconclusive
(too few view-cart requests to confirm 99.5 %), step 2 passes, step 3 is inconclusive, step 4 clearly
misses checkout latency, and step 5 is invalid (429s from the gateway, k6 behind). It is served over HTTP
too, so `run()` is tested end to end. [`testdata/k6-summary.json`](testdata/k6-summary.json) is a real
k6 v2.3.0 summary of the script, trimmed, with the fixture's schedule.
