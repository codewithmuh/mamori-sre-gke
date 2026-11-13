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
5. **Watch the watcher.** The prober runs inside the cluster it watches. If it stops reporting, `CUJ prober: no
   data` pages, because a silent prober would otherwise look like a healthy shop. When the whole cluster is down,
   that is the only page: nothing inside it can report failures. The SLO data itself lives in Cloud Monitoring,
   outside the cluster.
6. **One signal pages, one pager.** For each journey only the prober pages: it sees what a shopper sees, from the
   ingress gateway inward, and one signal means fewer duplicate pages. The mesh SLIs, the prober's slow tier and
   Datadog (its synthetics in the 14-day window, M3) raise tickets. Cloud Monitoring is the only thing that pages,
   so one incident doesn't page twice from two tools; the Datadog window compares the tools' detection times
   instead.
7. **New paging alerts start as tickets.** Any alert added by hand goes to email for a week first, so its trigger
   rate is known before it can wake anyone ([SRE workbook, On-Call](https://sre.google/workbook/on-call/)).
8. **Tune with evidence.** Every alert is classified in [incidents/alert-log.md](incidents/alert-log.md). Changes to
   thresholds, windows or routing go through a pull request that cites the log. Simply relaxing thresholds is rarely
   the right answer to a page under SLO-based alerting (workbook); removing duplicates and misrouted pages usually
   is.

## Alert path

```
Cloud Monitoring alert policies (generated, #18)
  ├─ page (severity=page, CRITICAL): prober fast and medium tiers, CUJ prober: no data
  │     ──▶ local.incident_notification_channels (#21)
  │           ├─ PagerDuty "mamori-shop" ─▶ push, then a phone call
  │           ├─ email "mamori alerts (email)"
  │           └─ Pub/Sub topic incident-alerts ─▶ AI agent (from M5)
  └─ ticket (severity=ticket, WARNING): prober slow tier, every mesh tier, CUJ mesh: no data
        ──▶ local.incident_ticket_notification_channels (#21)
              ├─ email
              └─ Pub/Sub topic incident-alerts
```

Cloud Monitoring's PagerDuty channel uses PagerDuty's Events API v1 with a service key, and you can't close a
Monitoring alert from PagerDuty ([decision-facts.md](decision-facts.md), D6). The alert closes in Cloud Monitoring,
and that resolves the PagerDuty incident; resolving it in PagerDuty first only makes Monitoring open it again. So:
acknowledge in PagerDuty, fix, let the alert close. The wiring is in [oncall.md](oncall.md) (#21); the on-call
policy is in [incidents/on-call.md](incidents/on-call.md).

## Alert inventory

**No alert exists until `slo_enabled` is true.** The Terraform variable defaults to `false`: no SLO, alert policy or
dashboard is created until it is set after the first `make up`, once the prober has been scraped (about 10
minutes). Until then the lab is unwatched, and nothing pages.

### SLOs

Each journey has one Cloud Monitoring custom service, `mamori-cuj-<id>`, with four SLOs over a rolling 28 days.
Two signals look at each journey from different sides:

| SLO id | Signal | What it measures | Target |
|---|---|---|---|
| `prober-availability` | Go prober (#16), one run per journey per minute, `source="scheduled"` | Share of probe runs that succeed (request-based) | 99.5 %; checkout 99.0 % |
| `prober-latency` | Prober step durations | Share of steps faster than the threshold (request-based) | 95 % under 500 ms; checkout 1,000 ms |
| `mesh-availability` | Istio sidecar metrics, client side | Share of good 1-minute windows; a minute is good when the product of the journey's critical backend services' success rates meets the target (windows-based) | 99.5 %; checkout 99.0 % |
| `mesh-latency` | Istio request durations | Share of good 1-minute windows; a minute is good when every critical backend service has at least 95 % of its requests under the threshold (windows-based) | 95 % of minutes |

The prober enters through the Istio ingress gateway inside the cluster, so it sees the gateway, the frontend and
every service behind it, but not the load balancer, the certificate or Cloud Armor. The mesh SLIs cover all
traffic, including the load generator's, but only the critical backend services: the frontend is left out of the
composites, and gRPC `OK`, `INVALID_ARGUMENT` and `NOT_FOUND` count as good. Because they judge each service call on
its own, the mesh SLIs miss slowness that comes from many fast calls made one after another; the prober times the
whole page.

### Burn-rate tiers

Google's budget fractions, with burn rates recomputed for a 28-day window; both conditions must hold. The slow tier's
long window is 24 hours instead of the workbook's 3 days because a Cloud Monitoring burn-rate condition can look
back at most 24 hours. Latency has its own, lower thresholds: a 95 % target caps the burn rate at 20× (everything
slow).

| Tier | Availability: burn, both windows | Latency: burn, both windows | Prober | Mesh |
|---|---|---|---|---|
| Fast | > 13.44 over 1 h and 5 min (2 % of the budget in an hour) | > 6.72 over 1 h and 5 min (about 34 % slow) | page | ticket |
| Medium | > 5.6 over 6 h and 30 min (5 % in six hours) | > 2.8 over 6 h and 30 min (14 % slow) | page | ticket |
| Slow | > 2.8 over 24 h and 2 h (10 % in a day) | > 1.4 over 24 h and 2 h (7 % slow) | ticket | ticket |

A page has policy severity `CRITICAL` and user label `severity=page`, and goes to PagerDuty, email and Pub/Sub; a
ticket has `WARNING` and `severity=ticket`, and goes to email and Pub/Sub.

**No-data alerts** (PromQL `absent_over_time(...[10m])`, user labels `cuj=all`, `alert=no-data`, `burn=none`):

| Display name | Terraform | Fires when | Route |
|---|---|---|---|
| `CUJ prober: no data` | `slo_prober_no_data` | The prober has sent nothing for 10 minutes. The only alert when the whole cluster is down | page |
| `CUJ mesh: no data` | `slo_mesh_no_data` | The mesh SLI series stop for 10 minutes: the managed rule evaluator or the Istio scrape stopped | ticket |

### Policies

4 journeys × 4 SLOs × 3 tiers = **48 burn-rate policies** (16 page, 32 ticket), plus **2 no-data policies**:
**50 in all, 17 paging, 33 tickets.**

- **Display name:** `CUJ <id>: <signal> <sli> <tier> burn`, for example `CUJ browse: prober availability fast burn`
  or `CUJ checkout: mesh latency slow burn`; and `CUJ prober: no data`, `CUJ mesh: no data`.
- **Terraform:** `google_monitoring_alert_policy.slo_<id_snake>_<signal>_<sli>_<tier>`, for example
  `slo_add_to_cart_mesh_availability_medium`; and `slo_prober_no_data`, `slo_mesh_no_data`.
- **User labels:** `generator=slo-gen`, `cuj`, `signal` (`prober`, `mesh`), `sli` (`availability`, `latency`),
  `alert` (`burn-rate`, `no-data`), `burn` (`fast`, `medium`, `slow`, `none`), `severity` (`page`, `ticket`).

| Journey | Runbook | Availability target | Latency threshold | Policies |
|---|---|---|---|---|
| `browse` | [runbooks/cuj-browse.md](runbooks/cuj-browse.md) | 99.5 % | 500 ms | 12 burn-rate (4 page, 8 ticket) |
| `add-to-cart` | [runbooks/cuj-add-to-cart.md](runbooks/cuj-add-to-cart.md) | 99.5 % | 500 ms | 12 burn-rate (4 page, 8 ticket) |
| `view-cart` | [runbooks/cuj-view-cart.md](runbooks/cuj-view-cart.md) | 99.5 % | 500 ms | 12 burn-rate (4 page, 8 ticket) |
| `checkout` | [runbooks/cuj-checkout.md](runbooks/cuj-checkout.md) | 99.0 % | 1,000 ms | 12 burn-rate (4 page, 8 ticket) |

### What it takes to fire (my math)

A "bad minute" is a failed (or, for latency, slow) probe, at one probe a minute, or a bad 1-minute mesh window.
Both conditions must hold. Pages come from the prober only; the same counts on the mesh signal open tickets.

| Tier | Availability, 99.5 % journeys | Availability, checkout (99.0 %) | Latency (95 %, every journey) |
|---|---|---|---|
| Fast | 5 bad minutes in the last hour, 1 in the last 5 minutes | 9 in the last hour, 1 in the last 5 minutes | 21 slow minutes in the last hour, 2 in the last 5 minutes |
| Medium | 11 in the last 6 hours, 1 in the last 30 minutes | 21 in 6 hours, 2 in the last 30 minutes | 51 in 6 hours, 5 in the last 30 minutes |
| Slow | 21 in the last 24 hours, 2 in the last 2 hours | 41 in 24 hours, 4 in the last 2 hours | 101 in 24 hours, 9 in the last 2 hours |

So a total outage pages after about 5 minutes (checkout 9), and once more from the medium tier if it lasts about
11 minutes (checkout 21). A total slowdown pages after about 21 minutes; browse, whose probe has two timed steps,
needs both slow for that, and about 40 minutes when only one is. A 10-minute slowdown pages nobody, so game days GD2
and GD6 run their latency faults for 60 minutes (`make gameday-start FAULT=<id> DURATION=60m`) to see whether, and
when, a latency alert fires ([incidents/game-days.md](incidents/game-days.md)).

### Other notifications (not generated, none of them page)

| Source | Route | Runbook |
|---|---|---|
| Budget `mamori-lab` at 50/90/100 %; spend guard | Cloud Billing email | [runbooks/spend-guard.md](runbooks/spend-guard.md) |
| Datadog synthetics (14-day window, #22) | Tickets only (priority 3), medium and slow tiers, short windows of at least 15 minutes; never PagerDuty | [datadog-window.md](datadog-window.md) |
| Prober health beyond "no data" | Dashboards | [runbooks/prober.md](runbooks/prober.md) |

## Where the noise will probably come from

These are predictions for the first weeks, written down so the alert log can confirm or refute them.

1. **Duplicates.** One cause opens one PagerDuty incident per paging policy that fires, and nothing merges them.
   With only the prober paging, a fault pages at most twice per journey it hurts: the fast tier, then the medium
   tier if it lasts about 11 minutes. A currency outage breaks all four journeys (every probe starts on the home
   page, which converts every price): **4 pages, up to 8**. A checkout-only fault: 1, up to 2. The whole cluster
   down: 1 page (`CUJ prober: no data`) and 1 ticket. Expected: pages per incident still above 1.
2. **The medium tier on sparse probe failures.** Eleven scattered failed probes in six hours, for example short
   Spot preemption blips, page if one of them falls in the last 30 minutes.
3. **Ticket volume.** 33 ticket policies, most of them on the mesh signal, can fill the inbox. Tickets are
   classified in the alert log like pages; a ticket nobody needs is noise too.
4. **Short blips near the line.** The mesh SLIs use a 2-minute rate, which stretches a short outage by up to two
   minutes; a 2-minute blip can count as 3–4 bad minutes against the fast tier's 5 (a ticket now, not a page).
5. **The opposite problem: misses and late pages.** Latency alerts need about 21 slow minutes in an hour. A partial
   failure pages late: if one checkout in four fails, the mesh fast tier opens a ticket after about 9 minutes, but
   the prober's page needs about 32 ([incidents/severity.md](incidents/severity.md)). A cluster that goes down pages
   only after the prober has been silent for 10 minutes. Misses are logged as class `M`.

## Tuning levers

Each change is one pull request that cites the alert log; the game days then check that detection didn't get worse.

- **Choose which signals page** with `alerting.pageSignals` in `slo/slos.yaml` (baseline: `[prober]`). If game
  days show partial failures paging too late, letting the mesh fast tier page is the lever, at the cost of more
  duplicates.
- **Move the medium tier to tickets** for a journey whose medium-tier pages turn out to be mostly transient.
- **Change windows or thresholds per journey** in `slo/slos.yaml`, when the log shows a clear pattern.
- **Fix the cause instead of the alert**: timeouts, retries and fail-open dependencies in M4 reduce real burn.
- **Snooze only for planned downtime.** `make slo-snooze` snoozes every generated policy (label
  `generator=slo-gen`) for `SLO_SNOOZE_MINUTES` (default 60). Use it before `make down` (the prober goes before
  `terraform destroy`, which would otherwise page) or a planned rebuild, and log it in the alert log. **Never**
  snooze or delete an alert to improve the numbers; a snooze expires on its own.

## Counting actionability before and after tuning

Goal G5 asks for alert counts and the share that was actionable, before and after tuning. Counted from
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
