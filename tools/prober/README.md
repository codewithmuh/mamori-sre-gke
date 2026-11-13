# prober

A Go service that walks Online Boutique's critical user journeys (CUJs) the way a shopper does, once a minute, and
exports the results as Prometheus metrics. The journey SLOs (`slo/slos.yaml`) are built on these metrics. The
service also runs journeys on demand over HTTP, for the dependency discovery run, game days and resilience
experiments. Journeys and starting targets come from [docs/prd.md](../../docs/prd.md). Runbook:
[docs/runbooks/prober.md](../../docs/runbooks/prober.md).

## Journeys

Every probe opens new connections and starts on the home page, because that is where it gets a product id (one of
the product links, at random). Each step is one HTTP request with its own timeout. Redirects are checked, not
followed. Any failed step fails the probe, and the steps after it don't run.

**Sessions.** Each probe is a returning shopper, not a new one: the frontend's `shop_session-id` cookie is kept
between probes. Each scheduled journey loop has one session, and each on-demand worker slot (journey × 1 to
`PROBER_RUN_CONCURRENCY`) has one, kept across calls. The reason is redis-cart. cartservice v0.10.7 keeps a cart under
the session id for good: emptying it writes an empty cart, with no expiry, instead of deleting the key
(`RedisCartStore.EmptyCartAsync`). A new session per probe would leave about 4,300 keys a day that never go away
(3 cart journeys × 1,440 probes). With reused sessions the prober holds at most 3 + 3 × 4 = 15 carts by default. A
session changes only when the frontend's 48-hour cookie expires or the pod restarts: about ten stale (empty) carts a
week from the scheduled loops, plus a few from on-demand slots (estimate).

| CUJ id | Steps |
|---|---|
| `browse` | `home` → `product` |
| `add-to-cart` | `home` → `add-to-cart` |
| `view-cart` | `home` → `add-to-cart` → `view-cart` |
| `checkout` | `home` → `add-to-cart` → `view-cart` → `place-order` |

What each step sends and what it must get back. Routes, forms and templates are those of the v0.10.7 frontend
(`src/frontend/main.go`, `handlers.go`, `templates/`):

| Step | Request | Passes when |
|---|---|---|
| `home` | `GET /` | 200, the page shows "Hot Products" and links at least one `/product/<id>` |
| `product` | `GET /product/<id>` | 200, the page has the add-to-cart form (`action=".../cart"`) for that `product_id` |
| `add-to-cart` | `POST /cart` with `product_id=<id>&quantity=1` | 302 with `Location: /cart`. The frontend sends it only after cartservice stored the item; every failure renders an error page with a 4xx or 5xx status |
| `view-cart` | `GET /cart` | 200, the page lists `SKU #<id>` and has a checkout form with every field the frontend validates |
| `place-order` | `POST /cart/checkout` with the fields of that form | 200, the page shows "Your order is complete!" |

- **add-to-cart stops at the redirect.** Following it to `GET /cart` would make the journey the same as view-cart, and
  would add the cart page's own dependencies (shipping quote, recommendations) to add-to-cart.
- **checkout uses the cart page's form,** like a browser: the demo's pre-filled address and fake card
  (`4432801561520454`, expiry January of next year, CVV `672`). No real money moves: paymentservice only checks the
  card number and expiry and pretends to charge it.
- **The home page is the first step of every journey.** Its critical services (currency, product catalog, cart) are
  therefore critical for every journey. The step metrics and `failedStep` show where a probe failed.
- **Clean-up.** After add-to-cart, view-cart and failed checkouts the prober empties its session's cart
  (`POST /cart/empty`), so the reused cart doesn't grow by one item per probe. This is best effort, limited to 3 s,
  and not part of the result. If it fails, the item stays until the next probe's clean-up, and the checks still hold
  (add-to-cart must return 302, view-cart must list the product just added).
