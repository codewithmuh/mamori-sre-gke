# spend-guard

A Cloud Run function that stops the lab's spend when the monthly budget runs out.

Google Cloud budgets only send alerts; they never stop anything. If the free-trial credit runs out, the trial closes
and takes the cluster with it. This function listens to the budget's Pub/Sub notifications and, once the cost reaches
the budget, scales the GKE node pool to zero. Nothing is deleted: `make resume` brings the nodes back. See decision D12
in [PLAN.md](../../PLAN.md) and the [runbook](../../docs/runbooks/spend-guard.md).

## What it does

For every budget notification:

1. Parse the Pub/Sub CloudEvent and the budget JSON inside it (`costAmount`, `budgetAmount`, `currencyCode`, ...).
2. If the message is invalid or a field is missing, or the currency is not USD: log an `ERROR` and do nothing.
3. If `costAmount < budgetAmount × THRESHOLD`: log at `INFO` and do nothing. This is the common case: Cloud Billing
   sends notifications several times a day, whatever the spend.
4. Otherwise read the node pool. If node autoscaling is on, turn it off (so the autoscaler can't add nodes back) and
   wait for that operation. Then set the node count to 0 and wait for it.

Safe to repeat: autoscaling that is already off is skipped, and resizing to 0 a pool that is already at 0 changes
nothing. Pub/Sub delivers at least once and out of order, so repeats are normal.

While another operation runs on the cluster (an upgrade, a repair, an earlier resize), GKE rejects a new one. The
function retries with backoff (5 s, doubling, at most 60 s between tries) for up to 7 minutes, inside the 540 s
function timeout. The resize itself can take longer: GKE respects PodDisruptionBudgets for up to an hour while it drains
nodes. Once the resize has started, the function stops waiting and logs `resize_running`; GKE finishes it alone.

With `DRY_RUN=true` it reads the node pool and logs the steps it would take, and changes nothing.

### Return value and retries

The trigger redelivers a message when the function returns an error (`RETRY_POLICY_RETRY`). So the function returns:

| Situation | Returns | Why |
|---|---|---|
| Invalid message, other currency, below threshold, done, dry run, resize still running | `nil` | Redelivery can't change the outcome |
| Wrong permissions, node pool or cluster not found, invalid request | `nil` (logged as `ERROR`) | Won't fix itself; the next notification tries again |
| Cluster still busy after 7 minutes, GKE API unavailable, operation failed, no GKE client | error | A retry a few minutes later can succeed |

### Logs

One JSON object per line on stdout, with `severity` and `message`, which Cloud Logging reads as the log level and
summary. Every line about a notification carries `event_id`, `budget`, `cost`, `budget_amount`, `currency`,
`spent_ratio`, `threshold`, `dry_run` and `node_pool`. The final line of each run has an `outcome`:

| `outcome` | Severity | Meaning |
|---|---|---|
| `below_threshold` | INFO | Nothing to do |
| `ignored` | ERROR | Not a valid budget notification, or not in USD |
| `dry_run` | WARNING | Would have acted; `planned_steps` lists what |
| `scaled_to_zero` | WARNING | Autoscaling off and node count 0 |
| `resize_running` | WARNING | Resize to 0 started; GKE is still draining nodes |
| `failed` | ERROR | Could not act; `retryable` says whether the message is redelivered |

## Inputs

Environment variables only, all set by Terraform (`infra/envs/lab`). They are checked at start; if one is invalid,
the function logs the problem and exits, so the deploy fails instead of leaving a guard that can never act.

| Variable | Required | Example | Meaning |
|---|---|---|---|
| `PROJECT_ID` | yes | `mamori-lab-123456` | Project of the cluster |
| `LOCATION` | yes | `asia-northeast1-a` | The cluster's zone (a region also works) |
| `CLUSTER_NAME` | yes | `mamori` | |
| `NODE_POOL` | yes | `spot-pool` | The pool to scale to zero |
| `THRESHOLD` | no, default `1.0` | `0.9` | Act when cost ≥ budget × THRESHOLD. A fraction, above 0 and at most 2 (`100` is rejected as a likely percentage) |
| `DRY_RUN` | no, default `false` | `true` | Log the planned actions only |

The event is a Pub/Sub message published by Cloud Billing, delivered by Eventarc as a CloudEvent of type
`google.cloud.pubsub.topic.v1.messagePublished`. Samples: [`testdata/pubsub-event.json`](testdata/pubsub-event.json)
(over budget) and [`testdata/pubsub-event-below-threshold.json`](testdata/pubsub-event-below-threshold.json); the
decoded budget JSON is [`testdata/budget-notification.json`](testdata/budget-notification.json). The format is
Google's [budget notification format](https://cloud.google.com/billing/docs/how-to/budgets-programmatic-notifications).

## Deployment (what Terraform must match)

| Setting | Value |
|---|---|
| Product | Cloud Run functions, Go runtime `go127` |
| Entry point | `ScaleToZero` (registered with `functions.CloudEvent`) |
| Source | this directory; the function package sits at the module root, next to `go.mod` and `go.sum` |
| Trigger | Eventarc, Pub/Sub topic `budget-alerts`, retry policy `RETRY_POLICY_RETRY` |
| Timeout | 540 s (the maximum for event-driven functions); the function stops waiting at 7 minutes |
| Instances | at most 1; 256 MiB is enough |

Files the build doesn't need, and that are best left out of the source zip so that a docs or test change doesn't
redeploy the function: `*_test.go`, `testdata/`, `cmd/`, `README.md`, `.golangci.yml`, `coverage.out`.

### Permissions

The runtime service account needs these IAM permissions on the project (a custom role), and nothing else:

| Permission | Used by |
|---|---|
| `container.clusters.get` | `projects.locations.clusters.nodePools.get` |
| `container.clusters.update` | `projects.locations.clusters.nodePools.setAutoscaling` and `.setSize` |
| `container.operations.get` | `projects.locations.operations.get` |

Source: the GKE [API permissions reference](https://cloud.google.com/kubernetes-engine/docs/reference/api-permissions)
and the REST reference pages of `setSize` and `setAutoscaling`. No Compute Engine permissions: GKE's own service agent
resizes the instance group. Writing logs to stdout needs no permission on Cloud Run.

Anyone who can publish to `budget-alerts` can send a fake "over budget" message and stop the cluster. That is useful
for testing (see the runbook), so keep publish rights to the billing service and the maintainer.

## Develop and test

Go isn't needed on the host; everything runs in pinned containers. From this directory:

```bash
GO_IMAGE=golang:1.27.3-trixie
LINT_IMAGE=golangci/golangci-lint:v2.15.0

# Tests with the race detector and coverage (CI fails below 80 %)
docker run --rm -v "$PWD":/src -w /src $GO_IMAGE \
  sh -c 'go test -race -covermode=atomic -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1'

# Vet, format check, lint
docker run --rm -v "$PWD":/src -w /src $GO_IMAGE sh -c 'go vet ./... && test -z "$(gofmt -l .)"'
docker run --rm -v "$PWD":/src -w /src $LINT_IMAGE golangci-lint run ./...

# Known vulnerabilities in the code paths actually used
docker run --rm -v "$PWD":/src -w /src $GO_IMAGE go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
```

The tests use a fake GKE client (`fake_test.go`) and cover payload parsing, threshold edges, dry run, the
autoscaling-on path, retries while another operation runs, operation failures, timeouts and idempotency.

### Run it locally

`cmd/local` serves the function with the Functions Framework, like Cloud Run does:

```bash
docker run --rm -p 127.0.0.1:8080:8080 -v "$PWD":/src -w /src \
  -e FUNCTION_TARGET=ScaleToZero -e DRY_RUN=true \
  -e PROJECT_ID=mamori-lab-123456 -e LOCATION=asia-northeast1-a -e CLUSTER_NAME=mamori -e NODE_POOL=spot-pool \
  golang:1.27.3-trixie go run ./cmd/local -host 0.0.0.0
```

In another terminal, send a sample in CloudEvents binary mode, as Eventarc does:

```bash
curl -sS localhost:8080 \
  -H "Content-Type: application/json" \
  -H "ce-id: local-1" \
  -H "ce-specversion: 1.0" \
  -H "ce-type: google.cloud.pubsub.topic.v1.messagePublished" \
  -H "ce-source: //pubsub.googleapis.com/projects/mamori-lab-123456/topics/budget-alerts" \
  -d @testdata/pubsub-event-below-threshold.json
```

That one logs `below_threshold` and needs no Google Cloud access. `testdata/pubsub-event.json` is over budget: without
credentials it logs `failed` ("could not find default credentials"), which still shows the parsing and the threshold
check. To dry-run it against the real cluster, log in with `gcloud auth application-default login`, add
`-v "$HOME/.config/gcloud:/root/.config/gcloud:ro"` to the `docker run`, and keep `DRY_RUN=true`.
