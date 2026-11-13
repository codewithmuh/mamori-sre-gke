# Runbook: CUJ checkout alert

Status: draft, M3 (#20), 2026-11-13. Not yet used on a real alert; queries checked against the names in the build
contract, not against live data.

**Alerts:** `CUJ checkout: <prober|mesh> <availability|latency> <fast|medium|slow> burn` and `CUJ checkout: prober
no data` (inventory: [../alerting.md](../alerting.md)). **Journey:** the shopper places an order with the demo's
pre-filled fake card (`POST /cart/checkout`). Targets: 99.0 % available, 95 % under 1,000 ms.

## What fired

`prober` = the end-to-end probe from the in-cluster ingress gateway; `mesh` = sidecar metrics of the journey's
critical services. `availability` = orders fail; `latency` = slower than 1,000 ms. `fast` and `medium` page (2 % of
the 28-day budget in an hour, 5 % in six hours; for checkout that is about 9 failed minutes in an hour);
`slow` is a ticket (10 % in a day). `prober no data`: see [cuj-browse.md](cuj-browse.md#cuj-browse-prober-no-data).

## Impact

Orders fail or hang. No real money moves (the card is fake and the payment service charges nothing). Severity
([../incidents/severity.md](../incidents/severity.md)): about half or more failing for 5 minutes is SEV1; a
page-tier alert below that is SEV2; a slow-tier ticket alone is SEV3.

## First five minutes

1. **Acknowledge** in PagerDuty, duplicates too. Don't resolve there.
2. **Game day?** `gameday status` and `#incidents`.
3. **Declare** in `#incidents` with a first severity
   ([../incidents/communication.md](../incidents/communication.md)).
4. **Size it** with queries 1 and 2. Checkout alone points at `checkoutservice`, `paymentservice` or a recent change
   to them; checkout plus other journeys points at what they share (cart, currency, catalog, shipping).
5. **What changed?** Argo CD history, recent merges to `deploy/`, a lost node, an active fault rule. A bad config or
   image for `checkoutservice` shows up here first.

## Likely causes

From Online Boutique v0.10.7 (`frontend` and `checkoutservice` source); the measured list in `slo/cuj-map.yaml` (#17)
wins. Checkout calls the most services, which is why its target is lower.

| Service | Why checkout needs it |
|---|---|
| `frontend` | Takes the order, then renders the confirmation (it fetches currencies again there) |
| `checkoutservice` | Runs the order; only checkout calls it |
| `cartservice` → `redis-cart` | Reads the cart (emptying it afterwards may fail without failing the order) |
| `productcatalogservice` | One lookup per cart item, one after another |
| `currencyservice` | Converts each item and the shipping cost |
| `shippingservice` | Quotes shipping, then ships the order. Shipping fails after the (fake) charge, so the order fails late |
| `paymentservice` | Charges the card |
| `istio-ingressgateway` (`istio-ingress`) | One replica; every probe enters here |

Not critical: `emailservice` (a failed confirmation email is logged, the order still succeeds) and
`recommendationservice` on the confirmation page (errors ignored, but no timeout: if it is slow, checkout is slow).

## Queries

```bash
pq() { curl -sS "https://monitoring.googleapis.com/v1/projects/$PROJECT_ID/location/global/prometheus/api/v1/query" \
  -H "Authorization: Bearer $(gcloud auth print-access-token)" --data-urlencode "query=$1" \
  | jq -r '.data.result[] | "\(.metric | del(.__name__) | tostring)  \(.value[1])"'; }

pq 'mamori:cuj_prober_availability:ratio_rate5m'        # 1. every journey, prober (1 = all good)
pq 'mamori:cuj_mesh_availability:ratio_rate5m'          #    every journey, mesh
pq 'sum by (step) (rate(mamori_cuj_step_total{cuj="checkout",source="scheduled",result="failure"}[10m]))'  # 2.
pq 'bottomk(5, mamori:service_mesh_availability:ratio_rate5m)'   # 3. worst services, client side
pq 'bottomk(5, mamori:service_mesh_latency:ratio_rate5m)'        # 4. slowest services
# 5. What checkoutservice's own calls return (Istio aborts and real errors, seen from its sidecar). Group by
#    destination_service_name: an injected abort has destination_workload="unknown", since no server answers.
pq 'sum by (destination_service_name, grpc_response_status)
    (rate(istio_requests_total{reporter="source",source_workload="checkoutservice"}[5m]))'
```

```bash
kubectl -n boutique get pods -o wide -l 'app in (checkoutservice,paymentservice,shippingservice,emailservice)'
kubectl -n boutique logs deploy/checkoutservice --since=15m | grep -iE 'error|fail' | tail -20
kubectl -n boutique rollout history deploy/checkoutservice
kubectl -n boutique get deploy checkoutservice -o jsonpath='{.spec.template.spec.containers[0].env}'; echo
kubectl -n argocd get applications
```

Compare the environment with Git (`deploy/online-boutique/` and the pinned upstream manifests): a changed service
address or setting is the classic bad config. More checks (nodes, fault rules, Argo CD history, Logs Explorer):
[cuj-browse.md](cuj-browse.md#queries).

## Mitigations

One at a time; wait for query 1 to move before the next.

1. **Remove an active fault** once it is the known cause: `gameday stop --all`.
2. **Roll back a recent change through Git** (`git revert`, pull request, merge); Argo CD syncs it. Never
   `kubectl rollout undo` while Argo CD self-heals: it puts the bad version straight back. If the bad config was
   applied outside Git, Argo CD may already be reverting it: check `kubectl -n argocd get application
   online-boutique` before acting. Break-glass steps in [cuj-browse.md](cuj-browse.md#mitigations).
3. **Restart a stuck service:** `kubectl -n boutique rollout restart deploy/<service>`.
4. **From M5:** the agent's proposal (often a rollback), approved only if its evidence matches.

## Escalation

No working mitigation after 30 minutes (SEV1) or 60 minutes (SEV2): stop diagnosing, roll back everything changed
in the last 24 hours, raise the severity ([../incidents/on-call.md](../incidents/on-call.md#escalation)). Check
[Google Cloud Service Health](https://status.cloud.google.com/).

## Afterwards

Resolve when checkout has been within target for 15 minutes and the alert has closed. Classify every alert in
[../incidents/alert-log.md](../incidents/alert-log.md); postmortem if
[../incidents/severity.md](../incidents/severity.md#when-a-postmortem-is-required) requires one.
