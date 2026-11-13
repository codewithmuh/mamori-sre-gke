# Runbook: CUJ view-cart alert

Status: draft, M3 (#20), 2026-11-13. Not yet used on a real alert; queries checked against the names in the build
contract, not against live data. The alerts exist only once `slo_enabled` is true
([../alerting.md](../alerting.md#alert-inventory)); until then nothing fires.

**Alerts:** `CUJ view-cart: <prober|mesh> <availability|latency> <fast|medium|slow> burn`, and the shared `CUJ prober:
no data` and `CUJ mesh: no data` (inventory: [../alerting.md](../alerting.md)). **Journey:** the shopper opens the
cart page (`GET /cart`), with shipping cost and recommendations. The cart page also holds the checkout form.

## What fired

`prober` = the end-to-end probe from the in-cluster ingress gateway; `mesh` = sidecar metrics of the journey's critical
backend services. `availability` = requests fail; `latency` = slower than 500 ms. `fast` and `medium` on the prober page
(2 % of the 28-day budget in an hour, 5 % in six hours; latency has lower thresholds of its own); every mesh alert and
the prober's `slow` tier (10 % in a day) are tickets. `CUJ prober: no data` and `CUJ mesh: no data` cover every journey:
see [cuj-browse.md](cuj-browse.md#no-data-alerts).

## Impact

Shoppers can't see their cart, and so can't reach the checkout form: no orders. Severity
([../incidents/severity.md](../incidents/severity.md)): about half or more failing for 5 minutes is SEV1; a
page-tier alert below that is SEV2; a slow-tier ticket alone is SEV3.

## First five minutes

1. **Acknowledge** in PagerDuty, duplicates too. Don't resolve there.
2. **Game day?** `make gameday-status` (from the repo root) and `#incidents`.
3. **Declare** in `#incidents` with a first severity
   ([../incidents/communication.md](../incidents/communication.md)).
4. **Size it** with queries 1 and 2. The prober runs view-cart as `home`, `add-to-cart`, then `view-cart`: a
   failure at an earlier step belongs to that journey's runbook. Browse failing too points at currency, the catalog
   or the cart; checkout failing too points at shipping, currency or the cart; view-cart alone points at
   `shippingservice`.
5. **What changed?** Argo CD history, recent merges to `deploy/`, a lost node, an active fault rule.

## Likely causes

From Online Boutique v0.10.7's frontend; the measured list in `slo/cuj-map.yaml` (#17) wins.

| Service | Why view-cart needs it |
|---|---|
| `frontend` | Renders the page |
| `currencyservice` | The currency list, and every item's price is converted |
| `cartservice` → `redis-cart` | Reads the cart. A `redis-cart` restart fails the page until `cartservice` reconnects, and empties every cart |
| `shippingservice` | The shipping quote on the page; only the cart page and checkout call it |
| `productcatalogservice` | One lookup per cart item, one after another: a slow catalog hurts carts with many items most |
| `istio-ingressgateway` (`istio-ingress`) | One replica; every probe enters here |

`recommendationservice` is not critical for errors (ignored), but the page waits for it with no timeout: if it is
slow, the cart page is slow. A latency alert with no errors on view-cart and browse together points there.

## Queries

```bash
pq() { curl -sS "https://monitoring.googleapis.com/v1/projects/$PROJECT_ID/location/global/prometheus/api/v1/query" \
  -H "Authorization: Bearer $(gcloud auth print-access-token)" --data-urlencode "query=$1" \
  | jq -r '.data.result[] | "\(.metric | del(.__name__) | tostring)  \(.value[1])"'; }

pq 'mamori:cuj_prober_availability:ratio_rate5m'        # 1. every journey, prober (1 = all good)
pq 'mamori:cuj_mesh_availability:ratio_rate5m'          #    every journey, mesh
pq 'sum by (step) (rate(mamori_cuj_step_total{cuj="view-cart",source="scheduled",result="failure"}[10m]))'  # 2.
pq 'bottomk(5, mamori:service_mesh_availability:ratio_rate5m)'   # 3. worst services, client side
pq 'bottomk(5, mamori:service_mesh_latency:ratio_rate5m)'        # 4. slowest services
```

```bash
kubectl -n boutique get pods -o wide \
  -l 'app in (frontend,currencyservice,cartservice,redis-cart,shippingservice,productcatalogservice,recommendationservice)'
kubectl -n boutique get events --sort-by=.lastTimestamp | tail -20
kubectl -n boutique logs deploy/frontend --since=15m | grep -iE 'shipping|currency|cart|error' | tail -20
kubectl -n argocd get applications
```

More checks (nodes, fault rules, Argo CD history, Logs Explorer): [cuj-browse.md](cuj-browse.md#queries).

## Mitigations

One at a time; wait for query 1 to move before the next.

1. **Remove an active fault** once it is the known cause: `make gameday-stop-all`.
2. **Roll back a recent change through Git** (`git revert`, pull request, merge). Never `kubectl rollout undo`
   while Argo CD self-heals; break-glass steps in [cuj-browse.md](cuj-browse.md#mitigations).
3. **Restart a stuck service:** `kubectl -n boutique rollout restart deploy/<service>`. After a `redis-cart`
   restart, restart `cartservice` if it still fails; carts are lost (a postmortem trigger).
4. **Capacity lost** (a node gone, pods `Pending`): wait for GKE to recreate the Spot VM.
5. **From M5:** the agent's proposal, approved only if its evidence matches.

## Escalation

No working mitigation after 30 minutes (SEV1) or 60 minutes (SEV2): stop diagnosing, roll back everything changed
in the last 24 hours, raise the severity ([../incidents/on-call.md](../incidents/on-call.md#escalation)). Check
[Google Cloud Service Health](https://status.cloud.google.com/).

## Afterwards

Resolve when view-cart has been within target for 15 minutes and the alert has closed. Classify every alert in
[../incidents/alert-log.md](../incidents/alert-log.md); postmortem if
[../incidents/severity.md](../incidents/severity.md#when-a-postmortem-is-required) requires one.