- **Failure reasons.** A wrong status is reported with what the response says: the frontend error page's detail
  (for example `could not retrieve currencies: rpc error: code = Unavailable desc = ...`) or Envoy's plain-text reason
  (`no healthy upstream`, `fault filter abort`). A 429 from the ingress gateway's local rate limit
  (`x-local-rate-limit: true`) is marked `(gateway rate limit)`. The internal port has no rate limit, so this marks a
  misconfigured gateway.

Requests carry `Host: mamori.codewithmuh.com` and `User-Agent: mamori-prober/<version>`.

### What the probe path covers, and what it doesn't

By default the prober calls the Istio ingress gateway's cluster-internal port inside the cluster
(`http://istio-ingressgateway.istio-ingress.svc.cluster.local:8080`) with the public Host header. Port 8080 has the
same routes as the public port 80, but it is not behind the gateway's public rate limit, so probes never compete
with shoppers for it, and the GKE Gateway doesn't expose it. The prober covers what a shopper's request meets from
the gateway on: the Istio Gateway and VirtualService, the frontend and every service behind it. **It does not cover
the GKE load balancer, Cloud Armor, the TLS certificate, DNS or the public port's rate limit.** The PRD's first plan
was to probe the public hostname from inside the cluster. That covers more, but the requests would leave through
Cloud NAT, come back through the load balancer (both cost a little), and share Cloud Armor's per-IP rate limit with
everything else behind the NAT address. Datadog synthetics (M3) give the outside view during their window.
To probe the public path instead, set `PROBER_TARGET_URL=https://mamori.codewithmuh.com` and allow egress to TCP 443
in `deploy/prober/networkpolicy.yaml`.

Like everything in the cluster, the prober shares the cluster's failure domain. If the cluster or the node it runs on
goes away, it stops reporting rather than reporting failures. Alert on missing data
([runbook](../../docs/runbooks/prober.md)).

## Metrics

On `:8080/metrics`. Only these four, without the Go runtime and process collectors: Managed Prometheus bills per
sample.

| Metric | Type | Labels |
|---|---|---|
| `mamori_cuj_probe_total` | counter | `cuj`, `result` (`success`, `failure`), `source` (`scheduled`, `on-demand`) |
| `mamori_cuj_probe_duration_seconds` | histogram | `cuj`, `source` |
| `mamori_cuj_step_total` | counter | `cuj`, `step`, `result`, `source` |
| `mamori_cuj_step_duration_seconds` | histogram | `cuj`, `step`, `source` |

- **SLOs read `source="scheduled"` only.** On-demand runs (discovery, game days, `make prober-run`) never touch an
  error budget.
- **Durations are recorded for successful probes and steps only.** A failure (a timeout included) counts against
  availability; a fast error page must not improve the latency SLI. So the latency SLI is "share of successful
  journeys (or steps) faster than the threshold".
- **Buckets** (seconds): 0.05, 0.1, 0.25, 0.5, 0.75, 1, 1.5, 2.5, 5, 10, 30. They hold the starting thresholds, 0.5 s
  and 1 s, as exact bounds.
- **A probe's duration** is the time from the first step's start to the last step's end, without the clean-up.
- **Steps after a failed step don't run** and aren't counted.
- **The scheduled series exist from startup at 0** (every journey, step and result), so failure ratios and absence
  alerts have data before the first failure. On-demand series appear with the first on-demand run.
- Step labels: `home`, `product`, `add-to-cart`, `view-cart`, `place-order`.

Managed Service for Prometheus scrapes it through `deploy/prober/podmonitoring.yaml` (every 60 s). In Cloud Monitoring
the series are `prometheus.googleapis.com/mamori_cuj_probe_total/counter` and so on, with resource labels
`cluster="mamori"`, `namespace="prober"`, `job="prober"`.

## On-demand API

`POST /v1/run` runs one journey `runs` times now and returns every result.

```bash
curl -sS -X POST localhost:8080/v1/run -H 'Content-Type: application/json' -d '{"cuj":"checkout","runs":20}'
```

