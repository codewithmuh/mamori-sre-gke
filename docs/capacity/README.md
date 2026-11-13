# Capacity test

How the lab measures the highest request rate the shop serves while every journey still meets its SLO,
and what that rate costs per 1,000 requests per second (PLAN.md M4, requirement R2; PRD success
measure "Capacity"). Nothing here has run yet: every number on this page is a setting or an estimate,
and the measured results go in a report made from [report-template.md](report-template.md).

| Piece | Where |
|---|---|
| k6 script (four journeys, step load) | [`deploy/loadtest/scripts/capacity.js`](../../deploy/loadtest/scripts/capacity.js) |
| Stack: namespace, metrics relay, PodMonitoring | [`deploy/loadtest/`](../../deploy/loadtest/) |
| The k6 Job | [`deploy/loadtest/run/job.yaml`](../../deploy/loadtest/run/job.yaml) |
| Report CLI | [`tools/loadtest/`](../../tools/loadtest/README.md) |
| Make targets | [`mk/loadtest.mk`](../../mk/loadtest.mk) |

## Run it

Before a run: the lab is up and `kubectl` points at it, `slo/slos.yaml` exists, and you can read
Cloud Monitoring (`gcloud auth application-default login` as a user with `roles/monitoring.viewer`).
The test spends error budget on purpose, so treat it like a game day: check the
[error-budget policy](../slo/error-budget-policy.md), pick a time when the budget allows it, and expect
burn-rate alerts at the top steps.

```sh
make loadtest-run STEPS=2 STEP_DURATION=2m       # smoke test of the plumbing (~2 minutes; its report says
                                                  # "invalid": too few requests to judge an SLO)
make loadtest-run STEPS=5,10,20,30,40,60,80,100   # the real run: 8 steps x 5 minutes = ~41 minutes
make loadtest-report RUN_ID=<the id it printed>   # a minute or two after the run, once the last scrape is in
make loadtest-clean                               # removes the namespace, the relay and the Job
```

