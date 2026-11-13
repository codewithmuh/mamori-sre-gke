# Resilience experiments: before and after

Each setting in [README.md](README.md) (S1–S10) is a claim: under this fault, the journeys do better with the
setting than without it. This page turns each claim into an experiment: a hypothesis, the fault from the game-day
library (`chaos/faults/`, #23) or the fault still needed, the switch that turns the setting off, and the metrics to
compare. Nothing has been measured yet. Every result cell says **TBD (measured)** until a run fills it, and
predicted numbers in the hypotheses are estimates or my math.

| Exp. | Settings under test | Fault | Main metric |
|---|---|---|---|
| E1 | S2 edge retries, S4 retry budget | F01-currency-abort; a 10 % variant (needed) | browse availability; frontend requests per gateway request |
| E2 | S1 edge timeouts | F02-productcatalog-latency, F24-productcatalog-bad-canary | browse p95 and max; browse availability |
| E3 | S3 mesh default retry, S2 for writes | F28-shipping-shiporder-abort; a currencyservice pod kill | Charge calls per order; browse availability |
| E4 | S5 connect timeout, S6 keepalive, S7 outlier detection | one of two productcatalogservice pods cut off (needed) | browse availability and p95, minute by minute |
| E5 | S5 connection-pool limits | F06-recommendation-cpu-stress under load | browse and view-cart p95; pool overflows |
| E6 | S9 HPAs | F06-recommendation-cpu-stress; a load ramp | p95; replicas; highest rate that holds the SLO |
| E7 | S10 load shedding | overload at 1.5× and 3× capacity | p95 of admitted requests; 429 and 5xx shares |
| E8 | S8 PodDisruptionBudgets | two-node drain (needed); spend-guard scale to zero | browse availability; drain time |
| E9 | all, as controls | F03-redis-cart-pod-kill, F04-checkout-bad-config, F05-node-preemption | availability and p95 of the affected journeys |
| E10 | HA option: a second frontend replica | F05-node-preemption | browse availability |

## Method

### Before and after

- **After** is the state in Git: every setting on.
- **Before** is the same cluster with only the setting under test switched off (Istio's or Kubernetes' default),
  the rest unchanged. One setting at a time, so a difference has one cause.
- Same day, same background load, back to back. Where a run is cheap, alternate: before, after, before, after.
  Each run is 5 minutes steady, the fault for its library duration (10 minutes unless the fault says otherwise),
  then 5 minutes of recovery.
- Start faults with the game-day CLI (`gameday start <id>`, #23). Write each run id from `gameday-runs/<run-id>.json`
  next to the result.
- The M3 game days run the same faults with this app already deployed. Their numbers are a cross-check, not the
  "before".

### Load

- The in-cluster load generator (10 users) runs as always. It calls the frontend directly, so it never meets the
  edge route or the rate limit.
- **Measured traffic** comes from the prober's on-demand API (#16) through the public path, as a user's would.
  This is the loop below. A minute-by-minute scheduled probe gives only 10 samples in a 10-minute fault, too few.
- E5, E6 and E7 also need the capacity load test (#29) through the gateway, at multiples of the highest rate that
  holds the SLO ("capacity" below; #29 measures it). The load test runs in the cluster and calls the gateway
  Service directly, so Cloud Armor's per-client limit does not cut it off.

```bash
# Drive on-demand probes for 20 minutes and keep every result. Run from a machine with kubectl access.
label=E1-before-1
kubectl -n prober port-forward svc/prober 8080:8080 >/dev/null 2>&1 &
pf=$!; trap 'kill $pf' EXIT; sleep 2
end=$((SECONDS + 1200))
while [ "$SECONDS" -lt "$end" ]; do
  for cuj in browse add-to-cart view-cart checkout; do
    curl -s -X POST localhost:8080/v1/run -H 'Content-Type: application/json' \
      -d "{\"cuj\":\"$cuj\",\"runs\":5}" |
      jq -c --arg at "$(date -u +%FT%TZ)" '. + {at: $at}' >>"runs-$label.jsonl"
  done
  sleep 20
done
```

### Metrics

Main metrics, for each journey the fault touches, over the fault window only:

1. **CUJ availability** = successful runs ÷ runs.
2. **CUJ p95 latency** of all runs (failed runs included, so a timeout counts at its full length).
3. **n**, the number of runs behind the two numbers above. With about 100 runs, availability has a resolution of
   about 1 %. Don't read differences smaller than that.

From the run file (`FROM`/`TO` are the fault's start and end, UTC):

```bash
jq -s --arg from "$FROM" --arg to "$TO" '
  map(select(.at >= $from and .at < $to))
  | group_by(.cuj)[]
  | [.[].results[]] as $r
  | {cuj: .[0].cuj, runs: ($r | length),
     availability: (($r | map(select(.ok)) | length) / ($r | length)),
     p95_ms: ($r | map(.durationMs) | sort | .[((length * 0.95) | ceil) - 1])}' "runs-$label.jsonl"
```

The same numbers from Managed Prometheus. The prober's on-demand series only appear after its first on-demand run
since the pod started, so compare increases within one prober pod:

```promql
# Availability of one journey over the window
sum(increase(mamori_cuj_probe_total{cuj="browse",source="on-demand",result="success"}[10m]))
  / sum(increase(mamori_cuj_probe_total{cuj="browse",source="on-demand"}[10m]))

# p95 latency of one journey over the window
histogram_quantile(0.95, sum by (le) (increase(mamori_cuj_probe_duration_seconds_bucket{cuj="browse",source="on-demand"}[10m])))

# Response codes at the edge (429 = shed, 504 = edge timeout, 503 with flag UO = pool overflow)
sum by (response_code, response_flags) (rate(istio_requests_total{reporter="source",source_workload="istio-ingressgateway"}[1m]))

# Retry amplification at the edge: requests reaching the frontend per request the gateway received
sum(rate(istio_requests_total{reporter="destination",destination_workload="frontend",source_workload="istio-ingressgateway"}[1m]))
  / sum(rate(istio_requests_total{reporter="source",source_workload="istio-ingressgateway"}[1m]))
```

The source side counts a request once, however often its sidecar retried it. The destination side counts every
attempt that arrived. Their ratio is the amplification. Setting-specific signals (Envoy counters, HPA replicas) are
named in each experiment. Envoy counters are read on the pod with `pilot-agent request GET stats` (commands in
[README.md](README.md#check-that-it-is-live)). Write them down at the start and end of each run. Istio creates
these counters only with the requested `proxyStatsMatcher` (README, "Where it lives"); without it, only the
Prometheus metrics above are available.

### Switching one setting off and on

Argo CD self-heals every Application, and `root` self-heals the Applications themselves. To switch a setting off
for a "before" run, pause auto-sync on `root` first, then on the Application that owns the object:

```bash
app=resilience   # the Application that owns the setting (last column of the table below)
kubectl -n argocd patch applications.argoproj.io root --type merge -p '{"spec":{"syncPolicy":{"automated":null}}}'
kubectl -n argocd patch applications.argoproj.io "$app" --type merge -p '{"spec":{"syncPolicy":{"automated":null}}}'
```

Then switch the setting off:

| Setting | Off for "before" | Application to pause |
|---|---|---|
| S1, S2 edge timeouts and retries | `git show <M1 commit>:deploy/online-boutique/public-virtualservice.yaml \| kubectl -n boutique apply -f -` (the M1 route: no timeout, mesh-default retries) | `online-boutique` |
| S3 mesh default retry | `kubectl -n istio-system edit configmap istio`: delete `defaultHttpRetryPolicy` from `mesh`. istiod reloads it in seconds | `istiod` |
| S4–S7 one host's DestinationRule | `kubectl -n boutique delete destinationrule <host>` | `resilience` |
| S8 one PodDisruptionBudget | `kubectl -n boutique delete pdb <name>` | `resilience` |
| S9 one HPA | `kubectl -n boutique delete hpa <name>`, then `kubectl -n boutique scale deploy <name> --replicas=1` | `resilience` |
| S10 load shedding | `kubectl -n istio-ingress exec deploy/istio-ingressgateway -c istio-proxy -- pilot-agent request POST 'runtime_modify?ingress_local_rate_limit_enforced=0'` (back with `=100`; a pod restart also resets it) | none |

Turn everything back on by re-applying `root`. It restores auto-sync on every child, and they put back what was
removed:

```bash
kubectl apply -f deploy/bootstrap/root-app.yaml
kubectl -n argocd get applications.argoproj.io      # wait until all are Synced and Healthy
```

Wait 2 minutes after restoring before an "after" run (configuration push, HPAs settling). Never leave `root` paused:
while it is, nothing merged to `main` reaches the cluster. `kubectl -n argocd get applications.argoproj.io root -o
jsonpath='{.spec.syncPolicy}'` shows it.

---

## E1. Edge retries and the retry budget (S2, S4)

**Hypothesis.** Retrying a failed page read helps when failures are partial and costs little when they are total.
The budget caps that cost under load.

- With F01 at its library setting (100 % UNAVAILABLE on currencyservice), browse is down either way. Every page
  calls currency, and retries cannot fix a hard failure. Without the budget, each failed page is tried 3 times:
  three times the load on the frontend and on every call a page makes before it reaches currency. With the budget,
  at load, retries stay under about 10 % of requests in flight.
- With a 10 % abort, a home page (10 currency calls) works about 35 % of the time and a product page (2 calls)
  about 81 %, so browse (home, then product) about 28 %. Two page retries lift that to about 72 % (my math,
  assuming independent aborts).

**Faults.** F01-currency-abort. Needed: the same fault at 10 % (`percentage: 10`). Injected aborts happen in the
frontend's sidecar before routing, so only the edge retry acts. The mesh retry never sees them.

**Switches.** S2 off: the M1 public route. S4 off: delete the `frontend` DestinationRule. Run the 100 % case twice
for S4: at prober load only (the budget's minimum of 3 concurrent retries lets every retry through, so expect no
difference), and with the load test at 0.5× capacity.

**Signals.** Amplification (query above). On the gateway: `cluster.outbound|80||frontend.boutique.svc.cluster.local.upstream_rq_retry`,
`.upstream_rq_retry_overflow` (retries the budget refused), `.upstream_rq_retry_success`.

| Run | Metric | Before | After | n, run ids |
|---|---|---|---|---|
| F01 100 %, prober load (S2) | browse availability | TBD (measured) | TBD (measured) | |
| | frontend requests per gateway request | TBD (measured) | TBD (measured) | |
| F01 100 %, 0.5× capacity (S4) | frontend requests per gateway request | TBD (measured) | TBD (measured) | |
| | browse p95 | TBD (measured) | TBD (measured) | |
| | retries refused by the budget | TBD (measured) | TBD (measured) | |
| F01 10 % (S2) | browse availability (predicted ≈ 28 % → ≈ 72 %) | TBD (measured) | TBD (measured) | |
| | browse p95 | TBD (measured) | TBD (measured) | |

## E2. Edge timeouts (S1)

**Hypothesis.** The edge timeout bounds the worst case at 6 s for a page and costs nothing while pages stay under
the 3 s per-try timeout. Where a try passes 3 s, cutting it and retrying helps if the slowness belongs to one pod,
and hurts if every try is slow.

- F02 (300 ms on every productcatalog call, about +2.1 s on a product page, #23) stays under 3 s. Expect the same
  availability and p95 before and after.
- F24 (a second productcatalog Deployment with 2 s extra latency behind the same Service) makes some product pages
  pass 3 s. Before, they succeed slowly. After, they are cut at 3 s and retried. Envoy's least-request balancing
  sends the retry's calls mostly to the healthy pod, so the retry likely succeeds. Expect the maximum latency to
  drop to ≤ 6 s; availability could move either way. That trade-off is what this measures.

**Switch.** S1 off: the M1 public route (no timeout).

**Signals.** Max and p99 of browse duration from the run file; 504s with flag `UT` at the gateway. With two or
more frontend pods, also `outlier_detection.ejections_enforced_total` on the gateway's frontend cluster: per-try
timeouts count as gateway errors, so slowness further down can get a healthy frontend pod ejected.

| Run | Metric | Before | After | n, run ids |
|---|---|---|---|---|
| F02 | browse availability | TBD (measured) | TBD (measured) | |
| | browse p95 | TBD (measured) | TBD (measured) | |
| F24 | browse availability | TBD (measured) | TBD (measured) | |
| | browse p95 / max | TBD (measured) | TBD (measured) | |
| | gateway 504 (UT) per minute | TBD (measured) | TBD (measured) | |

## E3. Writes are not repeated (S3, S2 for writes)

**Hypothesis.** With Istio's built-in default retry, a checkout that fails at shipping charges the card up to three
times: checkoutservice returns UNAVAILABLE after Charge, and the frontend's sidecar retries PlaceOrder. With the
safe default (S3) it charges once. Checkout fails either way: shipping is down.
The cost side: a read call that fails with UNAVAILABLE from a pod is no longer retried at that hop. During a
currencyservice pod kill (one replica), browse availability with S3 should be no worse than without it. With one
replica, a retry at that hop goes to the same dying pod anyway.

**Faults.** F28-shipping-shiporder-abort (#23: aborts only `/hipstershop.ShippingService/ShipOrder` with
UNAVAILABLE). For the cost side, a pod kill of currencyservice: the library's pod-failure fault for it, or
`kubectl -n boutique delete pod -l app=currencyservice` once a minute during the window.

**Switch.** S3 off: delete `defaultHttpRetryPolicy` from the `istio` ConfigMap (pause `root` and `istiod`).

**Signals.** Charge calls per PlaceOrder the frontend made (each attempt that reaches checkout charges once, and
the frontend's sidecar counts its own call once however often it retried):

```promql
sum(increase(istio_requests_total{reporter="destination",destination_workload="paymentservice"}[10m]))
  / sum(increase(istio_requests_total{reporter="source",source_workload="frontend",destination_workload="checkoutservice"}[10m]))
```

| Run | Metric | Before (Istio default) | After (S3) | n, run ids |
|---|---|---|---|---|
| F28 | Charge calls per order (predicted 3 → 1) | TBD (measured) | TBD (measured) | |
| | checkout p95 (failed checkouts) | TBD (measured) | TBD (measured) | |
| currencyservice pod kill | browse availability | TBD (measured) | TBD (measured) | |
| | browse p95 | TBD (measured) | TBD (measured) | |

## E4. One bad pod out of two (S5 connect timeout, S6 keepalive, S7 outlier detection)

**Hypothesis.** When one of two productcatalogservice pods stops answering the frontend while its readiness probe
stays green, the DestinationRule gets it out of the way within about 30 s. Without it, about half of the pages hang
or fail for the whole fault.

- Before (no DestinationRule): connects to the bad pod wait 10 s (Istio's default), the open HTTP/2 connection to it
  hangs until the edge gives up, nothing is ejected.
- After: the open connection is closed within about 13 s (PING every 10 s, 3 s to answer), new connects fail after
  1 s and are retried on the other pod (`connect-failure` is always retried), and after 3 gateway errors in a row
  the bad pod is ejected for 30 s, longer each time. Expect browse back above 99 % and p95 back under 500 ms within
  about 30 s of the fault start (estimate).

**Fault needed** (not in the library yet; requested from #23 through #32, since it needs two replicas): set the
productcatalogservice HPA's minimum to 2 for the test, then a Chaos Mesh NetworkChaos `partition` between the
frontend pods and one productcatalogservice pod. The kubelet still reaches the pod, so readiness stays green and
Kubernetes keeps it in the Service. That is the case only outlier detection can handle.

**Switch.** Delete the `productcatalogservice` DestinationRule.

**Signals.** Browse availability and p95 per minute. On the frontend's sidecar, for
`outbound|3550||productcatalogservice.boutique.svc.cluster.local`: `outlier_detection.ejections_enforced_total`,
`upstream_cx_connect_timeout`, `upstream_cx_destroy_local_with_active_rq`.

| Metric | Before | After | n, run ids |
|---|---|---|---|
| browse availability, whole fault | TBD (measured) | TBD (measured) | |
| browse availability, minute 2 to the end | TBD (measured) | TBD (measured) | |
| browse p95, whole fault | TBD (measured) | TBD (measured) | |
| time from fault start to first ejection | n/a | TBD (measured) | |

## E5. Connection-pool limits (S5)

**Hypothesis.** The caps (100 calls in flight, 50 queued, per client pod) never trip at normal or capacity load, and
trip under a real pile-up. Then they turn calls that would hang into instant errors. For recommendationservice the
frontend renders the page without recommendations, so pages get faster, not fail.

- At capacity load with no fault: zero overflows. This is the "no false trips" check, and it matters as much as the
  rest.
- F06 (CPU stress on recommendationservice) with its HPA held at one replica and the load test at 0.7× capacity:
  calls to the stressed pod slow down, and the frontend waits for them with no deadline. If more than 150 calls pile
  up from one frontend pod, the rest fail at once and the product and cart pages render without
  recommendations. If the pile-up never gets that big, the result is "no trip", which is also worth knowing. The
  right fix for this call would then be a deadline in the frontend fork (#28), as it already has for ads.

**Switch.** Delete the `recommendationservice` DestinationRule (and the `frontend` one for the capacity check).
Hold the HPA with `kubectl -n boutique patch hpa recommendationservice -p '{"spec":{"maxReplicas":1}}'` while
`resilience` is paused.

**Signals.** `upstream_rq_pending_overflow` and `upstream_rq_active` for the recommendation cluster on the frontend's
sidecar; 503s with flag `UO`; frontend memory.

| Run | Metric | Before | After | n, run ids |
|---|---|---|---|---|
| capacity load, no fault | pool overflows (expected 0 in both) | TBD (measured) | TBD (measured) | |
| F06 at 0.7× capacity | browse p95 | TBD (measured) | TBD (measured) | |
| | view-cart p95 | TBD (measured) | TBD (measured) | |
| | pool overflows | TBD (measured) | TBD (measured) | |
| | frontend memory, peak | TBD (measured) | TBD (measured) | |

## E6. Autoscaling (S9)

**Hypothesis.**

- F06: the HPA sees the stressed `server` container at about twice its CPU request and adds a second
  recommendationservice pod within about 1–2 minutes. Least-request balancing then sends most calls to the
  unstressed pod, and product and cart pages return to near their normal p95 within about 3 minutes of the fault
  start. Without the HPA they stay slow for the whole fault. This only works if F06's stressor runs in the `server`
  container's cgroup: the HPA measures that container alone.
- Load ramp (load test from 0.5× to 2× single-replica capacity over 20 minutes): with HPAs, p95 stays under 500 ms
  to a higher rate than without. The ratio between the two "highest rate that holds the SLO" numbers is the HPAs'
  measured gain.

**Switch.** Delete the HPA and scale the Deployment to 1 (table above). For the ramp, delete all four.

**Signals.** Replicas over time (`kubectl -n boutique get hpa --watch`, with timestamps), time from load step to a
new pod Ready, CPU per container.

| Run | Metric | Before | After | n, run ids |
|---|---|---|---|---|
| F06 | view-cart p95 | TBD (measured) | TBD (measured) | |
| | browse p95 | TBD (measured) | TBD (measured) | |
| | time to second replica Ready | n/a | TBD (measured) | |
| Load ramp | highest rate with browse p95 < 500 ms | TBD (measured) | TBD (measured) | |
| | replicas at peak (frontend / currency / productcatalog / recommendation) | 1 / 1 / 1 / 1 | TBD (measured) | |

## E7. Load shedding (S10)

**Hypothesis.** Above capacity, without shedding, everyone's requests slow down, timeouts and 5xx rise, and every
journey fails. With shedding, the excess gets 429 within milliseconds, and the admitted requests keep their p95
under 500 ms. At 1.5× capacity, with the limit at 90 % of capacity, about 40 % of requests are shed (1 − 0.9/1.5, my math). Journeys still fail when one of
their requests draws a 429; that is the price, and it counts against the SLO.

**Fault.** Not in the library: overload from the load test (#29) through the gateway at 1.5× and 3× capacity, with
the limit first set from #29's result (README, S10).

**Switch.** Enforcement off through the Envoy runtime (table above). No Git change.

**Signals.** Response-code mix at the gateway, p95 of the 200s:

```promql
histogram_quantile(0.95, sum by (le) (rate(istio_request_duration_milliseconds_bucket{reporter="source",source_workload="istio-ingressgateway",response_code="200"}[1m])))
```

Plus `ingress_local_rate_limit.http_local_rate_limit.rate_limited` on the gateway. It also counts while enforcement
is off, so the "before" run shows what would have been shed.

| Run | Metric | Before | After | n, run ids |
|---|---|---|---|---|
| 1.5× capacity | p95 of admitted (200) requests | TBD (measured) | TBD (measured) | |
| | share 429 / share 5xx | TBD (measured) | TBD (measured) | |
| | browse and checkout availability (prober) | TBD (measured) | TBD (measured) | |
| 3× capacity | p95 of admitted (200) requests | TBD (measured) | TBD (measured) | |
| | share 429 / share 5xx | TBD (measured) | TBD (measured) | |

## E8. Disruption budgets (S8)

**Hypothesis.**

- Two-node drain: with the frontend at two replicas on two nodes, draining both nodes back to back evicts both
  frontends at once without a budget, and browse fails until a replacement is Ready (about 15–30 s, estimate). With
  `maxUnavailable: 1`, the second eviction waits for the first replacement, and browse never fails.
- Spend guard: at rest (every HPA at one replica) the budgets add no wait to the scale-to-zero drain. Expect the
  same drain time as the M1 spend-guard test.

**Faults.** Needed: `kubectl drain <node-A> --ignore-daemonsets --delete-emptydir-data`, then the same for node B
right away, where A and B hold the two frontend pods (set the frontend HPA's minimum to 2 for the test, and
`kubectl uncordon` both afterwards). Spend guard: the "real run" in
[runbooks/spend-guard.md](../runbooks/spend-guard.md#test-it-safely), at a time an outage is fine.

**Switch.** Delete the `frontend` PodDisruptionBudget.

**Signals.** Browse availability per 10 s from the run file; for the spend guard, the time from the function's
"setting node count to 0" log line to `kubectl get nodes` showing none, and its outcome (`scaled_to_zero` or
`resize_running`).

| Run | Metric | Before | After | n, run ids |
|---|---|---|---|---|
| Two-node drain | browse availability during the drains | TBD (measured) | TBD (measured) | |
| | longest browse outage | TBD (measured) | TBD (measured) | |
| Spend guard at rest | drain time to 0 nodes | TBD (measured, M1) | TBD (measured) | |

## E9. Controls: faults these settings should not fix

A control shows the settings are not credited with luck. Expected: no real difference, because the fault is
outside what client-side policy can do. Run each once with every setting off (pause the Applications, delete the
`resilience` objects, put back the M1 public route and Istio's default retry, as in the table above) and once with
every setting on.

- **F03-redis-cart-pod-kill.** One stateful Redis with no disk. Carts are lost, and add-to-cart, view-cart and
  checkout fail until it is back. TCP keepalive only matters when a node vanishes, not for a pod kill.
- **F04-checkout-bad-config.** Every checkout replica is bad. Outlier detection cannot eject all of them, and never
  acts on one replica. The fix is a rollback (#25). With S3, failed checkouts are no longer repeated (fewer calls,
  same availability).
- **F05-node-preemption.** Spot preemptions ignore PodDisruptionBudgets, and every service has one replica at rest.
  Whatever ran on the lost node is down until it is rescheduled (about 1–2 minutes, estimate). The short connect
  timeout and the keepalives may shorten the tail. The real fix is more than one replica (E10).

| Fault | Metric | Before (all off) | After (all on) | n, run ids |
|---|---|---|---|---|
| F03 | add-to-cart, view-cart, checkout availability | TBD (measured) | TBD (measured) | |
| | time to recover | TBD (measured) | TBD (measured) | |
| F04 | checkout availability | TBD (measured) | TBD (measured) | |
| F05 | browse availability | TBD (measured) | TBD (measured) | |
| | time to recover | TBD (measured) | TBD (measured) | |

## E10. Option: a second frontend replica (costed, not on by default)

**Hypothesis.** With two frontend replicas on different nodes, F05 no longer takes the frontend down. Browse still
fails for pages that need a single-replica service from the lost node, so the gain is partial. It is largest when
the lost node held the frontend.

**Cost.** +120m CPU and +128 MiB at rest (frontend 100m/64 MiB plus its sidecar), about a fifth of the ≈ 550m left
for M2–M6. And with two replicas, the frontend's budget can hold a scale-to-zero drain for up to an hour (README,
"Spend guard"). Keep it only if E10 shows a clear gain.

**Change for the test.** Set the frontend HPA's `minReplicas: 2`, and spread the pods over nodes with this patch in
`deploy/online-boutique/kustomization.yaml`. `ScheduleAnyway` means a missing or cordoned node never leaves a pod
Pending:

```yaml
  - patch: |-
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: frontend
      spec:
        template:
          spec:
            topologySpreadConstraints:
              - maxSkew: 1
                topologyKey: kubernetes.io/hostname
                whenUnsatisfiable: ScheduleAnyway
                labelSelector:
                  matchLabels:
                    app: frontend
```

The same reasoning applies to the Istio ingress gateway, which also runs one replica (`deploy/istio/gateway-values.yaml`
calls raising it "a hardening task with a measured before/after"). Test it the same way, with a gateway pod's node as
the F05 target.

| Run | Metric | Before (1 frontend) | After (2, spread) | n, run ids |
|---|---|---|---|---|
| F05, node with a frontend pod | browse availability | TBD (measured) | TBD (measured) | |
| | time to recover | TBD (measured) | TBD (measured) | |
| F05, node without a frontend pod | browse availability | TBD (measured) | TBD (measured) | |
