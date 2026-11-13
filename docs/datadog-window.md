# The 14-day Datadog window

Status: planned, M3 (#22), 2026-11-13. Nothing here has run yet: no Datadog account exists, and every date and
number below is a plan or an estimate until the window happens.

Datadog's free trial lasts 14 days. Decision D3 ([PLAN.md](../PLAN.md) section 3) keeps Managed Prometheus and
Cloud Monitoring as the lab's telemetry all the time, and uses Datadog for one planned window, built as code
beforehand so all 14 days go to operating it during the game days. Requirement R5 asks for alert counts and the
share that was actionable, before and after tuning; this page is how the window produces those numbers for
Datadog.

## What runs during the window

| Piece | Where it lives | What it does |
|---|---|---|
| Four synthetic tests, one per journey | `infra/envs/datadog/synthetics.tf` | Run each journey every 5 minutes against `https://mamori.codewithmuh.com` from Datadog's Tokyo location on AWS (`aws:ap-northeast-1`), outside Google Cloud. The only signal that sees DNS, the certificate, the load balancer and Cloud Armor from outside |
| Datadog SLOs per journey | `infra/envs/datadog/slo_generated.tf.json`, written by `tools/slo-gen` (#18) | Journey SLOs built on the synthetics, next to the Cloud Monitoring ones |
| Three Kubernetes monitors | `infra/envs/datadog/monitors.tf` | Crash loops and unavailable replicas in `boutique`; fewer ready nodes than the pool size |
| Google Cloud integration | `infra/envs/datadog/gcp_integration.tf` | Load balancer, Cloud Armor and VM metrics, read through a keyless, read-only service account |
| Datadog Agent and Cluster Agent | `deploy/optional/datadog/` (Argo CD app, not under `deploy/apps/`) | Kubernetes metrics from the cluster (kubelet, kubernetes_state_core); Istio sidecar metrics too if the Agent's autodiscovery finds the sidecars |
| Make targets | `mk/datadog.mk` | `datadog-delegate`, `datadog-plan`, `datadog-apply`, `datadog-destroy`, `datadog-on`, `datadog-off` |

**Who pages.** Pages stay with Cloud Monitoring's burn-rate alerts through PagerDuty ([alerting.md](alerting.md),
[oncall.md](oncall.md)). Datadog notifies without paging: its monitors send to `notification_handles` (for example a
Slack channel), or show in Datadog only when that list is empty. Paging from Datadog as well would spend PagerDuty's
100 phone/SMS a month on duplicates. The one candidate for paging is an outside-only failure (DNS, certificate, load
balancer) that nothing else catches; the day-8 review decides that from the log (see [Tuning](#tuning-on-day-8)). If
it does page, it goes through Datadog's PagerDuty integration to the existing service `mamori-shop` with
`@pagerduty-mamori-shop` in the message; no PagerDuty objects are added to the Datadog root.

### The journey tests

Each run is a new shopper with its own session cookie (`shop_session-id=dd-synthetics-<uuid>`), so runs never
share a cart and synthetic carts are easy to spot. The steps are the in-cluster prober's (#16), so the two can be
compared run for run. Routes, form fields and page markers are Online Boutique v0.10.7's own.

| Test (Terraform name) | Steps | Passes when |
|---|---|---|
| `cuj_browse` | home, product page | 200s; home shows "Hot Products" and the "Demo shop, no real payments" banner; product page shows the product and "Add To Cart" |
| `cuj_add_to_cart` | home, add to cart, clean-up | `POST /cart` answers 302 to `/cart` (sent only after cartservice stored the item) |
| `cuj_view_cart` | home, add to cart, cart page, clean-up | cart page shows the item and "Place Order" |
| `cuj_checkout` | home, add to cart, cart page, place order | order page shows "Your order is complete!" |

Every step also fails above 2 s (`synthetics_max_step_ms`); latency itself is the SLOs' job. The clean-up step
(`POST /cart/empty`) may fail without failing the run. One failed run is retried once after 2 s before it counts.
Checkout uses the demo's pre-filled fake card, so every run places a fake order (about 290 a day); the payment and
email services are mocks.

Traffic: about 13 requests every 5 minutes from a handful of Datadog IP addresses, far below Cloud Armor's limit of
600 a minute per client IP (my math).

**Note from #18:** Datadog doesn't allow burn-rate or error-budget alerts on monitor-based SLOs built from
synthetic test monitors, so #18 puts its burn-rate monitors on a metric-based twin SLO per journey. See
`docs/slo/slo-design.md`.

### The Kubernetes monitors

Few, slow to fire, and each with a runbook below. They exist for what the journey SLOs can't say: a service that is
down but non-critical for every journey (so no budget burns), and a cluster that has lost capacity before anything
fails.

| Monitor | Fires when | Why that long |
|---|---|---|
| boutique crash loop | a pod in `boutique` restarted more than 3 times in 15 minutes | back-off gives about 6 restarts in 15 minutes; one OOM kill or flaky start stays quiet |
| boutique replicas unavailable | a Deployment in `boutique` had unavailable replicas for all of the last 15 minutes | a pod moved off a preempted node is normally back within minutes |
| ready nodes below pool size | fewer than 3 ready nodes for all of the last 20 minutes, or no node data for 30 minutes | a Spot preemption is normally replaced within minutes; no data means the Agent, the nodes or the cluster are gone |

### Resource requests (cluster)

| Pod | CPU | Memory request / limit | Count |
|---|---|---|---|
| Agent (DaemonSet), container `agent` | 40m | 192Mi / 512Mi | 3 (one per node) |
| Agent init containers (`init-volume`, `init-config`) | 10m | 32Mi / 128Mi | run once per pod |
| Cluster Agent | 20m | 96Mi / 256Mi | 1 |
| **Total while running** | **140m** | **672Mi** | |

That borrows about a quarter of the ~550m CPU left under the 70 % line ([deploy/README.md](../deploy/README.md)) for
two weeks, and nothing after `make datadog-off`. Logs, APM, process, network and security collection, service
discovery (system-probe), Kubernetes Explorer, remote configuration, the Datadog Operator and the admission webhook
are all off. The admission webhook would also be unreachable on a private cluster: the control plane reaches the
nodes on 443 and 10250 only. Secret and ConfigMap metrics are off too, so the Cluster Agent can't list Secrets
outside its own namespace. Check real use with `kubectl top pods -n datadog` on day 1 and adjust the requests.

## Day by day

The window is lined up with the game days in [incidents/game-days.md](incidents/game-days.md) (#20). Dates are a
plan and move with M2. Days 1–7 run the configuration as written here (the *before* period); day 8 tunes it; days
8–14 are the *after* period, with three game days in each half.

| Day | Date (JST) | Game day | What to do | What to look at |
|---|---|---|---|---|
| −7 to −1 | by Sat 2026-12-05 | | M2 done and the operated period started. This branch merged. `cloudasset.googleapis.com` enabled (bootstrap). Slack channel for Datadog notifications chosen | `terraform validate` and the manifest checks green in CI |
| 0 | Sun 2026-12-06, morning | | Sign up on the **AP1** (Japan) site; the trial clock starts. Create an API key and an application key. `make datadog-delegate`, `make datadog-plan`, `make datadog-apply`, `make datadog-on` | All four synthetics green; 3 hosts reporting; `kubernetes_state.node.by_condition` tagged `kube_cluster_name:mamori`; `gcp.loadbalancing.*` metrics arriving (about 15 minutes) |
| 1 | Sun 2026-12-06 | | Quiet baseline. Fix anything day 0 found (a monitor in "no data", a wrong tag) | `kubectl top pods -n datadog`; Cloud Monitoring's `time_series_billed_for_queries_count` (cost of the integration) |
| 2 | Mon 2026-12-07 | | Quiet baseline; first morning review of the Datadog log below | Any Datadog alert at all on a quiet day is a finding |
| 3 | Tue 2026-12-08 | GD1 `F01-currency-abort` | Run the game day; note the time each Datadog monitor fired next to Cloud Monitoring's page | Expected (hypothesis): all four synthetics fail, because every journey starts at the home page, which converts prices: one incident, several Datadog alerts |
| 4 | Wed 2026-12-09 | | Postmortem for GD1, with the Datadog view in it | |
| 5 | Thu 2026-12-10 | GD2 `F02-productcatalog-latency` | Run the game day | Expected: synthetics stay green (300 ms extra is under the 2 s step limit), only the latency SLOs react; the synthetics' response time graphs should show the step |
| 6 | Fri 2026-12-11 | | Postmortem for GD2 | |
| 7 | Sat 2026-12-12 | GD3 `F03-redis-cart-pod-kill` | Run the game day | Expected: a short blip. The question is whether the retry keeps it quiet (class `T` if it alerts) |
| 8 | Sun 2026-12-13 | | **Tuning day.** Weekly summary of days 1–7; one pull request with the tuning; `make datadog-apply` | The *before* numbers are fixed from here on |
| 9 | Mon 2026-12-14 | | Postmortem for GD3; check the tuning took effect | |
| 10 | Tue 2026-12-15 | GD4 (blind: F04, F05 or F06) | Run the game day | F04: only checkout should fail. F05: the ready-nodes monitor fires only if the node stays away 20 minutes; replicas-unavailable if pods stay Pending 15 minutes. F06: product and cart pages may cross 2 s |
| 11 | Wed 2026-12-16 | | Postmortem for GD4 | |
| 12 | Thu 2026-12-17 | GD5 (blind) | Run the game day | |
| 13 | Fri 2026-12-18 | GD6 (blind) | Run the game day; postmortem for GD5. Export what the case study needs (below) for days 1–13 | |
| 14 | Sat 2026-12-19 | | Postmortem for GD6. Export the last day. Turn everything off by the evening ([below](#turning-it-off)) | The *after* numbers |

Datadog's public pages don't say at what hour day 14 ends (checked 2026-11-13), so the last game day is on day 13
and day 14 is kept for the export and the teardown.

## Turning it on

Prerequisites: the lab is up (`make up`), `PROJECT_ID` and `TF_STATE_BUCKET` are set (`.env`), you are logged in
to gcloud with the project Owner account, and this branch is on `main` (the Argo CD app reads its values from
`main`). The Datadog keys go in your shell only, never in `.env` or a file in the repo:

```sh
export DD_API_KEY=...   # Organization settings > API keys
export DD_APP_KEY=...   # Organization settings > Application keys
# DD_SITE defaults to ap1.datadoghq.com; it must match datadog.site in deploy/optional/datadog/values.yaml

make datadog-delegate   # prints Datadog's delegate account (creates it once per Datadog organisation)
make datadog-plan       # read it: 4 synthetics, 3 monitors, the SLOs, 1 service account, 5 role bindings, 1 integration
make datadog-apply
make datadog-on         # namespace datadog, Secrets datadog-secret and datadog-cluster-agent-token, the Argo CD app
```

`infra/envs/datadog` is applied from a laptop, never by CI: it needs the Datadog keys, and it creates the service
account `datadog-integration` with four read-only roles (Monitoring Viewer, Compute Viewer, Cloud Asset Viewer,
Browser) and lets Datadog's delegate account impersonate it (Service Account Token Creator on that one account).
The CI identity `tf-apply` can't do either, by design. State is in the lab's state bucket under the prefix
`datadog`.

## Measuring

### What is counted

Datadog alerts go into the lab's one alert log, [incidents/alert-log.md](incidents/alert-log.md), with
`Source` = `Datadog` and the monitor's name in "Policy or monitor". Same units and classes as the Cloud Monitoring
rows: an *alert* is one monitor going into Alert (one per group for the Kubernetes monitors), classified `A`
actionable, `D` duplicate, `T` transient, `F` false, `R` wrong route, or `M` missed. Game-day alerts carry the game
day (`GD1`…`GD6`), so every number can be given with and without game days. The log's weekly summary counts the
pager (Cloud Monitoring) only; the Datadog rows feed the summary below.

For each Datadog alert, the note says **when Cloud Monitoring noticed the same incident** (its first alert, or
"never"). That is the comparison the window is for: which signal sees an incident first, and which one only adds
noise.

At the morning review, list the monitors that alerted in the last 24 hours (Monitors > Manage, filter
`tag:app:mamori`, then each monitor's history; `terraform output monitors_url` opens the list) and add a row per
alert.

### Summary

| Measure | Days 1–7 (before) | Days 8–14 (after) | Note |
|---|---|---|---|
| Datadog alerts (all / during game days) | | | |
| Actionable share (`A` ÷ all alerts) | | | with and without game days |
| Alerts per incident | | | aim: 1 |
| Incidents Datadog saw first | | | against Cloud Monitoring |
| Incidents only Datadog saw | | | candidates for an outside-only page |
| Missed incidents (`M`) | | | must not rise after tuning |
| Synthetic runs and failed runs per journey | | | from each test's history |
| Agent CPU and memory (peak) | | | `kubectl top pods -n datadog` |

Every number comes from a lab with synthetic load, over one week each, with three game days in each week; the case
study says so.

### Tuning on day 8

One pull request, citing the log. The knobs are Terraform variables with defaults in
`infra/envs/datadog/variables.tf`, so every change is reviewed and visible in Git:

| Knob | Default (before) | Typical change |
|---|---|---|
| `synthetics_retry_count` | 1 | 2, if single failed runs (`T`) dominate |
| `synthetics_min_failure_duration` | 0 s | 600 s (two failing runs in a row), if blips still alert |
| `synthetics_max_step_ms` | 2000 ms | raise if slow-but-working pages alert (`F`/`R`); lower if slow incidents were missed |
| `synthetics_tick_every` | 300 s | 60 s for the journey that should page, if any |
| `notification_handles` | `[]` | add `@pagerduty-mamori-shop` to the one test that catches outside-only failures, if the log shows any |
| Journey steps (`local.cuj_steps`) | every journey starts at home | start add-to-cart, view-cart and checkout at a product page, if one home-page failure keeps producing four alerts (`D`) |
| Monitor windows (`monitors.tf`) | 15 / 15 / 20 minutes | longer if Spot preemptions alert (`T`) |

Never snooze or delete a monitor to improve the numbers; a mute is logged with its reason and an end time.

### What to export before the end

Datadog keeps nothing for us after the trial (see below), so export on day 13 and day 14:

- each synthetic test's run history (pass/fail and step timings) for the 14 days;
- each monitor's alert history and the SLO status pages (screenshots or CSV, with the date range in the file name);
- the Datadog rows of the alert log and the summary table above, filled in.

They go next to the case study (M6). No API key, application key or session cookie value goes into the repo.

## Turning it off

In this order, by the evening of day 14 at the latest:

1. **Export** (above).
2. `make datadog-destroy`: deletes the SLOs, monitors, synthetic tests, the Google Cloud integration, the service
   account `datadog-integration` and its role bindings. Done first so no monitor fires about the Agent going away.
3. `make datadog-off`: deletes the Argo CD app (its finalizer removes the Agent DaemonSet, the Cluster Agent and
   everything else the chart made), then the two Secrets and the `datadog` namespace.
4. In Datadog: revoke the API key and the application key.
5. Check:
   - `kubectl get ns datadog` and `kubectl -n argocd get applications.argoproj.io datadog` say NotFound;
   - `gcloud iam service-accounts list --project "$PROJECT_ID"` has no `datadog-integration`;
   - after an hour, Datadog's host list shows no host reporting, and Monitors shows nothing tagged `app:mamori`.
6. Unset `DD_API_KEY` and `DD_APP_KEY` in your shell.

`make down` doesn't know about Datadog. If the lab is torn down during the window, run steps 2 and 3 first (step 3
needs the cluster).

## After the trial

Datadog's pricing page (FAQ, checked 2026-11-13): "When your free trial ends, you can choose to upgrade to a paid
plan or move to the Free tier. On the Free tier, you get up to five Basic hosts with 1-day metric retention, at no
cost." The trial needs no card. The public pages checked don't say what happens to monitors, synthetic tests or
SLOs at the end of the trial, so the plan doesn't depend on it: everything is deleted before day 14 ends, and the
account is left on the Free tier, unused. Nothing in the lab depends on Datadog afterwards.

List prices if the window were ever repeated on a paid plan ([decision-facts.md](decision-facts.md), D3):
infrastructure $15 per host a month (annual) or $18 (on demand); API tests $5 (annual) or $7.20 (on demand) per
10,000 runs. One week of these four tests is about 8,000 test runs (4 × 288 a day × 7, my math); whether Datadog
counts a multistep test run once or per step wasn't checked.

**Google Cloud cost of the integration (estimate).** Cloud Monitoring bills read calls by time series returned:
$0.50 per million after the first million a month per billing account (since 2025-10-02). Datadog polls the
project's metrics; the GKE namespace is turned off (`gcp_disabled_metric_namespaces`) because the Agent already
reports Kubernetes metrics. The number of series per poll is not known before the first day; check
`monitoring.googleapis.com/billing/time_series_billed_for_queries_count` on day 1 and multiply out to 14 days.

## Runbooks for the Kubernetes monitors

These monitors notify; they don't page. Start by asking whether a journey is hurt: a burning SLO or a red synthetic
means the journey's runbook (`docs/runbooks/cuj-<id>.md`) leads, and this one only explains the cause.

### Runbook: boutique crash loop

A pod in `boutique` restarted more than 3 times in 15 minutes.

1. `kubectl -n boutique get pods` and `kubectl -n boutique describe pod <pod>`: last state, exit code, reason
   (`OOMKilled`, `Error`), events.
2. `kubectl -n boutique logs <pod> -c server --previous` for the crash itself (`-c istio-proxy` if the sidecar is the
   one restarting).
3. Recent change? Argo CD history of `online-boutique`, and `gameday status` (a fault may be running on purpose).
4. Act: roll back a bad change through Git (Argo CD self-heals anything edited by hand); for `OOMKilled`, raise the
   memory limit in Git. A non-critical service (see `slo/cuj-map.yaml`) can wait for working hours.
5. Log the alert and its class.

### Runbook: boutique replicas unavailable

A Deployment in `boutique` had unavailable replicas for 15 minutes.

1. `kubectl -n boutique get deploy,pods -o wide`: which pod, which state (`Pending`, `CrashLoopBackOff`,
   `ImagePullBackOff`, not ready).
2. `Pending`: `kubectl -n boutique describe pod <pod>` (events: insufficient CPU or memory, no node). Usually a lost
   node; see the next runbook. `ImagePullBackOff`: check the image reference in Git. Not ready: the readiness probe
   and its dependency.
3. Crash loop: the runbook above.
4. Act on the cause; don't scale up to hide it (the cluster has a fixed size, so extra replicas would stay Pending
   too).
5. Log the alert and its class.

### Runbook: ready nodes below pool size

Fewer than 3 nodes have been ready for 20 minutes, or Datadog has had no node data for 30 minutes.

1. `kubectl get nodes`. Spot preemption: the node is gone or `NotReady`; GKE recreates the VM when Spot capacity
   comes back, which can take longer than usual in a busy zone.
2. `gcloud container node-pools describe spot-pool --cluster mamori --zone "$ZONE"` and the node pool's operations:
   is an upgrade or a repair running?
3. No data, nodes at 0: the spend guard scaled the pool to zero (budget alert). Follow
   [runbooks/spend-guard.md](runbooks/spend-guard.md); `make resume` only after raising the budget.
4. No data, nodes fine: `kubectl -n datadog get pods`; the Agent or Cluster Agent is down.
5. Nothing to fix by hand for a preemption: watch for `Pending` pods (previous runbook) and note how long the node
   took to return. Log the alert and its class.

## Not verified yet

All of this was checked offline only (`terraform validate`, tflint, chart render, kubeconform). Before or on day 0:

- The step assertions against the live shop (the header assertion on `location`, the form body with
  `body_type` plus an explicit `Content-Type` header, the `{{ uuid }}` local variable in a cookie header).
- The metric and tag names in the monitor queries against real data (`kube_cluster_name`, `condition:ready`,
  `status:true`, `pod_name`, `kube_deployment`).
- That the metric namespace id `gke` is accepted (it follows Datadog's metric prefix `gcp.gke.*`; the docs only show
  `pubsub` as an example). If the API rejects it, apply with `gcp_disabled_metric_namespaces = []`.
- Whether Datadog checks the impersonation when the integration is registered (the role bindings are made first
  either way).
- The Agent's real CPU and memory on these nodes.
- Whether the Agent's Istio autodiscovery finds the sidecars, which Istio 1.31 runs as native sidecars (init
  containers). Nothing here depends on Istio metrics in Datadog.

## Sources

Checked 2026-11-13.

- Datadog sites (AP1 is Japan): https://docs.datadoghq.com/getting_started/site/
- Google Cloud integration, service account impersonation (APIs and roles):
  https://docs.datadoghq.com/integrations/google-cloud-platform/
- Terraform provider DataDog/datadog v4.27.0 (2026-10-21; resources `datadog_synthetics_test`, `datadog_monitor`,
  `datadog_integration_gcp_sts`): https://github.com/DataDog/terraform-provider-datadog/releases/tag/v4.27.0
- Helm chart `datadog` 3.264.0 (2026-10-29), Agent 7.84.6 (2026-10-24):
  https://github.com/DataDog/helm-charts/releases/tag/datadog-3.264.0,
  https://github.com/DataDog/datadog-agent/releases/tag/7.84.6
- GKE settings for the chart (`providers.gke.cos`): https://docs.datadoghq.com/containers/kubernetes/distributions/
- Synthetic test local variables (`{{ uuid }}`) and request options: https://docs.datadoghq.com/synthetics/api_tests/http_tests/
- Multistep API tests (up to 10 steps, variables between steps): https://docs.datadoghq.com/synthetics/multistep/
- Synthetics managed locations and IP ranges per site: https://ip-ranges.ap1.datadoghq.com/synthetics.json
- kubernetes_state_core metrics: https://docs.datadoghq.com/integrations/kubernetes_state_core/
- Trial and Free tier: https://www.datadoghq.com/free-datadog-trial/, https://www.datadoghq.com/pricing/
- Cloud Monitoring read API pricing: https://cloud.google.com/stackdriver/pricing
- Online Boutique v0.10.7 frontend (routes, forms, templates):
  https://github.com/GoogleCloudPlatform/microservices-demo/tree/v0.10.7/src/frontend
