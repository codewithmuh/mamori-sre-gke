# deploy/

Everything that runs inside the GKE cluster, delivered by Argo CD from this repo (GitOps). Terraform
(`infra/`) makes the cluster and the cloud pieces the edge needs; `make up` installs Argo CD and hands
over to it.

## Layout

```
deploy/
├── bootstrap/
│   ├── argocd-values.yaml      Helm values for Argo CD (installed by `make up`, not by Argo CD)
│   └── root-app.yaml           Application "root": syncs deploy/apps from main
├── apps/                       one Argo CD Application per component, ordered by sync wave
│   ├── platform.yaml           wave -1 cluster-wide objects (deploy/platform)
│   ├── istio-base.yaml         wave 0  Istio CRDs                     -> istio-system
│   ├── istiod.yaml             wave 1  Istio control plane            -> istio-system
│   ├── istio-ingressgateway.yaml wave 2 Istio ingress gateway         -> istio-ingress
│   ├── online-boutique.yaml    wave 3  the shop (deploy/online-boutique) -> boutique
│   └── edge.yaml               wave 4  public load balancer (deploy/edge) -> istio-ingress
├── platform/                   PriorityClass mamori-platform (istiod, ingress gateway)
├── istio/                      Helm values for the three Istio charts (multi-source Applications)
├── online-boutique/            kustomize: pinned upstream v0.10.7 + Istio component + our patches
├── edge/                       GKE Gateway, HTTPRoutes, GCPBackendPolicy, HealthCheckPolicy
└── validate.sh                 offline render and schema check of everything above
```

Values files live outside `deploy/apps/` on purpose: Argo CD reads every YAML file in that directory as
a manifest.

## Request path

```
user ─HTTPS─▶ mamori-frontend-ip (global external Application Load Balancer, made by the GKE Gateway)
               │  TLS: certificate map mamori-cert-map    rate limit: Cloud Armor mamori-frontend-armor
               │  HTTP :80 is answered with a 301 to HTTPS
               ▼  NEG, straight to pod IPs, plain HTTP :80; health check :15021 /healthz/ready
             istio-ingressgateway pods (ns istio-ingress, Service type ClusterIP)
               │  Istio Gateway + VirtualService "frontend-public" accept only mamori.codewithmuh.com
               ▼
             frontend (ns boutique) ─gRPC─▶ the other services and redis-cart, all with Envoy sidecars

prober, k6 (in cluster) ─HTTP─▶ istio-ingressgateway.istio-ingress.svc.cluster.local:8080
               same pods, same Istio Gateway and VirtualService, no load balancer, no rate limit
```

There is no Service of type `LoadBalancer`. The upstream `frontend-external` Service is deleted by the
`service-mesh-istio` component, and istiod is told not to create gateways of its own
(`PILOT_ENABLE_GATEWAY_API_DEPLOYMENT_CONTROLLER=false`). `validate.sh` fails if one appears.

## Internal entry (port 8080)

Synthetic traffic from inside the cluster (the CUJ prober, k6 load tests) uses its own port on the
ingress gateway:

```
http://istio-ingressgateway.istio-ingress.svc.cluster.local:8080   with   Host: mamori.codewithmuh.com
```

- Same gateway pods and the same `frontend-public` VirtualService as public traffic, so it tests the
  same route (timeouts, retries, faults) to the frontend. Any other Host gets a 404.
- Not public: no HTTPRoute references port 8080, so the GKE Gateway builds no NEG for it, and the
  firewall only lets the load balancer reach ports 80 and 15021.
- Not rate limited: the gateway's local rate limit (an EnvoyFilter on the port-80 listener) and
  Cloud Armor only see public traffic. So a few clients at the public limit cannot starve the
  prober (and page on-call), and a load test does not use up the public's budget.
- Service port and target port are both 8080 (`http-internal` in `istio/gateway-values.yaml`); a
  NetworkPolicy for a caller must allow egress to TCP 8080 on pods labelled `istio: ingressgateway`
  in `istio-ingress`.

