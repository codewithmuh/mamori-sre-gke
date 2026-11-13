# Game days

Status: draft plan, M3 (#20), 2026-11-13. No game day has run. Every expected impact below is a hypothesis to test,
and every number is an estimate.

A game day breaks the shop on purpose, at a chosen time, and runs the whole incident process for real: alert, page,
declaration, runbook, mitigation, postmortem. It answers three questions about the lab's reliability work: **do the
alerts catch real journey damage, how fast does the response stop it, and were the pages worth waking up for?** The
method follows the [Principles of Chaos Engineering](https://principlesofchaos.org/): state the steady state, write a
hypothesis, inject a real-world fault, try to disprove the hypothesis, keep the blast radius small. Google's SRE
teams run similar exercises ([Accelerating SREs to On-Call and
Beyond](https://sre.google/sre-book/accelerating-sre-on-call/)).

The faults come from #23's library (`chaos/faults/<id>.yaml`); the `gameday` CLI starts and stops them
(`gameday list | start <id> [--duration] | stop <id> | stop --all | status`). How to install and use the tooling:
[../game-days.md](../game-days.md). This page is the plan.

## Schedule

Six game days in M3, two or three a week, inside the 14-day Datadog window
([../datadog-window.md](../datadog-window.md), planned as Sun 2026-12-06 to Sat 2026-12-19), so each incident is seen
by both Cloud Monitoring and Datadog. GD1–GD3 fall in the baseline week; alert tuning lands on day 8 (Sun
2026-12-13); GD4–GD6 fall in the week after tuning. GD6 is on the Friday, so the trial's last day is spare. Dates
are a plan (estimate) and move with M2.

| GD | Planned date | Fault | Mode | Main question |
|---|---|---|---|---|
| GD1 | Tue 2026-12-08 | `F01-currency-abort` | announced | Does the whole path work: alert, page, phone, Slack, runbook, fix? |
| GD2 | Thu 2026-12-10 | `F02-productcatalog-latency` | announced | Do latency alerts catch a slowdown, and how fast? |
| GD3 | Sat 2026-12-12 | `F03-redis-cart-pod-kill` | announced | Does a blip that heals itself stay quiet, as it should? |
| GD4–GD6 | Tue 2026-12-15, Thu 2026-12-17, Fri 2026-12-18 | `F04-checkout-bad-config`, `F05-node-preemption`, `F06-recommendation-cpu-stress` in random order | blind | How long do detection and diagnosis take when the responder doesn't know what or when? |

**Why blind runs.** With one person, the game master and the responder are the same person, so the responder knows
the fault and the start time. That makes the human "time to root cause" too good, and it is the baseline the agent
evaluation compares against (#26). For GD4–GD6, a small wrapper picks the order and a random start time inside a
two-hour window, and writes its choice to a file opened only afterwards:

```bash
# Blind start: random fault from the remaining ones, random delay up to 2 hours. Read ~/.gameday-blind afterwards.
ids=(F04-checkout-bad-config F05-node-preemption F06-recommendation-cpu-stress)   # remove the ones already run
id=${ids[RANDOM % ${#ids[@]}]}; delay=$((RANDOM % 7200))
echo "$(date -u +%FT%TZ) $id delay=${delay}s" > ~/.gameday-blind
( sleep "$delay" && gameday start "$id" ) >/dev/null 2>&1 &
```

This is only partly blind: by GD6 only one fault is left, and the responder knows the list. Postmortems record
`blind: yes / no`, and the evaluation should use the blind runs' numbers for the human baseline and say how many
there were.

## Before every game day

**Preconditions** (all must hold, or move the date):

- [ ] Every journey the fault may touch has at least twice the planned spend left in its 28-day budget, and the
      error-budget policy allows experiments ([../slo/error-budget-policy.md](../slo/error-budget-policy.md)).
- [ ] No open SEV1 or SEV2. No fault active (`gameday status`, label check in [on-call.md](on-call.md)).
- [ ] 3 nodes `Ready`; every Argo CD application `Synced` and `Healthy`; the prober reporting all four journeys;
      no alert policy snoozed.
- [ ] Fewer than 50 phone/SMS notifications used this month ([on-call.md](on-call.md)).
- [ ] Waking hours, with two free hours after the planned end. No Google Cloud incident in the region
      ([Service Health](https://status.cloud.google.com/)).
- [ ] The fault file read (`chaos/faults/<id>.yaml`: target, duration, expected answer) and the generated CUJ map
      (`slo/cuj-map.yaml`, #17) checked: if the map says a different set of critical services than this page,
      update the hypothesis first. The map is measured; this page is reasoning from the source code.

**Predict on paper** (15 minutes, a small [Wheel of
Misfortune](https://sre.google/sre-book/accelerating-sre-on-call/)): which alerts fire and when, which runbook
steps apply, what the fix is. Put the prediction in the game day's issue; the postmortem compares against it.

**Announce** in `#incidents` with the game-day template ([communication.md](communication.md)).

## During

- **Respond as to a real incident**: acknowledge, declare, follow the runbook, post tagged messages
  ([roles.md](roles.md)). Don't skip steps because it is "only" a game day: the steps are what is being tested.
- **Fix it the way the runbook says.** Removing the fault is a valid mitigation once the responder has found it as
  the cause, not before.
- **Watch the abort criteria** (below). The CLI's `--duration` timer is the safety net: the fault ends on its own.

## After

1. `gameday stop <id>` (if still running); check that nothing labelled `mamori.dev/fault=true` is left; post the
   stop message.
2. Resolve when the journeys have been within target for 15 minutes.
3. Collect the data before it ages out: the game-day timeline (`gameday-runs/<run-id>.json`), Cloud Monitoring
   alert incidents, the PagerDuty incident log, the Slack thread, Argo CD history, Kubernetes events in Cloud
   Logging.
4. Classify every alert that fired in [alert-log.md](alert-log.md).
5. Postmortem within 5 days ([postmortems/](postmortems/README.md)); follow-ups as GitHub issues; fill in the
   results table below.

## Abort criteria (every game day)

Stop the game day at once (`gameday stop --all`) if any of these happen; the specific criteria under each game day
come on top.

- Something breaks that the fault doesn't explain: treat it as a real incident.
- Impact spreads beyond the expected blast radius: another journey, another namespace, the control plane.
- A journey's remaining budget would drop below the error-budget policy's line.
- The fault outlives its duration, or `gameday stop` fails. Then delete what the label query lists:

  ```bash
  kubectl api-resources --verbs=list --namespaced -o name \
    | xargs -n1 kubectl get -n boutique -l mamori.dev/fault=true -o name --ignore-not-found 2>/dev/null \
    | xargs -r kubectl delete -n boutique
  ```

- Phone/SMS notifications used reach 80 this month, a budget alert arrives, or the responder has to leave.

## Roles in a game day

| Role | Who | When |
|---|---|---|
| Game master | The maintainer, or the blind wrapper | Plans, checks preconditions, starts the fault, watches abort criteria |
| Responder (IC, ops, comms, scribe) | The maintainer | From the first alert, exactly as in [roles.md](roles.md) |
| Safety timer | The `gameday` CLI's duration | Ends the fault even if nobody acts |
| AI agent | Not in M3 (it arrives in M5). Re-runs of these faults in M5 belong to the agent evaluation (#26), where the agent is an advisor | — |

## The six game days

Expected impact below is reasoned from Online Boutique v0.10.7's source (`src/frontend/handlers.go`,
`src/checkoutservice/main.go`) and the generated alerts in [../alerting.md](../alerting.md). Two facts from the
source matter throughout:

- **Every page reads the cart.** The home, product and cart pages all call `cartservice` (the header shows the cart
  size), so a cart failure breaks browsing too.
- **Recommendations and ads are non-critical for errors**, not for time. The frontend ignores errors from
  `recommendationservice` and gives `adservice` 100 ms, but waits for recommendations with no timeout.

Fault durations come from each fault file; 10 minutes is assumed below unless stated. Budget numbers are my math
against the 28-day budgets (201 failed minutes at 99.5 %, 403 for checkout at 99.0 %; latency: 5 % of requests).

### GD1: F01-currency-abort

| | |
|---|---|
| **Fault** | Istio returns errors on calls to `currencyservice` (share and duration in `chaos/faults/F01-currency-abort.yaml`) |
| **Hypothesis** | Browse, view-cart and checkout fail; add-to-cart keeps working. The fast-tier availability alerts for the three journeys page within about 5 minutes (checkout about 9). The runbook leads to the fault rule, and removing it brings the journeys back within a minute |
| **Why** | The home and product pages fetch currencies and convert every price; the cart page converts each item; checkout converts prices and shipping. `POST /cart` calls only the product catalog and the cart. (If the prober's add-to-cart journey follows the redirect to `GET /cart`, it fails too: a question for #16) |
| **Expected severity** | SEV1 if most calls fail (three journeys hard down) |
| **Blast radius** | Callers of one host in `boutique`; all shoppers on three journeys; no other namespace |
| **Planned budget** | 10 failed minutes: about 5 % of browse and view-cart, 2.5 % of checkout |
| **Abort if** | add-to-cart fails; errors outside `boutique`; the VirtualService is still there after stop |
| **Measure** | The full path, end to end: alert → PagerDuty → push → Slack → runbook. How many PagerDuty incidents one outage opens (up to 12 expected); time to acknowledge |
| **Feeds** | M4: prices could fall back to USD instead of failing every page ([PLAN.md](../../PLAN.md) section 4) |

### GD2: F02-productcatalog-latency

| | |
|---|---|
| **Fault** | Istio delays calls to `productcatalogservice`. **Run it for 60 minutes** (`--duration 60m`), see below |
| **Hypothesis** | Every journey slows; availability holds unless a delay passes a timeout. The cart page and checkout slow most, because they call the catalog once per cart item, one call after another. Latency alerts are slow by design: with a 95 % target, the fast tier needs two thirds of an hour's requests to be slow, so it pages about 40 minutes into a full slowdown; the medium tier needs about 100 minutes |
| **Why** | The catalog is called by the home page (list), the product page and `POST /cart` (get), the cart page and checkout (get per item), and `recommendationservice` (list) |
| **Expected severity** | SEV2 (journeys slow, not failing) |
| **Blast radius** | All four journeys' latency; every caller of one host |
| **Planned budget** | Latency only: 60 slow minutes is about 3 % of each latency budget. Availability: none expected |
| **Abort if** | availability alerts fire (requests failing, not just slow); impact on namespaces other than `boutique` |
| **Measure** | Whether a 10-minute slowdown would have been caught at all (by the math, no). Time from first slow probe to page. If no latency alert fires within 60 minutes, that is a result: the latency alerting is too slow or the thresholds too loose |

### GD3: F03-redis-cart-pod-kill

| | |
|---|---|
| **Fault** | Chaos Mesh kills the `redis-cart` pod |
| **Hypothesis** | Kubernetes starts a new `redis-cart` pod within a minute or two, with no human action. Until `cartservice` reconnects, every journey fails, browse included (pages read the cart). Every cart is empty afterwards: `redis-cart` keeps data in an `emptyDir`. **No page should fire**: under about 4 failed minutes in an hour stays below the fast tier. At most a slow-tier ticket |
| **Expected severity** | SEV3, or SEV4 if no journey burns noticeably. A postmortem is still required: carts were lost ([severity.md](severity.md)) |
| **Blast radius** | All four journeys, briefly; all (synthetic) carts |
| **Planned budget** | About 2 failed minutes: about 1 % of each 99.5 % journey, 0.5 % of checkout |
| **Abort if** | `redis-cart` not `Running` after 5 minutes; `cartservice` still failing 3 minutes after `redis-cart` is ready (then restart `cartservice`: a finding) |
| **Measure** | Alert actionability: a self-healing blip should not wake anyone. If something pages, which policy, and was it worth it? The mesh SLIs use a 2-minute rate, which can stretch a 2-minute outage into 3–4 bad minutes, close to the fast tier's 5: this game day shows how close. Time to recover with no human action |

### GD4–GD6 (blind, random order)

#### F04-checkout-bad-config

| | |
|---|---|
| **Fault** | A bad configuration change to `checkoutservice` (details in the fault file) |
| **Hypothesis** | Every order fails; the other three journeys are fine, because only `POST /cart/checkout` calls `checkoutservice`. Checkout's fast tier pages after about 9 failed minutes. The runbook's "what changed recently?" step finds the change, and a rollback fixes it |
| **Watch out** | If the change is applied straight to the Deployment that Argo CD manages, Argo CD's self-heal may put the good config back on its own within minutes, and the game day measures Argo CD instead of the responder. Record which happened |
| **Expected severity** | SEV1 (checkout hard down) |
| **Blast radius** | Checkout only; no data lost |
| **Planned budget** | 10 failed minutes: about 2.5 % of checkout |
| **Abort if** | any other journey fails; two rollback attempts fail; other Argo CD applications show sync errors |
| **Measure** | How a change-caused incident is found; time to roll back through Git and Argo CD; whether `kubectl rollout undo` was tempting (it is undone by self-heal) |

#### F05-node-preemption

| | |
|---|---|
| **Fault** | One of the three Spot nodes is preempted, as Google may do at any time with 30 seconds' notice (how it is simulated: the fault file) |
| **Hypothesis** | Pods on that node stop and are rescheduled onto the other two; with a third of the capacity gone some stay `Pending` until the node returns ([../design.md](../design.md), capacity). Journeys whose services ran there fail for a few minutes. Every service has one replica, so the outcome depends on what was on the node: the Istio ingress gateway (every journey), `redis-cart` (carts lost), the prober (the "prober no data" alert pages after 10 minutes). GKE recreates the VM when Spot capacity allows |
| **Expected severity** | SEV1 to SEV4, depending on what ran on the node; set it from the journeys, not the cause |
| **Blast radius** | The largest of the six: up to a third of all pods, including shared parts (gateway, Argo CD, prober, collectors) |
| **Planned budget** | Up to 5 failed minutes per journey (about 2.5 %), more if pods stay `Pending` |
| **Abort if** | a second node is lost; the node doesn't come back within 30 minutes (Spot stock-out: then no more game days until 3 nodes are back); pods `Pending` for more than 15 minutes |
| **Measure** | Recovery time with no human action; which journeys the node took with it; whether the lab noticed its own blindness (prober no data). Baseline for M4's replicas and PodDisruptionBudgets |

#### F06-recommendation-cpu-stress

| | |
|---|---|
| **Fault** | Chaos Mesh CPU stress inside the `recommendationservice` pod. **Run it for 60 minutes**, as GD2 |
| **Hypothesis** | Recommendations slow down. The frontend ignores their errors but waits for them, so the product page, the cart page and the order confirmation page slow too: latency burn on browse, view-cart and checkout, no availability burn. The stress stays inside the pod's CPU limit, so neighbours on the node barely notice (to check) |
| **Expected severity** | SEV2 if a latency alert pages, otherwise SEV3 |
| **Blast radius** | One pod, and its node's CPU |
| **Planned budget** | Latency only: at most about 3 % of the three latency budgets |
| **Abort if** | other pods on the node slow down; the node goes `NotReady`; any availability alert fires |
| **Measure** | Whether a "non-critical" dependency hurts journeys through latency; the runbooks' latency path. Feeds M4: a timeout and fail-open on recommendation calls |

## What to measure

The same for every game day. Times come from telemetry, not memory; postmortems record them in front matter
(`timeline`, see [postmortems/](postmortems/README.md)).

| Measure | Definition | Source |
|---|---|---|
| Fault start (T0) | When the CLI applied the fault | `gameday-runs/<run-id>.json` |
| Impact start | First failed probe or bad minute | `mamori_cuj_probe_total`, mesh SLIs |
| **Time to detect** | First alert opened − T0 | Cloud Monitoring alert incidents |
| Time to acknowledge | Acknowledged − first page | PagerDuty incident log |
| Time to root cause (human) | First `[rca]` message − T0 | Slack thread |
| **Time to mitigate** | Journey SLI back within target, held 5 minutes − T0 | SLI queries in the runbooks |
| **Alert actionability** | Every alert that fired, classified actionable, duplicate, transient or false; missed alerts too | [alert-log.md](alert-log.md) |
| Pages per incident | PagerDuty incidents opened for this one problem (aim: 1) | PagerDuty |
| Phone/SMS used | Calls and texts sent (aim: 0 on a game day) | PagerDuty notification log |
| Budget spent | Per journey: planned against actual | Cloud Monitoring SLO pages |
| Runbook fit | Steps that were wrong, missing or slow | Postmortem |
| Datadog comparison | Datadog's detection time for the same fault | Datadog monitors (#22) |

## Results

Filled in after each game day.

| GD | Date | Fault | Blind | Time to detect | Time to root cause | Time to mitigate | Pages (actionable) | Phone/SMS | Budget (planned / actual) | Postmortem |
|---|---|---|---|---|---|---|---|---|---|---|
| GD1 | | `F01-currency-abort` | no | | | | | | | |
| GD2 | | `F02-productcatalog-latency` | no | | | | | | | |
| GD3 | | `F03-redis-cart-pod-kill` | no | | | | | | | |
| GD4 | | | yes | | | | | | | |
| GD5 | | | yes | | | | | | | |
| GD6 | | | yes | | | | | | | |
