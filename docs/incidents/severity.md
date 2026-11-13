# Severity levels

Status: draft, M3 (#20), 2026-11-13. Not yet used on a real incident.

Severity says how badly shoppers are hurt right now, and so how hard to respond. It is measured on the critical user
journeys (CUJs) and their error budgets, never on the health of one service: a crashed pod that no journey notices
is not an incident. The on-call person, acting as incident commander ([roles.md](roles.md)), sets it. From M5 the AI
agent proposes one; the human decides. The postmortem reviews it.

## Levels

| Level | Shopper impact (any one is enough) | Error-budget signal | Response |
|---|---|---|---|
| **SEV1** | A journey is **hard down**: about half or more of its requests or probes fail for 5 minutes or more. Or every journey is down at once (cluster, zone, load balancer, spend guard) | Burn rate around 100× or more (99.5 % target) or 50× (checkout, 99.0 %): the whole 28-day budget would be gone in under 14 hours | Page. Declare at once. Drop everything else. Updates every 15 minutes. Postmortem required |
| **SEV2** | A journey is **degraded**: a visible share of shoppers fail or wait too long, but fewer than half fail | A page-tier burn-rate alert fired: 2 % of the 28-day budget in an hour (fast tier) or 5 % in six hours (medium tier) | Page. Declare. Updates every 30 minutes. Postmortem required |
| **SEV3** | **Small or slow damage**: only a ticket-tier (slow-burn) alert; or a non-critical part of a page is missing (ads, recommendations) while the journey still succeeds; or the SLO data is missing (prober no data) and nothing else shows impact | Slow tier: 10 % of the budget in a day. Or unknown, because monitoring is blind | Ticket (email). Handle in waking hours, the same or next day. Declare only if it gets worse |
| **SEV4** | **No shopper impact now**: lost redundancy (one Spot node gone while every SLO holds), a near miss, a gap found in an alert or runbook | No measurable burn | A GitHub issue. No declaration |

**Why any journey hard down is SEV1.** Every journey in this shop sits on the way to an order: browsing finds the
product, add-to-cart fills the cart, and the cart page holds the checkout form. A journey that is hard down blocks
orders, so there is no "less important" journey at SEV1. Checkout's lower availability target reflects how many
services it calls (see [../prd.md](../prd.md)), not lower importance.

**A page is not automatically SEV2.** The alert tier says how urgent it is to look; severity says what was found.
The "prober no data" alert pages, but if the mesh SLIs show every journey working, the incident is SEV3: the lab is
blind, not broken.

### The burn rates behind the levels

Burn rate is how fast the error budget is being spent: 1× spends exactly the 28-day budget in 28 days. Error ratio =
burn rate × (1 − target). The tiers are the generated alerts from #18 (Google's multiwindow, multi-burn-rate
alerts, recomputed for a 28-day window); [../alerting.md](../alerting.md) has the full inventory.

| Tier (alert) | Burn rate, both windows | Error ratio at 99.5 % | Error ratio at 99.0 % (checkout) | Budget spent |
|---|---|---|---|---|
| SEV1 (no alert of its own) | about 100× (50× for checkout) | ≥ 50 % | ≥ 50 % | whole budget in under 14 hours |
| Fast (page) | > 13.44× over 1 h and 5 min | > 6.72 % | > 13.44 % | 2 % in 1 hour |
| Medium (page) | > 5.6× over 6 h and 30 min | > 2.8 % | > 5.6 % | 5 % in 6 hours |
| Slow (ticket) | > 2.8× over 24 h and 2 h | > 1.4 % | > 2.8 % | 10 % in 24 hours |

My math. Latency SLOs have a 95 % target, so their budget is 5 % of requests; the most a latency SLO can burn is 20×
(every request slow), and its fast tier needs more than two thirds of requests to be slow.

## Rules

- **When unsure between two levels, pick the higher one** (PagerDuty's
  [severity guide](https://docs.pagerduty.com/ops-guides/incident-response-guide/severity-levels)). Lowering it later
  is cheap; responding too slowly is not.
- **Don't debate severity during the incident.** Change it when the facts change, say so in the incident thread, and
  review the choice in the postmortem.
- **Several journeys hurt:** the severity of the worst one.
- **Impact, not intent.** A game day or the spend guard that takes a journey down is SEV1 like any other outage. The
  postmortem says it was planned and how much budget was meant to be spent ([../prd.md](../prd.md), error-budget
  policy outline).
- **Raise it** when mitigation has not started to work within 30 minutes (SEV2 → SEV1 response, even if the numbers
  sit between levels), or when a journey's 28-day budget runs out during the incident.
- **Lower it** only after the mitigation has held for 15 minutes.

## When a postmortem is required

Adapted from the triggers in Google's
[Postmortem Culture](https://sre.google/sre-book/postmortem-culture/) chapter and the error-budget policy outline in
[../prd.md](../prd.md). Any one is enough:

1. SEV1 or SEV2.
2. Any incident that used more than 20 % of a journey's 28-day budget (whatever its severity).
3. Every game day, including one where nothing went wrong ([game-days.md](game-days.md)).
4. A person noticed the problem before any alert did (a monitoring failure).
5. Data was lost, for example every cart after `redis-cart` restarted.
6. From M5: the agent proposed an unsafe action, or the executor blocked or failed an action.

Anything else may get a short postmortem when there is something to learn. Template:
[postmortem-template.md](postmortem-template.md).

## Examples (expected, not yet observed)

| What happens | Expected severity | Why |
|---|---|---|
| Most product pages return errors for 10 minutes | SEV1 | Browse hard down |
| One checkout in four fails, for an hour | SEV2 | Checkout's fast tier (> 13.44 % at 99 %) pages after about half an hour; most orders still succeed |
| The cart page takes 2 s instead of 300 ms for nearly every request | SEV2 | View-cart latency fast tier pages |
| Recommendations are missing from product pages, pages load fine | SEV3 | Non-critical part missing; browse still succeeds |
| The prober stops reporting; mesh SLIs are normal | SEV3 | Blind, not broken |
| One Spot node is preempted; pods move; no journey burns | SEV4 | Lost redundancy, no impact |
| The spend guard scales the pool to zero | SEV1 | Every journey down; the postmortem names it as a cost decision |
