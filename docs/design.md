# Design: mamori-sre-gke

Status: draft, M0 (2026-11-13). This describes the planned system; nothing is deployed yet. Update this page in the
same pull request that changes the design. Product view: [prd.md](prd.md). Scope and decisions:
[PLAN.md](../PLAN.md). Threats: [threat-model.md](threat-model.md).

## Components

Every component in the [PLAN.md](../PLAN.md) section 4 sketch, plus the M1 pieces from #6–#9. "Arrives" is the
milestone that adds it (plan, not fact).

| Component | What it does | Where it runs | Arrives |
|---|---|---|---|
| GitHub monorepo | `infra/`, `deploy/`, `tools/` (Go), later `agent/` and `services/` (the fork) | GitHub | M0 |
| CI (GitHub Actions) | Checks every PR (one required check, `CI result`); `agent.yml`, `evals.yml` and `slo.yml` run as separate workflows outside `CI result`; Terraform plan on PR posted as a sticky comment; apply by hand behind the protected `lab` environment; keyless via Workload Identity Federation | GitHub | M1 |
| Network | VPC `mamori-vpc`, subnet `mamori-tokyo`, Cloud NAT `mamori-nat`; nodes have no public IPs | Google Cloud | M1 |
| GKE Standard cluster `mamori` | Zonal (`asia-northeast1-a`), REGULAR channel, Workload Identity, Gateway API, Managed Service for Prometheus | Google Cloud | M1 |
| Node pool `spot-pool` | 3 × e2-standard-2 Spot, fixed size in M1 | Google Cloud | M1 |
| Artifact Registry `mamori` | Images for our own tools and the forked services, with a cleanup policy and immutable tags; created in `infra/bootstrap`, so it survives `make down` | Google Cloud | M1 |
| Edge | GKE Gateway → global external Application Load Balancer, Cloud Armor rate limits, Certificate Manager certificate | Google Cloud | M1 |
| Argo CD | GitOps: app-of-apps `root` syncs `deploy/apps` from `main` | `argocd` | M1 |
| Istio | Sidecars, ingress gateway, traffic control, fault injection, request metrics | `istio-system`, `istio-ingress` | M1 |
| Online Boutique v0.10.7 | The workload: 10 application services (frontend over HTTP, the rest gRPC), `redis-cart`, and a Locust load generator. Upstream counts 11 services including the load generator | `boutique` | M1 |
| Budget and spend guard | Budget `mamori-lab` → Pub/Sub `budget-alerts` → Cloud Run function `spend-guard` scales the node pool to zero at 100 % | Google Cloud | M1 |
| Journey tests and discovery run | Playwright tests drive each journey while Istio aborts one upstream service at a time; output `cuj-map.yaml` | CI and cluster | M2 |
| CUJ prober (Go) | Runs every journey every minute against the public hostname | Cluster (placement decided in M2) | M2 |
| SLO generator (Go CLI) | Reads `cuj-map.yaml`, writes recording rules, Cloud Monitoring SLOs, burn-rate alerts, Datadog SLOs and monitors, dashboards | CI or a laptop | M2 |
| OTel Collector | Receives OpenTelemetry from the services; exports traces to Cloud Trace | Cluster | M2 |
| Managed Prometheus, Cloud Monitoring, Cloud Logging, Cloud Trace | Metrics, SLOs, burn-rate alerts, logs, traces | Google Cloud | M1–M2 |
| PagerDuty (free) and Slack (free) | Pages to a phone; incident channel; later, agent approvals | SaaS | M3 |
| Datadog Agent and Datadog config | Monitors, SLOs and synthetics from Terraform, for one planned 14-day window | Cluster and SaaS | M3 |
| Chaos Mesh | Pod, node and resource faults for game days (Istio covers dependency faults) | Cluster | M3 |
| HPA, PDBs, Istio resilience settings | Timeouts, retry budgets, outlier detection, load shedding | Cluster | M4 |
| AI incident agent (Python) | Alert → triage → root cause → one proposed action, posted to Slack | Cluster | M5 |
| Executor (Go) | Runs one approved, allow-listed action with its own narrow RBAC | Cluster | M5 |
| Claude on Vertex AI | The agent's model, global endpoint, called with Workload Identity | Google Cloud | M5 |
| Gatekeeper | Readiness checks as admission policies and audit reports; CI turns them into a weekly evidence bundle | Cluster and CI | M6 |

