# Runbook: CUJ prober

The prober (`prober` Deployment in namespace `prober`) runs the four journeys (browse, add-to-cart, view-cart,
checkout) through the Istio ingress gateway once a minute. Its `mamori_cuj_*` metrics with `source="scheduled"` feed the
journey SLOs. How it works, its steps and its metrics: [tools/prober/README.md](../../tools/prober/README.md).

Use this page when:

- **The prober is not reporting:** an alert on missing prober data, or gaps in the journey SLO charts.
- **Every journey fails:** all four journey SLOs burn at once.
- **One journey fails:** one journey SLO burns. The journey runbooks (`docs/runbooks/cuj-<id>.md`) cover the shop
  side; this page helps tell a broken shop from a broken prober.

The commands assume `kubectl` points at the lab cluster (`make up` sets that up).

## Quick look

```bash
kubectl -n prober get deploy,pods -o wide
kubectl -n prober logs deploy/prober --since=15m | grep -v '"ok":true' | tail -20   # failures and warnings only
make prober-metrics                                                                  # counters, through a port-forward
make prober-run CUJ=checkout RUNS=5                                                  # run a journey now and see each result
```

In Cloud Monitoring (Metrics Explorer → PromQL):

```promql
# Probes per journey in the last 10 minutes: about 10 each when healthy, nothing when the prober is down.
sum by (cuj) (increase(mamori_cuj_probe_total{source="scheduled"}[10m]))

# Failure ratio per journey.
sum by (cuj) (rate(mamori_cuj_probe_total{source="scheduled", result="failure"}[10m]))
  / sum by (cuj) (rate(mamori_cuj_probe_total{source="scheduled"}[10m]))

# Which step fails.
sum by (cuj, step) (increase(mamori_cuj_step_total{source="scheduled", result="failure"}[10m]))

# The shop as the mesh sees it, for comparison (the load generator keeps traffic flowing).
sum by (response_code) (rate(istio_requests_total{reporter="destination", destination_workload="frontend"}[5m]))
```

In Logs Explorer:

```text
resource.type="k8s_container"
resource.labels.namespace_name="prober"
jsonPayload.event="probe_completed"
jsonPayload.ok=false
```

`jsonPayload.failed_step` and `jsonPayload.error` say where and why. The error carries the frontend error page's detail
(`could not retrieve currencies: rpc error: code = Unavailable ...`) or Envoy's reason (`no healthy upstream`,
`fault filter abort`, `upstream request timeout`).

## The prober is not reporting

**Impact.** No new SLO data. That is a gap, not an outage: the shop may be fine. The SLOs neither burn nor recover
during the gap, so a real outage in that time goes unseen by the prober. The mesh metrics still work.

**Confirm and fix:**

