# mamori-sre-gke

This repo runs [Online Boutique](https://github.com/GoogleCloudPlatform/microservices-demo), Google's open-source demo
shop (11 services in its own count, talking gRPC), on a GKE cluster in Tokyo. The shop is the given workload: it was
written by others and is not the work here. The work is the SRE layer around it: Terraform and keyless CI, GitOps,
SLOs built around user journeys, alerts that page a phone, game days with postmortems, and an AI incident agent that
can only act through guardrails and a human approval.

*Mamori* (守り) means protection: keeping something safe.

## Status

**M0 (plan and repo) in progress.** Nothing is deployed yet: there is no cluster, no SLO data and no incident so far.
Work is tracked in the [milestones](https://github.com/codewithmuh/mamori-sre-gke/milestones) and
[issues](https://github.com/codewithmuh/mamori-sre-gke/issues); [PLAN.md](PLAN.md) section 5 has the dates.

## What the project will show

Each item is planned, not done. The project counts as done only when it has run for weeks and its numbers are
measured.

- **Build and tear down from nothing:** `make up` creates the network, cluster, edge and apps; `make down` removes
  them and checks that nothing billable is left. All infrastructure is Terraform; CI has no keys.
- **SLOs for user journeys, not services:** fault injection finds which services each journey depends on; SLOs and
  burn-rate alerts are generated from that map.
- **Incident response:** paging to a phone, severity levels and roles, 5–6 game days, a blameless postmortem for
  each.
- **An AI incident agent with guardrails:** it reads telemetry and proposes a fix; a separate executor with narrow
  permissions runs only allow-listed actions after a human approves. Evaluated on 20–30 injected incidents.
- **Resilience, capacity and governance:** before/after numbers for timeouts, retries and load shedding; the highest
  load that still meets the SLOs; readiness rules as admission policies.

## Architecture sketch

From [PLAN.md](PLAN.md) section 4; [docs/design.md](docs/design.md) has the details.

```
GitHub (monorepo: infra/, deploy/, services/ fork, tools/ in Go, agent/)
  │  PR → plan comment ─ manual apply (lab env)        images → Artifact Registry
  ▼                                                     │
GCP project (asia-northeast1)                           ▼
  VPC ─ GKE Standard (zonal, Spot pool) ◀──── Argo CD (app-of-apps)
          │
          ├─ Istio ── Online Boutique (11 services, gRPC) + Redis
          │           ▲ load: Locust (steady) · CUJ prober in Go (every minute)
          │           ▲ faults: Istio aborts/delays · Chaos Mesh · Spot preemption
          │           ▲ users: mamori.codewithmuh.com ─ load balancer + Cloud Armor rate limits
          │
          ├─ OTel Collector ─▶ Managed Prometheus · Cloud Trace · Cloud Logging
          │                     └▶ Cloud Monitoring SLOs + burn-rate alerts ─┐
          ├─ Datadog Agent (14-day window) ─▶ Datadog SLOs, monitors, synth ─┤
          │                                                                  ▼
          ├─ Gatekeeper (readiness policies) ─▶ weekly evidence bundle   PagerDuty ─▶ phone
          │                                                                  │
          └─ AI incident agent (Python) ◀── alert webhook ───────────────────┘
               │ read-only tools: PromQL, Logging, Trace, kubectl get/describe/logs,
               │ Argo CD history, Istio config, past postmortems
               │ model: Claude on Vertex AI (Workload Identity)
               ▼
             Slack: impact summary, ranked RCA, proposed action ─▶ human approves
               ▼
             Executor (Go, narrow RBAC): rollback · scale within bounds · restart ·
             remove a fault rule. One action per incident, audit-logged, kill switch.
```

## Repository layout

| Path | What it holds | Arrives |
|---|---|---|
| `infra/` | Terraform: `bootstrap/` (state bucket, keyless CI identity) and `envs/lab/` (network, cluster, edge, budget) | M1 |
| `deploy/` | Argo CD applications: Istio, Online Boutique, the public edge | M1 |
| `tools/` | Go tooling: the spend guard first; the prober, SLO generator and executor later | M1 onwards |
| `docs/` | PRD, design, threat model, decision records, runbooks, research | M0 |
| `agent/` | The AI incident agent | M5 |
| `services/` | Forked Online Boutique services, once one needs a code change | later |

## Cost, and what the numbers mean

- **Cost (estimate):** about $150–190 a month while the cluster runs, from list prices on 2026-11-13
  ([PLAN.md](PLAN.md) section 6). Measured numbers replace this in M1.
- **Cost controls:** Spot nodes only; a budget with alerts at 50/90/100 %; at 100 % a small function scales the node
  pool to zero; `make down` with a check for billable leftovers.
- **It is a lab:** one zonal cluster on Spot VMs, synthetic load (a Locust load generator and a prober), and faults
  injected on purpose. Every number this repo reports comes from that lab, not from production traffic.
- **Demo shop:** once it is up at `mamori.codewithmuh.com`, every page carries a "demo shop, no real payments" banner
  and checkout uses the demo's pre-filled fake card. Please don't type real card details.

## Documents

| Document | What it is |
|---|---|
| [PLAN.md](PLAN.md) | Scope, decisions D1–D13, milestones, cost, risks |
| [docs/prd.md](docs/prd.md) | Who it serves, critical user journeys, starting SLO targets, non-goals, success measures |
| [docs/design.md](docs/design.md) | Components, request and telemetry paths, failure domains, security, cost controls, names |
| [docs/threat-model.md](docs/threat-model.md) | Assets, entry points, threats and mitigations |
| [docs/adr/README.md](docs/adr/README.md) | Index of architecture decision records |
| [docs/cost-and-tooling-research.md](docs/cost-and-tooling-research.md) | Prices, free tiers and tool facts behind the plan, with sources |
| [docs/runbooks/README.md](docs/runbooks/README.md) | Runbook index |
| [CONTRIBUTING.md](CONTRIBUTING.md) | How work flows: issue, branch, PR, review |

## License

[MIT](LICENSE) © 2026 Muhammad Rashid ([CodeWithMuh](https://youtube.com/@codewithmuh)).
Online Boutique is © Google LLC under the
[Apache License 2.0](https://github.com/GoogleCloudPlatform/microservices-demo/blob/main/LICENSE).