```json
{
  "cuj": "checkout",
  "runs": 20,
  "successes": 19,
  "failures": 1,
  "results": [
    {"ok": true, "durationMs": 412, "failedStep": "", "error": ""},
    {"ok": false, "durationMs": 6013, "failedStep": "place-order",
     "error": "POST /cart/checkout: status 500, want 200: failed to complete the order: rpc error: code = Unavailable desc = fault filter abort"}
  ]
}
```

- `cuj`: `browse`, `add-to-cart`, `view-cart` or `checkout`. Every journey can run on demand, scheduled or not.
- `runs`: 1 to 100; left out, 1. The runs go `PROBER_RUN_CONCURRENCY` (default 4) at a time. Each worker is one
  returning shopper (see Sessions), so a call's runs share at most that many sessions. `results` are in run order.
  `durationMs` covers the journey's steps.
- **One call per journey at a time.** A second call for the same journey gets `409`; calls for different journeys run
  side by side.
- **How long a call takes** (estimate, not measured): about 0.1–0.3 s per step on a healthy shop. The worst case is
  every step timing out: 2, 2, 3 or 4 steps × `PROBER_STEP_TIMEOUT` (12 s), plus 3 s of clean-up, per run. That is
  about 51 s per checkout run, so about 4 minutes for 20 checkouts and 21 minutes for 100. The server has no write
  timeout; set the client's timeout to match.
- If the caller disconnects, or the prober is stopping, the runs still in progress are cancelled and not recorded.
- Errors are JSON, `{"error": "<what went wrong>", "reason": "<code>"}`:

| Status | `reason` | When |
|---|---|---|
| 400 | `invalid-request` | Not one JSON object, an unknown field, `runs` outside 1–100, body over 1 KiB |
| 400 | `unknown-cuj` | `cuj` is not one of the four ids |
| 409 | `busy` | An on-demand call for this journey is in progress |
| 503 | `shutting-down` | The prober is stopping |
| 503 | `cancelled` | The caller went away before the runs finished |

The API has no authentication. It is reachable only inside the cluster (and the NetworkPolicy admits only Managed
Prometheus) or through `kubectl port-forward`, which needs Kubernetes RBAC: `make prober-run CUJ=checkout RUNS=5`
(`mk/prober.mk`).

## Health

| Path | 200 when | Used by |
|---|---|---|
| `/readyz` | The prober has started and is not stopping. It never depends on the shop: a prober that went unready whenever the shop failed would stop reporting exactly when it matters | readiness probe |
| `/healthz` | Every scheduled journey loop showed progress within one interval plus its longest possible probe plus 30 s (60 s + 4 × 12 s + 3 s + 30 s = 141 s for checkout with the defaults) | liveness probe: a stuck loop restarts the pod |

## Configuration

Environment variables only. They are checked at start; if one is invalid, the prober logs every problem and exits
with status 2.

