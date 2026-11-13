# Decision facts for ADRs 0002–0014

Facts for decisions D1–D13 in [PLAN.md](../PLAN.md) section 3. This file is the input for the ADRs; the maintainer
writes each ADR from scratch. It holds no decisions and no ADR text.

- **Per decision:** the question, the option listed in PLAN.md, real alternatives, facts per option, constraints the
  maintainer already fixed (PLAN.md section 8), and open questions for the ADR.
- **Sources:** numbered per section. Every source was checked on 2026-11-13; re-check prices before relying on them.
- **Prices:** list prices in USD, Tokyo (asia-northeast1) where they vary by region. "My math" means computed from the
  listed rates at 730 hours a month. Every cost here is an estimate, not a measurement.
- **The lab:** zonal GKE Standard cluster in asia-northeast1, one Spot node pool of 3 × e2-standard-2 (6 vCPU and
  24 GiB in all), Google Cloud free-trial account until M5 (PLAN.md sections 3, 6 and 8).

| D | Topic | ADR |
|---|---|---|
| D1 | Workload | 0002 |
| D2 | Cluster type | 0003 |
| D3 | Telemetry | 0004 |
| D4 | CUJ SLI method | 0005 |
| D5 | Delivery | 0006 |
| D6 | Paging and incident channel | 0007 |
| D7 | Chaos | 0008 |
| D8 | Agent model and auth | 0009 |
| D9 | Agent action safety | 0010 |
| D10 | Policy engine | 0011 |
| D11 | Frontend exposure | 0012 |
| D12 | Spend guard | 0013 |
| D13 | Where the agent code lives | 0014 |

## D1: Workload (ADR 0002)

**Question:** which application is the fleet of services the SRE layer runs on?