## Online Boutique changes

`online-boutique/kustomization.yaml` takes upstream v0.10.7 (remote refs pinned to commit
`5b608cb`, the tag's commit) and the `service-mesh-istio` component, then:

- deletes the component's Gateway API `Gateway` (class `istio`) and `HTTPRoute`. In v0.10.7 the
  component has no classic Istio Gateway with `istio: ingressgateway` labels to match; its Gateway
  API Gateway makes istiod deploy a gateway of its own behind a `LoadBalancer` Service. We use the
  Helm-installed gateway instead and bind to it with `public-gateway.yaml` (selector
  `istio: ingressgateway`, the label `gateway-values.yaml` produces).
- fixes the component's mesh `VirtualService`, which hard-codes the `default` namespace.
- sets `FRONTEND_MESSAGE`. The v0.10.7 frontend reads it in `src/frontend/handlers.go` and
  `templates/header.html` prints it in a bar at the top of every page, error pages included:
  "Demo shop, no real payments. Do not enter real card details."
- pins every image by digest, and pins `redis:alpine` (floating upstream) to `8.10.3-alpine`.

The namespace `boutique` carries `istio-injection: enabled`. The load generator stays (10 users, in
cluster, straight to the frontend Service). Its init container reaches the frontend because Istio
1.31 runs the sidecar as a native sidecar on Kubernetes 1.36, started before other init containers.

## Versions

| What | Version | Where it is set |
|---|---|---|
| Argo CD chart (argo-helm) | 10.11.6 (Argo CD v3.5.6) | Makefile (`make up`), `validate.sh` default |
| Istio charts `base`, `istiod`, `gateway` | 1.31.4, from `https://blob.istio.io/istio-release/charts` | `apps/istio-*.yaml` (three places, keep equal) |
| Online Boutique | v0.10.7 (commit `5b608cb`) | `online-boutique/kustomization.yaml` |
| Kubernetes validated against | 1.36.4 (GKE REGULAR default) | `validate.sh` |

Istio 1.31 supports Kubernetes 1.32 to 1.37, which covers everything GKE's REGULAR channel offers
today (1.34 to 1.37, default 1.36). 1.31.4 (2026-11-06) is younger than the usual two weeks. It is
taken anyway because it is a security release: it fixes a sidecar bypass through stale conntrack
entries and an istiod crash loop (both High), and Istio lists it as the first 1.31 patch with no
known CVEs. The fallback is 1.31.3 (2026-10-22). The old chart bucket
(`istio-release.storage.googleapis.com/charts`) stops at 1.30.x.

## Bring up (`make up`)

After Terraform has made the cluster, static IP, certificate map and Cloud Armor policy:

1. Get credentials for the cluster.
2. Install Argo CD:
   `helm upgrade --install argocd argo-cd --repo https://argoproj.github.io/argo-helm --version 10.11.6 -n argocd --create-namespace -f deploy/bootstrap/argocd-values.yaml --wait`
3. `kubectl apply -f deploy/bootstrap/root-app.yaml`
4. Wait. Argo CD syncs the waves in order and waits for each to be healthy (`argocd-values.yaml`
   restores the Application health check that app-of-apps ordering needs). Useful checks:
   `kubectl -n argocd wait applications.argoproj.io/root --for=jsonpath='{.status.health.status}'=Healthy --timeout=20m`,
   then `kubectl -n boutique rollout status deploy/frontend` and
   `kubectl -n istio-ingress wait gateways.gateway.networking.k8s.io/mamori-frontend --for=condition=Programmed --timeout=15m`.
5. Add the DNS records Terraform prints (the A record and the certificate's DNS authorization
   CNAME). Until the certificate is active, HTTPS fails; HTTP still redirects.

Argo CD follows `main`. A change merged to `deploy/` reaches the cluster within about three minutes
(reconcile every 120 s plus jitter).

## When a sync is stuck

Argo CD's automated sync does not run again for a commit whose sync failed. So every Application
here, root included, has `syncPolicy.retry` with no limit (10 s, doubling, at most every 3 min): a
child that is Degraded for a while (a preempted node, a slow start) is picked up again on its own,
and the later waves still get created. To look, and to push by hand once the cause is fixed:

```sh
kubectl -n argocd get applications.argoproj.io     # sync status and health per component
kubectl -n argocd get applications.argoproj.io root -o jsonpath='{.status.operationState.message}'

# With the argocd CLI, through the port-forward in "Argo CD access":
argocd login localhost:8080 --username admin --insecure
argocd app terminate-op root   # only if a sync is still running, waiting on something broken
argocd app sync root           # fresh sync from main; the children follow wave by wave

# Without the CLI, start a sync of root:
kubectl -n argocd patch applications.argoproj.io root --type merge \
  -p '{"operation":{"initiatedBy":{"username":"admin"},"sync":{"revision":"main","prune":true}}}'
```

The same works for one child (for example `online-boutique`) when only it is stuck.

## Tear down (`make down`)

The GKE Gateway controller owns load-balancer pieces that Terraform does not know about. If the cluster
goes first, they stay behind and cost money, and the static IP cannot be deleted while a forwarding
rule uses it. So, in this order:

1. `kubectl -n argocd delete applications.argoproj.io root --wait --timeout=15m`. Every Application has the
   `resources-finalizer.argocd.argoproj.io` finalizer, so this deletes the child Applications and
   everything they deployed, including the Gateway. Delete root first: while it exists, Argo CD
   self-heal would re-create anything deleted by hand.
2. `kubectl delete gateways.gateway.networking.k8s.io -n istio-ingress mamori-frontend --ignore-not-found --wait --timeout=10m`.
   A fallback for step 1. Use the full resource name: with Istio installed, plain `gateway` can mean
   Istio's `networking.istio.io` Gateway. The GKE Gateway controller runs in Google's control plane,
   so this works even when the nodes are scaled to zero and Argo CD is not running.
3. Wait until the load balancer is released: the static IP `mamori-frontend-ip` shows status
   `RESERVED` instead of `IN_USE`, and no forwarding rules, target proxies, URL maps, backend services
   or health checks starting `gkegw1-` and no NEGs starting `k8s1-` are left.
4. `terraform destroy`, then `scripts/leftover-check.sh`.

## Argo CD access

No Ingress and no public Service. Use a port-forward:

```sh
kubectl -n argocd port-forward svc/argocd-server 8080:443
# open https://localhost:8080 (self-signed certificate), user "admin"
kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d
```

## Resource budget

Cluster: 3 × e2-standard-2 Spot (2 vCPU, 8 GiB each), GKE 1.36. The target is to keep total requests
under 70 % of allocatable. Below 67 % (two nodes out of three), the pods of a drained or preempted
node still fit on the other two.

**Allocatable per node.** GKE reserves CPU as 6 % of the first core + 1 % of the second: 70m, leaving
**1930m**. Memory (versions before 1.37.2): 25 % of the first 4 GiB + 20 % of the next 4 GiB = 1.8 GiB,
plus 100 MiB for eviction. The kernel also reports a little under 8 GiB, so expect about **5.8 GiB**
per node (estimate; check with `kubectl describe node`). From GKE 1.37.2 the memory reservation is
the smaller of that and 15 MiB × max Pods per node + 500 MiB; with the node pool's 64 Pods per node
that is 1.4 GiB, so about 0.4 GiB more per node after that upgrade. Source: GKE "About node sizing".

| Requests | CPU | Memory | Notes |
|---|---|---|---|
| GKE system pods | ~1350m | ~1.5 GiB | **estimate**: kube-dns ×2, konnectivity, metrics-server, and per node fluentbit, anetd (Dataplane V2), metrics agent, PD CSI, Managed Prometheus collector |
| Argo CD (5 pods) | 195m | 624 MiB | controller 100m/320Mi, repo-server 50m/128Mi, server 25m/96Mi, appset 10m/48Mi, redis 10m/32Mi; Dex and notifications off |
| istiod (2 pods) | 200m | 512 MiB | 2 × 100m / 256 MiB; chart default 500m / 2 GiB each |
| Istio ingress gateway (1 pod) | 50m | 128 MiB | chart default 100m / 128 MiB, autoscaling off |
| Online Boutique (12 pods) | 1570m | 1368 MiB | upstream requests, unchanged; load generator 300m / 256 MiB |
| Envoy sidecars (12 × 20m / 64 MiB) | 240m | 768 MiB | chart default 12 × 100m / 128 MiB = 1200m / 1.5 GiB |
| **Total** | **~3605m** | **~4.8 GiB** | |
| Allocatable (3 nodes) | 5790m | ~17.4 GiB | |
| **Share of allocatable** | **~62 %** | **~28 %** | |
| 70 % line | 4053m | ~12.2 GiB | headroom ~450m CPU |

CPU is the limit. With Istio's default sizes the same stack would request about 5.4 vCPU (~94 %), which
is why istiod and the sidecars are trimmed. Measure real use in M1 (`kubectl top`, Managed Prometheus)
and adjust; the sidecar size is in `istio/istiod-values.yaml`. Surge upgrades add a fourth node for a
while, which only adds room.

**Priority and replicas.** istiod's injection webhook has `failurePolicy: Fail`, so while no istiod
runs, no pod can be created in `boutique` or `istio-ingress`. That includes every pod being
rescheduled off a preempted or drained node. Two measures:

- istiod and the ingress gateway use PriorityClass `mamori-platform` (`platform/priorityclass.yaml`,
  value 100000). When the two remaining nodes are short of room, they are scheduled first and may
  preempt shop pods, which wait Pending instead.
- istiod runs two replicas spread over nodes, with a PodDisruptionBudget (minAvailable 1) that the
  chart adds. A single node loss then leaves the webhook up. The second replica costs 100m / 256 MiB,
  which the budget above allows.

The ingress gateway stays at one replica. A second replica for the front door is a hardening change
to make with a measured before and after.

## Validate offline

```sh
deploy/validate.sh          # helm template + kubectl kustomize + kubeconform -strict + no LoadBalancer
```

It needs `helm`, `kubectl`, `kubeconform` or `docker` (it falls back to a pinned kubeconform image),
and network for charts, remote kustomize bases and schemas. It reads each chart's version and values
file from the Application itself and finds every kustomization under `deploy/`, so it checks what
Argo CD will render. CRD schemas (Argo CD, Gateway API, Istio, GKE `networking.gke.io` policies) come
from the datreeio CRDs-catalog, pinned to a commit; a kind without a schema fails the run. `make lint`
and CI run it with the Makefile's Argo CD chart version.

CI also runs `trivy fs --scanners vuln,misconfig,secret --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 .`
over the repo; `deploy/` passes it with no ignores.

## What the cluster needs from Terraform

`infra/envs/lab` provides these; if a name changes there, change it here too.

- Gateway API (`CHANNEL_STANDARD`) and the HTTP load balancing add-on.
- Global static IP `mamori-frontend-ip`, certificate map `mamori-cert-map` with an entry for
  `mamori.codewithmuh.com`, global Cloud Armor policy `mamori-frontend-armor`. The Gateway stays
  unprogrammed until they exist.
- Firewall `mamori-allow-control-plane-webhooks`: TCP 15017 from the control plane range to the
  nodes. Istio's injection and validation webhooks listen there; without it no pod in `boutique`
  can be created on a private cluster. A new admission webhook on another port needs its own rule.
- Firewall `mamori-allow-lb-to-ingress-gateway`: `35.191.0.0/16` and `130.211.0.0/22` to the nodes on
  TCP 80 (the gateway pods' serving port) and 15021 (their readiness port), for load-balancer health
  checks and traffic. The GKE Gateway controller usually creates a similar rule itself.
