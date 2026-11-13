# Runbook: CUJ browse alert

Status: draft, M3 (#20), 2026-11-13. Not yet used on a real alert; queries checked against the names in the build
contract, not against live data.

**Alerts:** `CUJ browse: <prober|mesh> <availability|latency> <fast|medium|slow> burn` and `CUJ browse: prober no
data` (inventory: [../alerting.md](../alerting.md)). **Journey:** the shopper opens the home page (`GET /`) and a
product page (`GET /product/{id}`).

## What fired

| Part of the name | Means |
|---|---|
| `prober` | The Go prober's runs fail or are slow. It enters at the Istio ingress gateway inside the cluster, so it covers the gateway, the frontend and every service behind it, but not the load balancer, certificate or Cloud Armor |
| `mesh` | The Istio sidecar metrics: the product of the journey's critical services' success rates, or their latency, per minute |
| `availability` / `latency` | Requests fail / requests are slower than 500 ms |
| `fast`, `medium` | Page: 2 % of the 28-day budget gone in an hour, or 5 % in six hours |
| `slow` | Ticket: 10 % gone in a day. Look within working hours |
| `prober no data` | The prober stopped reporting. The shop may be fine: see the last section |

## Impact

Shoppers can't see the shop, or it is slow. Every order starts with browsing, so a browse outage stops orders too.
Severity ([../incidents/severity.md](../incidents/severity.md)): about half or more of requests failing for 5
minutes is SEV1; a page-tier alert below that is SEV2; a slow-tier ticket alone is SEV3.

## First five minutes

1. **Acknowledge** in PagerDuty, including duplicates ("acknowledge all"). Don't resolve there; the alert closes by
   itself.
2. **Game day?** `gameday status` and `#incidents`. If a fault is running and explains what you see, follow the game
   day ([../incidents/game-days.md](../incidents/game-days.md)). Anything it doesn't explain is a real incident.
3. **Declare** in `#incidents` with a first severity ([../incidents/communication.md](../incidents/communication.md)).
4. **Size it:** run queries 1 and 2 below. Is it browse only, or other journeys too? Several journeys at once point
   at what they share: `cartservice`/`redis-cart`, `currencyservice`, `productcatalogservice`, the frontend or the
   ingress gateway.
5. **What changed?** Argo CD history, recent merges to `deploy/`, a Terraform apply, a lost node, an active fault
   rule. Most incidents follow a change.

## Likely causes

Critical services, read from Online Boutique v0.10.7's frontend: a failure here fails the page. The measured list
is in `slo/cuj-map.yaml` (#17); check it, it wins over this table.

| Service | Why browse needs it |
|---|---|
| `frontend` | Serves every page |
| `productcatalogservice` | The home page lists products; the product page loads one |
| `currencyservice` | The currency list, and every price is converted, one product after another on the home page |
| `cartservice` → `redis-cart` | Every page shows the cart size. A `redis-cart` restart fails pages until `cartservice` reconnects, and empties all carts |
| `istio-ingressgateway` (`istio-ingress`) | One replica; every outside request and every probe enters here |

Not critical for errors: `adservice` (the frontend waits 100 ms, then shows no ad) and `recommendationservice`
(errors ignored). But the frontend waits for recommendations with no timeout, so a **slow** recommendation service
makes product pages slow: a latency alert with no errors.

Platform causes: a Spot node preempted (pods moving, some `Pending`), the spend guard (no nodes at all,
[spend-guard.md](spend-guard.md)), a bad sync from Argo CD, `istiod` down (existing config keeps working, changes
don't arrive). If the prober is green but people outside see errors, look at the edge: load balancer, certificate,
Cloud Armor's rate limit (429).

## Queries

PromQL against Managed Service for Prometheus, from a shell (or paste the query into Metrics Explorer's PromQL tab):

```bash
pq() { curl -sS "https://monitoring.googleapis.com/v1/projects/$PROJECT_ID/location/global/prometheus/api/v1/query" \
  -H "Authorization: Bearer $(gcloud auth print-access-token)" --data-urlencode "query=$1" \
  | jq -r '.data.result[] | "\(.metric | del(.__name__) | tostring)  \(.value[1])"'; }

pq 'mamori:cuj_prober_availability:ratio_rate5m'        # 1. every journey, prober (1 = all good)
pq 'mamori:cuj_mesh_availability:ratio_rate5m'          #    every journey, mesh
# 2. which prober step fails
pq 'sum by (step) (rate(mamori_cuj_step_total{cuj="browse",source="scheduled",result="failure"}[10m]))'
pq 'bottomk(5, mamori:service_mesh_availability:ratio_rate5m)'   # 3. worst services (label destination_service_name)
pq 'bottomk(5, mamori:service_mesh_latency:ratio_rate5m)'        # 4. slowest services (share under threshold)
```

```bash
kubectl get nodes                                          # 3 Ready? one preempted?
kubectl -n boutique get pods -o wide                       # not Running, restarts, which node
kubectl -n boutique get events --sort-by=.lastTimestamp | tail -20
kubectl -n boutique logs deploy/frontend --since=15m | grep -iE 'error|fail' | tail -20
kubectl -n argocd get applications                         # all Synced and Healthy?
kubectl -n argocd get application online-boutique \
  -o jsonpath='{range .status.history[*]}{.deployedAt}{"  "}{.revision}{"\n"}{end}' | tail -3
kubectl api-resources --verbs=list --namespaced -o name \
  | xargs -n1 kubectl get -n boutique -l mamori.dev/fault=true -o name --ignore-not-found 2>/dev/null  # fault rules
```

Logs Explorer: `resource.type="k8s_container" resource.labels.namespace_name="boutique" severity>=ERROR`.
Kubernetes events older than an hour: `logName="projects/PROJECT_ID/logs/events"`.

## Mitigations

One at a time; wait for query 1 to move before the next ([../incidents/roles.md](../incidents/roles.md)).

1. **Remove an active fault** once you know it is the cause: `gameday stop --all`, then the label query above must
   return nothing.
2. **Roll back a recent change through Git**: `git revert <sha>`, pull request, merge; Argo CD syncs it. Don't use
   `kubectl rollout undo`: Argo CD's self-heal puts the Git version straight back. If a SEV1 can't wait for CI,
   break glass and pause auto-sync, root first (it would restore the child):
   `kubectl -n argocd patch application root --type merge -p '{"spec":{"syncPolicy":{"automated":null}}}'`, then
   the same for `online-boutique`; roll back by hand; afterwards `kubectl apply -f deploy/bootstrap/root-app.yaml`
   turns auto-sync back on, and the fix must be in Git by then. Note every step in the thread.
3. **Restart a stuck service**: `kubectl -n boutique rollout restart deploy/<service>` (Argo CD's own restart action
   does the same and doesn't fight self-heal). After a `redis-cart` restart, restart `cartservice` if it still fails.
4. **Capacity lost** (node gone, pods `Pending`): GKE recreates the Spot VM when capacity allows; wait and watch
   `kubectl get pods -A --field-selector=status.phase=Pending`. Nothing cheap fixes a Spot stock-out.
5. **From M5:** the agent's proposal in the thread. Approve only if its evidence matches what the queries show.

## Escalation

Time boxes from [../incidents/on-call.md](../incidents/on-call.md#escalation): no working mitigation after
30 minutes (SEV1) or 60 minutes (SEV2) → stop diagnosing, use the generic mitigations above for everything changed
in the last 24 hours, raise the severity. Check [Google Cloud Service Health](https://status.cloud.google.com/).
Last resort: rebuild with `make down` and `make up`.

## "CUJ browse: prober no data"

The prober has sent nothing for browse for 10 minutes. The same alert usually fires for all four journeys.

1. Check the mesh SLIs (query 1, mesh line). If they are normal, the shop is fine and the lab is blind: SEV3.
2. `kubectl -n prober get pods` and `kubectl -n prober logs deploy/prober --since=15m`; then the prober's runbook,
   [prober.md](prober.md) (#16).
3. If every pod in the cluster stopped reporting, suspect the nodes or the spend guard, not the prober.

## Afterwards

Resolve when browse has been within target for 15 minutes and the alert has closed. Classify every alert in
[../incidents/alert-log.md](../incidents/alert-log.md); write a postmortem if
[../incidents/severity.md](../incidents/severity.md#when-a-postmortem-is-required) requires one.
