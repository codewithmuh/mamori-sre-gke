# Risk register

Status: first version, 2026-11-13 (#31). Nothing is deployed yet, so no mitigation below has been tested in
operation; "Mitigating" means the control is built or planned, not proven. Review every risk on its date, after any
incident it played a part in, and when the weekly [evidence bundle](evidence-bundle.md) shows something new.

The first ten rows come from [PLAN.md](../../PLAN.md) section 7; the rest were added here, from the
[design](../design.md)'s accepted failure domains, the [threat model](../threat-model.md) and what the M1 build turned
up. Threats with their technical mitigations stay in the threat model; this page tracks what could stop the project,
cost money or mislead someone, and who looks after it.

## How to read it

- **Owner:** this is a one-person lab, so every owner is the maintainer. The column names the role the risk belongs
  to: *service owner* (availability, SLOs), *on-call*, *budget owner*, *security*, *project lead* (scope and
  schedule).
- **Likelihood:** *Low* not expected during the plan; *Medium* could happen once or twice; *High* expected, or
  already happening.
- **Impact:** *Low* absorbed within a day; *Medium* costs days of schedule, a visible SLO breach or a small bill;
  *High* stops the project, costs money beyond the plan, or harms someone outside it.
- **Status:** *Open* (no control yet), *Mitigating* (control built or planned, not yet proven), *Mitigated* (control
  in place and shown to work, with evidence linked), *Accepted* (kept on purpose, with the reason), *Closed* (no
  longer applies).
- **Review by:** the milestone date in [PLAN.md](../../PLAN.md) section 5 when it is next looked at.

## Register

| ID | Risk | Likelihood | Impact | Owner | Mitigation | Status | Review by |
|---|---|---|---|---|---|---|---|
| R01 | **Built, never run** (the secure-file-transfer pattern): the code is finished but the lab never operates long enough to produce measured numbers | Medium | High | Project lead | Run from M1; "done" means measured numbers ([PLAN.md](../../PLAN.md) section 1); small product scope (D1); the case study's numbers stay "TBD (measured)" until they are | Mitigating | 2026-12-05 (start of the operated period) |
| R02 | **Too many projects in flight**: other portfolio work pulls hours away and the plan slips | Medium | Medium | Project lead | Other portfolio projects paused while this runs; reuse secure-file-transfer's SLO, runbook and drill formats instead of writing new ones | Mitigating | 2026-12-19 |
| R03 | **Bill surprise**: a runaway or forgotten resource (an orphaned load balancer, a debug VM, log volume) costs more than planned | Medium | Medium | Budget owner | Budget `mamori-lab` alerts at 50/90/100 %; spend guard scales the node pool to zero at 100 % (D12, [runbook](../runbooks/spend-guard.md)); `make down` removes the Gateway first and ends with `scripts/leftover-check.sh`; Spot only; Terraform drift in the weekly bundle shows resources that changed outside Terraform | Mitigating | 2026-11-25 (spend guard test, #15) |
| R04 | **A trial account can't raise quotas**: a surge upgrade or the capacity test needs more vCPUs than the region allows | Medium | Medium | Service owner | Check the region's `CPUS` and global `CPUS_ALL_REGIONS` quota in M1 (#15); surge of one node fits in 8 vCPU; capacity test (M4) after the paid upgrade | Mitigating | 2026-11-25 |
| R05 | **The public shop looks like phishing** (Safe Browsing could flag codewithmuh.com) **or someone types a real card** | Low | High | Security | "Demo shop, no real payments" banner on every page; checkout keeps the demo's fake card; no accounts; Cloud Armor rate limits (D11) | Mitigating | 2026-11-25 |
| R06 | **PagerDuty's free plan allows 100 phone/SMS pages a month**; a noisy alert uses them up and real pages stop | Medium | Medium | On-call | Count pages weekly; fast burn pages, slow burn only notifies; a noisy alert is an alert-fatigue finding (R5) and gets tuned; push notifications are unlimited | Mitigating | 2026-12-19 |
| R07 | **Datadog's 14-day trial is wasted** on setup instead of operation | Medium | Medium | Project lead | Datadog config written as Terraform first; the account is created only when the Terraform is ready and the game days are scheduled ([datadog-window.md](../datadog-window.md), #22) | Mitigating | 2026-12-05 |
| R08 | **"Online Boutique is a tutorial"**: reviewers dismiss the work because the app is a demo | Medium | Low | Project lead | The README says in its first paragraph that the app is the given and the SRE layer is the work; the fork carries real reliability changes (M4) | Mitigating | 2027-01-23 |
| R09 | **Lab numbers read as production claims** | Medium | Medium | Project lead | Every number in the case study and the evidence bundle says it comes from a lab with synthetic load; the project is always described as a lab | Mitigating | 2027-01-23 |
| R10 | **The AI agent does something unsafe**: prompt injection through logs or alerts, a wrong diagnosis acted on, or a destructive action | Medium | High | Security | Privilege separation (D9): the agent has read-only RBAC and no write credentials; the executor runs only allow-listed actions in `boutique`, within bounds, one per incident, after a human approves in Slack from an allow-list; audit log; kill switch; evaluated on 20–30 injected incidents before any automatic action ([EVALS.md](../../EVALS.md)); unsafe actions executed must be zero | Mitigating | 2027-01-09 |
| R11 | **Single zone**: the cluster, its control plane and every node sit in `asia-northeast1-a`; a zone incident takes everything down | Low | High | Service owner | Accepted for a lab ([design](../design.md), failure domains): multi-zone or multi-region doubles cost and loses the free-tier cluster fee. The SLOs count a zone outage like any other; the postmortem says so. The SLO data lives in Cloud Monitoring, outside the zone | Accepted | 2027-01-23 |
| R12 | **Spot nodes**: Google reclaims nodes at short notice, possibly all three at once, or Spot capacity runs out for a while | High | Medium | Service owner | Accepted as free real incidents (D2) and used in game days (`F05-node-preemption`); PDBs and HPA in M4; preemptions show in node ages in the weekly bundle; fallback if Spot stock-outs persist: an on-demand pool for the length of the outage (about 1.7× the node cost, my math from the design's 40 % Spot discount) | Accepted | 2026-12-19 |
| R13 | **Public control-plane endpoint**: the Kubernetes API accepts connections from any address (IAM and RBAC still apply), so it is exposed to scanning and to any unauthenticated API-server vulnerability | Low | High | Security | Trivy finding `AVD-GCP-0061` ignored inline with the reason; private nodes; no keys anywhere; `master_authorized_networks` can be set to a home address (it then blocks CI's cluster evidence, which is recorded as "not collected"); GKE patches the control plane automatically on the REGULAR channel; the bundle records control-plane versions weekly. Alternative to evaluate: the DNS-based endpoint with IAM-only access | Accepted | 2026-12-05 |
| R14 | **Trial credit exhausted before the M5 upgrade**: the trial closes and stops the lab; Claude on Vertex AI can't be paid from the credit at all | Medium | High | Budget owner | The budget stays below the remaining credit until M5 (D12); the spend guard acts at 100 %; upgrade to a paid account at the start of M5 keeps the unused credit; the credit expires 2027-02-12, after the plan ends; billing data arrives late, so the guard limits spend rather than stopping it instantly | Mitigating | 2026-12-19 |
| R15 | **Supply chain**: a compromised or hijacked container image, Helm chart, GitHub Action, Go module or Terraform provider runs in CI or in the cluster | Low | High | Security | Every version pinned (actions by commit SHA, images by tag or digest, charts and providers by version, provider checksums in lock files); Dependabot with a 14-day cooldown; Trivy and gitleaks in CI; a weekly SBOM (syft) and full scans in the evidence bundle, with signed provenance; `tools/evidence` uses the Go standard library only | Mitigating | 2027-01-23 |
| R16 | **Keyless CI bound to the wrong token claims**: a Workload Identity binding that doesn't match GitHub's tokens either breaks deploys or, if written too loosely, lets other workflows act as a privileged identity | Medium | High | Security | Found in M1 on 2026-11-13: repositories created after 2026-07-15 get GitHub's immutable `sub` format (`repo:<owner>@<id>/<repo>@<id>:...`), which the first `tf-apply` binding didn't match; fixed by binding the immutable prefix and also pinning `repository_id`. Every binding is narrow by construction (`tf-apply`: environment `lab`; `evidence-reader`: own pool, `main`, one workflow file); check the first real runs of `terraform-apply` and `evidence` | Mitigating | 2026-11-25 |
| R17 | **Evaluation leakage**: the agent sees the fault library's expected answers, so its accuracy numbers are inflated | Low | Medium | Project lead | `chaos/faults/` is never baked into the agent's image or mounted; evaluation runs record the agent's inputs; reviewed before `EVALS.md` is published | Mitigating | 2027-01-09 |
| R18 | **One on-call person**: nights and weekends depend on one phone; fatigue or absence delays response | High | Medium | On-call | Time to acknowledge is measured, not promised; game days scheduled in working hours; maintenance window at 03:00 JST; the spend guard and budgets work without a human | Accepted | 2026-12-19 |
| R19 | **The maintainer's own account is project Owner**; a stolen session controls the whole project | Low | High | Security | Needed once for bootstrap; day-to-day changes go through CI identities; 2-step verification on the account (confirm at bootstrap); review whether Owner can be dropped after M6 (from the threat model's accepted risks) | Accepted | 2027-01-23 |
| R20 | **Evidence goes stale or quietly incomplete**: the weekly bundle stops running, or a section is missing and nobody notices | Medium | Low | Service owner | Missing inputs are recorded as "not collected: <reason>", never dropped; the run's job summary shows what needs a look; a failed run shows in the weekly bundle's own "CI on main" section the week after | Mitigating | 2027-01-23 |

## Changes

| Date | Change |
|---|---|
| 2026-11-13 | First version: R01–R10 from PLAN.md section 7, R11–R20 added (#31) |
