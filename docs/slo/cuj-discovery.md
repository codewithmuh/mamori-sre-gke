# CUJ discovery by fault injection

Status: built and unit-tested on 2026-11-13, **never run**: no cluster exists yet. Every duration and budget figure
below is an estimate until the first run.

The SLOs in this lab are per user journey (CUJ), and a journey's availability is the product of the success rates of
the services it critically depends on (decision D4, [PLAN.md](../../PLAN.md) section 4). Which services those are is
not written down by hand. It is measured: break one upstream service at a time, run every journey, and see which
journeys fail. The tool is [`tools/cuj-discovery`](../../tools/cuj-discovery/README.md); its output is
`slo/cuj-map.yaml`, which the SLO generator (#18) turns into recording rules, SLOs and alerts.

## Method

### 1. Journeys

The prober (#16) defines the journeys. Every run is a new session that starts on the home page, so the home page's
dependencies show up in every journey.

| Journey | Steps (step names as in the prober's `failedStep`) |
|---|---|
| `browse` | `home` (GET /), `product` (GET /product/{id}) |
| `add-to-cart` | `home`, `add-to-cart` (POST /cart; passes on the 302, which is not followed) |
| `view-cart` | `home`, `add-to-cart`, `view-cart` (GET /cart) |
| `checkout` | `home`, `add-to-cart`, `view-cart`, `place-order` (POST /cart/checkout with the demo's fake card) |

A run fails if any step fails. The tool uses the prober's on-demand API, `POST /v1/run {"cuj","runs"}`. Those runs are
recorded with `source="on-demand"`, which the SLOs ignore.

### 2. Preflight and baseline

Before touching anything the tool checks that no fault of any kind is in place in `boutique` (a game day running at
the same time would make the results meaningless), and that each target can be broken as planned (see Safety). Then
it runs each journey 10 times. If a single run fails, it stops: a shop that is already failing can't tell us what a
fault breaks.

### 3. One fault at a time

**gRPC services** (adservice, cartservice, checkoutservice, currencyservice, emailservice, paymentservice,
productcatalogservice, recommendationservice, shippingservice): an Istio `VirtualService` for the service's host
with an HTTP fault that aborts 100 % of requests with gRPC status `UNAVAILABLE`:

```yaml
apiVersion: networking.istio.io/v1
kind: VirtualService
metadata:
  name: cuj-discovery-currencyservice
  namespace: boutique
  labels: {mamori.dev/fault: "true", app.kubernetes.io/managed-by: cuj-discovery, ...}
spec:
  hosts: [currencyservice.boutique.svc.cluster.local]
  http:
    - fault:
        abort: {grpcStatus: UNAVAILABLE, percentage: {value: 100}}
      route:
        - destination: {host: currencyservice.boutique.svc.cluster.local}
```

- `grpcStatus` is part of `HTTPFaultInjection.Abort` in the Istio 1.31 reference (checked on istio.io for 1.31.4,
  2026-11-13); the docs ask for the name, `UNAVAILABLE`, not the number 14.
- The abort happens in the *caller's* sidecar, before routing. The service itself keeps running and stays healthy,
  so this measures "callers can't reach it", which is what a real outage looks like to the frontend.
- Istio's default retries don't hide it: the fault filter answers before the router, and Istio's reference notes that
  timeouts and retries are not enabled on a route with client-side faults.
- It only works if Istio treats the port as HTTP/2. Online Boutique names its gRPC ports `grpc`; the preflight
  refuses a target whose Service port isn't named `grpc*`/`http*` (or has a matching `appProtocol`), because otherwise
  the fault would silently do nothing and the service would look non-critical.
- The contract keeps per-host VirtualServices for upstream services free for faults (two VirtualServices for one
  host are undefined in Istio), and the preflight refuses if any other VirtualService already routes a target host.

**redis-cart** is plain TCP (port `tcp-redis`), and Istio's fault injection is HTTP only. Options considered:

| Option | Why (not) |
|---|---|
| **Scale the Deployment to 0, then back (chosen)** | Clear-cut: the connection breaks at once and every Redis call fails. Restores exactly. Needs no extra component |
| Istio `AuthorizationPolicy` DENY on redis-cart | Also an object with a label, but Envoy checks a TCP connection when it opens. cartservice keeps one long-lived Redis connection, which would survive until Envoy drains it, so the fault would start at an unknown time |
| Chaos Mesh `NetworkChaos` partition | Works, but depends on Chaos Mesh (#23) being installed and adds a privileged component to a measurement that needs none |
| `EnvoyFilter` with a TCP fault | Not a stable Istio API; fragile across upgrades |

How scale-to-zero is made safe: the tool first writes a marker `ConfigMap boutique/cuj-discovery-redis-cart` with the
old replica count (labelled like every fault), then scales to 0 and waits until the pods are gone. Removing the fault
scales back, then deletes the marker. If the tool dies in between, the marker is what `cleanup` uses to restore it.
The Deployment itself is never labelled `mamori.dev/fault`, so the executor's `remove-fault` (#25) can never delete
Redis. Side effects: every cart is lost (they live in an `emptyDir` anyway). cartservice itself stays up and Ready:
in v0.10.7 its health check never touches Redis (`RedisCartStore.Ping()` always returns `true`), so it keeps
answering, with `FailedPrecondition` ("Can't access cart storage"), and recovers as soon as the new redis-cart pod is
Ready, which should take seconds (estimate).

The marker ConfigMap carries no `mamori.dev/expires-at` (see Safety): a reaper that deleted it would leave
redis-cart at 0 and lose the replica count.

Argo CD: the upstream redis-cart manifest sets no `replicas`, so Argo CD's diff has nothing to compare and self-heal
does not undo the scale (rendered manifests checked 2026-11-13; only the load generator sets `replicas`). If that
changes (a manifest that sets `replicas`, an HPA on redis-cart), the tool's verify step notices that the fault
did not hold and discards the results.

After a fault is in place, the tool waits `--settle` (15 s) so the new route reaches every sidecar, then runs each
journey 20 times.

### 4. Check, remove, recover

After the runs it checks that the fault is still there. If the executor, a person, Argo CD or an autoscaler removed
it during the measurement, the numbers mean nothing, so the run stops. Then it removes the fault and runs every
journey twice, every 10 s, until all pass (up to 3 minutes). Measuring the next service while the shop is still
recovering from the last one would blame the wrong service.

### 5. Classify and write

`failureRate = failed runs / runs` for every (journey, service) case. A service is **critical** for a journey when
the rate is at least `--threshold` (0.5), else **non-critical**. The map lists every broken service in exactly one
of the two lists, with its rate. `frontend` is also in every journey's `critical` list, without a `results` entry:
every prober step is a request to it, so it is critical by construction and is never broken on purpose. Journeys and
lists are sorted, so the same results always give the same file.

Only a full run (all four journeys, all ten services) may write `slo/cuj-map.yaml`, the file slo-gen reads. A run
with `--cujs` or `--services` must name another file with `--out`, and `cuj-discovery validate` rejects a map with
journeys or services missing unless given `--partial`.

## Safety

- **Only namespace `boutique`.** Hard-coded, no flag.
- **Labels.** Everything it creates carries `mamori.dev/fault: "true"` (the lab-wide fault label the executor and
  the game-day CLI know), `app.kubernetes.io/managed-by: cuj-discovery`, `mamori.dev/discovery-run: <run id>` and
  `mamori.dev/discovery-target: <service>`, and an annotation saying how to remove it.
- **Expiry.** Each VirtualService carries the lab-wide annotation `mamori.dev/expires-at` (RFC 3339, UTC: the run's
  start plus `--timeout`), so #23's in-cluster reaper can delete it if this process dies. The redis-cart marker
  ConfigMap deliberately has none: deleting it would not scale redis-cart back. A reaper must leave objects without
  `expires-at` alone (and so never deletes ConfigMaps from this tool); `make discovery-cleanup` restores redis-cart.
- **Refuses to start** when any object of any kind in `boutique` is labelled `mamori.dev/fault=true` (ours from a
  killed run, or a game-day fault: VirtualServices, Chaos Mesh objects, anything the API lists), when another
  VirtualService routes a target host, when a target port would be treated as TCP, or when redis-cart is already at
  0.
- **Always cleans up.** Each fault is removed before the next one starts and on every way out: success, error,
  Ctrl-C, SIGTERM, SIGHUP (a closed terminal), and `--timeout`. Removal runs on its own context, so it still runs
  when the run's context has been cancelled, and each step (delete the VirtualService, scale redis-cart back, delete
  the marker) retries with backoff (1 s doubling to 15 s) for up to 5 minutes, logging every failure as an `ERROR`.
  The API path can blip like the prober's port-forward can (a zonal control-plane upgrade, the laptop's network, an
  expired token), and one failed delete must not leave a 100 % abort in place. A final sweep then deletes anything
  still carrying this run's label, with the same retries. While it cleans up, further signals are ignored, and
  SIGPIPE is ignored so that writing to a closed terminal or `| tee` can't kill it first.
- **`cleanup` subcommand** (`make discovery-cleanup`) for the one case the tool can't handle itself: being killed
  (`kill -9`, a closed laptop, a lost network). It removes only objects with this tool's labels, restores redis-cart
  from the marker (only for the known redis-cart target, only with a replica count of 1-100, and only if it is still
  at 0), and lists any other faults it leaves alone. It has `--dry-run` too.
- **`--dry-run`** runs the read-only preflight and prints the plan and every manifest, without changing anything,
  calling the prober or writing the map.
- **Prober access.** `--prober-url` if you already have a port-forward or another way in; otherwise the tool opens
  its own port-forward to `prober/prober:8080` through the Kubernetes API (WebSockets, falling back to SPDY, like
  kubectl) and closes it at exit.

## What a run costs

**Time** (estimate, my math): the prober estimates 0.3-1 s per healthy run and runs 4 at a time. Per service: 15 s
settle, 4 journeys × 20 runs (5 rounds of 4; failing runs fail fast at the first broken step), then recovery. About
1-2 minutes per gRPC service and a little longer for redis-cart, whose pod has to start again, so roughly
**10-30 minutes** for the ten services. A slow shop is bounded by the prober's per-step timeouts; `--timeout` stops
the whole run at 90 minutes.

**Error budget.** The scheduled prober and the load generator use the same shop, so every fault that breaks a
journey burns that journey's real budget. browse depends critically on four services in the example map, each broken
for roughly 1-2 minutes, plus redis-cart's recovery: about **5-10 failed prober minutes**, around 3-5 % of browse's
28-day budget of about 202 minutes at 99.5 % (estimate, my math). Checkout has seven critical services: about 7-14
minutes of its 403. So:

- Best: run it once **before the operated period starts** (M2), when no SLO is live yet.
- Later: schedule it like a game day under the [error-budget policy](../prd.md#error-budget-policy-outline), log the
  burn, and don't run it when a journey's budget is spent.

## How to read the output

```yaml
apiVersion: mamori/v1
kind: CUJMap
metadata: {generatedAt: ..., generator: cuj-discovery/0.1.0, onlineBoutique: v0.10.7, runsPerCase: 20,
           failureThreshold: 0.5, fault: "one service at a time: Istio VirtualService abort ..."}
journeys:
  - id: view-cart
    critical: [cartservice, currencyservice, frontend, productcatalogservice, redis-cart, shippingservice]
    nonCritical: [adservice, checkoutservice, emailservice, paymentservice, recommendationservice]
    results:
      shippingservice: {failureRate: 1.0, runs: 20}
      ...
```

- **`critical`**: with this service down the journey fails. These are the services whose success rates the SLO
  generator multiplies into the journey's availability, and whose latencies it watches. `frontend` is listed without
  a result (see above).
- **`nonCritical`**: the journey kept working with the service down. That means *either* the frontend handles the
  error (ads and recommendations: Online Boutique logs and carries on) *or* the journey never calls the service
  (payment during browse). The map can't tell the two apart; the code can.
- **`failureRate` 1.0 or 0.0** is the clean case. **Anything in between** needs a look before the map is used: a step
  that only sometimes calls the service, flaky runs, the gateway's rate limit (the log warns when failed runs show HTTP
  429), or a fault that took effect late. The summary lists these cases. Re-run just that case, for example
  `make discovery-run DISCOVERY_ARGS="--services adservice --cujs browse --runs 50 --out /tmp/check.yaml"`.
- **Which step broke** is in the log, not the map: each `case measured` line has `failed_steps` (for example
  `{"home": 20}`) and the first error. Save the log (`2> discovery.log`) next to the change that updates the map.
- **Surprises are findings.** A dependency that is critical but shouldn't be (PLAN.md's example: currency taking down
  every page when prices could fall back to USD) becomes M4 hardening work. When that work lands, the next run should
  move the service to `nonCritical`, which is the measurable proof that the hardening worked.
- **Compare runs** with `git diff slo/cuj-map.yaml`; only `generatedAt` changes when nothing else did.

Until the first run, [`slo/cuj-map.example.yaml`](../../slo/cuj-map.example.yaml) stands in. It is clearly marked
as an example: its header explains the guesses (from Online Boutique v0.10.7's frontend handlers and checkout
service, and the prober's steps), and its numbers are guesses, not results.

## Re-running

Re-run after anything that can change a dependency: a new Online Boutique version, a change to the forked services
(#28), resilience work that makes a dependency fail open (#27, M4), or a change to the prober's journey steps. Commit
the new map in its own pull request so the regenerated SLOs are reviewed with it.

## How CI could re-run it (design sketch, not built)

PLAN.md section 4 asks for the map never to go stale. A workflow that does this later:

1. **Trigger.** `workflow_dispatch`, plus after a deploy of changed services has synced (for example a scheduled
   check that the running images differ from the last map's run). Never on pull requests: it needs the live cluster
   and spends error budget.
2. **Gate.** The protected GitHub environment `lab` (manual approval), a `concurrency` group so two runs never
   overlap, and a check that no journey's error budget is spent. The tool's preflight already refuses while a game
   day's fault is in place.
3. **Identity.** Workload Identity Federation to a dedicated service account with only
   `roles/container.clusterViewer` (to get credentials), plus Kubernetes RBAC bound to that identity:
   in `boutique`, `virtualservices` and `configmaps`: get, list, create, delete; `deployments`: get, patch (only
   `resourceNames: [redis-cart]` for patch); `pods`: list; `services`: get. In `prober`: `services` get, `pods` list,
   `pods/portforward` create. Cluster-wide `list` on `virtualservices` (otherwise the tool checks `boutique` only and
   says so), and `list` on other kinds in `boutique` for the fault check (without it the tool warns per kind and still
   checks its own kinds). New service accounts go in `infra/bootstrap/`, as the contract requires.
4. **Reach.** The control-plane endpoint is public, optionally limited by `master_authorized_networks`
   (`infra/envs/lab/gke.tf`). With that list set, GitHub-hosted runners can't connect; a self-hosted runner or GKE's
   DNS-based endpoint would be needed (not checked yet).
5. **Steps.** Get credentials, `make discovery-run`, `make discovery-validate`, upload the log as an artifact. If the
   map changed apart from `generatedAt`, open a pull request with it (an action pinned by commit SHA), so a person
   reviews any new critical dependency and the SLO generator's output in the same PR. A dependency that turned
   critical is a regression to discuss, not something to merge quietly.
6. **Time.** 10-30 minutes (estimate), well inside a job's limits.

## Limits

- **Abort only.** A slow dependency (a delay fault) is not measured; ads, for example, have a 100 ms timeout in the
  frontend and should survive a delay. A `delay` mode would be a small extension.
- **One service at a time.** No combinations.
- **The prober's path only.** Requests that enter through the ingress gateway, as the prober sends them. The load
  generator's paths are not measured.
- **Faults act in callers' sidecars.** Every pod in `boutique` has one, so this holds today.
- **Behaviour of v0.10.7 and today's prober steps.** Re-run when either changes.
- **Unverified so far:** everything that needs a cluster. In particular how quickly the journeys pass again once
  redis-cart is back, whether 15 s of settle time is enough for xDS to reach every sidecar, and the run time.
