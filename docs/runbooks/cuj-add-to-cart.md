# Runbook: CUJ add-to-cart alert

Status: draft, M3 (#20), 2026-11-13. Not yet used on a real alert; queries checked against the names in the build
contract, not against live data. The alerts exist only once `slo_enabled` is true
([../alerting.md](../alerting.md#alert-inventory)); until then nothing fires.

**Alerts:** `CUJ add-to-cart: <prober|mesh> <availability|latency> <fast|medium|slow> burn`, and the shared `CUJ
prober: no data` and `CUJ mesh: no data` (inventory: [../alerting.md](../alerting.md)). **Journey:** the shopper adds
a product to the cart (`POST /cart`, answered with a 302 to the cart page). The prober runs it as two steps, `home`
(where it gets a product id) then `add-to-cart`, and stops at the 302 without following it. Its latency SLO times only
the `add-to-cart` step; a failure at either step fails the journey.

## What fired

`prober` = the end-to-end probe from the in-cluster ingress gateway; `mesh` = sidecar metrics of the journey's critical
backend services. `availability` = requests fail; `latency` = slower than 500 ms. `fast` and `medium` on the prober page
(2 % of the 28-day budget in an hour, 5 % in six hours; latency has lower thresholds of its own); every mesh alert and
the prober's `slow` tier (10 % in a day) are tickets. `CUJ prober: no data` and `CUJ mesh: no data` cover every journey:
see [cuj-browse.md](cuj-browse.md#no-data-alerts).

## Impact

Shoppers can't fill a cart, so no orders can be placed. Severity
([../incidents/severity.md](../incidents/severity.md)): about half or more failing for 5 minutes is SEV1; a
page-tier alert below that is SEV2; a slow-tier ticket alone is SEV3.

## First five minutes

1. **Acknowledge** in PagerDuty, duplicates too. Don't resolve there.
2. **Game day?** `make gameday-status` (from the repo root) and `#incidents`.
3. **Declare** in `#incidents` with a first severity
   ([../incidents/communication.md](../incidents/communication.md)).
4. **Size it** with queries 1 and 2. A failure at step `home` means the home page is broken, and then every
   journey fails: suspect what the home page needs (currency, catalog, cart, frontend, gateway). A failure at step
   `add-to-cart` with the others fine points at the cart (`cartservice`/`redis-cart`) or the catalog.
5. **What changed?** Argo CD history, recent merges to `deploy/`, a lost node, an active fault rule.

## Likely causes

From Online Boutique v0.10.7's frontend; the measured list in `slo/cuj-map.yaml` (#17) wins.

| Service | Why add-to-cart needs it |
|---|---|
| `frontend` | Serves the home page and handles `POST /cart` |
| `currencyservice` | The home page (the probe's first step) fetches currencies and converts every price |
| `productcatalogservice` | The home page lists products; `POST /cart` looks up the product before adding it |
| `cartservice` → `redis-cart` | The home page shows the cart size; `POST /cart` stores the item. `redis-cart` keeps carts in memory (`emptyDir`): if its pod restarts or moves, calls fail until it is ready again, and every cart is emptied |
| `istio-ingressgateway` (`istio-ingress`) | One replica; every probe enters here |

`POST /cart` itself calls only the catalog and the cart; currency matters because of the `home` step. Shipping and
the cart page don't matter: the prober stops at the 302. A `422` for bad input is not an error (only 5xx counts).

## Queries

```bash
pq() { curl -sS "https://monitoring.googleapis.com/v1/projects/$PROJECT_ID/location/global/prometheus/api/v1/query" \
  -H "Authorization: Bearer $(gcloud auth print-access-token)" --data-urlencode "query=$1" \
  | jq -r '.data.result[] | "\(.metric | del(.__name__) | tostring)  \(.value[1])"'; }

pq 'mamori:cuj_prober_availability:ratio_rate5m'        # 1. every journey, prober (1 = all good)
pq 'mamori:cuj_mesh_availability:ratio_rate5m'          #    every journey, mesh
pq 'sum by (step) (rate(mamori_cuj_step_total{cuj="add-to-cart",source="scheduled",result="failure"}[10m]))'  # 2.
# 3. success ratio of the services the journey needs (client side, so Istio aborts count)
pq 'mamori:service_mesh_availability:ratio_rate5m{destination_service_name=~"cartservice|currencyservice|frontend|productcatalogservice"}'
# 4. share of requests under 500 ms for the same services
pq 'mamori:service_mesh_latency:ratio_rate5m{threshold_ms="500",destination_service_name=~"cartservice|currencyservice|frontend|productcatalogservice"}'
```

```bash
kubectl -n boutique get pods -o wide -l 'app in (redis-cart,cartservice,currencyservice,productcatalogservice,frontend)'
kubectl -n boutique get events --sort-by=.lastTimestamp | grep -iE 'redis-cart|cartservice' | tail -10
kubectl -n boutique logs deploy/cartservice --since=15m | grep -iE 'redis|error|fail' | tail -20
kubectl -n argocd get applications
```

The `app` label is upstream Online Boutique's. More checks (nodes, fault rules, Argo CD history, Logs Explorer):
[cuj-browse.md](cuj-browse.md#queries).

## Mitigations

One at a time; wait for query 1 to move before the next.

1. **Remove an active fault** once it is the known cause: `make gameday-stop-all`.
2. **`redis-cart` restarted or moved:** wait for it to be `Ready`, then check `cartservice` recovers within a few
   minutes; if not, `kubectl -n boutique rollout restart deploy/cartservice`. Carts are lost either way; say so in
   the thread (it is a postmortem trigger: data lost).
3. **Roll back a recent change through Git** (`git revert`, pull request, merge). Never `kubectl rollout undo`
   while Argo CD self-heals; break-glass steps in [cuj-browse.md](cuj-browse.md#mitigations).
4. **Restart a stuck service:** `kubectl -n boutique rollout restart deploy/<service>`.
5. **From M5:** the agent's proposal, approved only if its evidence matches.

## Escalation

No working mitigation after 30 minutes (SEV1) or 60 minutes (SEV2): stop diagnosing, roll back everything changed
in the last 24 hours, raise the severity ([../incidents/on-call.md](../incidents/on-call.md#escalation)). Check
[Google Cloud Service Health](https://status.cloud.google.com/).

## Afterwards

Resolve when add-to-cart has been within target for 15 minutes and the alert has closed. Classify every alert in
[../incidents/alert-log.md](../incidents/alert-log.md); postmortem if
[../incidents/severity.md](../incidents/severity.md#when-a-postmortem-is-required) requires one.