`loadtest-run` prints the run id first, turns the ingress gateway's rate limit enforcement off for the
run (see [below](#the-ingress-gateways-rate-limit)), follows k6's log, and saves `k6.log`,
`summary.json` and `run.env` in `loadtest-results/<run id>/`. `loadtest-report` writes `report.md` and `report.json`
next to them. Without a saved summary, give the time range by hand:
`make loadtest-report FROM=2027-01-12T10:00:05Z TO=2027-01-12T10:46:00Z STEPS=... STEP_DURATION=5m`.

Choose the steps from the smoke test: if 2 RPS already shows high latency, start lower. The search stops
at the first step that does not pass, so the steps must rise, and the last one should be above the
expected limit (the report warns when every step held).

## Method

### Where the load comes from

k6 runs as a Job in the `loadtest` namespace, without an Istio sidecar, and sends plain HTTP to the
Istio ingress gateway Service (`istio-ingressgateway.istio-ingress.svc.cluster.local`) with the header
`Host: mamori.codewithmuh.com`, the only host the gateway's `Gateway` and `VirtualService` accept. That
is the same path as the prober and the same pods as public traffic, minus the load balancer.

Why not the public URL: Cloud Armor limits each client IP to 600 requests a minute
(`armor_rate_limit_per_minute` in `infra/envs/lab`), which is 10 requests per second from one generator.
Real users each have their own IP; one generator would hit the limit, not the shop's capacity. So the
measured path leaves out the external Application Load Balancer, TLS and Cloud Armor. They are managed
services that scale on their own; checking them needs a test from many IPs, which this lab doesn't do.

### The ingress gateway's rate limit

M4 adds load shedding at the Istio ingress gateway: Envoy's local rate limit, a token bucket that
answers 429 above a set rate ([`deploy/resilience/ingress-rate-limit.yaml`](../../deploy/resilience/ingress-rate-limit.yaml),
#27). k6 goes through the gateway, so with the limit enforced the test would find the limit (a 40 req/s
placeholder), not the shop's knee. So:

- `make loadtest-run` turns enforcement off with Envoy's runtime switch on every gateway pod
  (`runtime_modify?ingress_local_rate_limit_enforced=0`) and turns it back on (`=100`) when the command
  ends, however it ends. The switch lives in the pod, not in Git, so Argo CD has nothing to undo; a
  gateway pod that restarts during the run comes back enforcing. `LOADTEST_GATEWAY_LIMIT=keep` leaves
  the limit alone, for example to check that it sheds load at the right rate.
  `make loadtest-gateway-limit-on` turns it back on by hand.
- The report counts k6's 429 answers per step and warns when there are any: that step measured the
  limit, not the shop.
- The report suggests a value for the limit: 90 % of the capacity, rounded down (`rateLimitShare` in
  `tools/loadtest/config.yaml`). It goes into `tokens_per_fill`; the burst (`max_tokens`) stays twice
  that. The bucket is per gateway pod, so with N gateway replicas each gets 1/N of it. #27 asks for the
  capacity with the HPAs at their maximum: check the HPAs' replica counts at the highest passing step
  and say in the report how far they had scaled.

### Load shape

- **Steps.** `STEPS` lists requests per second, for example `5,10,20,40`. Each step lasts
  `STEP_DURATION` (default 5 minutes). k6 moves to the new rate over the first 10 s of a step (`RAMP`).
- **Open model.** Each journey is a k6 `ramping-arrival-rate` scenario: journeys start at the planned
  rate whether or not earlier ones have finished. A slow shop shows up as latency and errors, not as
  less load, which is how real users behave. If the shop is so slow that k6 runs out of virtual users
  (`MAX_VUS`, 100 per journey), k6 counts `dropped_iterations` and the report marks the step.
- **Mix.** `MIX` splits journeys (not requests) between the four CUJs: by default browse 50 %, add to
  cart 20 %, view cart 15 %, checkout 15 %. Every journey starts a new session with an empty cart.

| Journey (CUJ id) | Requests it sends | Counted as |
|---|---|---|
| `browse` | `GET /`, `GET /product/{random id}` | browse, browse |
| `add-to-cart` | `POST /cart` (answer 302, not followed) | add-to-cart |
| `view-cart` | `POST /cart`, then `GET /cart` | add-to-cart, view-cart |
| `checkout` | `POST /cart`, then `POST /cart/checkout` with the demo's pre-filled fake card | add-to-cart, checkout |

With the default mix a journey sends 1.8 requests on average, so 10 RPS is about 5.6 journeys a second.
The dependent request is sent even when adding the item failed, so every journey always sends the same
number of requests and the measured rate stays comparable with the plan.

### What is measured

For every request k6 counts, per journey (label `cuj`):

- `k6_mamori_cuj_requests_total`: every request.
- `k6_mamori_cuj_good_total`: requests that got the expected answer: no network error, status 200
  (302 for `POST /cart`), and for checkout the "Your order is complete!" page.
- `k6_mamori_cuj_fast_total{threshold_ms}`: good requests faster than each threshold in
  `LATENCY_THRESHOLDS_MS` (default 100, 250, 500, 750, 1000, 1500, 2000, 3000 ms). One series per
  threshold, like histogram buckets, so the result is exact at the SLO's threshold. A failed request
  never counts as fast.

Per step and journey: **availability** = good ÷ requests, **latency SLI** = fast ÷ requests at the
journey's `latency.thresholdMs`, both over the step's **measured window**: the step minus its first
60 s (warm-up, configurable), so 4 of every 5 minutes.

These are the SLOs' targets applied to a load-test view of each journey, not the 28-day SLOs
themselves. The SLOs' own signals don't fit a 4-minute step: the prober runs each journey once a
minute (4 samples per step), the mesh metrics have no per-journey split (no path label), and the
D4 "product of the critical APIs" SLI needs per-API latency thresholds from M2. k6 measures the
user-facing request of each journey, at the gateway, with the SLO's own threshold.

### How the numbers reach Managed Service for Prometheus

```
k6 Job ──OTLP gRPC──▶ k6-metrics (OpenTelemetry Collector, core distribution)
                         └─ :8889/metrics ◀── PodMonitoring k6-metrics ── managed collectors ──▶ Cloud Monitoring
```

- Managed Service for Prometheus has no remote-write endpoint to send to, and k6 v2.4 deprecated its
  Prometheus remote-write output in favour of OpenTelemetry. So k6 uses its `opentelemetry` output.
- The relay turns OTLP into a Prometheus endpoint, and the cluster's managed collectors scrape it like
  any other pod. They already write to Cloud Monitoring, so the test needs no Google service account,
  IAM role or Workload Identity binding of its own.
- The relay is a Deployment, not a sidecar: it outlives each k6 Job, so the last values of a run are
  still scraped after k6 exits. It keeps them for 60 minutes (`metric_expiration`).
- The k6 summary JSON is saved too. It holds whole-run totals only (it cannot split by step), but it
  carries the run's schedule (`setup_data`), which `loadtest report --summary` reads.
- About 200 series, scraped every 10 s: cents per run (estimate).

### When a step passes

A step **passes** when, in its measured window:

1. every journey in `slo/slos.yaml` meets its availability target and its latency target;
2. every journey has at least 100 requests (`minRequestsPerJourney`): with fewer, 99.5 % cannot be told
   apart from 100 %;
3. k6 delivered at least 95 % of the planned rate (`minDeliveredRatio`); otherwise the step did not
   really test that rate.

A miss of 1 makes the step **fail**; only 2 or 3 makes it **invalid**. A step that had not finished by
the end of the time range is **not-run**.

The **capacity** is the measured request rate of the highest step reached by passing every step below
it. The search stops at the first step that does not pass; if a higher step passes again, the report
warns that the run is noisy and should be repeated rather than taking the higher number.

### Cost per 1K RPS

```
cluster $/hour    = nodes × (node price per hour + disk GB × disk price per GB-month ÷ 730)
                  = 3 × ($0.051584 + 30 × $0.052 ÷ 730) = $0.1612/hour, $117.65/month (list price)
cost per 1K RPS   = cluster $/hour × 1000 ÷ capacity in RPS
```

Prices are Tokyo list prices for Spot e2-standard-2 and pd-standard from
[cost-and-tooling-research.md](../cost-and-tooling-research.md), set in
[`tools/loadtest/config.yaml`](../../tools/loadtest/config.yaml). The cluster fee is covered by the free
tier; the load balancer, Cloud Armor, NAT, Managed Prometheus and logging are left out because they are
not in the test's path or do not grow with it.

The figure assumes cost grows in proportion to load: more of the same nodes at the same price. That is
an approximation in both directions. Nodes come in whole units; and about 1.35 vCPU per cluster goes to
GKE's system pods, plus Argo CD and istiod, whatever the load, so a bigger cluster would spend a smaller
share on overhead and cost less per 1K RPS than this figure.

### The load generator shares the nodes

The pool has three nodes and no autoscaling, and a separate node for k6 needs CPU quota and a Terraform
change. So k6 runs on the same nodes as the shop. That is accounted for in four ways:

1. **Bounded.** k6 requests 250m CPU and 512 MiB and may burst to 1 CPU and 1 GiB. The relay requests
   25m and 64 MiB (limit 200m, 256 MiB). Both exist only during a test. Memory is not the cluster's
   limit (about 26 % of it is requested); CPU is.
2. **Placed.** The Job prefers a node without the ingress gateway or the frontend, the two pods every
   request passes (a soft rule: on three nodes there may be no such node).
3. **Measured.** Each step reports k6's CPU from the free GKE system metric
   `kubernetes.io/container/cpu/core_usage_time` (namespace `loadtest`), next to the nodes' CPU
   utilisation. The report warns when k6 used more than 80 % of its CPU limit (the generator itself
   may have added latency), and marks a step invalid when k6 could not deliver the planned rate.
4. **Priced.** The report gives a second cost figure without k6's share: the cost per 1K RPS times
   (1 − k6 CPU ÷ allocatable CPU at the highest passing step). It is an estimate: it assumes the shop
   could have used that CPU.

For a cleaner run, once the account can raise quotas (after the M5 upgrade): add a temporary one-node
pool with a taint for k6 and point the Job at it, or run k6 from a VM outside the cluster against the
public URL with `armor_rate_limit_per_minute` raised for the test (this also covers the load balancer).

## What limits the result

Expect the first of these to end the search; the report and the dashboards show which one did.

- **CPU, not memory.** The base already requests about 61 % of the allocatable CPU and 26 % of the
  memory ([deploy/README.md](../../deploy/README.md), resource budget). Node CPU near 100 % at the
  failing step points here.
- **The frontend.** A Go pod renders every HTML page and calls most services. #27 gives it (and the
  currency, catalog and recommendation services) an HPA of up to 2–3 replicas; replicas take a minute
  or two to arrive, and new pods compete for the same CPU. Its CPU is likely the first limit (estimate).
- **The ingress gateway.** One replica, requesting 50m CPU with a 1-CPU limit, in front of every
  request. Its local rate limit is off during the run (see above); 429s in a step mean it was not.
- **Checkout's fan-out.** One checkout calls six services, so its latency usually breaks first; its
  SLO is 1,000 ms for that reason.
- **Shared services.** `currencyservice` (Node.js) is on every page; `redis-cart` is one pod without a
  persistent disk.
- **Sidecars.** Each Envoy sidecar was trimmed to 20m CPU; under load they compete with their app.
- **Background traffic.** The Locust load generator (10 users) and the prober keep running. Their
  load is in *Frontend req/s* but not in the measured rate, so the capacity is slightly understated.
- **Spot.** A preemption during a run removes a third of the capacity. The report warns when the node
  count changes in a step; repeat such a run.
- **The generator.** See above: its CPU warning and the delivered check.
- **Lab, not production.** One zone, small Spot nodes, synthetic traffic, Online Boutique's demo
  services. The number describes this lab only and is not a production claim.

## How to read the report

The report (`report.md`, structure as in [report-template.md](report-template.md)) has:

- **Result.** The highest rate that held every SLO (measured requests per second, with the planned
  step), the first step that did not and why, the cluster cost, the cost per 1K RPS with and
  without the load generator's share, and the suggested gateway rate limit (90 % of the capacity).
- **Steps.** One row per step: planned and measured RPS, the share of the plan k6 delivered, and per
  journey *availability · share under the latency threshold · p95 latency* (bold when an SLO was
  missed), then k6's CPU (cores), the nodes' average CPU utilisation, the node count and the
  frontend's total request rate from the mesh, and the verdict.
- **Why steps did not pass** and **Warnings**, step by step.

Reading it:

- The verdict changes from `pass` to `fail` at the capacity. Look at which journey and which SLO broke
  first, and at node CPU and k6 CPU in that row.
- `invalid` with "delivered" means k6 fell behind: raise `MAX_VUS` (and check k6's CPU) or lower the
  steps. `invalid` with "requests" means the step was too short or the rate too low for that journey.
- A warning about the node count or about a pass after a failure: repeat the run.
- p95 comes from k6's duration histogram with fixed buckets, so it is approximate; the pass rule uses
  the exact threshold counters.
- n/a in a context column means that query returned nothing (for example, no mesh metrics scraped
  yet). It never changes a verdict.

## Troubleshooting

| Symptom | Check |
|---|---|
| The Job stays `Pending` | Not enough free CPU for 250m on any node: `kubectl describe pod -n loadtest -l job-name=k6`. Run when fewer M2–M6 pods are up, or lower the request. |
| The report says "no k6 data" | The relay was not scraped: `kubectl -n loadtest get podmonitoring k6-metrics -o yaml` (status), the managed collection is on, and a global metric filter (if any) lets `k6_.*` through. Also check `RUN_ID` and the time range. |
| Every request fails | The `Host` header: `TARGET_HOST` must match the Istio `Gateway` host (`FRONTEND_HOSTNAME`). |
| "did not count requests under N ms" | An SLO threshold is not in `LATENCY_THRESHOLDS_MS`; add it and run again. |
| "requests got 429" in a step | The gateway's rate limit was enforced: `LOADTEST_GATEWAY_LIMIT=keep`, `kubectl exec` was not allowed, or a gateway pod restarted. Repeat the run. |
| The gateway still limits after a run | The run was interrupted before its exit trap ran: `make loadtest-gateway-limit-on`. |
| `context query failed` in the log | Only that context column is n/a. GKE system metrics take a few minutes to arrive. |