### Capacity

Three e2-standard-2 nodes give 6 vCPU and 24 GB before GKE's reservations; allocatable is lower. Online Boutique's
Deployments request about 1.57 vCPU and 1.34 GiB ([research](cost-and-tooling-research.md)); every Istio sidecar adds
its own requests on top. Total requests stay under about 70 % of allocatable, and `deploy/` records the resource
budget (#8). With one of three nodes gone (a Spot preemption), only about two thirds of the capacity is left, so some
pods can stay `Pending` until the node returns (my math). That is accepted; see failure domains.

## Request path

```
shopper's browser
  │  https://mamori.codewithmuh.com (A record added by hand where codewithmuh.com's DNS is hosted)
  ▼
global static IP mamori-frontend-ip
  ▼
global external Application Load Balancer, created by the GKE Gateway mamori-frontend
  · TLS with the Google-managed certificate in cert map mamori-cert-map; HTTP redirects to HTTPS
  · Cloud Armor mamori-frontend-armor: per-client-IP rate limit, 429 above it, otherwise allow
  ▼  container-native load balancing (NEGs) straight to pods; health check from a HealthCheckPolicy
Istio ingress gateway pods (namespace istio-ingress)
  ▼  Istio Gateway frontend-public + VirtualService (deploy/online-boutique/public-*.yaml;
     the Istio component's own Gateway is deleted)
frontend (Go, HTTP)  ◀── Locust load generator calls frontend:80 directly, skipping the edge
  ▼  gRPC through Envoy sidecars
product catalog · currency · cart (→ redis-cart) · recommendation · ad · shipping
checkout (→ cart, product catalog, currency, shipping, payment, email)
```

- **One public entry.** No Service of type `LoadBalancer` anywhere. Upstream's `frontend-external` load balancer is
  removed by its own Istio component, so the Gateway is the only way in from the internet.
- **Two kinds of traffic.** The load generator stays inside the cluster, so it never meets Cloud Armor or the load
  balancer. The prober and real visitors come through the edge, so they test what a shopper would.

## Telemetry and alert path

```
Envoy sidecars, app metrics ─┐
CUJ prober (M2) ─────────────┴─▶ Managed Service for Prometheus ─▶ Cloud Monitoring
                                                                   ├─ recording rules, dashboards
                                                                   └─ SLOs, burn-rate alerts
                                                                      ├─ page tier ─▶ PagerDuty ─▶ phone
                                                                      ├─ every alert ─▶ email
                                                                      └─ every alert ─▶ Pub/Sub incident-alerts
                                                                         └▶ agent pulls mamori-agent-incidents (M5)
services (OpenTelemetry) ─▶ OTel Collector (traces only) ─▶ Cloud Trace
container stdout ─▶ GKE logging agent ─▶ Cloud Logging
Datadog Agent (M3 window) ─▶ Datadog monitors, SLOs, synthetics ─▶ Slack or email, never pages
```

- **SLIs** come from sidecar metrics (`istio_requests_total`, `istio_request_duration_milliseconds`) and the prober;
  the definitions are in [prd.md](prd.md). How to express "product of the critical APIs' success rates" in Cloud
  Monitoring and in Datadog is an M2 design task.
- **The sketch in PLAN.md** draws metrics, traces and logs all behind the OTel Collector. In Online Boutique the
  Collector component exports traces only, and GKE ships logs and Managed Prometheus scrapes metrics without it. M2
  decides which signals actually go through the Collector.
- **Alert routing:** fast burn pages through PagerDuty; slow burn notifies without paging. From M5 the same alerts
  also call the agent's webhook.
- **Watching the watcher:** the prober and the collectors run in the cluster they watch. If the cluster goes down,
  they stop reporting instead of reporting failures. The SLO data itself lives in Cloud Monitoring, outside the
  cluster; M2 adds alerts on missing data.

## Change paths

| What changes | Path | Rollback |
|---|---|---|
| Kubernetes config and app versions (`deploy/`) | PR → CI renders and validates manifests → review → squash merge to `main` → Argo CD `root` syncs the child apps in sync-wave order: Istio base (0), istiod (1), Istio ingress gateway (2), Online Boutique (3), edge (4); `prober`, `observability`, `chaos-mesh`, `gatekeeper` (5); `slo`, `policy`, `resilience` (6); `agent`, `executor` (7) | Revert the commit; Argo CD syncs the revert |
| Our images (`tools/`, later `services/`) | PR → CI tests → image built and pushed to Artifact Registry `mamori` → a version bump in `deploy/` → Argo CD | Revert the version bump |
| Infrastructure (`infra/envs/lab`) | PR → CI authenticates through Workload Identity Federation as `tf-plan` (read-only; plans run without taking the state lock) → `terraform plan` posted as one sticky PR comment (fork and Dependabot PRs skip this job) → review → merge → `terraform-apply.yml`, started by hand (`workflow_dispatch`) from `main`: a preview plan as `tf-plan` in the job summary, then approval on the protected GitHub environment `lab`, the only place that can impersonate `tf-apply`, which plans again and applies that plan | Revert and apply again |
| Bootstrap (`infra/bootstrap`) | Applied once by hand with the maintainer's own credentials and local state; it creates the state bucket and the CI identity | By hand |
| Lab lifecycle | `make up` builds everything from nothing; `make down` asks you to type the project ID, removes the root app and the Gateway, waits for the load balancer to go, runs `terraform destroy`, then `scripts/leftover-check.sh`; `make resume` restores the node pool after the spend guard | — |

Open points, decided before the milestone that needs them:

- **Executor vs GitOps (M5).** If Argo CD self-heals, a rollback, restart or scale done directly on the cluster is
  undone on the next sync. Executor actions must go through Argo CD (or a reverting commit), or the affected fields
  must be left to Kubernetes (for example, replicas owned by an HPA).
- **Fault rules (M2, M3).** Istio faults for the discovery run and game days are applied either through Git or
  outside Argo CD's view; whichever it is, Argo CD must not remove them mid-experiment, and the executor's "remove a
  fault rule" must know where they live.

## Failure domains accepted for a lab

| Domain | What fails | Why it is accepted | What it costs us |
|---|---|---|---|
| One zone (`asia-northeast1-a`) | A zone outage stops everything | Multi-region and DR are out of scope; a regional cluster also loses the free-tier cluster fee | Any zone incident burns every journey's budget |
| Zonal control plane | One control plane; during its upgrades the Kubernetes API is briefly unavailable (workloads keep running) | Free tier covers one zonal cluster | Argo CD syncs and `kubectl` pause; the agent's read tools fail for that time |
| Spot nodes | Google can reclaim any node at short notice, in the worst case all three at once, and Spot capacity can run out for a while | About 40 % cheaper than on-demand (my math from list prices); preemptions are free real incidents (D2) | Capacity drops by a third per lost node; a stock-out could take the shop down |
| One node pool, fixed size | No autoscaling in M1; surge upgrades need room for a fourth node within the trial's CPU quota | Keeps cost flat and predictable; quota is checked in M1 (#15) | Upgrades and preemptions are visible in the SLOs |
| One cluster | Shop, mesh, Argo CD, prober, collectors and, later, the agent share one failure domain | Cost; one cluster to run | A cluster-wide failure blinds the in-cluster tools; Cloud Monitoring keeps the data |
| `redis-cart` in the cluster | Upstream's Redis has no persistent disk (an `emptyDir` volume); when the pod moves, for example after a preemption, every cart is lost | Carts are synthetic | Add-to-cart and checkout fail while it moves; carts start empty afterwards |
| Spend guard | At 100 % of budget, the node pool goes to zero and the shop is down on purpose | Cost wins over availability in a lab (D12) | Counted against the error budget; the postmortem names it |
| One on-call person | Nights and weekends depend on one phone | Lab | Time to acknowledge is measured, not promised |

## Security boundaries

- **No keys anywhere.** CI reaches Google Cloud only through Workload Identity Federation: pool `github`, provider
  `github-actions`, accepting tokens only for the repository `codewithmuh/mamori-sre-gke`. Any branch or PR may use
  `tf-plan` (read-only, plus what state locking needs); fork and Dependabot PRs skip the plan job. Only the
  protected GitHub environment `lab` may use `tf-apply`, which holds the narrowest set of roles that can apply the
  lab, each one explained in `infra/`.
- **Workloads use Workload Identity for GKE.** Each Kubernetes service account that needs Google Cloud access maps to
  its own IAM identity. Nodes run as `gke-nodes`, with minimal roles.
- **Private nodes.** No public IPs on nodes; outbound traffic goes through Cloud NAT. Whether the control-plane
  endpoint is also restricted is set in `infra/` (#6).
- **One public entry** (see request path), with HTTPS only, a Google-managed certificate and Cloud Armor rate limits.
  Argo CD and every other UI stay private; reach them with `kubectl port-forward`.
- **The demo can't take money.** A banner on every page; checkout keeps the demo's pre-filled fake card; no accounts.
- **The agent cannot write (D9).** The agent's identity has read-only Kubernetes RBAC and read-only Google Cloud
  roles, plus the right to call Claude on Vertex AI. It can only propose. The executor is a separate Go program with
  its own identity and narrow RBAC. It runs one allow-listed action per incident (restart, scale within bounds,
  remove a fault object) after a human approves in Slack, writes an audit log, and has a kill switch. A rollback is
  not one of them: it is a Git revert done by a person, since Argo CD's self-heal undoes rollbacks on the cluster. The
  guardrails sit in RBAC and code, not in the prompt.
- **The spend guard can only shrink.** Its service account `spend-guard` has a custom role that reads the cluster and
  node pool and changes the pool's size and autoscaling, nothing else.
- **No secrets in Git.** gitleaks in pre-commit and CI; `.env` is ignored. Third-party tokens (PagerDuty, Slack,
  Datadog) arrive in M3 and M5 and never go in Git or Terraform state.

## Cost controls

Estimates and the full table are in [PLAN.md](../PLAN.md) section 6: about $150–190 a month while running (estimate).

- **Cheap by design:** a zonal cluster (its fee is covered by the free tier), Spot nodes only, e2-standard-2,
  30 GB pd-standard disks, a fixed-size pool, an Artifact Registry cleanup policy, and a small metric volume.
- **Budget:** `mamori-lab`, default $200 a month (`monthly_budget_usd`), alerts at 50/90/100 % of actual spend.
  It subtracts free-tier credits (so the free cluster fee doesn't count) but not the trial credit, so it tracks real
  spend. Until the account is upgraded at the start of M5, the budget stays below the trial credit (D12).
- **Spend guard:** at 100 % the `spend-guard` function turns node autoscaling off if it is on and sets `spot-pool` to
  zero nodes. It deletes nothing; `make resume` brings the nodes back, but only after the budget is raised or the
  next month starts, or the guard fires again within hours. It has a dry-run mode. Billing data arrives
  with a delay, so the guard limits spend; it does not stop it instantly. Runbook:
  [runbooks/spend-guard.md](runbooks/spend-guard.md).
- **Edge cost:** Cloud Armor and load-balancer logging bill every request, denied ones included, and the spend guard
  can't stop them. Denied requests are mostly excluded from logging, a request-count alert fires early, and
  `make edge-off` removes the public entry ([PLAN.md](../PLAN.md) section 6).
- **Teardown:** `make down` removes the Gateway first, because the Gateway controller would otherwise leave billable
  load-balancer pieces behind, then destroys everything and fails if anything billable is left.
- **Measure, then replace estimates:** cost per day measured in M1 (#15); Infracost replaces the list-price estimates.

## Names

Fixed names, so the branches built in parallel fit together.

| Thing | Name |
|---|---|
| Google Cloud project | Terraform variable `project_id`, no default (example `mamori-lab-123456`) |
| Region and zone | `asia-northeast1`; variable `zone`, default `asia-northeast1-a` |
| Labels on every resource that takes them | `app = "mamori"`, `env = "lab"`, `managed-by = "terraform"` |
| Terraform state | Bucket `${project_id}-tfstate`; `infra/envs/lab` uses prefix `lab` |
| Keyless CI | Workload identity pool `github`, provider `github-actions`; service accounts `tf-plan` and `tf-apply`; GitHub environment `lab` |
| GitHub repository variables | `GCP_PROJECT_ID`, `GCP_WIF_PROVIDER`, `GCP_TF_PLAN_SA`, `GCP_TF_APPLY_SA`, `TF_STATE_BUCKET` |
| GitHub repository secret | `GCP_BILLING_ACCOUNT_ID` (a secret so it is masked in public logs) |
| Network | VPC `mamori-vpc`; subnet `mamori-tokyo` with secondary ranges `pods` and `services`; router `mamori-router`; NAT `mamori-nat` |
| Cluster | `mamori` (zonal) |
| Node pool and node identity | `spot-pool`; service account `gke-nodes` |
| Artifact Registry | Docker repository `mamori` in `asia-northeast1` |
| Edge | Static IP `mamori-frontend-ip`; Cloud Armor policy `mamori-frontend-armor`; certificate `mamori-frontend-cert`; certificate map `mamori-cert-map` |
| Public hostname | Variable `frontend_hostname`, default `mamori.codewithmuh.com` |
| Budget | `mamori-lab`, variable `monthly_budget_usd` (default 200); Pub/Sub topic `budget-alerts` |
| Spend guard | Cloud Run function `spend-guard`, entry point `ScaleToZero`, service account `spend-guard`; variable `spend_guard_dry_run` |
| Namespaces | `argocd`, `istio-system`, `istio-ingress`, `boutique`, `prober`, `observability`, `chaos-mesh`, `mamori-agent`, `gatekeeper-system`, `loadtest` (capacity tests only), `datadog` (14-day window only) |
| Argo CD | Root Application `root` → `deploy/apps` on `main` |
| Gateway | `mamori-frontend`, class `gke-l7-global-external-managed` |
| Local config for `make` | `PROJECT_ID`, `ZONE`, `TF_STATE_BUCKET`, `FRONTEND_HOSTNAME`, `BILLING_ACCOUNT_ID` (environment or a gitignored `.env`) |

## Decisions and their ADRs

Each decision in [PLAN.md](../PLAN.md) section 3 gets an ADR in [adr/](adr/), written by the maintainer in M0 (#5).
`0001` is the template's own record. The index is [adr/README.md](adr/README.md).

| # | Decision | ADR |
|---|---|---|
| D1 | Workload: Online Boutique, forked | [0002](adr/) |
| D2 | Cluster: GKE Standard, zonal, one Spot pool of 3 × e2-standard-2 | [0003](adr/) |
| D3 | Telemetry: Managed Prometheus, Cloud Monitoring SLOs, Cloud Logging, Cloud Trace; Datadog for 14 days | [0004](adr/) |
| D4 | CUJ SLI method: fault-injection discovery | [0005](adr/) |
| D5 | Delivery: Argo CD, Artifact Registry, Terraform from GitHub Actions with Workload Identity Federation | [0006](adr/) |
| D6 | Paging and incident channel: PagerDuty free plan, Slack free workspace | [0007](adr/) |
| D7 | Chaos: Istio fault injection, Chaos Mesh, real Spot preemptions | [0008](adr/) |
| D8 | Agent model and auth: Claude on Vertex AI (global endpoint) with Workload Identity | [0009](adr/) |
| D9 | Agent action safety: privilege separation; a Go executor runs approved, allow-listed actions | [0010](adr/) |
| D10 | Policy engine: OPA Gatekeeper | [0011](adr/) |
| D11 | Frontend exposure: public subdomain behind a load balancer with Cloud Armor | [0012](adr/) |
| D12 | Spend guard: budget alerts; scale the node pool to zero at 100 % | [0013](adr/) |
| D13 | Agent code lives in this repo as `agent/` | [0014](adr/) |
