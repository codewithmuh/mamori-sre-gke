# Error-budget policy

Status: adopted for M2 (2026-11-13). Nothing has run yet; the first entries in the [log](#policy-log)
come from the first operated weeks. This turns the outline in [prd.md](../prd.md#error-budget-policy-outline)
into rules. It follows the example policy in Google's SRE Workbook
([Error Budget Policy](https://sre.google/workbook/error-budget-policy/)), cut down to a lab run by one
person.

## Why this exists

An SLO only matters if missing it changes what we do. This page says what changes, at which point,
and who decides. It applies to each critical user journey (CUJ) separately: a burning checkout budget
freezes work that touches checkout, not the whole lab.

## Scope

| CUJ | Availability target | Latency target | Window |
|---|---|---|---|
| `browse` | 99.5 % | 95 % under 500 ms | rolling 28 days |
| `add-to-cart` | 99.5 % | 95 % under 500 ms | rolling 28 days |
| `view-cart` | 99.5 % | 95 % under 500 ms | rolling 28 days |
| `checkout` | 99.0 % | 95 % under 1,000 ms | rolling 28 days |

The targets are the PRD's provisional starting points. The source of truth is `slo/slos.yaml`, and its
SLO generator writes the Cloud Monitoring SLOs, burn-rate alerts and the *Mamori CUJ SLOs* dashboard
(see [slo-design.md](slo-design.md)). If the two disagree, `slo/slos.yaml` wins and this table is
wrong.

Each journey has two SLIs, the prober (end to end, through the public load balancer) and the mesh
(requests between services). Each has its own budget. This policy acts on whichever is worse.

## The budget

- **Budget = 1 − target** over the rolling 28 days. For 99.5 % that is 0.5 % of probes or requests;
  for 99.0 %, 1 %.
- **In prober minutes** (my math, one probe a minute, 40,320 minutes in 28 days): 99.5 % allows about
  202 failed minutes (3.4 hours); 99.0 % allows about 403 (6.7 hours).
- **Budget spent** is the share of that allowance already used in the current window. Read it from
  the *Mamori CUJ SLOs* dashboard (error budget remaining = 100 % − spent).
- **Rolling window:** failures leave the window 28 days after they happened, so a spent budget comes
  back by itself if the burn stops. A freeze ends when the budget is back above the threshold that
  started it, not on a date.

Quick reference for one incident (my math, assuming the journey fails completely while it lasts):

| Share of the 28-day budget | 99.5 % journeys | `checkout` (99.0 %) |
|---|---|---|
| 20 % (postmortem line, below) | 40 minutes | 81 minutes |
| 50 % | 101 minutes | 202 minutes |
| 100 % | 202 minutes | 403 minutes |

A partial failure burns proportionally slower: 10 % of requests failing for 400 minutes costs as much
as a total failure for 40.

## What happens, by budget spent

The on-call checks the budget at least once a week (Monday) and whenever a burn-rate alert fires.

| Budget spent (per CUJ) | State | What happens |
|---|---|---|
| under 50 % | **Normal** | Changes, releases and game days go ahead. Postmortem follow-ups are planned like any other work. |
| 50 % or more | **Caution** | Find out where the budget went: which incidents, which services, whether the burn is still going on. Write it in the log. A game day that touches the journey needs an estimate of its burn first (fault duration × expected failure rate), and runs only if at least 25 % of the budget would be left afterwards. Postmortem follow-ups for this journey go to the top of the backlog. |
| 75 % or more | **Protect** | No game days or load tests that touch the journey. Changes to services on its critical path (from `slo/cuj-map.yaml`) only if they fix reliability, security or cost, or are low risk with a tested rollback. At least one reliability task for the journey each week until the budget recovers below 75 %. |
| 100 % (spent) | **Freeze** | Only these changes touch the journey until its budget is back above zero: fixes that stop or reduce the burn, security fixes, cost fixes (the spend guard included), and postmortem follow-ups. No game days, load tests or experiments on it. If no single incident explains the overspend, write a postmortem for the overspend itself (many small failures add up too). |

A change "touches" a journey if it changes a service, mesh rule or edge component on that journey's
critical path, or shared infrastructure everything depends on (cluster, node pool, Istio, the edge).
Shared-infrastructure changes are judged against the journey with the least budget left.

## Postmortems

- **Any single incident that uses more than 20 % of a journey's 28-day budget gets a blameless
  postmortem**, with at least one follow-up that addresses the root cause and is tracked as an issue.
  (From the SRE Workbook's example policy. The workbook's version also asks for a P0 action.)
- **The same class of failure** (same root cause or same fault type) using more than 20 % of a
  budget across several incidents during the operated period (the workbook says a quarter; this lab
  runs for about seven weeks) gets one combined postmortem, and a fix in the next milestone.
- Pages that use less than 20 % still get a short incident record. The incident process and template
  are in `docs/incidents/` (#20).

## Game days count

Game days spend budget on purpose. They are scheduled when the budget allows (see the table above),
and their burn is **counted, not excluded**: a shopper would have felt it. The game-day postmortem
states the planned burn (estimated before) and the actual burn (measured after). A game day that burns
much more than planned is itself a finding.

## The spend guard counts too

At 100 % of the money budget, the spend guard scales the node pool to zero and the shop goes down on
purpose (D12). That downtime burns the error budget like any other outage, and its postmortem names it
as a cost decision. See [runbooks/spend-guard.md](../runbooks/spend-guard.md).

## Blind spots: when nothing measures the outage

The prober and the metric collectors run in the cluster they watch. If the nodes are gone (spend guard,
a Spot stock-out, all three nodes preempted), they stop reporting, and Cloud Monitoring sees no data
rather than failures. The budget then looks untouched while the shop is down.

- The prober's "no data" alerts (#18) catch this.
- **Rule:** every known outage with no data is logged here with its start and end, and counted as
  failed minutes for every journey when the budget is reported (weekly review, case study). Reports
  state when a figure includes such a correction.
- During the Datadog window (M3), Datadog synthetics probe from outside Google Cloud and give an
  independent view of the same periods.

## Who decides, and exceptions

- **The on-call SRE decides.** In this lab that is also the service owner and the only engineer, so the
  checks are the written rules above and the log below, not a second person.
- **Exceptions** (a change during a freeze that the table doesn't allow) are allowed only with a
  written reason in the log, before the change, and are reviewed in the next weekly check.
- **Disputed measurements** (for example, a burn caused by a bug in the prober, or by an SLI that
  counts something shoppers never see): like the workbook's "miscategorized errors", such burn doesn't
  trigger the actions in the table. The recorded numbers are not edited, though. Fix the SLI in a pull
  request, log the dispute, and say in reports which figures it affects.

## Changing the SLOs

Targets and thresholds change only through a pull request to `slo/slos.yaml`, with the reason and the
measured data behind it, and a row in the log. The provisional targets are reset once, after the first
full 28-day window of data (around 2027-01-02 if the operated period starts on 2026-12-05, my math),
and after that only when the data says the target is wrong for shoppers, not to make a bad month look
good.

## Policy log

Every use of this policy goes here: state changes, freezes, exceptions, game-day budget checks,
corrections for blind spots, SLO changes and the weekly check. The case study counts these rows to show
the policy was used, not just written. Newest first.

| Date (JST) | CUJ | Budget spent | State | What happened / what was decided | Link |
|---|---|---|---|---|---|
| | | | | *No entries yet. The first is the first weekly check after the operated period starts.* | |