| Variable | Default | Meaning |
|---|---|---|
| `PROBER_TARGET_URL` | `http://istio-ingressgateway.istio-ingress.svc.cluster.local:8080` | Where requests go. `http` or `https`; a path prefix is allowed (like the frontend's `BASE_URL`) |
| `PROBER_HOST` | `mamori.codewithmuh.com` | Host header: the hostname the Istio Gateway accepts. Change it together with `deploy/online-boutique/public-gateway.yaml` |
| `PROBER_INTERVAL` | `60s` | Time between scheduled probes of one journey (1s–1h). The journeys start spread over one interval |
| `PROBER_STEP_TIMEOUT` | `12s` | Limit for one step, body included (100ms–5m, at most the interval). Longer than the route timeouts #27 plans for the gateway (6 s GET, 10 s POST), so a slow page fails as the gateway's 504, which mesh metrics can attribute |
| `PROBER_CUJS` | all four | Comma-separated journeys to probe on the schedule, or `none` (on-demand only, e.g. on a laptop) |
| `PROBER_LISTEN_ADDR` | `:8080` | Metrics, health and the API |
| `PROBER_RUN_CONCURRENCY` | `4` | Runs in parallel within one on-demand call (1–10) |
| `PROBER_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |

**Scheduling.** Each scheduled journey has its own loop, so a slow journey never delays another, and a probe never
overlaps the next probe of the same journey. If a probe runs longer than the interval (possible only when 4 × the step
timeout + 3 s is more than the interval), the runs it overlapped are skipped, not caught up, and logged as
`probe_skipped`. On SIGTERM the prober stops at once: probes in progress are dropped, not recorded as failures, so
a rollout never burns error budget.

## Logs

One JSON object per line on stdout, with `severity` and `message` for Cloud Logging.

| `event` | Severity | Fields |
|---|---|---|
| `probe_completed` | INFO (success) or WARNING (failure) | `cuj`, `source`, `ok`, `duration_ms`, `steps_ms` (per step), and on failure `failed_step`, `error` |
| `probe_skipped` | WARNING | `cuj`, `skipped` |
| `on_demand_run_completed` | INFO | `cuj`, `runs`, `successes`, `failures`, `duration_ms` |

Scheduled probes log every result (about 5,800 lines a day). On-demand calls log one summary line; each failed run is
logged at DEBUG.

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

# Image
docker build -t prober:dev .
```

The tests run every journey against a fake shop (`fakeshop_test.go`, an `httptest` server). Its pages copy the parts
of the v0.10.7 templates the prober reads, and it checks the Host header like the Istio Gateway does. The tests cover
every step's failure modes (error pages, Envoy replies, missing markers, wrong redirects, lost cart items, incomplete
forms, timeouts, refused connections), clean-up and session reuse (the fake keeps emptied carts, like cartservice),
scheduling without overlap, shutdown without recording, the API's validation, concurrency limit, 409 and
cancellation, and the metrics.

### Run it on a laptop

Against the lab's gateway through a port-forward, on-demand only:

```bash
kubectl -n istio-ingress port-forward svc/istio-ingressgateway 8081:8080 &
docker build -t prober:dev .
docker run --rm -p 127.0.0.1:8080:8080 --read-only \
  -e PROBER_TARGET_URL=http://host.docker.internal:8081 -e PROBER_CUJS=none prober:dev
curl -sS -X POST localhost:8080/v1/run -d '{"cuj":"browse","runs":3}'
```

## Image and deployment

- `Dockerfile`: multi-stage, static binary on `gcr.io/distroless/static-debian13:nonroot`, both pinned by digest; user
  65532; works with a read-only root filesystem (it writes nothing to disk). `VERSION` (build arg, default `0.1.0`)
  goes into the User-Agent.
- Image: `asia-northeast1-docker.pkg.dev/PROJECT_ID/mamori/prober:0.1.0`, built and pushed by CI (#28).
- Kubernetes: `deploy/prober/`, synced by the Argo CD Application `prober` (`deploy/apps/prober.yaml`, wave 5).
  Namespace `prober` without sidecar injection, Service `prober.prober.svc:8080`, PodMonitoring, and a NetworkPolicy
  (in: Managed Prometheus on 8080; out: DNS, and the gateway pods on their internal port 8080).
- **The Deployment ships with `replicas: 0`.** Until the image is in Artifact Registry and `scripts/set-registry.sh`
  has replaced `PROJECT_ID`, a pod would sit in `ImagePullBackOff`, keep the `prober` app unhealthy and hold Argo CD's
  root sync at wave 5, so later waves would never apply. After the first image publish (see `docs/images.md`), set
  `replicas: 1` in `deploy/prober/deployment.yaml` in a pull request. Until then there is no prober data, and the
  journey SLOs have none either.
- Requests: 10m CPU and 32 MiB memory, 128 MiB memory limit, no CPU limit (throttling would show up as journey
  latency). Real use is about 1–3m CPU and 15–25 MiB (estimate; measure with `kubectl top`).