| Option | Cost and size | Limits | For this lab | Operational load | Maturity, version, licence |
|---|---|---|---|---|---|
| Online Boutique, forked (listed in PLAN.md) | The 12 Deployments in the v0.10.7 release manifest request 1,570m CPU and 1,368 MiB in all; loadgenerator 300m/256Mi, redis-cart 70m/200Mi [3] | "a cloud-first microservices demo application"; 11 microservices (10 app services + Locust loadgenerator) in Go, C#, Node.js, Python, Java over gRPC; cart in Redis; payment, shipping and email are mocks [1] | Requests are ≈ 27 % of the 3 nodes' ≈ 5.8 allocatable vCPU, before sidecars and system pods (my math, see D2). Kustomize components include `service-mesh-istio`, `network-policies`, `without-loadgenerator`, `google-cloud-operations`, `non-public-frontend`, `shopping-assistant` [2]; the Istio component labels namespaces for sidecar injection and replaces the frontend-external load balancer with an Istio gateway [4] | Releases in 2026: v0.10.5 (03-11), v0.10.6 (07-13), v0.10.7 (09-18); last push 2026-10-27 [5] | Apache-2.0; v0.10.7 (2026-09-18); not archived [5] |
| OpenTelemetry Demo (Astronomy Shop) | Kubernetes install needs "6 GB of free RAM for the application" [7] | Kubernetes 1.24+, Helm 3.14+, chart `open-telemetry/opentelemetry-demo` [7]; `src/` holds about 30 components, including Kafka, PostgreSQL, flagd, OpenSearch, Jaeger, Grafana, Prometheus [6] | Ships built-in failure flags through flagd, e.g. `paymentFailure`, `cartFailure`, `productCatalogFailure`, `adHighCpu`, `kafkaQueueProblems`, `failedReadinessProbe` [8] | Brings its own telemetry stack alongside the app [6] | Apache-2.0; 3.1.0 (2026-09-18) [6] |
| Own Go services (PLAN.md's main alternative) | Nothing to download; build time instead | Scope set by the maintainer | PLAN.md section 3 estimates 4–6 more weeks before any SRE work | All code owned by the maintainer | n/a |
| Sock Shop (`microservices-demo/microservices-demo`) | n/a | Repository archived (read-only), last push 2023-12-05 [9] | n/a | No new upstream changes while archived [9] | Apache-2.0; archived [9] |

**Already fixed (PLAN.md section 8):** Online Boutique, forked. M1 (issue #8) deploys v0.10.7 from a pinned kustomize
remote base with the Istio component.

**Open questions for the ADR**

- How the fork is kept: a GitHub fork, or a copy under `services/` as the PLAN.md section 4 sketch shows; how upstream
  releases are pulled in.
- Which images are built from the fork and which come from upstream releases.
- How the "demo shop, no real payments" banner is added (a frontend change in the fork, or something else).
- Whether the optional `shopping-assistant` component [2] stays out.

**Sources**

1. https://github.com/GoogleCloudPlatform/microservices-demo/blob/main/README.md (checked 2026-11-13)
2. https://github.com/GoogleCloudPlatform/microservices-demo/tree/main/kustomize/components (checked 2026-11-13)
3. https://github.com/GoogleCloudPlatform/microservices-demo/blob/v0.10.7/release/kubernetes-manifests.yaml (checked 2026-11-13)
4. https://github.com/GoogleCloudPlatform/microservices-demo/blob/v0.10.7/kustomize/components/service-mesh-istio/README.md (checked 2026-11-13)
5. https://github.com/GoogleCloudPlatform/microservices-demo/releases (checked 2026-11-13)
6. https://github.com/open-telemetry/opentelemetry-demo (releases and `src/`) (checked 2026-11-13)
7. https://opentelemetry.io/docs/demo/kubernetes-deployment/ (checked 2026-11-13)
8. https://opentelemetry.io/docs/demo/feature-flags/ (checked 2026-11-13)
9. https://github.com/microservices-demo/microservices-demo (checked 2026-11-13)

## D2: Cluster type (ADR 0003)

**Question:** which GKE mode, topology and node shape runs the lab?

| Option | Cost | Limits | For this lab | Operational load | SLA |
|---|---|---|---|---|---|
| GKE Standard, zonal, one Spot pool of 3 × e2-standard-2 (listed in PLAN.md) | Cluster fee $0.10/h, covered by the GKE free tier ($74.40/month per billing account, one zonal or Autopilot cluster) [1]. e2-standard-2 Spot in Tokyo $0.051584/h [2]; pd-standard $0.052/GiB-month [3]. 3 nodes + 3 × 30 GB ≈ $112.97 + $4.68 ≈ $118/month (my math) | Spot: "no guarantee of availability"; 30 s default notice (15 s for regular Pods, then 15 s for system Pods); reclaim is not covered by PodDisruptionBudgets; no maximum runtime [4]. Spot prices can change up to once a day [2] | Allocatable per node ≈ 1.93 vCPU and ≈ 6.1 GiB, so ≈ 5.8 vCPU and ≈ 18 GiB for 3 nodes before system Pods (my math from the reservation formulas in [5]). The Autopilot workload restrictions [6] don't apply, so privileged DaemonSets (chaos tools, agents) can run. GKE's guidance is to keep at least one standard (non-Spot) pool for system components such as DNS [4] | Node upgrades, surge, repair and sizing are the maintainer's | 99.5 % for the control plane of zonal Standard clusters [1] |
| Autopilot with Spot Pods | Same free tier covers the cluster fee [1]. Tokyo: $0.0571/vCPU-h and $0.0063215/GiB-h; Spot Pods $0.0171 and $0.0018964 [1]. 5 vCPU / 10 GiB of Spot Pods ≈ $76/month; same on regular Pods ≈ $255/month (my math) | Privileged containers blocked unless deployed by a Google Cloud partner; host namespaces blocked; hostPath write blocked (read-only `/var/log`); no SSH to nodes; `NET_ADMIN` dropped unless allowed per cluster [6]. Minimum requests 250m / 512 MiB (50m / 52 MiB on bursting clusters); default 0.5 vCPU / 2 GiB when unset; CPU:memory 1:1 to 1:6.5 [7] | Chaos Mesh's chaos-daemon must run privileged (D7) [8]; Istio CNI is not available on Autopilot [9]. Billing follows Pod resource requests [1], so sidecars and the load generator count | "Google manages your infrastructure configuration, including your nodes" [10] | 99.95 % for the control plane; 99.9 % for Pods in multiple zones [1]; covers Pod compute too [10] |
| GKE Standard, regional | Free tier does not cover regional clusters' fee [1]: $0.10/h ≈ $73/month (my math), plus nodes | Same Spot facts as the zonal option [4] | Same node facts as the zonal option | Same as the zonal option | 99.95 % for the control plane [1] |

**Already fixed (PLAN.md section 8):** GKE Standard, zonal, 3 × e2-standard-2 Spot. M1 (issue #6) builds release channel
REGULAR, zone `asia-northeast1-a` by default, fixed size 3, 30 GB pd-standard disks, private nodes.

**Open questions for the ADR**

- Whether system Pods (DNS and others) stay on Spot or get a small on-demand pool [4], and what that costs.
- What happens when all 3 Spot nodes are reclaimed at once and none come back: an accepted incident, or a fallback?
- The resource budget: Istio sidecars, Argo CD, Managed Prometheus, chaos tools and the agent within ≈ 5.8 vCPU.
- A trial account can't request quota increases [11]: does the Tokyo CPU quota cover 3 nodes plus a surge node?

**Sources**

1. https://cloud.google.com/kubernetes-engine/pricing (checked 2026-11-13)
2. https://cloud.google.com/spot-vms/pricing (checked 2026-11-13)
3. https://cloud.google.com/compute/disks-image-pricing (checked 2026-11-13)
4. https://docs.cloud.google.com/kubernetes-engine/docs/concepts/spot-vms (checked 2026-11-13)
5. https://docs.cloud.google.com/kubernetes-engine/docs/concepts/plan-node-sizes (checked 2026-11-13)
6. https://docs.cloud.google.com/kubernetes-engine/docs/concepts/autopilot-security (checked 2026-11-13)
7. https://docs.cloud.google.com/kubernetes-engine/docs/concepts/autopilot-resource-requests (checked 2026-11-13)
8. https://chaos-mesh.org/docs/faqs/ (checked 2026-11-13)
9. https://istio.io/latest/docs/setup/platform-setup/gke/ (checked 2026-11-13)
10. https://docs.cloud.google.com/kubernetes-engine/docs/concepts/autopilot-overview (checked 2026-11-13)
11. https://docs.cloud.google.com/free/docs/free-cloud-features (checked 2026-11-13)

## D3: Telemetry (ADR 0004)

**Question:** which backends hold metrics, logs, traces and SLOs, and when does Datadog run?

| Option | Cost | Limits | For this lab | Operational load | Maturity, version, licence |
|---|---|---|---|---|---|
| Managed Service for Prometheus + Cloud Monitoring SLOs + Cloud Logging + Cloud Trace, with Datadog in a 14-day window (listed in PLAN.md) | Managed Prometheus $0.06 per million samples (first 50 billion), GKE control-plane metrics included at that rate [1]; ~2,000 series every 30 s ≈ 175 M samples ≈ $10.5/month (my math, series count from PLAN.md section 6). Logging $0.50/GiB after 50 GiB/project/month, 30 days storage included [1]. Trace $0.20 per million spans after 2.5 M [1]. Uptime checks free up to 1 M executions/project [1]. Alerting policies free until at least 2027-09-01, then $0.35 per metric reference per month [1] | SLOs are request-based or windows-based; rolling 1–30 days or calendar; up to 500 SLOs per service; a latency-percentile metric needs a windows-based SLO [4]. Burn-rate alerts use `select_slo_burn_rate`, lookback ≤ 24 h [5]. A request-based `good_total_ratio` takes exactly two of good, bad and total filters [6] | Datadog trial: 14 days, whole platform, no card [2]; the trial page doesn't say what happens after [2]. Datadog free tier: up to 5 hosts, 1-day metric retention, no container monitoring on those hosts [3]. Datadog list prices after that: infrastructure $15/host/month (annual) or $18 (on demand), APM $31/$36, API tests $5/$7.20 per 10k runs, browser tests $12/$18 per 1k runs [3] | Managed backends: no storage, upgrades or retention to run | Terraform: google provider v8.11.0 (2026-11-10) [7], Datadog provider v4.30.0 (2026-11-10) [8] |
| Self-hosted kube-prometheus-stack (with Grafana, Loki) | No list price; runs on the 3 nodes' CPU, memory and disks | Retention and storage sized by the maintainer; SLOs as PromQL rules rather than Cloud Monitoring SLO objects | Competes with the app for ≈ 5.8 vCPU | Storage, upgrades, retention and backups are the maintainer's | Chart 93.2.2 (2026-11-11), Apache-2.0 [9] |
| Grafana Cloud free tier | $0 within limits | 10,000 active series, 50 GB logs, 50 GB traces, 14-day retention, 3 users, 100,000 API synthetic test executions/month [10] | Includes Incident Response & Management for 3 users (see D6) [10] | Managed; agents in-cluster | SaaS |

**Pinned in this repo:** google provider 8.9.0 (`infra/bootstrap`, `infra/envs/lab`, `infra/envs/datadog`); Datadog
provider 4.27.0 (`infra/envs/datadog`); OpenTelemetry Collector 0.164.0 (contrib image in
`deploy/observability/otel-collector.yaml`). The table lists the latest upstream releases.

**Already fixed:** not answered in PLAN.md section 8. M1 (issue #6) creates the cluster with Managed Service for
Prometheus on.

**Open questions for the ADR**

- Where SLOs and burn-rate rules live: Cloud Monitoring SLO objects, PromQL rules, or both (PLAN.md section 4 has the
  generator write both, plus Datadog).
- What runs in Datadog during the 14 days, and how the Datadog Agent fits the resource budget.
- What happens to Datadog monitors and SLOs after day 14, when only the infrastructure free tier is left.
- Log volume and retention needed for postmortems and the case study.

**Sources**

1. https://cloud.google.com/stackdriver/pricing (checked 2026-11-13)
2. https://www.datadoghq.com/free-datadog-trial/ (checked 2026-11-13)
3. https://www.datadoghq.com/pricing/ (checked 2026-11-13)
4. https://docs.cloud.google.com/stackdriver/docs/solutions/slo-monitoring (checked 2026-11-13)
5. https://docs.cloud.google.com/stackdriver/docs/solutions/slo-monitoring/alerting-on-budget-burn-rate (checked 2026-11-13)
6. https://github.com/hashicorp/terraform-provider-google/blob/main/website/docs/r/monitoring_slo.html.markdown (checked 2026-11-13)
7. https://github.com/hashicorp/terraform-provider-google/releases (checked 2026-11-13)
8. https://github.com/DataDog/terraform-provider-datadog/releases (checked 2026-11-13)
9. https://github.com/prometheus-community/helm-charts/releases (checked 2026-11-13)
10. https://grafana.com/pricing/ (checked 2026-11-13)

## D4: CUJ SLI method (ADR 0005)

**Question:** how are SLIs for the critical user journeys (browse, add to cart, checkout) defined and computed?

| Option | What it measures | Cost | Limits | For this lab |
|---|---|---|---|---|
| Fault-injection discovery (listed in PLAN.md, section 4) | Istio aborts one upstream gRPC service at a time while E2E tests run; a failing journey marks that API critical. Availability = product of the critical APIs' success rates; latency = lowest achievement rate | Test runs plus Istio config; no extra service | Istio abort takes `httpStatus` or `grpcStatus`; faults apply at the client-side sidecar, and "timeouts or retries will not be enabled when faults are enabled on the client side" [2]. The Cloud Monitoring request-based ratio takes exactly two filters [3], so a product of several ratios needs a derived series (PLAN.md section 4 lists this as an M2 design task) | Needs the Istio sidecars from D1/D7 and an E2E suite per journey |
| Hand-picked per-service SLOs (PLAN.md's alternative) | Success and latency of each service on its own | Same telemetry as D3 | Says nothing directly about a journey; the workbook calls a dependency critical "if its unavailability means that your service is also unavailable" [1] | 11 services → up to 11 × 2 SLOs (availability and latency; my math) |
| Black-box journey SLIs from a prober | The prober's journey success and latency, end to end | One run a minute = 43,800 runs/month per journey (my math). Cloud Monitoring synthetic monitors $1.20 per 1,000 runs after 100 free; uptime checks free up to 1 M/project [4] | Probers "may miss issues that affect only a subset of users" [1] | PLAN.md section 4 already plans a Go CUJ prober |
| Request SLIs at the load balancer, per journey URL | Edge success and latency of the frontend routes in each journey | Load balancer metrics (D11) | Load balancer SLIs are "closer to the user's experience" than server logs, which "miss requests that fail to reach the backend" [1] | Only covers what the frontend returns |

Common facts: an SLI is "the number of good events divided by the total number of events"; the workbook's SLI menu for
request-driven services is availability, latency and quality [1]. "A critical user journey is a sequence of tasks that
is a core part of a given user's experience" [1]. The workbook's multiwindow, multi-burn-rate table for a 99.9 % SLO
pages at 14.4× over 1 h (5 min short window) and 6× over 6 h (30 min), and tickets at 1× over 3 days (6 h) [5].

**Pinned in this repo:** Istio charts 1.31.4 (`deploy/apps/istio-*.yaml`). The Istio facts above come from the
current reference on istio.io [2].

**Already fixed:** not answered in PLAN.md section 8. PLAN.md section 4 names the journeys: browse (home and product
page), add to cart, checkout, maybe view cart.

**Open questions for the ADR**

- How the product of success rates and the "lowest achievement rate" are expressed in Cloud Monitoring, PromQL and
  Datadog.
- Which traffic counts: load generator, prober, or real visitors through the public frontend (D11).
- What counts as a journey failure in the E2E tests, and how often discovery re-runs in CI.
- SLO targets and windows per journey, and the error-budget policy that uses them.

**Sources**

1. https://sre.google/workbook/implementing-slos/ (checked 2026-11-13)
2. https://istio.io/latest/docs/reference/config/networking/virtual-service/ (checked 2026-11-13)
3. https://github.com/hashicorp/terraform-provider-google/blob/main/website/docs/r/monitoring_slo.html.markdown (checked 2026-11-13)
4. https://cloud.google.com/stackdriver/pricing (checked 2026-11-13)
5. https://sre.google/workbook/alerting-on-slos/ (checked 2026-11-13)

## D5: Delivery (ADR 0006)

**Question:** how do changes reach the cluster, and how does CI reach Google Cloud and report Terraform plans?

Deploying to the cluster:

| Option | Cost | Limits | For this lab | Operational load | Maturity, version, licence |
|---|---|---|---|---|---|
| Argo CD, app-of-apps (listed in PLAN.md) | No licence cost; runs in-cluster | RBAC grants app actions (`get`, `sync`, `rollback`, `action`, ...); resource actions are `action/<group>/<kind>/<name>`; `rollback` "Cannot rollback when auto-sync is enabled" [4] | "Web UI which provides real-time view of application activity" [13]; Helm chart `argo-cd` 10.18.0 (2026-11-11) [3] | In-cluster controller, repo server, UI to run and upgrade | v3.6.1 (2026-11-04); CNCF graduated 2022-12-06; Apache-2.0 [1][2] |
| Flux | No licence cost; runs in-cluster | "The Flux project does not provide a UI of its own" [7] | Separate UIs exist in its ecosystem [7] | In-cluster controllers to run and upgrade | v2.10.0 (2026-10-26); CNCF graduated 2022-11-30; Apache-2.0 [5][6] |
| Cloud Deploy | First active multi-target pipeline per billing account free, $5/month for each more; single-target pipelines free; Cloud Build billed for render and deploy [8] | A pipeline counts as active in a month when a release or rollout is created in it [8] | Nothing to run in the cluster | Managed by Google [8] | Billed per active pipeline [8] |

CI to Google Cloud and plan comments:

| Option | Facts | Version, licence |
|---|---|---|
| GitHub Actions + Workload Identity Federation (listed in PLAN.md) | Methods in order: Direct WIF (preferred by the action's docs), WIF through a service account, service account key; "Always add an Attribute Condition"; job needs `id-token: write`; GitHub's OIDC token lasts 5 min, access tokens 1 h by default [9]. Standard GitHub-hosted runners are free in public repositories [10] | `google-github-actions/auth` v3.0.0 (2025-08-28), Apache-2.0 [9] |
| `actions/github-script` sticky comment (listed in PLAN.md; used by #9) | A workflow step with an authenticated GitHub API client posts the plan as one PR comment, found again by a hidden marker and updated on each push; no extra tool to install | GitHub-maintained action; version is the SHA pinned in `.github/workflows/ci.yml` |
| tfnotify | Posts Terraform plan and apply results as PR comments; tfcmt is a fork of tfnotify v0.7.0, whose code was under MIT [11] | v1.1.14 (2026-09-10), GitHub releases |
| tfcmt | Fork of tfnotify "enhancing tfnotify in many ways including Terraform >= v0.15 support"; GitHub only [11] | v4.14.20 (2026-10-27), MIT [11] |
| dflook/terraform-github-actions | Plan action attaches the plan to the PR as a comment; the apply action fails "if the plan is not found or has changed" [12] | v3.1.3 (2026-11-10); no LICENSE file in the repository [12] |

**Pinned in this repo:** Argo CD chart 10.11.6, which is Argo CD v3.5.6 (`ARGOCD_CHART_VERSION` in `Makefile`);
google provider 8.9.0 (`infra/**/versions.tf`). The tables list the latest upstream releases.

**Already fixed:** not answered in PLAN.md section 8. M1 branches already assume: Argo CD from its Helm chart plus a
root app-of-apps (issue #8); a WIF pool limited to this repository, with `tf-plan` open to any branch and `tf-apply`
only to the GitHub environment `lab` (issue #6).

**Open questions for the ADR**

- Auto-sync and self-heal on or off, given that Argo CD's `rollback` is blocked while auto-sync is on [4] (affects D9).
- Direct WIF or service-account impersonation (M1 uses impersonation).
- Which plan-comment tool, pinned to which version, and who approves applies.
- How images are built, signed and promoted into Artifact Registry.

**Sources**

1. https://github.com/argoproj/argo-cd/releases (checked 2026-11-13)
2. https://www.cncf.io/projects/argo/ (checked 2026-11-13)
3. https://github.com/argoproj/argo-helm/releases (checked 2026-11-13)
4. https://argo-cd.readthedocs.io/en/latest/operator-manual/rbac/ (checked 2026-11-13)
5. https://github.com/fluxcd/flux2/releases (checked 2026-11-13)
6. https://www.cncf.io/projects/flux/ (checked 2026-11-13)
7. https://fluxcd.io/flux/faq/ (checked 2026-11-13)
8. https://cloud.google.com/deploy/pricing (checked 2026-11-13)
9. https://github.com/google-github-actions/auth (checked 2026-11-13)
10. https://docs.github.com/en/billing/concepts/product-billing/github-actions (checked 2026-11-13)
11. https://github.com/suzuki-shunsuke/tfcmt (checked 2026-11-13)
12. https://github.com/dflook/terraform-github-actions (checked 2026-11-13)
13. https://argo-cd.readthedocs.io/en/latest/ (feature list) (checked 2026-11-13)

## D6: Paging and incident channel (ADR 0007)

**Question:** what pages a phone, and where do incident channels and agent approvals happen?

| Option | Cost | Limits | For this lab | Maturity, version, licence |
|---|---|---|---|---|
| PagerDuty Free + Slack Free (listed in PLAN.md) | $0. PagerDuty Professional is $21/user/month (annual) or $25 (monthly) [1] | PagerDuty Free: 5 users, 1 on-call schedule, 1 escalation policy, 100 SMS or phone notifications a month; email and push included; status pages not included [1]. Slack Free: 90 days of history visible, content older than 1 year deleted, up to 10 apps [2][3] | Events API v2: integration key sent as `routing_key`; payload ≤ 512 KB; rate limits are dynamic, retry 429s [4]. Cloud Monitoring's PagerDuty channel uses Events API v1 with a service key; you can't close a Monitoring alert from PagerDuty; all severities arrive as "critical" unless mapped [5]. Alertmanager's `pagerduty_config` uses `routing_key` for Events API v2 [6]. Slack interactive buttons work over Socket Mode without a public Request URL; Socket Mode apps can't be listed in the public Slack Marketplace [7] | SaaS; Alertmanager v0.35.0 (2026-11-09), Apache-2.0 [8] |
| ntfy (ntfy.sh or self-hosted) | ntfy.sh paid plans $5, $10, $20 a month, with 3, 20, 50 phone calls [9] | Anonymous ntfy.sh: 250 messages a day per IP, 5 emails, 0 phone calls [10] | Can be self-hosted [9] | Apache-2.0 and GPLv2; v2.37.2 (2026-11-04) [11] |
| Grafana Cloud IRM (free tier) | $0 within limits | 3 active IRM users a month [12] | The self-hosted Grafana OnCall OSS entered maintenance on 2025-03-11 and was archived on 2026-03-24 [13] | SaaS |

**Pinned in this repo:** PagerDuty Terraform provider 3.37.0 (`infra/envs/oncall`).

**Already fixed (PLAN.md section 8):** PagerDuty free plan for paging; Slack free workspace for incident chat and
approvals.

**Open questions for the ADR**

- How Cloud Monitoring alerts reach PagerDuty: the built-in Events API v1 channel [5], or a webhook or Pub/Sub path to
  Events API v2.
- How the 100 phone/SMS notifications a month are spent, and which alerts only push.
- Slack's 10-app limit: PagerDuty, Cloud Monitoring, Datadog and the agent's approval app.
- Socket Mode or a public HTTPS endpoint for the approval buttons.
- Severity levels and who is on the single schedule.

**Sources**

1. https://www.pagerduty.com/pricing/incident-management/ (checked 2026-11-13)
2. https://slack.com/pricing/free (checked 2026-11-13)
3. https://slack.com/help/articles/7050776459923-Pricing-changes-for-the-Pro-plan-and-updates-to-the-Free-plan (checked 2026-11-13)
4. https://docs.pagerduty.com/developer/events-api-v2-overview (checked 2026-11-13)
5. https://docs.cloud.google.com/monitoring/support/notification-options (checked 2026-11-13)
6. https://prometheus.io/docs/alerting/latest/configuration/ (checked 2026-11-13)
7. https://docs.slack.dev/apis/events-api/using-socket-mode (checked 2026-11-13)
8. https://github.com/prometheus/alertmanager/releases (checked 2026-11-13)
9. https://ntfy.sh/#pricing (checked 2026-11-13)
10. https://ntfy.sh/v1/account (anonymous limits as returned by the API) (checked 2026-11-13)
11. https://github.com/binwiederhier/ntfy/releases (checked 2026-11-13)
12. https://grafana.com/pricing/ (checked 2026-11-13)
13. https://github.com/grafana/oncall (checked 2026-11-13)

## D7: Chaos (ADR 0008)

**Question:** which tools inject faults for CUJ discovery, game days and the agent's evaluation?

| Option | Privileges needed | Fault types | For this lab | Maturity, version, licence |
|---|---|---|---|---|
| Istio fault injection + Chaos Mesh + real Spot preemptions (listed in PLAN.md) | Istio faults are VirtualService config applied at the client-side sidecar [3]. Chaos Mesh's chaos-daemon "must run as a privileged DaemonSet"; its namespace needs the `privileged` Pod Security level [6] | Istio: delays (`fixedDelay`) and aborts (`httpStatus` or `grpcStatus`) for a percentage of requests; client-side timeouts and retries are off while a fault is on [3]. Chaos Mesh kinds include PodChaos, NetworkChaos, StressChaos, IOChaos, TimeChaos, HTTPChaos, KernelChaos, JVMChaos and GCPChaos [13]. Spot: 30 s notice, PDBs not honoured (D2) | Chaos Mesh on containerd needs `chaosDaemon.runtime=containerd` and the socket path [7]. On private GKE nodes, Istio's validation webhook needs port 15017 open from the control plane [9] | Istio 1.32.0 (2026-11-09), CNCF graduated 2023-07-12, Apache-2.0 [1][2]. Chaos Mesh v2.8.8 (2026-11-06), CNCF incubating since 2022-02-16, Apache-2.0 [4][5] |
| LitmusChaos | Network-fault helper Pods run privileged with `hostPID` and `NET_ADMIN`/`SYS_ADMIN` [10]; faults take `CONTAINER_RUNTIME` and `SOCKET_PATH` [11] | Pod (delete, CPU, memory, network, DNS, HTTP) and node (drain, taint, CPU, memory, restart) faults [11] | Privileged helper Pods, so the same Pod Security question as Chaos Mesh [10] | 3.33.0 (2026-10-15), CNCF incubating since 2022-01-11, Apache-2.0 [8][12] |
| Istio fault injection only, plus Spot preemptions and `kubectl` | No privileged components beyond the mesh | HTTP/gRPC delays and aborts [3]; node loss from Spot; Pod deletion by hand | Fewer components on ≈ 5.8 vCPU | As above |

**Pinned in this repo:** Istio charts `base`, `istiod` and `gateway` 1.31.4 (`deploy/apps/istio-*.yaml`); Chaos Mesh
chart and images 2.8.7 (`deploy/apps/chaos-mesh.yaml`, `deploy/chaos-mesh/values.yaml`). The table lists the latest
upstream releases.

**Already fixed:** not answered in PLAN.md section 8. D2 (GKE Standard) allows privileged DaemonSets; D1's Istio
component gives every Online Boutique Pod a sidecar.

**Open questions for the ADR**

- Which fault types game days and the 20–30-incident fault library need beyond Istio delays and aborts.
- The chaos tool's CPU and memory on 3 small nodes.
- Who may create chaos objects (RBAC, namespaces), and a stop switch for running experiments.
- Whether fault definitions live in Git (Argo CD would sync and self-heal them) or are applied by hand.

**Sources**

1. https://github.com/istio/istio/releases (checked 2026-11-13)
2. https://www.cncf.io/projects/istio/ (checked 2026-11-13)
3. https://istio.io/latest/docs/reference/config/networking/virtual-service/ (checked 2026-11-13)
4. https://github.com/chaos-mesh/chaos-mesh/releases (checked 2026-11-13)
5. https://www.cncf.io/projects/chaosmesh/ (checked 2026-11-13)
6. https://chaos-mesh.org/docs/faqs/ (checked 2026-11-13)
7. https://chaos-mesh.org/docs/production-installation-using-helm/ (checked 2026-11-13)
8. https://github.com/litmuschaos/litmus/releases (checked 2026-11-13)
9. https://istio.io/latest/docs/setup/platform-setup/gke/ (checked 2026-11-13)
10. https://github.com/litmuschaos/litmus-go/blob/master/chaoslib/litmus/network-chaos/lib/network-chaos.go (checked 2026-11-13)
11. https://github.com/litmuschaos/chaos-charts/tree/master/faults/kubernetes (checked 2026-11-13)
12. https://www.cncf.io/projects/litmus/ (checked 2026-11-13)
13. https://github.com/chaos-mesh/chaos-mesh/tree/master/api/v1alpha1 (checked 2026-11-13)

## D8: Agent model and auth (ADR 0009)

**Question:** how does the incident agent call Claude, and with which credential?

| Option | Cost (per million tokens, input/output) | Limits | For this lab | Auth |
|---|---|---|---|---|
| Claude on Vertex AI ("Claude Models on Google Cloud (Google managed)"), global endpoint (listed in PLAN.md) | Global: Opus 5.5 $4/$20, Sonnet 5.5 $2/$10, Haiku 5.5 $0.10/$0.50 (prompts up to 100k tokens) [1]. Multi-region and regional endpoints cost 10 % more [2] | Regional endpoints serve Claude Sonnet 4.6 and earlier only; newer models are on global or the `us`/`eu` multi-regions [2]. Not supported there: Files API, Message Batches, Models API, code execution, web fetch, MCP connector, Managed Agents, server-side fallbacks [2]. Quotas set by Google Cloud [2] | The $300 trial credit can't be used for partner generative AI models; trial accounts can't use Marketplace or request quota increases [3] | IAM role with `aiplatform.endpoints.predict`, e.g. `roles/aiplatform.user`; Application Default Credentials [2]. From GKE: Workload Identity Federation for GKE, which must be enabled on Standard clusters; a Kubernetes ServiceAccount can be granted IAM roles directly [8] |
| Claude API direct (Anthropic) | Same list prices: Opus 5.5 $4/$20, Sonnet 5.5 $2/$10, Haiku 5.5 $0.10/$0.50 [4] | Usage tiers with monthly spend caps (Start tier $500); an organization can set its own lower spend limit, and requests fail once it is reached [5] | Billed by Anthropic, outside the Google Cloud bill [4] | API keys (personal or service account, with optional expiry), or Workload Identity Federation: a short-lived token exchanged from an OIDC issuer such as Google Cloud or Kubernetes service accounts [6] |
| Claude Models on Google Cloud (Anthropic managed) | Billed through Google Cloud Marketplace in Claude Consumption Units ($0.01 each) [7] | Setup needs permission to buy in Google Cloud Marketplace; creates a new Anthropic organization; location `global`; its SDK client is in beta [7] | Not reachable on a trial account, which can't use Marketplace [3] | Google credentials through ADC; IAM roles `roles/claude.user` and others [7] |

**Pinned in this repo:** model `claude-opus-5-5` on Vertex AI's global endpoint (`AGENT_PROVIDER=vertex`,
`AGENT_VERTEX_REGION=global`), with `claude-opus-5` as the client-side refusal fallback
(`agent/src/mamori_agent/config.py`, `deploy/agent/deployment.yaml`). Claude Opus 5 on the global endpoint: $5/$25 per
million input/output tokens [1].

**Already fixed (PLAN.md section 8):** Claude through Vertex AI; the account is upgraded to paid at the start of M5.
PLAN.md section 6 estimates ~$20–40 for the evaluation.

**Open questions for the ADR**

- Which model(s) for triage, RCA and summaries, and a cost-per-investigation target.
- Claude quota in a newly upgraded project, and the retry or fallback path on 429s (no server-side fallbacks on
  Vertex AI [2]).
- Whether the direct-API fallback is kept, and how its credential is handled (key or federation [6]).
- Prompt and response logging, and data handling for incident data.

**Sources**

1. https://cloud.google.com/model-garden/partner-models/pricing (checked 2026-11-13)
2. https://platform.claude.com/docs/en/build-with-claude/claude-models-on-google-cloud/google-managed (checked 2026-11-13)
3. https://docs.cloud.google.com/free/docs/free-cloud-features (checked 2026-11-13)
4. https://platform.claude.com/docs/en/about-claude/pricing (checked 2026-11-13)
5. https://platform.claude.com/docs/en/api/rate-limits (checked 2026-11-13)
6. https://platform.claude.com/docs/en/manage-claude/authentication (checked 2026-11-13)
7. https://platform.claude.com/docs/en/build-with-claude/claude-models-on-google-cloud/anthropic-managed (checked 2026-11-13)
8. https://docs.cloud.google.com/kubernetes-engine/docs/how-to/workload-identity (checked 2026-11-13)

## D9: Agent action safety (ADR 0010)

**Question:** how are the agent's proposed write actions limited, approved and run?

| Option | Where the limit is enforced | Facts |
|---|---|---|
| Privilege separation: a read-only agent proposes; a Go executor with its own narrow RBAC runs allow-listed actions after approval (listed in PLAN.md) | Executor code + Kubernetes RBAC on the executor's service account | RBAC grants verbs on resources and subresources (`deployments/scale`, `pods/log`); `resourceNames` can narrow by name, but "You cannot restrict deletecollection or top-level create requests by resource name" [1] |
| Agent with a write-capable kubeconfig and prompt rules (PLAN.md's alternative) | The agent's Role, plus the prompt | RBAC "Permissions are purely additive (there are no 'deny' rules)" [1]: the agent can make any call its Role allows |
| Actions through Argo CD's API under Argo CD RBAC | Argo CD project roles and policies | Argo CD RBAC actions: `get`, `create`, `update`, `delete`, `sync`, `rollback`, `action`, `override`, `invoke`; resource actions such as `action/apps/Deployment/restart`; rollback only to revisions in history, and not while auto-sync is on [2] |
| Read-only agent; humans run every action | People | No write credential outside the humans' own |

Related facts: ValidatingAdmissionPolicy has been stable since Kubernetes v1.30; its CEL rules can compare `object`
with `oldObject` and read the request (including the user), with actions `Deny`, `Warn` and `Audit` [3]. Role
changes are guarded by the `escalate` and `bind` verbs [1].

**Already fixed:** not answered in PLAN.md section 8. PLAN.md section 4 lists the actions (rollback, scale within
bounds, restart, remove a fault rule) and one action per incident, an audit log and a kill switch.

**Open questions for the ADR**

- How "scale within bounds" is enforced, since RBAC on `deployments/scale` can't bound the replica count: executor
  code, an admission policy [3], or both.
- Which rollback path fits D5: a Git revert, Argo CD rollback (blocked with auto-sync [2]), or `kubectl`.
- How the executor checks that a Slack approval is genuine, current and for this exact action.
- The executor's identity, namespaces and verbs, written down per action.

**Sources**

1. https://kubernetes.io/docs/reference/access-authn-authz/rbac/ (checked 2026-11-13)
2. https://argo-cd.readthedocs.io/en/latest/operator-manual/rbac/ (checked 2026-11-13)
3. https://kubernetes.io/docs/reference/access-authn-authz/validating-admission-policy/ (checked 2026-11-13)

## D10: Policy engine (ADR 0011)

**Question:** which admission and audit engine turns the readiness checks into policies and evidence?

| Option | Policy language | Audit and reports | For this lab | Maturity, version, licence |
|---|---|---|---|---|
| OPA Gatekeeper (listed in PLAN.md) | ConstraintTemplates and Constraints executed by OPA [2] | Audit of existing resources; mutation via `Assign`, `AssignMetadata`, `ModifySet`, `AssignImage` [2] | On private GKE nodes the control plane reaches the webhook on port 8443, which the default firewall rule doesn't open [3] | v3.23.1 (2026-08-27), Apache-2.0 [1]; OPA CNCF graduated 2021-01-29 [4] |
| Kyverno | YAML and CEL; CEL policy types `ValidatingPolicy`, `MutatingPolicy`, `GeneratingPolicy`, `DeletingPolicy`, `ImageValidatingPolicy` [7] | Validate, mutate, generate, clean up; image verification; reports in the OpenReport format [7] | Works through admission controls and background scans; complements the built-in admission policies [11] | v1.20.0 (2026-11-12), CNCF graduated 2026-03-16, Apache-2.0 [5][6] |
| Built-in ValidatingAdmissionPolicy | CEL [8] | `validationActions`: `Deny`, `Warn`, `Audit` (failures recorded in the API audit event) [8] | Nothing to install; validation only | Stable since Kubernetes v1.30 [8] |
| GKE Policy Controller | Gatekeeper constraints, with a prebuilt policy library and bundles [9] | Audit and admission control; a Google Cloud console dashboard shows compliance and enforcement status [9] | Installed on clusters connected to a fleet [9]; included in GKE "at no extra cost" [10] | "Based on the open source Open Policy Agent Gatekeeper project" [9] |

**Pinned in this repo:** Gatekeeper chart 3.23.1 (`deploy/apps/gatekeeper.yaml`) and image v3.23.1 by digest
(`deploy/gatekeeper/values.yaml`), the same as the latest upstream release in the table.

**Already fixed:** not answered in PLAN.md section 8. PLAN.md (M6) wants readiness policies from a public
production-readiness checklist and a weekly evidence bundle from CI.

**Open questions for the ADR**

- Which checklist items become policies, and in what mode (deny, warn, audit).
- What output the weekly evidence bundle reads (audit results, reports, audit-log events).
- Webhook failure policy, and its effect on Argo CD syncs and Istio sidecar injection when the webhook is down.
- Firewall rules for webhook ports on private nodes, and the engine's resource cost.

**Sources**

1. https://github.com/open-policy-agent/gatekeeper/releases (checked 2026-11-13)
2. https://open-policy-agent.github.io/gatekeeper/website/docs/ (checked 2026-11-13)
3. https://open-policy-agent.github.io/gatekeeper/website/docs/vendor-specific/ (checked 2026-11-13)
4. https://www.cncf.io/projects/open-policy-agent-opa/ (checked 2026-11-13)
5. https://github.com/kyverno/kyverno/releases (checked 2026-11-13)
6. https://www.cncf.io/projects/kyverno/ (checked 2026-11-13)
7. https://kyverno.io/docs/introduction/ (checked 2026-11-13)
8. https://kubernetes.io/docs/reference/access-authn-authz/validating-admission-policy/ (checked 2026-11-13)
9. https://docs.cloud.google.com/kubernetes-engine/policy-controller/docs/overview (checked 2026-11-13)
10. https://cloud.google.com/kubernetes-engine/pricing (checked 2026-11-13)
11. https://github.com/kyverno/kyverno/blob/main/README.md (checked 2026-11-13)

## D11: Frontend exposure (ADR 0012)

**Question:** how, and when, can people outside the cluster reach the shop's frontend?

| Option | Cost | Limits | For this lab |
|---|---|---|---|
| Public subdomain through a GKE Gateway (global external Application Load Balancer), Cloud Armor rate limits and a Certificate Manager certificate (listed in PLAN.md) | Global forwarding rules: first 5 $0.025/h together (≈ $18.25/month, my math), $0.01/h each after; data processed by the load balancer in Tokyo $0.012/GiB in and out [3]. Static IP attached to a forwarding rule: free; reserved and unused: charged ($0.01/h in the default table) [3]. Cloud Armor Standard: $0.006849315/h per policy (≈ $5/month), $0.001369863/h per rule (≈ $1/month), $0.75 per million requests on global policies [4]. Certificate Manager: first 100 certificates a month free per project [5] | Class `gke-l7-global-external-managed` supports Cloud Armor backend policies, Google-managed certificates with Certificate Manager, HTTP-to-HTTPS redirect and IAP [1]. Health checks need HTTP 200 and don't follow Pod probes; set a HealthCheckPolicy when the path isn't `/` [2]. Rate-limit rules: throttle or ban, deny status 403/404/429/502, keys include client IP, interval 10–3600 s, enforced per region and "approximate" [6] | The pages checked don't say whether rate-limit rules need Cloud Armor Enterprise [4][6]. Outside probes and synthetics can reach the public URL |
| Private, reached with `kubectl port-forward` | $0 for the edge | "Forward one or more local ports to a pod" [7]: only the person running it can reach the shop | No outside probes or synthetics against a public URL |
| Public on demo days only | Same rates as the public option, only while it exists [3][4] | Edge pieces are created and deleted per demo | SLO data from outside covers demo days only |

**Already fixed (PLAN.md section 8):** public at a subdomain of codewithmuh.com, rate-limited. M1 (issue #6) uses the
hostname variable `frontend_hostname` (default `mamori.codewithmuh.com`); DNS stays outside Google Cloud and the records
are added by hand.

**Open questions for the ADR**

- Rate-limit numbers per client IP, and whether the in-cluster load generator and prober go through the edge at all.
- Whether outside synthetics (Datadog, Cloud Monitoring) get an exemption from the rate limit.
- What to do on abuse: a deny-all Armor rule, removing the route, or scaling the frontend to zero.
- Banner and fake-card safeguards for a public shop on the maintainer's domain (PLAN.md section 7).

**Sources**

1. https://docs.cloud.google.com/kubernetes-engine/docs/how-to/gatewayclass-capabilities (checked 2026-11-13)
2. https://docs.cloud.google.com/kubernetes-engine/docs/how-to/configure-gateway-resources (checked 2026-11-13)
3. https://cloud.google.com/vpc/network-pricing (checked 2026-11-13)
4. https://cloud.google.com/armor/pricing (checked 2026-11-13)
5. https://cloud.google.com/certificate-manager/pricing (checked 2026-11-13)
6. https://docs.cloud.google.com/armor/docs/rate-limiting-overview (checked 2026-11-13)
7. https://kubernetes.io/docs/reference/kubectl/generated/kubectl_port-forward/ (checked 2026-11-13)

## D12: Spend guard (ADR 0013)

**Question:** what stops spend when the budget runs out, without losing the lab?

| Option | What happens at the limit | Facts |
|---|---|---|
| Budget alerts at 50/90/100 %, and at 100 % a function scales the node pool to zero (listed in PLAN.md) | Nodes go away; the cluster, its config and the edge stay | Cloud Run functions run as Cloud Run services: free tier 180,000 vCPU-seconds, 360,000 GiB-seconds and 2 million requests a month; builds use Cloud Build and Artifact Registry [5]. Eventarc Standard: events from Pub/Sub sources $0, first 50,000 chargeable events a month free; Pub/Sub billed separately [6]. The zonal cluster fee stays inside the GKE free tier at zero nodes [7] |
| Alerts only (PLAN.md's alternative) | Nothing stops; emails and messages only | "Setting an alerts-only budget doesn't automatically cap Google Cloud or Google Maps Platform usage or spending" [1] |
| Disable billing on the project from a budget message (PLAN.md's alternative) | Billing removed from the project | Google's example "removes Cloud Billing from your project, shutting down all resources"; "Resources might be irretrievably deleted"; it doesn't guarantee spend stays under the budget [3] |

Budget facts for every option: costs reach Cloud Billing with a delay, and a monthly budget keeps tracking for the first
two days of the next month [1]. Credits and discounts can be included or left out; with none selected, the budget tracks
spend before credits [1]. Pub/Sub budget messages arrive "multiple times per day", the first can take several hours,
delivery is at least once and may be out of order; fields include `costAmount`, `budgetAmount`, `currencyCode`,
`costIntervalStart`, and `alertThresholdExceeded`, which is absent until a threshold is crossed [2]. If a trial ends
(credit spent or 90 days), the account closes, resources stop, and after a 30-day grace period they are deleted [4].

**Already fixed (PLAN.md section 8):** budget alerts; scale the node pool to zero at 100 %. PLAN.md section 3 keeps
the budget below the trial credit until M5. M1 (issue #6) sets thresholds 50/90/100 % of actual spend, credits
excluded, default $200, notifications to Pub/Sub topic `budget-alerts`.

**Open questions for the ADR**

- What stays billable at zero nodes (load balancer, Cloud Armor, NAT, logging, Managed Prometheus, Artifact Registry),
  and whether the guard also removes the Gateway.
- The budget amount against the $300 credit, given cost delay and the 2-day carry-over [1].
- How the function handles duplicate and out-of-order messages [2], and how it is tested (dry run, test budget).
- How the lab resumes, and the budget after the M5 upgrade to a paid account.

**Sources**

1. https://docs.cloud.google.com/billing/docs/how-to/budgets (checked 2026-11-13)
2. https://docs.cloud.google.com/billing/docs/how-to/budgets-programmatic-notifications (checked 2026-11-13)
3. https://docs.cloud.google.com/billing/docs/how-to/disable-billing-with-notifications (checked 2026-11-13)
4. https://docs.cloud.google.com/free/docs/free-cloud-features (checked 2026-11-13)
5. https://cloud.google.com/run/pricing (checked 2026-11-13)
6. https://cloud.google.com/eventarc/pricing (checked 2026-11-13)
7. https://cloud.google.com/kubernetes-engine/pricing (checked 2026-11-13)

## D13: Where the agent code lives (ADR 0014)

**Question:** does the agent's code live in this repository or in its own?

| Option | Facts |
|---|---|
| This repository, as `agent/` (listed in PLAN.md) | Workflows can run only when matching paths change; a required check skipped by a path filter stays "Pending" and blocks the merge [1]. CODEOWNERS requests reviews by path [2]. release-please's manifest mode exists for monorepos and makes one combined release PR for all configured packages [3] |
| Its own public repository, deployed from here (PLAN.md's alternative) | Separate CI, releases and issues; standard GitHub-hosted runners are free for public repositories either way [4] |

**Already fixed (PLAN.md section 8):** it moves into this repo as `agent/`. PLAN.md section 9: `ai-sre-investigator` is
not on GitHub yet.

**Open questions for the ADR:** whether git history is kept on import; how Python CI sits next to Go; whether the agent
gets its own version in release-please.

**Sources**

1. https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax (checked 2026-11-13)
2. https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/customizing-your-repository/about-code-owners (checked 2026-11-13)
3. https://github.com/googleapis/release-please/blob/main/docs/manifest-releaser.md (checked 2026-11-13)
4. https://docs.github.com/en/billing/concepts/product-billing/github-actions (checked 2026-11-13)
