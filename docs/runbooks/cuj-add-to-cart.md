# Runbook: CUJ add-to-cart alert

Status: draft, M3 (#20), 2026-11-13. Not yet used on a real alert; queries checked against the names in the build
contract, not against live data.

**Alerts:** `CUJ add-to-cart: <prober|mesh> <availability|latency> <fast|medium|slow> burn` and `CUJ add-to-cart:
prober no data` (inventory: [../alerting.md](../alerting.md)). **Journey:** the shopper adds a product to the cart
(`POST /cart`, which redirects to the cart page).

## What fired

`prober` = the end-to-end probe from the in-cluster ingress gateway; `mesh` = sidecar metrics of the journey's
critical services. `availability` = requests fail; `latency` = slower than 500 ms. `fast` and `medium` page (2 % of
the 28-day budget in an hour, 5 % in six hours); `slow` is a ticket (10 % in a day). `prober no data`: see
[cuj-browse.md](cuj-browse.md#cuj-browse-prober-no-data); it is the same for every journey.

## Impact

Shoppers can't fill a cart, so no orders can be placed. Severity
([../incidents/severity.md](../incidents/severity.md)): about half or more failing for 5 minutes is SEV1; a
page-tier alert below that is SEV2; a slow-tier ticket alone is SEV3.

## First five minutes

1. **Acknowledge** in PagerDuty, duplicates too. Don't resolve there.
2. **Game day?** `gameday status` and `#incidents`.
3. **Declare** in `#incidents` with a first severity
   ([../incidents/communication.md](../incidents/communication.md)).
4. **Size it** with queries 1 and 2. If browse, view-cart and checkout fail too, suspect the cart
   (`cartservice`/`redis-cart`) first: every page reads it.
5. **What changed?** Argo CD history, recent merges to `deploy/`, a lost node, an active fault rule.

## Likely causes

From Online Boutique v0.10.7's frontend; the measured list in `slo/cuj-map.yaml` (#17) wins.

| Service | Why add-to-cart needs it |
|---|---|
| `frontend` | Handles `POST /cart` |
| `productcatalogservice` | Looks up the product before adding it |
| `cartservice` → `redis-cart` | Stores the item. `redis-cart` keeps carts in memory (`emptyDir`): if its pod restarts or moves, adds fail until `cartservice` reconnects, and every cart is emptied |
| `istio-ingressgateway` (`istio-ingress`) | One replica; every probe enters here |

The handler calls nothing else. If the prober's add-to-cart journey follows the redirect to `GET /cart`, the cart
page's services (currency, shipping) can fail it too: compare with the view-cart SLIs. A `422` for bad input is not
an error (only 5xx counts).

## Queries

```bash
pq() { curl -sS "https://monitoring.googleapis.com/v1/projects/$PROJECT_ID/location/global/prometheus/api/v1/query" \
  -H "Authorization: Bearer $(gcloud auth print-access-token)" --data-urlencode "query=$1" \
  | jq -r '.data.result[] | "\(.metric | del(.__name__) | tostring)  \(.value[1])"'; }

pq 'mamori:cuj_prober_availability:ratio_rate5m'        # 1. every journey, prober (1 = all good)
pq 'mamori:cuj_mesh_availability:ratio_rate5m'          #    every journey, mesh
pq 'sum by (step) (rate(mamori_cuj_step_total{cuj="add-to-cart",source="scheduled",result="failure"}[10m]))'  # 2.
pq 'mamori:service_mesh_availability:ratio_rate5m{destination_service_name=~"cartservice|productcatalogservice"}'  # 3.
# 4. share of requests under 500 ms for the same services
pq 'mamori:service_mesh_latency:ratio_rate5m{threshold_ms="500",destination_service_name=~"cartservice|productcatalogservice"}'
```

```bash
kubectl -n boutique get pods -o wide -l 'app in (redis-cart,cartservice,productcatalogservice,frontend)'
kubectl -n boutique get events --sort-by=.lastTimestamp | grep -iE 'redis-cart|cartservice' | tail -10
kubectl -n boutique logs deploy/cartservice --since=15m | grep -iE 'redis|error|fail' | tail -20
kubectl -n argocd get applications
```

The `app` label is upstream Online Boutique's. More checks (nodes, fault rules, Argo CD history, Logs Explorer):
[cuj-browse.md](cuj-browse.md#queries).

## Mitigations

One at a time; wait for query 1 to move before the next.

1. **Remove an active fault** once it is the known cause: `gameday stop --all`.
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
