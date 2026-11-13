# Threat model

Status: draft, M0 (2026-11-13), for the planned system in [design.md](design.md). Nothing is deployed yet, so every
mitigation below is planned, with the milestone that adds it. Revisit this page when a new entry point or identity
arrives (Datadog and PagerDuty in M3, the agent and executor in M5).

## Assets

| Asset | Why it matters |
|---|---|
| The Google Cloud project and its bill | A runaway or abused resource spends the trial credit; if the credit runs out before the M5 upgrade, the trial closes and stops the lab |
| The CI identity and Terraform state | Whoever can apply Terraform controls the project; state can hold resource details |
| The cluster and the shop's availability | The SLOs, game days and case study all depend on it |
| The codewithmuh.com domain's reputation | A public shop with a card form could be flagged as phishing |
| Telemetry, postmortems and evaluation data | The agent reasons from them, and the case study's numbers come from them |
| Third-party tokens (PagerDuty, Slack, Datadog; M3, M5) | They can page a phone, post in the incident channel, or read telemetry |

## Entry points

| Entry point | Who can reach it |
|---|---|
| Public frontend `mamori.codewithmuh.com` (load balancer, Cloud Armor, Istio ingress gateway) | Anyone on the internet |
| GitHub: pull requests and Actions, which reach Google Cloud through Workload Identity Federation | Anyone can open a PR; only this repo's workflows get a Google Cloud identity |
| Argo CD | Pulls from `main` of this public repo; its UI and API are not exposed (port-forward only) |
| Kubernetes API endpoint | Google Cloud IAM principals with cluster access |
| Pub/Sub topic `budget-alerts` → `spend-guard` function | Cloud Billing publishes; anyone else needs IAM on the topic |
| The agent (M5): alert webhooks in, telemetry and logs read, Slack messages out | Alerting systems; indirectly, anyone whose request ends up in a log line |
| The executor (M5) | Only approved actions from the agent's Slack flow |
| Supply chain: upstream images, Helm charts, GitHub Actions, Go modules | Their publishers |

## Threats and mitigations

| Threat | Mitigation | When |
|---|---|---|
| Request flood or scraping on the public shop burns money and error budget | Cloud Armor per-client-IP rate limit (429); fixed-size node pool caps compute; budget alerts and the spend guard cap the bill | M1 |
| The domain is flagged as phishing, or someone types a real card | "Demo shop, no real payments" banner on every page; checkout keeps the demo's pre-filled fake card; no accounts; upstream's payment service charges nothing | M1 |
| A malicious PR runs code in CI with a Google Cloud identity | Workload Identity Federation accepts only this repo; any PR gets `tf-plan` at most (read-only), and fork and Dependabot PRs skip the plan job; `tf-apply` only from the protected `lab` environment, started by hand from `main` and approved; no keys to steal; Actions pinned to commit SHAs | M1 |
| Terraform state leaks resource details or secrets | Private state bucket with uniform access and public access prevention; no secrets managed in Terraform | M1 |
| An unreviewed change reaches the cluster | Argo CD deploys only what is on `main`; `main` is protected (PR, review, green CI) | M1 |
| Argo CD or another admin UI is exposed | No Service of type `LoadBalancer`; the Gateway routes only the shop's hostname; other UIs through `kubectl port-forward` | M1 |
| A compromised pod reaches Google Cloud or the node | Private nodes; minimal node service account `gke-nodes`; Workload Identity per workload instead of node credentials; upstream containers run as non-root | M1 |
| Privileged chaos tooling widens the blast radius | Chaos Mesh only for game days, in its own namespace; Gatekeeper makes every exception explicit | M3, M6 |
| Someone triggers the spend guard to take the shop down | Only Cloud Billing should publish to `budget-alerts`; the function can only shrink the pool, never create or delete; `make resume` restores it | M1 |
| The spend guard fails and the bill keeps growing | Budget alerts at 50/90/100 % also go to the maintainer; Spot only; `make down` with a leftover check | M1 |
| Prompt injection through logs, alerts or traces makes the agent act | The agent holds no write credentials (D9); the executor runs only allow-listed actions, within bounds, one per incident, after a human approves; audit log and kill switch | M5 |
| The agent sends sensitive data to the model | The shop has no real users or personal data; the model runs on Vertex AI in the same Google Cloud project, reached with Workload Identity, not an API key | M5 |
| A third-party token leaks | Kept out of Git and Terraform state; gitleaks in pre-commit and CI; each token scoped to one use | M3, M5 |
| A poisoned image, chart or action | Every version pinned (images and charts by version, Actions by SHA); Dependabot; Trivy scans | M1 |
| No record of who changed what | Every change goes through a PR; Argo CD sync history; Cloud Audit Logs; the executor's audit log | M1, M5 |

## Accepted risks

| Risk | Why accepted | Review |
|---|---|---|
| The maintainer's own account is owner of the project | One-person lab; bootstrap needs it once | M6 (risk register) |
| A flood from many IPs gets past per-IP rate limits | Lab; the budget and spend guard cap the cost | After M1, with measured traffic |
| Billing data arrives late, so the spend guard acts after some overspend | Budgets work on delayed billing data; the guard limits, not prevents | M1, when the guard is tested (#15) |
