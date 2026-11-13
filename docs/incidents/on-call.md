# On-call

Status: draft, M3 (#20), 2026-11-13. Policy only, not yet tested with a real page. How PagerDuty, the schedule, the
escalation policy and the notification channels are built in Terraform is in [../oncall.md](../oncall.md) (#21).

## Who is on call, and when

- **One person:** the maintainer, the only member of the single PagerDuty schedule `mamori-primary`. PagerDuty's
  free plan allows 5 users, 1 on-call schedule and 1 escalation policy ([../decision-facts.md](../decision-facts.md),
  D6).
- **Pages go out 24 hours a day.** A staffed 24/7 rotation is a non-goal ([../prd.md](../prd.md)): with one person,
  time to acknowledge, day or night, is **measured, not promised**.
- **Goals, not promises:** acknowledge within 5 minutes when awake (the SRE book's typical value for user-facing
  systems, [Being On-Call](https://sre.google/sre-book/being-on-call/)); at night, best effort. Each week the alert
  log reports the median and worst time to acknowledge.
- **Waking hours** (default 08:00–23:00 local time) are when risky work happens: game days, Terraform applies, node
  pool changes, and merges to `deploy/` that change the shop. Start them only when you will be around for the next
  two hours.
- **Night pages are allowed and reviewed.** Every page between 23:00 and 08:00 gets a line in the next morning's
  review: if it could have waited until morning without hurting a journey, it should have been a ticket. That is a
  tuning finding for [../alerting.md](../alerting.md).

## What pages, and what doesn't

| Signal | Goes to | Phone? |
|---|---|---|
| Page-tier burn-rate alerts (fast and medium tiers), "prober no data" | PagerDuty service `mamori-shop`, email, Pub/Sub `incident-alerts` | Yes, if not acknowledged in time (below) |
| Ticket-tier burn-rate alerts (slow tier) | Email, Pub/Sub `incident-alerts` | Never |
| Budget notifications (50/90/100 %), spend guard | Email from Cloud Billing ([../runbooks/spend-guard.md](../runbooks/spend-guard.md)) | Never |
| Datadog monitors during the 14-day window (M3) | Not PagerDuty: one pager per symptom (see [../alerting.md](../alerting.md)) | Never |
| Anything about a cause rather than a symptom (CPU, restarts, one pod down) | Dashboards only | Never |

The full inventory and the design rules are in [../alerting.md](../alerting.md).

## How a page reaches the phone

From #21's wiring, plus personal settings that stay out of Terraform because contact details are personal data:

| Step | Setting |
|---|---|
| Schedule `mamori-primary` | One member, one weekly shift starting Monday 09:00 JST |
| Escalation policy `mamori-default` | One level: the schedule. While nobody acknowledges, it notifies again after 15 and 30 minutes, then stops (3 rounds in all, to protect the phone budget) |
| Service `mamori-shop` | Every incident is high urgency. An acknowledged incident re-triggers after 30 minutes if not resolved. No auto-resolve: Cloud Monitoring resolves it when the alert closes |
| Personal notification rules (PagerDuty app) | High urgency: push at once, **phone call after 5 minutes** if still unacknowledged. No SMS. Low urgency: push and email only |
| Phone | Allow PagerDuty through Do Not Disturb, or don't: a personal choice. Either way the time to acknowledge is recorded |

**Acknowledge in PagerDuty, never resolve there.** Resolving a PagerDuty incident while its Cloud Monitoring alert is
still open makes Monitoring open it again. Acknowledge, fix, and let the alert close by itself; that resolves the
PagerDuty incident.

## The 100 phone/SMS notifications a month

PagerDuty's free plan includes **100 SMS or phone notifications a month**; email and push are included
([pricing](https://www.pagerduty.com/pricing/incident-management/), checked 2026-11-13). The pricing page does not
say what happens after the hundredth. Plan as if calls and texts stop until the next month while push and email
carry on, and check it in the first month.

**How the budget is spent:**

- **Push first, call only if needed.** A page acknowledged within 5 minutes costs nothing from the 100. No SMS at
  all: a text and a call for the same page would count twice.
- **Game days cost zero calls.** The responder is at the desk and acknowledges at once.
- **Long incidents re-page.** An acknowledged PagerDuty incident triggers again after 30 minutes if the alert is
  still open. Acknowledge again within 5 minutes, or each re-trigger can cost a call.
- **Worst case (my math).** One outage can open several PagerDuty incidents, one per Cloud Monitoring alert policy
  (about 6 at once for an outage on three journeys, up to 12 once the medium tier fires too, see
  [../alerting.md](../alerting.md)). Each one left unacknowledged calls at 5 minutes in each of its 3 rounds: up to
  3 calls each, so 36 calls, over a third of the month's allowance, from one unanswered night. Acknowledge
  everything at once ("acknowledge all" in the app), and treat duplicate pages as an alert-tuning finding.

**Thresholds** (counted from the PagerDuty notification log, recorded weekly in [alert-log.md](alert-log.md)):

| Used this month | Action |
|---|---|
| 50 | Alert-tuning review before the next game day; no new paging alert without a measured trigger rate |
| 80 | Personal rule switches to push only for the rest of the month; post it in `#incidents`; accept slower night response |
| 100 | Push and email only until the 1st; note it in the alert log as an alert-fatigue finding (R5) |

The monthly count against 100 is one of the PRD's success measures.

## Handoff, when there is only you

A solo on-call still has shifts: at the desk, away, asleep, off for days. The handoff is a short note to your future
self in `#incidents` before going away for more than a few hours, and a read of anything that fired when you come
back. The SRE workbook's handoff ([On-Call](https://sre.google/workbook/on-call/)) is a message to the next person;
here the next person is you, tired or eight hours later.

**Before going away (checklist):**

- [ ] No fault is active: `gameday status` shows nothing, and nothing in `boutique` carries the label
  `mamori.dev/fault=true`:

  ```bash
  kubectl api-resources --verbs=list --namespaced -o name \
    | xargs -n1 kubectl get -n boutique -l mamori.dev/fault=true -o name --ignore-not-found 2>/dev/null
  ```

- [ ] No change half done: every Argo CD application is `Synced` and `Healthy`
  (`kubectl get applications -n argocd`); nothing merged in the last hour that you haven't watched land.
- [ ] No forgotten snooze on an alert policy in Cloud Monitoring.
- [ ] From M5: the executor's kill switch (ConfigMap `executor-config`, key `enabled` in `mamori-agent`) is in the
  state you intend.
- [ ] Open incidents and tickets: state and next step written down.
- [ ] Each journey's remaining error budget, and anything burning slowly.
- [ ] Phone/SMS notifications used this month.

**Coming back:** read every page and ticket that arrived, classify each in [alert-log.md](alert-log.md), and act on
anything still open. Template for both notes: [communication.md](communication.md).

**Longer absences** (travel, the year-end holidays 2026-12-29 to 2027-01-03): run the checklist, no game day in the
two days before, and leave paging on. The error budget absorbs a slow response, and the SLO record should show what
happened while nobody watched. Don't switch alerts off to make the numbers look better.

## Escalation

In a team, an unanswered page goes to a secondary on-call, and a hard problem goes to an expert. Here there is no
second person, so escalation means escalating the response:

1. **PagerDuty repeats the page** to the same person, after 15 and after 30 minutes.
2. **Raise the severity** when the mitigation isn't working ([severity.md](severity.md)).
3. **Switch from diagnosing to generic mitigations** on a time box. If the first approach hasn't helped after
   30 minutes (SEV1) or 60 minutes (SEV2), stop looking for the cause and use the runbook's generic mitigations:
   remove any active fault, roll back every change from the last 24 hours (through Git, so Argo CD doesn't undo it),
   restart the failing service.
4. **Rebuild as the last resort.** If the cluster itself is broken, `make down` and `make up` rebuild the lab from
   nothing. Carts are lost (they are synthetic); the SLO history stays in Cloud Monitoring.
5. **Look outside** when the cause may not be ours: [Google Cloud Service Health](https://status.cloud.google.com/)
   for zone, GKE or load-balancer problems; the GKE release notes; the issue trackers of Online Boutique, Istio,
   Argo CD and Chaos Mesh. Google Cloud's free Basic Support covers billing, not technical cases
   ([support overview](https://docs.cloud.google.com/support/docs/overview)), and this lab buys no paid support.
6. **The agent is not an escalation path.** From M5 it is an advisor from the first minute ([roles.md](roles.md)).

## On-call load

The SRE book caps on-call load at about two incidents per 12-hour shift and says "all paging alerts should also be
actionable" ([Being On-Call](https://sre.google/sre-book/being-on-call/)). Tracked weekly in
[alert-log.md](alert-log.md):

- pages per week, night pages per week, incidents per week, pages per incident (aim: 1);
- if pages average more than two a day for a week, stop other work (game days, M4 hardening) and fix the alerts
  first: the SRE book's answer to operational overload;
- after a night incident, take the time back the next day. A tired responder makes the next incident worse.
