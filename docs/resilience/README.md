# Resilience settings (M4, issue #27)

Status: designed and checked offline on 2026-11-13. Nothing has run on a cluster yet, so every value below is a
starting estimate. [experiments.md](experiments.md) says how each one is measured, before and after, against the
faults in the game-day library. Numbers move from "estimate" to "measured" only there.

What this adds to the shop (Online Boutique v0.10.7 with Istio 1.31.4):

- **DestinationRules** for every boutique host: retry budgets, connection-pool limits, connect timeouts, TCP
  keepalive, outlier detection.
- **Edge timeouts and retries** on the public route (gateway → frontend), split into static files, page reads and
  writes.
- **A safe mesh-wide default retry policy**: retry a call only when it cannot have reached the service.
- **PodDisruptionBudgets** that allow one eviction at a time.
- **HorizontalPodAutoscalers** (CPU) for the four services on the hot path.
- **Load shedding** at the Istio ingress gateway: a token-bucket rate limit that answers 429.

## Where it lives

| File | What |
|---|---|
| `deploy/apps/resilience.yaml` | Argo CD Application `resilience`, sync wave 6, automated prune and self-heal |
| `deploy/resilience/destination-rules.yaml` | 11 DestinationRules: frontend, the 9 gRPC services, redis-cart |
| `deploy/resilience/pod-disruption-budgets.yaml` | 11 PodDisruptionBudgets: the 10 services and redis-cart |
| `deploy/resilience/horizontal-pod-autoscalers.yaml` | 4 HPAs: frontend, currencyservice, productcatalogservice, recommendationservice |
| `deploy/resilience/ingress-rate-limit.yaml` | EnvoyFilter `ingress-local-rate-limit` in `istio-ingress` |
| `deploy/online-boutique/public-virtualservice.yaml` | Edge timeouts and retries. **Requested change** to a base file; #32 applies it |
| `deploy/istio/istiod-values.yaml` | `meshConfig.defaultHttpRetryPolicy` (already in #32's integration tree), and `proxyStatsMatcher` for the counters that show these settings at work. **Requested change** to a base file |
| `deploy/apps/online-boutique.yaml` | `ignoreDifferences` on Deployment replicas, so Argo CD leaves them to the HPAs. **Requested change** |

The exact diffs for these three base files, plus small index and runbook edits, are in the #27 pull request; #32
applies them after the merge. Until then the edge keeps Istio's defaults (no timeout, 2 retries on connection-level
failures), and the mesh keeps Istio's built-in retry policy.

## Rules this design follows