1. **Is the pod running?** `kubectl -n prober get pods`, then `kubectl -n prober describe pod -l app.kubernetes.io/name=prober`
   and `kubectl -n prober get events --sort-by=.lastTimestamp | tail`.

   | What you see | Cause | Fix |
   |---|---|---|
   | `CrashLoopBackOff`, log line `invalid configuration` | A bad `PROBER_*` value in `deploy/prober/deployment.yaml` (the line lists every problem) | Fix it in Git; Argo CD syncs |
   | `ImagePullBackOff` | `PROJECT_ID` still in the image name (run `scripts/set-registry.sh`), or the image was never pushed | Push the image (CI, #28) or fix the reference |
   | `Pending` | No room: a Spot node was preempted or the pool is at 0 (spend guard, see [spend-guard.md](spend-guard.md)) | Wait for the node, or `make resume` |
   | Restarts, liveness failures, log `scheduled probes stopped making progress` | A probe loop hung (a bug) | The restart is the mitigation; collect logs and open an issue |
   | No pod and no Deployment | The Argo CD Application `prober` is missing or failing | `kubectl -n argocd get application prober`; check its sync status |

2. **Is it scraped?** If the pod runs and `make prober-metrics` shows counters going up, but Cloud Monitoring has
   nothing new: `kubectl -n prober describe podmonitoring prober` shows the scrape status per endpoint, once target
   status reporting is on: `features.targetStatus.enabled: true` in `kubectl -n gmp-public edit operatorconfig config`.
   Google recommends it only while investigating, so turn it off afterwards.
   - `context deadline exceeded` or `connection refused`: check the ingress rule for `gmp-system` in
     `deploy/prober/networkpolicy.yaml`.
   - Collectors down: `kubectl -n gmp-system get pods`. Managed collection is part of GKE, so check the GKE release
     notes and the cluster's monitoring settings.
3. **Is the cluster there?** If every in-cluster signal stopped at once, the cluster or its nodes are gone
   (preemption of all nodes, spend guard, an outage). The prober can't report on a cluster it runs in.

Write the gap down in the error-budget log, so the SLO report doesn't read it as a quiet period.

## Every journey fails

All four journeys start on the home page (`GET /`), so a shared cause shows as `failed_step="home"` everywhere. First
decide whether the **shop** or the **probe path** is broken: compare with the mesh query above and with a request of
your own.

```bash
kubectl -n istio-ingress port-forward svc/istio-ingressgateway 8081:80 &
curl -sS -o /dev/null -w '%{http_code}\n' -H 'Host: mamori.codewithmuh.com' http://127.0.0.1:8081/
curl -sS -o /dev/null -w '%{http_code}\n' https://mamori.codewithmuh.com/        # the public path
```

**The shop is broken** (your own requests fail too, the mesh shows 5xx from the frontend): the prober is right. Go to
the journey runbooks and the incident process. Typical errors in the probe log:

| Error | Likely cause |
|---|---|
| `status 503, want 200: no healthy upstream` | No ready frontend pods |
| `status 500, want 200: could not retrieve currencies / products / cart: rpc error: ...` | A home-page dependency (currency, product catalog, cart) failed; the detail names it |
| `status 504 ...: upstream request timeout` | The frontend or a dependency is slow; the gateway's route timeout fired |
| `fault filter abort` in the detail | An Istio fault is injected: a game day or discovery run in progress? `kubectl -n boutique get virtualservice -l mamori.dev/fault=true` |

**Only the prober fails** (your own requests and the mesh look fine): the probe path is broken.

| Error | Likely cause | Fix |
|---|---|---|
| `GET /: status 404, want 200` with no detail | Envoy has no route for the Host header: `PROBER_HOST` and the host in `deploy/online-boutique/public-gateway.yaml` differ | Make them equal |
| `dial tcp: lookup istio-ingressgateway...: no such host` or `i/o timeout` on DNS | DNS egress blocked or kube-dns down | Check the DNS rule in `deploy/prober/networkpolicy.yaml`, `kubectl -n kube-system get pods -l k8s-app=kube-dns` |
| `dial tcp 10.x.x.x:80: i/o timeout` | Egress to the gateway pods blocked (labels or port changed) | The gateway pods must carry `istio: ingressgateway` and serve on 80 |
| `connection refused` | Gateway pods not running | `kubectl -n istio-ingress get pods` |
| `status 429 ... (gateway rate limit)` | The gateway's local rate limit sheds load: shoppers get 429s too, so this is real user pain unless only the prober is affected | See the resilience docs; check the request rate |
| `the page has no "Hot Products"` | The page changed (the frontend fork, #28) or rendered only partly | If the shop works in a browser, update the markers in `tools/prober/journey.go` |

## One journey fails

1. **Find the failing step.** Use the step query or the log query above, or reproduce it with
   `make prober-run CUJ=<id> RUNS=10` and read `failedStep` and `error`.
2. **Map the step to services.** The measured map is `slo/cuj-map.yaml` (#17). Roughly, from the frontend's code:

   | Step | Request | Calls that must succeed |
   |---|---|---|
   | `home` | `GET /` | currency, product catalog, cart (ads are optional) |
   | `product` | `GET /product/<id>` | product catalog, currency, cart (recommendations and ads are optional) |
   | `add-to-cart` | `POST /cart` | product catalog, cart (→ redis-cart) |
   | `view-cart` | `GET /cart` | currency, cart, shipping, product catalog (recommendations are optional) |
   | `place-order` | `POST /cart/checkout` | checkout → cart, product catalog, currency, shipping, payment, email |

3. **Check what changed.** A game-day fault (`kubectl -n boutique get virtualservice -l mamori.dev/fault=true`), a
   rollout (`kubectl -n boutique rollout history deploy/<service>`, Argo CD history), a Spot preemption
   (`kubectl get nodes`: a node only minutes old replaced one), or redis-cart restarting (carts are lost, add-to-cart
   fails while it moves).
4. **Is it the prober?** If the step fails only for the prober and a browser can do the same thing, compare the error
   with the page. A changed template or form (for example from the frontend fork) needs a prober change. A page that
   needs more than 12 s fails the step in the prober, while a patient user might still get it.

## A false alarm from the prober

If the prober itself was wrong (a bug, a stale marker), its failures still burned error budget. Fix it through a pull
request. Argo CD self-heals manual edits, so `kubectl scale` or `kubectl edit` doesn't last. Record the window and the
cause in the error-budget log, so the SLO review can set it aside. Don't delete metrics.

## Escalate

This is a one-person lab: the on-call SRE owns the prober too. If the cause is in the prober's code, open an issue with
the failing step, the error text and the time range. If it is in GKE or Managed Service for Prometheus, check the
Google Cloud status dashboard and the GKE release notes before digging further.
