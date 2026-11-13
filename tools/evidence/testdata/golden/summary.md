# Evidence bundle

Generated 2026-11-16T00:20:00Z from commit `0123456789ab` (refs/heads/main) of `codewithmuh/mamori-sre-gke` by [evidence run 9100000005](https://github.com/codewithmuh/mamori-sre-gke/actions/runs/9100000005).
Schema `mamori.evidence/v1`. A lab with synthetic traffic: these numbers describe the lab, not a production service.

**9 of 9 areas collected.** Needs a look: CI on main, Vulnerabilities and misconfigurations (Trivy), Terraform drift (infra/envs/lab), Gatekeeper audit, Argo CD applications.

| Area | Status | Needs a look | Summary |
|---|---|---|---|
| CI on main | collected | yes | 4 workflows on main, 1 not passing (terraform-apply: failure) |
| Vulnerabilities and misconfigurations (Trivy) | collected | yes | 3 vulnerabilities, 3 misconfigurations, 0 secrets; 1 finding would fail CI's scan |
| Secrets in Git history (gitleaks) | collected | no | no secrets found in the Git history |
| Software bill of materials (syft) | collected | no | SPDX-2.3, 5 packages |
| Terraform drift (infra/envs/lab) | collected | yes | 1 change to review: 0 create, 1 update, 0 replace, 0 delete; 1 object changed outside Terraform |
| Cluster versions and nodes | collected | no | RUNNING, control plane 1.37.2-gke.1100000, node pools 1.37.2-gke.1100000, 3/3 nodes Ready, channel REGULAR |
| Gatekeeper audit | collected | yes | 2 constraints, 2 violations (dryrun 2), last audit 2026-11-16T00:15:00Z |
| Argo CD applications | collected | yes | 2/3 applications Synced and Healthy (not: istiod OutOfSync/Healthy) |
| SLOs and error budgets | collected | no | 3 SLOs: 0 exhausted, 1 low (under 25 % left), 1 without data; least budget left: browse availability (20.2 %) |

## CI on main

4 workflows on main, 1 not passing (terraform-apply: failure).

| Workflow | Result | Event | Commit | Finished | Run |
|---|---|---|---|---|---|
| ci (`.github/workflows/ci.yml`) | success | push | `0123456789ab` | 2026-11-15T21:09:31Z | [#42](https://github.com/codewithmuh/mamori-sre-gke/actions/runs/9100000004) |
| evidence (`.github/workflows/evidence.yml`) | success | schedule | `333333333333` | 2026-11-09T00:25:00Z | [#2](https://github.com/codewithmuh/mamori-sre-gke/actions/runs/9100000000) |
| release (`.github/workflows/release.yml`) | skipped | push | `0123456789ab` | 2026-11-15T21:00:20Z | [#12](https://github.com/codewithmuh/mamori-sre-gke/actions/runs/9000000009) |
| terraform-apply (`.github/workflows/terraform-apply.yml`) | failure | workflow_dispatch | `222222222222` | 2026-11-14T09:20:00Z | [#7](https://github.com/codewithmuh/mamori-sre-gke/actions/runs/9100000001) |

## Vulnerabilities and misconfigurations (Trivy)

3 vulnerabilities, 3 misconfigurations, 0 secrets; 1 finding would fail CI's scan.

Trivy 0.76.1, scanned 2026-11-16T00:19:12.217454586Z.

| Kind | Critical | High | Medium | Low | Unknown |
|---|---|---|---|---|---|
| Vulnerabilities | 1 | 1 | 0 | 0 | 1 |
| Misconfigurations | 0 | 0 | 1 | 2 | 0 |
| Secrets | 0 | 0 | 0 | 0 | 0 |

HIGH and CRITICAL findings:

| Severity | Kind | ID | Where | Package | Fixed in | Title |
|---|---|---|---|---|---|---|
| CRITICAL | vulnerability | TEST-0002 | tools/spend-guard/go.mod | golang.org/x/example v0.1.0 | - | Test entry: critical without a fix |
| HIGH | vulnerability | TEST-0001 | tools/spend-guard/go.mod | golang.org/x/example v0.1.0 | 0.1.1 | Test entry: fixable high vulnerability |

## Secrets in Git history (gitleaks)

No secrets found in the Git history.

## Software bill of materials (syft)

SPDX-2.3, 5 packages.

Document `codewithmuh/mamori-sre-gke`, created 2026-11-16T00:19:40Z by Organization: Anchore, Inc, Tool: syft-1.55.0. Packages by type: github 2, golang 2, other 1.
The full SBOM is `inputs/sbom.spdx.json`.

## Terraform drift (infra/envs/lab)

1 change to review: 0 create, 1 update, 0 replace, 0 delete; 1 object changed outside Terraform.

`terraform plan -detailed-exitcode` in `infra/envs/lab` exited 2 (result: changes).

| Resource | Action |
|---|---|
| `google_compute_security_policy.frontend` | update |

Changed outside Terraform since the last apply:

| Resource | Change |
|---|---|
| `google_compute_security_policy.frontend` | update |

## Cluster versions and nodes

RUNNING, control plane 1.37.2-gke.1100000, node pools 1.37.2-gke.1100000, 3/3 nodes Ready, channel REGULAR.

Cluster `mamori` in asia-northeast1-a.

| Node pool | Version | Status | Machine type | Spot | Autoscaling |
|---|---|---|---|---|---|
| spot-pool | 1.37.2-gke.1100000 | RUNNING | e2-standard-2 | yes | no |

| Node | Pool | Kubelet | Ready | Spot | Created | OS image |
|---|---|---|---|---|---|---|
| gke-mamori-spot-pool-1a2b3c4d-abcd | spot-pool | v1.37.2-gke.1100000 | yes | yes | 2026-11-15T03:12:44Z | Container-Optimized OS from Google |
| gke-mamori-spot-pool-1a2b3c4d-efgh | spot-pool | v1.37.2-gke.1100000 | yes | yes | 2026-11-13T22:01:09Z | Container-Optimized OS from Google |
| gke-mamori-spot-pool-1a2b3c4d-ijkl | spot-pool | v1.37.2-gke.1100000 | yes | yes | 2026-11-16T00:02:51Z | Container-Optimized OS from Google |

## Gatekeeper audit

2 constraints, 2 violations (dryrun 2), last audit 2026-11-16T00:15:00Z.

| Constraint | Control | Action | Violations | Last audit |
|---|---|---|---|---|
| K8sNoLoadBalancer/no-loadbalancer | - | deny | 0 | 2026-11-16T00:15:00Z |
| K8sRequiredProbes/pr-02-probes | PR-02 | dryrun | 2 | 2026-11-16T00:15:00Z |

Violations stored by the audit (Gatekeeper keeps a limited number per constraint; the counts above cover all):

| Constraint | Object | Message |
|---|---|---|
| K8sRequiredProbes/pr-02-probes | Deployment boutique/loadgenerator | container <main> has no readinessProbe \| add one |
| K8sRequiredProbes/pr-02-probes | Deployment boutique/redis-cart | container <redis> has no livenessProbe |

## Argo CD applications

2/3 applications Synced and Healthy (not: istiod OutOfSync/Healthy).

| Application | Sync | Health | Target | Synced revision | Last operation |
|---|---|---|---|---|---|
| argocd/istiod | OutOfSync | Healthy | 1.31.1 | 1.31.1 | Failed |
| argocd/online-boutique | Synced | Healthy | main | 0123456789ab | Succeeded |
| argocd/root | Synced | Healthy | main | 0123456789ab | Succeeded |

## SLOs and error budgets

3 SLOs: 0 exhausted, 1 low (under 25 % left), 1 without data; least budget left: browse availability (20.2 %).

| Service | SLO | Goal | Window | Compliance | Budget left | State |
|---|---|---|---|---|---|---|
| CUJ browse | browse availability | 99.5 % | rolling 28d | 99.6 % | 20.2 % | low |
| CUJ browse | browse latency | 95.0 % | rolling 28d | no data | no data | no data |
| CUJ checkout | checkout availability | 99.0 % | rolling 28d | 99.9 % | 87.0 % | ok |

## Inputs

Copies of these files are in `inputs/`; `SHA256SUMS` covers every file of the bundle.

| File | Status | SHA-256 | Bytes |
|---|---|---|---|
| `ci-runs.json` | collected | f08c4459e976d4c71c47c5b3ebeb758ab87f5d82e94b92e79730786ed27424cb | 2977 |
| `trivy.json` | collected | 867673cde8f027e9c10b97a0075949d7a142af48f416b974b2c96e6a0cbc62d2 | 2033 |
| `gitleaks.json` | collected | 37517e5f3dc66819f61f5a7bb8ace1921282415f10551d2defa5c3eb0985b570 | 3 |
| `sbom.spdx.json` | collected | b17c52cc9db0ab56b059f6aa12ce9b68f2346f229fbcd614ed83c11c291bef52 | 5174 |
| `terraform-drift.json` | collected | fdd6bf350f1bee50507d82c06df276b1f1ec85c9b997fb5a7bcd8a3e146a1355 | 362 |
| `gke-cluster.json` | collected | cdd65b8a7bd15371161f9273b1d9790d9a5773d4faf4d056cf3b22bc989d2572 | 398 |
| `nodes.json` | collected | c0899e8b5ba91b96477d7c924d06522de902bf2aed6719ace6335d47dc1ba7cd | 1442 |
| `gatekeeper-constraints.json` | collected | dd56fabb874a4df416a497647d2feb5c716d7d1436925afb8c1099a2a885b5c5 | 1159 |
| `argocd-applications.json` | collected | 7b1f2745e0a44522f13a1ff594da79a797faaeaed0e045f6b11fdcab2f22dac6 | 1113 |
| `slo-status.json` | collected | fa9f88d8b26a7d560600804901a96a7c83ad631c8dfb6ff7a877ffbe2a90fdef | 1001 |

How to check this bundle: [docs/compliance/evidence-bundle.md](https://github.com/codewithmuh/mamori-sre-gke/blob/main/docs/compliance/evidence-bundle.md).