1. **One VirtualService per host.** Istio's behaviour with two sidecar VirtualServices for one host is undefined.
   The per-host VirtualServices for the upstream services are reserved for fault injection: the discovery run (#17)
   and the game days (#23). So nothing here can set a per-call timeout or a per-method retry rule for a gRPC call
   between services. What is left, and what this uses: one DestinationRule per host (traffic policy for whoever
   calls it, which works next to any fault VirtualService), the one public route (gateway → frontend), and the
   mesh-wide default retry policy.
2. **Retry at one layer.** Page reads are retried whole, at the edge. Calls between services are retried only when
   the call cannot have reached the service. Retries at two layers multiply: 3 tries at the edge × 3 in the mesh
   would be 9 calls to a failing service.
3. **Every retry has a budget.** Retries in flight may be at most 10 % of active requests per client, so a broad
   failure cannot triple the load.
4. **Fail fast, shed early.** Short connect timeouts, caps on calls in flight, a rate limit at the gateway. A quick
   error frees resources; a request queued behind a slow service holds them.
5. **Never turn a partial failure into a total one.** Outlier detection never ejects more than half of a service's
   pods, and switches off below half healthy.
6. **Free at rest, and never in the way of the spend guard.** HPAs start at one replica, so nothing extra runs at
   rest. PodDisruptionBudgets allow one eviction, so they never block a drain of a one-replica workload.
7. **mTLS as in the base.** No DestinationRule has a `tls` block, so Istio's auto mTLS stays in charge, exactly as
   before: the base has no PeerAuthentication (PERMISSIVE), and sidecar-to-sidecar traffic is mutual TLS. Checked
   in istiod 1.31.4 (`pilot/pkg/networking/core/cluster_tls.go`): with no `tls` in the rule, auto mTLS applies.

## Which calls are safe to retry

From the Online Boutique v0.10.7 source (tag commit `5b608cb`, read on 2026-11-13). "Idempotent" means doing it
twice leaves the same result as doing it once.

| Caller → service | Call | Idempotent | Notes |
|---|---|---|---|
| frontend → productcatalogservice | ListProducts, GetProduct | yes | every page; checkout too |
| frontend → currencyservice | GetSupportedCurrencies, Convert | yes | about 10 Convert calls per home page |
| frontend → cartservice | GetCart | yes | home, product and cart pages (cart size in the header) |
| frontend → cartservice | AddItem | **no** | adds the quantity again: `existingItem.Quantity += quantity` (`RedisCartStore.cs`) |
| frontend → cartservice | EmptyCart | yes | same end state |
| frontend → recommendationservice | ListRecommendations | yes | product and cart pages; the frontend logs its errors and renders without it, but waits with no deadline |
| frontend → adservice | GetAds | yes | the frontend sets a 100 ms deadline and shows no ad on error |
| frontend → shippingservice | GetQuote | yes | cart page |
| frontend → checkoutservice | PlaceOrder | **no** | new order id; charges, ships and emails |
| checkoutservice → cart, productcatalog, currency, shipping | GetCart, GetProduct, Convert, GetQuote | yes | |
| checkoutservice → shippingservice | ShipOrder | **no** | new tracking id each time (a mock) |
| checkoutservice → paymentservice | Charge | **no** | new transaction id each time (a mock card) |
| checkoutservice → emailservice | SendOrderConfirmation | **no** | sends again; checkoutservice only logs its failure |
| checkoutservice → cartservice | EmptyCart | yes | result ignored |
| cartservice → redis-cart | Redis over TCP | n/a | no HTTP retries exist for TCP |

The frontend's HTTP routes (`src/frontend/main.go`): every GET and HEAD route reads, or clears cookies
(`/logout`). The POST routes are `/cart` (AddItem), `/cart/empty`, `/setCurrency` (a cookie), `/cart/checkout`
(PlaceOrder) and `/bot` (the shopping assistant, not deployed). The edge treats every non-GET, non-HEAD request as a
write.

**A duplicate-charge path under Istio's defaults.** With no retry policy of its own, every route gets Istio's
built-in one (istiod 1.31.4, `pilot/pkg/networking/core/route/retry/retry.go`): 2 retries on
`connect-failure,refused-stream,unavailable,cancelled,retriable-status-codes`. Here that covers every gRPC call
between services. Now look at `PlaceOrder` (`src/checkoutservice/main.go`): it charges the card, then calls
ShipOrder, and if shipping fails it returns `codes.Unavailable`. The frontend's sidecar sees UNAVAILABLE and calls
PlaceOrder again, up to twice. Each attempt charges the (mock) card and fails at shipping again. So one failed
checkout can charge three times. The same default could also repeat AddItem, Charge or SendOrderConfirmation if a
server sidecar answers UNAVAILABLE after its app crashed mid-call. That case is rarer, but it is the same flaw.

So the mesh-wide default becomes `connect-failure,refused-stream,reset-before-request`: retry only when the call
never reached the service (requested change to `deploy/istio/istiod-values.yaml`). What it costs: a read call that
fails with UNAVAILABLE from the service is no longer retried at that hop. The edge retries the whole page instead,
and outlier detection moves traffic away from a pod that keeps failing. Experiment E3 measures both sides (#23
added fault `F28-shipping-shiporder-abort` for it).

## The settings

| # | Setting | Value (estimate) | Where | Experiment |
|---|---|---|---|---|
| S1 | Edge timeouts | page reads and static files: 6 s in all, 3 s per try; writes: 10 s | public route | E2 |
| S2 | Edge retries | reads and static: 2 retries on `5xx` (includes connect failures, resets, per-try timeouts); writes: 2 retries on `connect-failure,refused-stream,reset-before-request` only | public route | E1, E3 |
| S3 | Mesh default retry | 2 retries on `connect-failure,refused-stream,reset-before-request` | `meshConfig` | E3 |
| S4 | Retry budget | retries in flight ≤ 10 % of active requests, never fewer than 3 | every HTTP and gRPC DestinationRule | E1 |
| S5 | Connection pools | connect timeout 1 s; ≤ 100 calls in flight and ≤ 50 queued per client pod, and again on the server's own sidecar for all callers together; frontend: ≤ 100 connections | every DestinationRule | E5 |
| S6 | TCP keepalive | 30 s idle, then 3 probes 10 s apart: an idle connection to a vanished peer is closed in about a minute | every DestinationRule | E9 |
| S7 | Outlier detection | leaf services: eject after 3 gateway errors or 5 errors of any 5xx kind in a row; services that call others: only after 3 failures to reach the pod in a row; 30 s, growing; ≤ 50 % of pods; off below 50 % healthy | DestinationRules except redis-cart | E4 |
| S8 | PodDisruptionBudgets | `maxUnavailable: 1`, `unhealthyPodEvictionPolicy: AlwaysAllow` | 11 PDBs | E8 |
| S9 | HPAs | app container CPU at 80 % of its request; frontend and currency 1–3, productcatalog and recommendation 1–2 | 4 HPAs | E6 |
| S10 | Load shedding | 40 requests/s, bursts to 80, per gateway pod; static files exempt; 429 | EnvoyFilter | E7 |

### Edge timeouts and retries (S1, S2)

The public route is the one place where a timeout and a retry policy can be set without a per-host VirtualService
for an upstream service. It is also where a user's request starts, so a timeout here bounds the whole page.

- **6 s for a page, 3 s per try.** The latency SLO threshold is 500 ms (1,000 ms for checkout). A page still
  loading after 3 s has already failed it. A second try, on another frontend pod when there is one, has a chance
  of being fast. 6 s stays under the prober's step timeout (12 s, #16) and the load balancer's backend timeout
  (30 s, `deploy/edge/backend-policy.yaml`), so a slow page shows up in the mesh metrics as this gateway's 504
  (response flag `UT`), not as a client giving up. The cost: a page that is uniformly slower than 3 s turns from a
  slow success into an error. #23 sized the library's latency faults below that (F02 adds about 2.1 s to a
  product page). F24 (a slow canary) can cross it, and there a retry may help. E2 measures this trade-off.
- **Writes: 10 s, retried only before they reach the frontend.** A repeated checkout is a second order. A
  checkout that times out at the edge may still complete behind it, so the write timeout is long (10 s, ten times
  the checkout SLO threshold).
- **Static files** get their own route only so that the rate limit can skip them (S10).
- When the edge gives up, Envoy resets the request to the frontend. Go cancels the request's context, and the
  frontend passes `r.Context()` into every gRPC call, so the calls below it are cancelled too.

### Retry budget, connection pools, keepalive, outlier detection (S4–S7)

In each DestinationRule. The file header in `destination-rules.yaml` explains each value.

- **Retry budget, not `maxRetries`.** Istio 1.31's `trafficPolicy.retryBudget` maps to Envoy's retry budget
  (checked in istiod 1.31.4, `cluster_traffic_policy.go`, `applyRetryBudget`). Envoy uses the budget *instead of*
  the fixed `max_retries` circuit breaker (Envoy `circuit_breaker.proto`). A fixed count is too loose at low load
  and too tight at high load. Istio's default is no limit at all (2^32−1 concurrent retries, no budget). The 10 % follows
  the per-client retry budget in Google's SRE book (chapter "Handling Overload"). The minimum of 3 keeps single
  retries working at the lab's low traffic.
- **Connection pools** cap the calls in flight from one client pod to one service: 100 active plus 50 queued.
  Past that, the call fails at once (503, Envoy flag `UO`). The caps sit far above expected use (about 10× the
  calls in flight at the capacity limit, estimate), so they only trip when a service is badly behind. Then they
  keep a slow service from holding every goroutine and socket of its callers. For the frontend (HTTP/1.1), the same
  caps bound the requests the gateway sends to it: a second, concurrency-based layer of load shedding. Istio also
  applies a DestinationRule's `connectionPool` to the receiving pod's own sidecar (its inbound cluster; istiod
  1.31.4 `cluster_builder.go`), so the same caps hold there for all callers together.
- **Connect timeout 1 s** instead of Istio's 10 s: a pod IP that has gone away fails fast and the call is retried
  on another pod (`connect-failure` is always retried).
- **TCP keepalive** (30 s idle, then 3 probes 10 s apart) closes an idle connection to a vanished peer in about a
  minute instead of the kernel's two hours. It is handled by the kernel, so the apps never see it. It does not
  help a connection with calls in flight: those wait until the edge gives up on the page (6 s), and the connection
  until TCP's retransmission timeout (up to about 15 minutes on Linux, estimate). A per-call deadline would fix
  that, and it needs a VirtualService for the host, which is reserved for faults.
  **Why not HTTP/2 PINGs** (`http2KeepAlive`): the inbound rule above means the server's own sidecar would also
  ping its gRPC app. gRPC servers allow one ping per 5 minutes without data and answer more with GOAWAY
  `too_many_pings`: connection churn, and noise in what the agent reads. Under CPU stress the ping timeout would
  also turn slowness into resets.
- **Outlier detection.** For the leaf services, a pod is ejected after 3 gateway errors in a row (connect failure,
  reset, timeout, or 502/503/504: the signs of a sick pod), or after 5 errors of any 5xx kind in a row. For gRPC,
  Envoy turns a `grpc-status` in the response headers (a trailers-only error, what these unary services send) into
  an HTTP code for this purpose (Envoy `router.cc`): UNAVAILABLE becomes 503, DEADLINE_EXCEEDED 504.
  The three services that call others (frontend, checkoutservice, recommendationservice) count only failures to
  reach the pod itself: `splitExternalLocalOriginErrors: true` with `consecutiveLocalOriginFailures: 3`, and both
  response-based counters at 0 (leaving a field out would keep an Envoy default). In split mode, Envoy counts
  connect failures and timeouts as local-origin failures and every response as external (Envoy
  `outlier_detection_impl.cc`). Their error responses usually mean a dependency failed. checkoutservice even
  answers UNAVAILABLE (a gateway error) when shipping fails, so counting responses would eject healthy checkout
  pods during a shipping fault, and every pod fails the same way. Ejection lasts
  30 s and grows each time. At most half of the pods are ejected, and with fewer than half healthy Envoy ignores
  ejections (`minHealthPercent: 50`, Envoy's panic threshold). So with one replica, outlier detection never acts:
  ejecting the only pod would turn some errors into all errors. It starts to matter when an HPA has scaled a
  service out, or in the HA option (E10). redis-cart has none: TCP, one stateful pod. One known weak spot: the
  gateway's per-try timeout (3 s) is a timeout, so it still counts as a local-origin failure against the frontend
  pod, even when a slow dependency is to blame. With two or more frontend pods, a uniformly slow dependency can get
  one of them ejected for 30 s. E2 watches for that.
- **What Istio-injected faults cannot test.** Istio aborts and delays run in the calling sidecar's fault filter,
  before routing. They never reach the retry policy, the pools or outlier detection at that hop. Istio's docs say
  so for retries and timeouts ("timeouts or retries will not be enabled when faults are enabled on the client
  side"). So F01 (currency abort) tests the edge retry and its budget, not the mesh retry. Outlier detection
  needs a real failure in one of two pods (E4).

### PodDisruptionBudgets (S8)

One per shop workload, `maxUnavailable: 1`. With one replica that allows the eviction, so it never blocks a node
drain. With an HPA at two or more replicas, it paces evictions: during a GKE surge upgrade (one node at a time), the
second pod is not evicted before the first one's replacement is Ready. `unhealthyPodEvictionPolicy: AlwaysAllow`
(stable since Kubernetes 1.31) lets a pod that is not Ready be evicted anyway, so a broken pod never blocks a drain.
Spot preemptions ignore PodDisruptionBudgets: these only cover voluntary evictions (drains, upgrades, the spend
guard's resize).

**Spend guard.** GKE respects PodDisruptionBudgets for up to one hour when it removes nodes (GKE docs, "Resize a
cluster"). Scaling the pool to zero is the one drain where a replacement pod has nowhere to run. So a budget can
hold it in one case only: an HPA has scaled a Deployment to two or more replicas. At rest every HPA is at one
replica and nothing waits. Worst case, the last node lives up to an hour longer: about $0.05 of Spot time per node
(e2-standard-2 Spot in Tokyo, $0.051584/h, my math). To cut it short:
`kubectl -n boutique delete pdb -l app.kubernetes.io/component=resilience`. Argo CD puts them back once the nodes
return. This line is a requested addition to `docs/runbooks/spend-guard.md`. E8 measures the drain time with the
budgets in place.

### HPAs (S9)

| HPA | Min–max | Why |
|---|---|---|
| frontend | 1–3 | Every request; renders HTML and fans out to 10–15 gRPC calls per page |
| currencyservice | 1–3 | Node.js, single-threaded; about 10 Convert calls per home page |
| productcatalogservice | 1–2 | Every page and checkout |
| recommendationservice | 1–2 | Python; product and cart pages; the target of F06 (CPU stress) |

Not autoscaled: cartservice (cheap calls, and bound by the single redis-cart), checkout, payment, shipping and
email (one call each per order), adservice (a JVM, whose start-up CPU spikes skew a CPU target; the frontend caps
it at 100 ms anyway), redis-cart (stateful), the load generator. The capacity test (#29) may change this list.

- **Target: the `server` container's CPU at 80 % of its request** (`type: ContainerResource`, stable since
  Kubernetes 1.30). The Envoy sidecar's CPU is left out. The upstream request is 100m and the limit 200m. Throttling
  at the limit adds latency, so scaling starts well before it. If a service already runs above about 60 % of its
  request at steady load, its request is too low. Raise the request; don't let the HPA scale at rest.
- **Behaviour:** scale up at once, up to 2 pods per 15 s. Scale down only after 5 minutes of lower load, one pod a
  minute.
- **Argo CD:** the upstream Deployments set no `replicas`, so Argo CD does not diff it today.
  `ignoreDifferences` on `/spec/replicas` with `RespectIgnoreDifferences=true` (requested change to
  `deploy/apps/online-boutique.yaml`) keeps it that way if a manifest ever sets one.
- **Executor (#25):** `scale` refuses any Deployment an HPA targets (confirmed by #25). Its default bounds cover
  only the Deployments without an HPA.

### Load shedding (S10)

An Envoy local rate limit (token bucket) on the ingress gateway's port-80 listener: 40 requests/s sustained,
bursts to 80. Above that the gateway answers **429** at once, with header `x-local-rate-limit: true`, and the
requests it lets through keep their latency.

- **Public traffic only.** Port 80 is what the GKE load balancer sends to. Both EnvoyFilter patches match
  `portNumber: 80` only: the HTTP filter on that listener, and the static-file exemption in its route table
  (`http.80`). The cluster-internal port 8080 that #8 adds on the gateway, for the prober and load tests, is a
  separate Envoy listener with its own route table (`http.8080`). So in-cluster probes and load tests are never
  throttled, and they measure the shop rather than the limit. Never widen the match to all ports. Port 8080 is a
  second server in the same Istio Gateway, and the public VirtualService binds that Gateway with no port match, so
  the edge timeouts and retries (S1, S2) apply on 8080 too (confirmed with #8): probes get the same route policy
  as users, only without the rate limit.

- **Layers:** Cloud Armor limits each client IP at Google's edge (fairness). This filter limits the total entering
  the cluster (capacity). The frontend DestinationRule caps requests in flight (concurrency).
- **Static files are exempt.** A browser fetches many per page and they cost the frontend almost nothing, so
  counting them would make the number mean nothing about load. The exemption follows the route name `static` on
  the public route. If that route is missing, static files count against the limit (safe, not open).
- **The number is a placeholder.** It should be about 90 % of the highest rate that still meets the latency SLO
  with the HPAs at their maximum. The capacity test (#29) finds that rate on port 8080, outside the limit. For a
  with-and-without comparison on port 80 (E7), enforcement can be switched off through the Envoy runtime, with no
  Git change (see the file header).
- **One gateway replica** (`deploy/istio/gateway-values.yaml`), so this is the shop's total. With N replicas it
  would be N × 40/s.
- **429 counts as a failure** for the users who get it, and in the Cloud Monitoring view of the edge. The prober
  labels a 429 "(gateway rate limit)" (#16), but on port 8080 it never gets one. Shedding does not make overload
  free. It turns a slow collapse for everyone into fast refusals for some.
- **The in-cluster load generator** calls the frontend Service directly and is never limited here.
- **EnvoyFilter** is Istio's documented way to do this in 1.31 (no first-class API). It exposes Envoy internals:
  re-check it on every Istio upgrade. `istioctl validate` warns about exactly that.

## Resource budget

| What | CPU requests | Memory requests |
|---|---|---|
| At rest (all HPAs at 1 replica; DestinationRules, PDBs and the EnvoyFilter run no pods) | **0** | **0** |
| frontend, up to 2 more pods (100m + 20m sidecar, 64 + 64 MiB each) | up to 240m | up to 256 MiB |
| currencyservice, up to 2 more pods (same sizes) | up to 240m | up to 256 MiB |
| productcatalogservice, up to 1 more pod | up to 120m | up to 128 MiB |
| recommendationservice, up to 1 more pod (100m + 20m, 220 + 64 MiB) | up to 120m | up to 284 MiB |
| **All HPAs at their maximum** | **up to 720m** | **up to 924 MiB** |

At rest this takes none of the ≈ 550m left under the 70 % line for M2–M6. At full scale-out the cluster would hold
about 3,505m + 720m = 4,225m of requests before the other M2–M6 areas, about 73 % of the 5,790m allocatable, and up
to about 82 % with all of their budget used (my math, from `deploy/README.md`). That still fits on three nodes. It
is above the two-thirds line, though, so a node lost during a load peak leaves some pods Pending until load falls.
That is accepted: the extra pods exist only during peaks, and they are the ones that wait. The EnvoyFilter adds a
little CPU per request in the gateway (not measured).

## Check that it is live

```bash
kubectl -n argocd get applications.argoproj.io resilience          # Synced, Healthy
kubectl -n boutique get destinationrules,pdb,hpa
kubectl -n istio-ingress get envoyfilters.networking.istio.io

# The policy Envoy actually got, as seen from the frontend's sidecar (cluster for productcatalogservice):
kubectl -n boutique exec deploy/frontend -c istio-proxy -- \
  pilot-agent request GET 'config_dump?resource=dynamic_active_clusters&name_regex=.*productcatalogservice.*' \
  | grep -E -A3 '"(circuit_breakers|retry_budget|outlier_detection|connect_timeout|tcp_keepalive)"'

# Counters: retries, retries refused by the budget, queue overflows, connect timeouts, ejections.
# Istio creates these only with the requested proxyStatsMatcher (and after the proxy restarts).
kubectl -n boutique exec deploy/frontend -c istio-proxy -- pilot-agent request GET stats \
  | grep -E 'productcatalogservice.*(upstream_rq_retry|pending_overflow|connect_timeout|ejections_enforced_total)'

# Rate limit counters on the gateway (enforced, rate_limited, ok).
kubectl -n istio-ingress exec deploy/istio-ingressgateway -c istio-proxy -- pilot-agent request GET stats \
  | grep ingress_local_rate_limit

# Edge routes, with their timeouts and retry policies.
kubectl -n istio-ingress exec deploy/istio-ingressgateway -c istio-proxy -- \
  pilot-agent request GET 'config_dump?resource=dynamic_route_configs' | grep -E '"name": "(static|reads|writes)"'

# Same checks Istio's webhook runs, offline (pinned to the mesh version):
docker run --rm -v "$PWD":/w:ro -w /w \
  istio/istioctl:1.31.4@sha256:a95d863708d2d19c3ce4ec498b9e1f35c241b6be3d008cee49ccf5c7968fc46f \
  validate -f deploy/resilience/destination-rules.yaml -f deploy/resilience/ingress-rate-limit.yaml
```

Envoy's own counters above exist only with the requested `proxyStatsMatcher`. By default Istio creates just a few
of them (istiod 1.31.4, `pkg/bootstrap/config.go`), and that is not enough to see retries, ejections or rate
limiting. The experiments read them on the pod. They cost nothing in Managed Prometheus: #19's scrape keeps only
`istio_requests_total`, `istio_request_duration_milliseconds_*` and the TCP connection counters, and drops every
`envoy_*` series. Adding these to dashboards would mean about 900 more series, about $4.70 a month (#19's estimate).

## Not verified yet

Everything here is checked offline only: rendered by `deploy/validate.sh` (kubeconform, strict, Kubernetes 1.36.4)
and checked with `istioctl validate` 1.31.4. Still to confirm on the cluster:

- that the gateway route names `static`, `reads`, `writes` reach Envoy as written (istiod 1.31.4 names a route after
  the VirtualService route's `name`, `route.go`), so the rate-limit exemption matches;
- that #8's internal port 8080 shows up as its own listener (`0.0.0.0_8080`, route table `http.8080`) without the
  rate-limit filter, and with the three edge routes;
- that `pilot-agent request POST runtime_modify` switches enforcement (Istio's bootstrap has an admin runtime layer);
- that gRPC errors from these services arrive trailers-only, so outlier detection counts them;
- that `istio_requests_total` at the gateway shows the 429s (response flag `RL`);
- every value marked as an estimate.
