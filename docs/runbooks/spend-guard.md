# Runbook: spend guard fired (node pool scaled to zero)

The spend guard is a Cloud Run function (`spend-guard`) behind the `mamori-lab` budget. When the month's cost
reaches the budget (times `THRESHOLD`, default 1.0), it turns node autoscaling off and sets node pool `spot-pool` to
0 nodes. It deletes nothing. Why it exists: decision D12 in [PLAN.md](../../PLAN.md); how it works:
[tools/spend-guard/README.md](../../tools/spend-guard/README.md).

The commands below assume `ZONE` (default `asia-northeast1-a`) and the project are set, as in `.env`.

## How to tell it fired

- **Budget email.** Cloud Billing emails the billing account's admins at 50, 90 and 100 % of the budget. The guard
  acts at 100 % (with the default threshold), so the 100 % email comes around the same time.
- **The shop is down.** The frontend returns 5xx errors from the load balancer, and the availability SLO alerts fire.
- **No nodes.**

  ```bash
  kubectl get nodes                       # "No resources found"
  gcloud container operations list --zone "$ZONE" \
    --filter="operationType=SET_NODE_POOL_SIZE" --sort-by="~startTime" --limit=3
  ```

- **The function's logs.** In Logs Explorer:

  ```text
  resource.type="cloud_run_revision"
  resource.labels.service_name="spend-guard"
  jsonPayload.outcome=("scaled_to_zero" OR "resize_running" OR "failed")
  ```

  Or from a shell:

  ```bash
  gcloud logging read 'resource.type="cloud_run_revision" AND resource.labels.service_name="spend-guard" AND jsonPayload.outcome:*' \
    --freshness=2d --format='table(timestamp, severity, jsonPayload.outcome, jsonPayload.spent_ratio, jsonPayload.message)'
  ```

  `below_threshold` lines several times a day are normal. `spent_ratio` is cost ÷ budget (1.0 = 100 %). A `stale` line
  is an old message the guard skipped on purpose (published over an hour ago, or from an earlier budget month).

## Impact

- **The shop is down for everyone, and the SLOs burn error budget. That is intended:** in this lab, keeping the trial
  credit (and the account) beats availability. Mark the window in the SLO notes as a spend-guard stop, so it isn't read
  as a reliability failure.
- Nodes go away within minutes, but GKE respects PodDisruptionBudgets for up to an hour while draining, so Spot VM
  spend can go on that long. The log then shows `resize_running` instead of `scaled_to_zero`.
- Some things keep costing while the pool is at zero: the load balancer and its static IP, the Cloud Armor policy,
  Cloud NAT, Artifact Registry storage and logs (see PLAN.md section 6 for estimates). The control plane is covered by
  the GKE free tier. `make down` stops all of it.
- The cluster, its config and Argo CD's state are kept; workloads come back when nodes do.

## Confirm it was the guard, and why the money went

1. Find the `scaled_to_zero` (or `resize_running`) line with the query above: it shows `cost`, `budget_amount` and
   `spent_ratio`. If there is no such line, someone or something else scaled the pool (check the operations list and
   the Admin Activity audit logs).
2. Look at what spent the money: Billing → Reports, this project, grouped by SKU. Expected: the Spot nodes, the load
   balancer, Managed Prometheus. Unexpected: a load test left running, a log flood, a forgotten resource.

## Resume

**Decide first.** The month's cost is still over the budget, and budget notifications keep arriving several times a
day. If you resume without changing anything, the guard scales the pool to zero again at the next notification,
within hours. (Old messages can't do this: the guard skips anything published more than an hour ago or dated in an
earlier budget month, so a redelivered message from before the resume or from last month is logged as `stale`.)
Choose one:

- **Raise the budget** (`monthly_budget_usd` in `infra/envs/lab`, through a PR and the normal apply). Until the
  account is upgraded in M5, keep it below the trial credit that is left, or a runaway can still end the trial.
- **Wait for the next month.** The cost resets at the start of the next budget period (the `cost_interval_start` in the
  logs; the 1st of the month, US Pacific time).
- **Stay down** if the spend was a mistake you still need to fix.

Then:

```bash
make resume    # sets spot-pool back to 3 nodes
```

`make resume` does the same as
`gcloud container clusters resize mamori --node-pool spot-pool --num-nodes 3 --zone "$ZONE" --quiet`.
The guard also turned node autoscaling off; in M1 the pool has a fixed size, so there is nothing to undo. If a later
milestone turns autoscaling on, `terraform apply` sets it back.

Check that it is back:

```bash
kubectl get nodes                                   # 3 nodes Ready
kubectl get applications -n argocd                  # Synced, Healthy
curl -sS -o /dev/null -w '%{http_code}\n' "https://$FRONTEND_HOSTNAME/"   # 200
```

## If the guard failed

`outcome="failed"` means it could not act; `retryable` says whether Pub/Sub redelivers the message.

- `retryable=false`: a setup problem that won't fix itself, such as a missing permission (`PermissionDenied`) or a
  wrong cluster or node pool name (`NotFound`). Fix the Terraform (custom role, env vars) and apply.
- `retryable=true` with "still blocked": another operation held the cluster for 7 minutes (an upgrade, or the guard's
  own resize still draining). The message is retried; check `gcloud container operations list --zone "$ZONE"`.
- If spend keeps rising meanwhile, scale down by hand:
  `gcloud container clusters resize mamori --node-pool spot-pool --num-nodes 0 --zone "$ZONE" --quiet`.
  `make down` removes everything billable. Disabling billing on the project is the last resort (Google may delete
  resources).

## Test it safely

Nothing here needs a real overspend: anything published to `budget-alerts` looks like a budget notification to the
guard.

1. **Offline.** Unit tests and a local run with the sample events: see the
   [README](../../tools/spend-guard/README.md#develop-and-test). No Google Cloud access needed.
2. **Dry run in the cloud.** Apply with `spend_guard_dry_run = true`, then publish the over-budget sample. Pub/Sub
   stamps the publish time; the `sed` dates the sample in the current month, or the guard would skip it as stale:

   ```bash
   now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
   gcloud pubsub topics publish budget-alerts \
     --message="$(sed "s/\"costIntervalStart\": \"[^\"]*\"/\"costIntervalStart\": \"$now\"/" tools/spend-guard/testdata/budget-notification.json)" \
     --attribute=schemaVersion=1.0,budgetId=manual-test
   ```

   Expect `outcome="dry_run"` with `planned_steps` in the logs within a minute or two, and no change to the cluster.
   This proves the trigger, the parsing and the read permission.
3. **Real run.** At a time an outage is fine (M1, before the game days): apply with `spend_guard_dry_run = false`,
   publish the same message, and watch: `scaled_to_zero` or `resize_running` in the logs, `kubectl get nodes` empty, the
   shop down. Write down how long the drain took (a measured number for the case study). Then `make resume`. A manual
   test does not fire again by itself: the real notifications that follow are still below the budget.

## Afterwards

Note in the incident log what spent the money, how long the shop was down and whether the budget changed. If the spend
was a surprise, write a short blameless postmortem.
