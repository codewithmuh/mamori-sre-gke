# Runbook: CUJ checkout alert

Status: draft, M3 (#20), 2026-11-13. Not yet used on a real alert; queries checked against the names in the build
contract, not against live data. The alerts exist only once `slo_enabled` is true
([../alerting.md](../alerting.md#alert-inventory)); until then nothing fires.

**Alerts:** `CUJ checkout: <prober|mesh> <availability|latency> <fast|medium|slow> burn`, and the shared `CUJ prober:
no data` and `CUJ mesh: no data` (inventory: [../alerting.md](../alerting.md)). **Journey:** the shopper places an
order with the demo's pre-filled fake card (`POST /cart/checkout`). Targets: 99.0 % available, 95 % under 1,000 ms.

## What fired

`prober` = the end-to-end probe from the in-cluster ingress gateway; `mesh` = sidecar metrics of the journey's critical
backend services. `availability` = orders fail; `latency` = slower than 1,000 ms. `fast` and `medium` on the prober page
(2 % of the 28-day budget in an hour, about 9 failed probes; 5 % in six hours; latency has lower thresholds of its own);
every mesh alert and the prober's `slow` tier (10 % in a day) are tickets. `CUJ prober: no data` and `CUJ mesh: no data`
cover every journey: see [cuj-browse.md](cuj-browse.md#no-data-alerts).

## Impact

Orders fail or hang. No real money moves (the card is fake and the payment service charges nothing). Severity
([../incidents/severity.md](../incidents/severity.md)): about half or more failing for 5 minutes is SEV1; a
page-tier alert below that is SEV2; a slow-tier ticket alone is SEV3.

## First five minutes

1. **Acknowledge** in PagerDuty, duplicates too. Don't resolve there.
2. **Game day?** `make gameday-status` (from the repo root) and `#incidents`.
3. **Declare** in `#incidents` with a first severity
   ([../incidents/communication.md](../incidents/communication.md)).
4. **Size it** with queries 1 and 2. The prober runs checkout as `home`, `add-to-cart`, `view-cart`, then
   `place-order`: a failure at an earlier step belongs to that journey's runbook; a failure at `place-order` is
   checkout's own. Checkout alone points at `checkoutservice`, `paymentservice` or a change to them; checkout plus
   other journeys points at what they share (cart, currency, catalog, shipping).
5. **What changed?** Argo CD history, recent merges to `deploy/`, a lost node, an active fault rule. Also mesh
   config made outside Git, which Argo CD doesn't show: `kubectl -n boutique get
   sidecars,virtualservices,authorizationpolicies`. An Istio Sidecar, VirtualService or AuthorizationPolicy can cut
   `checkoutservice` off from a dependency while every pod stays `Ready`.

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
kubectl -n boutique get sidecars,virtualservices,authorizationpolicies --show-labels   # mesh config, in Git or not
kubectl -n boutique logs deploy/checkoutservice --since=15m | grep -iE 'error|fail' | tail -20
kubectl -n boutique rollout history deploy/checkoutservice
kubectl -n boutique get deploy checkoutservice -o jsonpath='{.spec.template.spec.containers[0].env}'; echo
kubectl -n argocd get applications
```

Query 5 names the failing call. `paymentservice` answering `UNAVAILABLE` while the payment pods get no traffic
points at mesh config on checkout's side, not at payment. Any Sidecar, VirtualService or AuthorizationPolicy in
`boutique` that isn't in `deploy/` is drift; one labelled `mamori.dev/fault=true` is a game-day fault. A changed
service address in the Deployment's environment (compare with Git) is the other classic bad config. More checks
(nodes, fault rules, Argo CD history, Logs Explorer): [cuj-browse.md](cuj-browse.md#queries).

## Mitigations

One at a time; wait for query 1 to move before the next.

1. **Remove an active fault** once it is the known cause: `make gameday-stop-all`.
2. **Remove other mesh config made outside Git** once it is the known cause: save it for the postmortem
   (`kubectl -n boutique get <kind>/<name> -o yaml`), then `kubectl -n boutique delete <kind>/<name>`.
3. **Roll back a recent Git change** (`git revert`, pull request, merge); Argo CD syncs it. Never
   `kubectl rollout undo` while Argo CD self-heals: it puts the bad version straight back. Break-glass steps in
   [cuj-browse.md](cuj-browse.md#mitigations).
4. **Restart a stuck service:** `kubectl -n boutique rollout restart deploy/<service>`.
5. **From M5:** the agent's proposal, approved only if its evidence matches. The executor refuses rollbacks: if the
   agent suggests one, a person does it as a Git revert.

## Escalation

No working mitigation after 30 minutes (SEV1) or 60 minutes (SEV2): stop diagnosing, roll back everything changed
in the last 24 hours, raise the severity ([../incidents/on-call.md](../incidents/on-call.md#escalation)). Check
[Google Cloud Service Health](https://status.cloud.google.com/).

## Afterwards

Resolve when checkout has been within target for 15 minutes and the alert has closed. Classify every alert in
[../incidents/alert-log.md](../incidents/alert-log.md); postmortem if
[../incidents/severity.md](../incidents/severity.md#when-a-postmortem-is-required) requires one.
