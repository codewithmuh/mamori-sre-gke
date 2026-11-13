# Case study: running a microservices shop on GKE under user-journey SLOs

Status: **skeleton**, 2026-11-13 (#31). Nothing has run yet. Every number is "TBD (measured)" and stays that way until
it comes from the lab, with the date range and the source it was measured from. The project is done when this page is
filled from operation, not when the code is complete ([PLAN.md](../PLAN.md) section 1).

**Read every number as a lab number:** one zonal GKE cluster on Spot VMs, synthetic traffic from a load generator and
a prober, faults injected on purpose. Nothing here is a production claim.

## Rules for filling this page

- A number goes in only with its **source** (a dashboard, a Cloud Monitoring query, a postmortem, an evidence
  bundle, `EVALS.md`, a bill) and its **date range**.
- Estimates stay labelled as estimates; a measured number replaces an estimate, never sits beside it unlabelled.
- When a target was missed, say so and say why. A missed SLO with a good postmortem is better evidence than a
  suspiciously perfect one.
- The weekly [evidence bundles](compliance/evidence-bundle.md) of the operated weeks are attached to the `v1.0`
  release, so the numbers here stay checkable after GitHub's 90-day artifact retention.

## At a glance

| | Value | Source |
|---|---|---|
| Weeks operated (cluster always on, steady load) | TBD (measured) | evidence bundles, Cloud Monitoring |
| Journeys under SLOs | TBD (measured) | `slo/slos.yaml`, evidence bundles |
| Journeys that met their SLO over the full window | TBD (measured) | Cloud Monitoring SLO reports |
| Game days run / postmortems written / follow-ups closed | TBD (measured) | [docs/incidents/](incidents/) |
| Agent top-1 / top-3 root-cause accuracy | TBD (measured) | [EVALS.md](../EVALS.md) |
| Unsafe agent actions executed | TBD (measured; must be 0) | [EVALS.md](../EVALS.md), executor audit log |
| Highest load that met the SLOs | TBD (measured) | [docs/capacity/](capacity/) |
| Total spend for the plan | TBD (measured) | Cloud Billing |

## 1. Problem

What production SRE work on Google Cloud asks for, and the lesson this project started from: an earlier project had
SLOs, alerts, runbooks and drills that were designed and statically tested but never operated. Here, operated
evidence is the goal. ([PLAN.md](../PLAN.md) section 1.)

- The workload: Online Boutique v0.10.7, 10 application services talking gRPC, written by others (D1).
- The constraints: one person, a $300 trial credit, about 7 weeks of operation (estimate), a public frontend.
- What "good" was meant to look like: TBD (written from the PRD's success measures once the results are in).

## 2. Approach

- SLOs for user journeys, not services: journeys found by fault injection (D4), SLOs generated from one spec.
- Everything as code and keyless: Terraform, Argo CD, Workload Identity Federation.
- Break it on purpose: game days with Istio faults, Chaos Mesh and real Spot preemptions; a blameless postmortem for
  each.
- An AI incident agent that can only propose; a separate executor acts after a human approves (D9).
- Hardening and capacity work driven by what the incidents showed, with before/after numbers.
- Governance: readiness rules as Gatekeeper policies, a weekly signed evidence bundle, a risk register.

Decisions and their reasons: [ADRs](adr/README.md).

## 3. Architecture

Short version of [design.md](design.md): request path, telemetry and alert path, change paths, failure domains
accepted for a lab. Diagram: TBD (from the final design).

| Item | Planned | As operated |
|---|---|---|
| Cluster | GKE Standard, zonal, `asia-northeast1-a` | TBD (measured) |
| Nodes | 3 × e2-standard-2 Spot | TBD (measured) |
| Resource requests vs allocatable | below the 70 % line in [deploy/](../deploy/README.md) | TBD (measured) |
| Control-plane and node versions over the period | REGULAR channel | TBD (measured; evidence bundles) |

## 4. SLOs

| Journey | Target (availability / latency) | Critical dependencies found by discovery | Result over the window | Budget spent | Burn-rate alerts (fast / slow) |
|---|---|---|---|---|---|
| browse | TBD (from `slo/slos.yaml`) | TBD (measured; `slo/cuj-map.yaml`) | TBD (measured) | TBD (measured) | TBD (measured) |
| add-to-cart | TBD | TBD (measured) | TBD (measured) | TBD (measured) | TBD (measured) |
| view-cart | TBD | TBD (measured) | TBD (measured) | TBD (measured) | TBD (measured) |
| checkout | TBD | TBD (measured) | TBD (measured) | TBD (measured) | TBD (measured) |

- Window: TBD (dates of the first full 28-day window).
- Targets changed after the baseline, and why: TBD.
- Times the error-budget policy was invoked, and what it stopped: TBD (measured; policy log).
- Prober vs mesh SLIs: where they disagreed and what that showed: TBD (measured).
- Alert quality: alerts and pages per week, share actionable, before and after tuning: TBD (measured).

## 5. Incidents

| Date | Fault or cause | Journeys hit | Severity | Time to detect | Time to mitigate | Budget burned | Postmortem |
|---|---|---|---|---|---|---|---|
| TBD | TBD | TBD | TBD | TBD (measured) | TBD (measured) | TBD (measured) | TBD |

- Game days run: TBD (measured). Unplanned incidents (Spot preemptions, upgrades, spend guard): TBD (measured).
- Follow-ups opened and closed: TBD (measured).
- Pages used against PagerDuty's 100 a month: TBD (measured).
- Human time to root cause in game days (the agent's baseline): TBD (measured).

## 6. Agent evaluation

From [EVALS.md](../EVALS.md): 20–30 injected incidents with known root causes.

| Measure | Value |
|---|---|
| Incidents evaluated | TBD (measured) |
| Detection time (median, p90) | TBD (measured) |
| Top-1 / top-3 root-cause accuracy | TBD (measured) |
| Time to root cause vs the human baseline | TBD (measured) |
| Correct-action rate | TBD (measured) |
| Unsafe actions attempted / blocked / executed | TBD (measured) / TBD (measured) / TBD (measured; must be 0) |
| Cost and time per investigation | TBD (measured) |

Where it failed, and why: TBD.

## 7. Resilience

| Change | Before | After | Source |
|---|---|---|---|
| Timeouts and retry budgets | TBD (measured) | TBD (measured) | [docs/resilience/](resilience/) |
| Outlier detection | TBD (measured) | TBD (measured) | |
| Load shedding | TBD (measured) | TBD (measured) | |
| Non-critical dependencies fail open (fork change) | TBD (measured) | TBD (measured) | |
| HPA and PDBs under a Spot preemption | TBD (measured) | TBD (measured) | |

## 8. Capacity

| Measure | Value | Source |
|---|---|---|
| Highest request rate that met every SLO | TBD (measured) | [docs/capacity/](capacity/) |
| First bottleneck | TBD (measured) | |
| Cost per 1,000 requests per second | TBD (measured) | |
| What it would take to reach a higher target | TBD | |

## 9. Cost

The estimates made before the build are in [PLAN.md](../PLAN.md) section 6; each row compares against them.

| Item | Measured | Against the estimate | Source |
|---|---|---|---|
| Per day while running | TBD (measured) | TBD | Cloud Billing |
| Nodes, load balancer and Cloud Armor, Managed Prometheus, logging | TBD (measured) | TBD | Cloud Billing |
| Claude on Vertex AI for the evaluation | TBD (measured) | TBD | Cloud Billing |
| Whole plan | TBD (measured) | TBD | Cloud Billing |
| Times the spend guard fired | TBD (measured) | — | spend-guard runbook log |

## 10. Lessons

What worked, what didn't, what I would do differently, and what would change for a real production service. TBD,
written after the operated period. Keep it specific: each lesson points to an incident, a number or a decision record.

## Evidence

- Weekly evidence bundles: TBD (links to the `v1.0` release assets).
- Postmortems: [docs/incidents/](incidents/).
- Agent evaluation: [EVALS.md](../EVALS.md).
- Risk register: [compliance/risk-register.md](compliance/risk-register.md).
- Decisions: [adr/](adr/README.md).
