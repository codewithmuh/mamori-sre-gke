# Alerting

Status: draft, M3 (#20), 2026-11-13. Nothing has fired yet. The alert inventory below describes what the SLO
generator (#18) produces; [../slo/generated/](../slo/generated/) and [slo/slo-design.md](slo/slo-design.md) are the
source of truth when they disagree with this page. Every count of what it takes to page is my math, not a
measurement.

## Principles

1. **Alert on what shoppers feel, not on causes.** Every alert is about a critical user journey (CUJ) failing or
   slowing down. A crashed pod, high CPU or a restart loop that no journey notices is a dashboard item, not an
   alert. "What's broken" is the symptom; "why" is the cause, and the cause is found during the response
   ([SRE book, Monitoring Distributed Systems](https://sre.google/sre-book/monitoring-distributed-systems/)).
2. **Alert on error-budget burn, not on raw thresholds.** Each alert asks "is this journey spending its 28-day error
   budget too fast?", using Google's multiwindow, multi-burn-rate alerts
   ([SRE workbook, Alerting on SLOs](https://sre.google/workbook/alerting-on-slos/)). The long window makes sure
   enough budget is at stake to matter; the short window, 1/12 of the long one, makes the alert stop soon after the
   damage stops.
3. **Page only for what needs a person now.** A page must be urgent, actionable and about real or imminent shopper
   pain. What can wait a day is a ticket (email). Everything else is a dashboard. Before adding a paging alert, ask
   the SRE book's questions: does it catch something otherwise unseen that is urgent, actionable and user-visible?
   Could I ever ignore it as harmless? Can I act on it? Is something else already paging for the same problem?
4. **Every alert says what is burning and links its runbook.** The generated policies carry the runbook link
   (`docs/runbooks/cuj-<id>.md`), the SLO, target, window, burn threshold and a query to confirm.
5. **Watch the watcher.** The prober runs inside the cluster it watches. If it stops reporting, a "prober no data"
   alert pages, because a silent prober would otherwise look like a healthy shop. The SLO data itself lives in
   Cloud Monitoring, outside the cluster.
6. **One pager per symptom.** Cloud Monitoring is the only thing that pages. During the 14-day Datadog window (M3),
   Datadog monitors notify by Slack or email, so one incident doesn't page twice from two tools; the window compares
   the two tools' detection times instead.
7. **New paging alerts start as tickets.** Any alert added by hand goes to email for a week first, so its trigger
   rate is known before it can wake anyone ([SRE workbook, On-Call](https://sre.google/workbook/on-call/)).
8. **Tune with evidence.** Every alert is classified in [incidents/alert-log.md](incidents/alert-log.md). Changes to
   thresholds, windows or routing go through a pull request that cites the log. Simply relaxing thresholds is rarely
   the right answer to a page under SLO-based alerting (workbook); removing duplicates and misrouted pages usually
   is.

## Alert path

```
Cloud Monitoring alert policies (generated, #18)
  ├─ page tier (severity=page) ──▶ local.incident_notification_channels (#21)
  │                                 ├─ PagerDuty "mamori-shop" ─▶ push, then a phone call
  │                                 ├─ email "mamori alerts (email)"
  │                                 └─ Pub/Sub topic incident-alerts ─▶ AI agent (from M5)
  └─ ticket tier (severity=ticket) ─▶ local.incident_ticket_notification_channels (#21)
                                      ├─ email
                                      └─ Pub/Sub topic incident-alerts
```

Cloud Monitoring's PagerDuty channel uses PagerDuty's Events API v1 with a service key, and you can't close a
Monitoring alert from PagerDuty ([decision-facts.md](decision-facts.md), D6). The alert closes in Cloud Monitoring,
and that resolves the PagerDuty incident; resolving it in PagerDuty first only makes Monitoring open it again. So:
acknowledge in PagerDuty, fix, let the alert close. The wiring is in [oncall.md](oncall.md) (#21); the on-call
policy is in [incidents/on-call.md](incidents/on-call.md).

## Alert inventory

### SLOs

Each journey has one Cloud Monitoring custom service, `mamori-cuj-<id>`, with four SLOs over a rolling 28 days.
Two signals look at each journey from different sides:

| SLO id | Signal | What it measures | Target |
|---|---|---|---|
| `prober-availability` | Go prober (#16), one run per journey per minute, `source="scheduled"` | Share of probe runs that succeed (request-based) | 99.5 %; checkout 99.0 % |
| `prober-latency` | Prober step durations | Share of steps faster than the threshold (request-based) | 95 % under 500 ms; checkout 1,000 ms |
| `mesh-availability` | Istio sidecar metrics, client side | Share of good 1-minute windows; a minute is good when the product of the journey's critical services' success rates meets the target (windows-based) | 99.5 %; checkout 99.0 % |
| `mesh-latency` | Istio request durations | Share of good 1-minute windows; a minute is good when latency achievement is at least 95 % (windows-based) | see [slo/slo-design.md](slo/slo-design.md) |

The prober enters through the Istio ingress gateway inside the cluster, so it sees the gateway, the frontend and
every service behind it, but not the load balancer, the certificate or Cloud Armor. The mesh SLIs cover all
traffic, including the load generator's, but only the services, not the gateway path end to end.

### Burn-rate tiers

Google's budget fractions, with burn rates recomputed for a 28-day window. The slow tier's long window is 24 hours
instead of the workbook's 3 days because a Cloud Monitoring burn-rate condition can look back at most 24 hours.

| Tier | Condition (both windows) | Budget spent at that rate | Policy severity | Label `severity` | Route |
|---|---|---|---|---|---|
| Fast | burn > 13.44 over 1 h and over 5 min | 2 % in 1 hour | `CRITICAL` | `page` | PagerDuty, email, Pub/Sub |
| Medium | burn > 5.6 over 6 h and over 30 min | 5 % in 6 hours | `CRITICAL` | `page` | PagerDuty, email, Pub/Sub |
| Slow | burn > 2.8 over 24 h and over 2 h | 10 % in 24 hours | `WARNING` | `ticket` | email, Pub/Sub |
| No data | no `mamori_cuj_probe_total{source="scheduled"}` for the journey for 10 minutes | — | `CRITICAL` | `page` | PagerDuty, email, Pub/Sub |

### Policies

4 journeys × 4 SLOs × 3 tiers = **48 burn-rate policies**, plus **4 "prober no data"** policies: **52 in all, 36
of them paging, 16 tickets.**

- **Display name:** `CUJ <id>: <signal> <sli> <tier> burn`, for example `CUJ browse: prober availability fast burn`
  or `CUJ checkout: mesh latency slow burn`; and `CUJ <id>: prober no data`.
- **Terraform:** `google_monitoring_alert_policy.slo_<id_snake>_<signal>_<sli>_<tier>`, for example
  `slo_add_to_cart_mesh_availability_medium`.
- **User labels:** `generator=slo-gen`, `cuj`, `signal` (`prober`, `mesh`), `sli` (`availability`, `latency`),
  `alert` (`burn-rate`, `no-data`), `burn` (`fast`, `medium`, `slow`, `none`), `severity` (`page`, `ticket`).

| Journey | Runbook | Availability target | Latency threshold | Policies |
|---|---|---|---|---|
| `browse` | [runbooks/cuj-browse.md](runbooks/cuj-browse.md) | 99.5 % | 500 ms | 12 burn-rate (8 page, 4 ticket) + no data |
| `add-to-cart` | [runbooks/cuj-add-to-cart.md](runbooks/cuj-add-to-cart.md) | 99.5 % | 500 ms | 12 burn-rate (8 page, 4 ticket) + no data |
| `view-cart` | [runbooks/cuj-view-cart.md](runbooks/cuj-view-cart.md) | 99.5 % | 500 ms | 12 burn-rate (8 page, 4 ticket) + no data |
| `checkout` | [runbooks/cuj-checkout.md](runbooks/cuj-checkout.md) | 99.0 % | 1,000 ms | 12 burn-rate (8 page, 4 ticket) + no data |

### What it takes to fire (my math)

A "bad minute" is a failed probe (one probe a minute) or a bad 1-minute mesh window. Both conditions must hold.

| Tier | 99.5 % journeys | Checkout (99.0 %) |
|---|---|---|
| Fast (page) | 5 bad minutes in the last hour, 1 in the last 5 minutes | 9 in the last hour, 1 in the last 5 minutes |
| Medium (page) | 11 bad minutes in the last 6 hours, 1 in the last 30 minutes | 21 in 6 hours, 2 in the last 30 minutes |
| Slow (ticket) | 21 bad minutes in the last 24 hours, 2 in the last 2 hours | 41 in 24 hours, 4 in the last 2 hours |

Latency SLOs have a 95 % target, so their budget is 5 % of requests and the most they can burn is 20× (everything
slow). The prober latency fast tier therefore needs more than two thirds of an hour's steps to be slow: about 40
minutes into a total slowdown. A 10-minute slowdown pages nobody. Game day GD2 tests whether that is acceptable
([incidents/game-days.md](incidents/game-days.md)).

### Other notifications (not generated, none of them page)

| Source | Route | Runbook |
|---|---|---|
| Budget `mamori-lab` at 50/90/100 %; spend guard | Cloud Billing email | [runbooks/spend-guard.md](runbooks/spend-guard.md) |
| Datadog monitors and synthetics (14-day window, #22) | Slack or email, never PagerDuty | [datadog-window.md](datadog-window.md) |
| Prober health beyond "no data" | Dashboards | [runbooks/prober.md](runbooks/prober.md) |

## Where the noise will probably come from

These are predictions for the first weeks, written down so the alert log can confirm or refute them.

1. **Duplicates.** One cause opens one PagerDuty incident per policy that fires. A currency outage that breaks
   three journeys can fire both signals and both paging tiers on each: up to **12 PagerDuty incidents for one
   problem**. Nothing in the current path merges them. Expected: pages per incident well above 1.
2. **The medium tier on sparse signals.** Eleven scattered bad minutes in six hours, for example short Spot
   preemption blips, page if one of them falls in the last 30 minutes.
3. **Prober no data pages four times**: one prober serves four journeys, so one prober outage fires four policies.
4. **Short blips near the line.** The mesh SLIs use a 2-minute rate, which stretches a short outage by up to two
   minutes; a 2-minute blip can count as 3–4 bad minutes against the fast tier's 5.
5. **The opposite problem: misses.** Latency alerts are slow (above). A short slowdown, or one under the threshold,
   may never alert. Misses are logged as class `M`.

## Tuning levers

Each change is one pull request that cites the alert log; the game days then check that detection didn't get worse.

- **Restrict a tier to one signal** with #18's knob in `slo/slos.yaml` (for example: pages from the mesh signal
  only, prober ticket-only, or the reverse). This removes half of the duplicates without code changes.
- **Move the medium tier to tickets** for a journey whose medium-tier pages turn out to be mostly transient.
- **One "prober no data" policy** instead of four (a change to the generator, #18).
- **Change windows or thresholds per journey** in `slo/slos.yaml`, when the log shows a clear pattern.
- **Fix the cause instead of the alert**: timeouts, retries and fail-open dependencies in M4 reduce real burn.
- **Never** snooze or delete an alert to improve the numbers; a snooze is logged and expires.

## Counting actionability before and after tuning

Requirement R5 asks for alert counts and the share that was actionable, before and after tuning. Counted from
[incidents/alert-log.md](incidents/alert-log.md):

- **Units.** An *alert* is one Cloud Monitoring alert incident (one policy opening). A *page* is an alert that
  reached PagerDuty. An *incident* is one underlying problem, however many alerts it fired.
- **Classes.** Each alert is `A` actionable, `D` duplicate, `T` transient, `F` false or `R` wrong route; incidents
  that no alert caught are `M` missed. Definitions in the log.
- **Periods.** *Before*: from the start of the operated period (M2, planned for 2026-12-05) until the tuning pull
  request merges, at least 7 days, with the alerts exactly as generated (`baseline`). *After*: the same number of
  days after the merge.
- **Replay.** Different weeks have different incidents, so the comparison is also run on the same data: the
  recorded SLI series of the *before* period (`mamori:cuj_*:ratio_rate5m` and `_rate1h`, kept in Managed
  Prometheus) are re-evaluated with the *after* thresholds as PromQL, to show which alerts would have fired. This is
  an approximation of Cloud Monitoring's own burn-rate evaluation, and is reported as such.

**Report** (in the case study, with dates and counts, never percentages alone; the numbers come from a lab with
synthetic load):

| Measure | Before | After | Note |
|---|---|---|---|
| Alerts per week (pages / tickets) | | | |
| Actionable share of pages, with and without game days | | | `A ÷ all pages` |
| Pages per incident | | | Aim: 1 ([SRE book](https://sre.google/sre-book/being-on-call/): a 1:1 alert-to-incident ratio) |
| Night pages per week | | | |
| Missed incidents (`M`) | | | Must not rise |
| Median time to acknowledge | | | |
| Phone/SMS notifications used | | | Against PagerDuty's 100 a month |
| Time to detect on game days | | | Must not get worse |

## Sources

- Google SRE book: [Monitoring Distributed Systems](https://sre.google/sre-book/monitoring-distributed-systems/),
  [Being On-Call](https://sre.google/sre-book/being-on-call/).
- Google SRE workbook: [Alerting on SLOs](https://sre.google/workbook/alerting-on-slos/),
  [On-Call](https://sre.google/workbook/on-call/).
- Google Cloud: [Alerting on your burn rate](https://docs.cloud.google.com/stackdriver/docs/solutions/slo-monitoring/alerting-on-budget-burn-rate)
  (fast and slow burn starting points; the 24-hour lookback limit).
- PagerDuty and Cloud Monitoring channel facts: [decision-facts.md](decision-facts.md), D6.
