---
id: "YYYY-MM-DD-short-slug"
date: "YYYY-MM-DD"
faultId: null
services: []
rootCause: "One line, in system terms: what failed and why."
severity: "SEV2"
# Optional keys below. Keep them; use null where unknown. Times are RFC3339 in UTC.
cujs: []
timeline:
  impactStart: null
  faultStart: null
  detected: null
  acknowledged: null
  rootCauseIdentified: null
  mitigated: null
  resolved: null
---

# Postmortem: <short title, what shoppers saw>

> **How to use this template.** Copy it to `docs/incidents/postmortems/YYYY-MM-DD-<slug>.md` (the UTC day the impact
> started, a short lowercase slug), fill it in, and delete every quoted guidance block like this one. Front matter
> rules, review steps and the index are in `docs/incidents/postmortems/README.md`.
>
> **Front matter.** The first six keys are fixed: the AI agent (#24) reads them to answer "has this happened
> before?". All values are quoted strings so YAML doesn't turn dates into another type. `id` is the file name without
> `.md`. `date` is the UTC day the impact started. `faultId` is the game-day fault id (for example
> `"F01-currency-abort"`) or `null` for a real incident. `services` lists the Deployment names in `boutique` at the
> root of it, sorted. `rootCause` is one line. `severity` is the final one, `"SEV1"` to `"SEV4"`. The optional
> `cujs` and `timeline` keys feed the agent evaluation (#26): time to detect = `detected` − `faultStart` (or
> `impactStart` for a real incident), human time to root cause = `rootCauseIdentified` − `faultStart`, time to
> mitigate = `mitigated` − `faultStart`.
>
> **Blameless.** Write about the system, never about a person's character or competence. Use roles ("the on-call")
> rather than names. Ask how and what, not who. Describe what was known at each moment, without hindsight ("should
> have"). Every human mistake points to something the system made easy: a missing check, a confusing alert, a
> runbook gap. That is what the action items fix. Writing a postmortem is not punishment; it is how the lab learns
> ([SRE book](https://sre.google/sre-book/postmortem-culture/)).

| | |
|---|---|
| Status | Draft / In review / Reviewed / Closed |
| Owner | `<GitHub handle>` |
| Severity | SEV<n> (first set as SEV<n>; changed at HH:MM because ...) |
| Journeys hit | <browse, add-to-cart, view-cart, checkout> |
| Planned? | No / Game day GD<n>, fault `<F0x-id>`, run id `<run-id>`; blind: yes / no |
| Impact window (UTC) | <start> – <end> (<minutes> min) |
| Time to detect | <minutes> (first alert opened − impact start, or − fault start on a game day) |
| Time to acknowledge | <minutes> (page acknowledged − first page) |
| Time to root cause (human) | <minutes> (first `[rca]` message − impact or fault start) |
| Time to mitigate | <minutes> (SLI back within target and held 5 min − impact or fault start) |
| Error budget used | <journey> <SLO>: <x> % of the 28-day budget (planned: <y> %) |
| Alerts | <n> pages (PagerDuty incidents), <m> tickets; actionable: <k> of <n + m> (`docs/incidents/alert-log.md`) |
| Phone/SMS used | <n> (this month so far: <total> of 100) |
| Agent (from M5) | Top hypothesis right: yes / no; time to root cause <minutes>; proposal approved: yes / no / none |
| Follow-ups | #<issue>, #<issue> |

## Summary

> Two or three sentences: what shoppers saw, for how long, why, and how it was stopped.

## Impact

> Numbers from the SLIs, each with the query or dashboard it came from. Impact often starts before detection.

- **Shoppers:** <which journeys failed or were slow, what share of requests and probes, for how long>.
- **Error budget:** <per journey and SLO, from the Cloud Monitoring SLO page>.
- **Other effects:** <for example, every cart emptied when `redis-cart` restarted; the load generator's carts too>.

## Timeline (UTC)

> Build it from telemetry, not memory, and start before the impact. Go forward in time, not backwards from the fix,
> to avoid hindsight. Facts only. Every row has a source: a PromQL query, a log query, a PagerDuty log entry, a
> Slack message link, an Argo CD revision, the game-day timeline file (`gameday-runs/<run-id>.json`), or an agent
> or executor log line.

| Time | Event | Source |
|---|---|---|
| HH:MM:SS | <last change before the incident, e.g. a merge to `deploy/` and Argo CD sync> | <commit, Argo CD history> |
| HH:MM:SS | <fault started (game day) or first bad probe> | <gameday-runs file, `mamori_cuj_probe_total`> |
| HH:MM:SS | <alert opened: policy display name> | <Cloud Monitoring incident> |
| HH:MM:SS | <page sent, push, phone call> | <PagerDuty incident log> |
| HH:MM:SS | <acknowledged> | <PagerDuty incident log> |
| HH:MM:SS | <declared SEV<n>> | <Slack link> |
| HH:MM:SS | <`[finding]`, `[decision]`, `[action]` lines> | <Slack links, kubectl or Argo CD history> |
| HH:MM:SS | <`[rca]` root cause stated> | <Slack link> |
| HH:MM:SS | <mitigation applied> | <Slack link, audit log> |
| HH:MM:SS | <SLI back within target> | <query> |
| HH:MM:SS | <alert closed; resolved> | <Cloud Monitoring, PagerDuty> |

## Detection

> Which signal noticed it first: an alert, the agent, a person, a dashboard? Which alerts fired, in what order, and
> which should have fired and didn't? Was each page actionable (classify each in the alert log)?

## Response

> What was done, in order, and what each step changed. Which runbook was used, and where it was wrong or missing a
> step. How the one person covered the four roles (`docs/incidents/roles.md`) and where that hurt.

## Agent's part (from M5)

> The agent's proposed severity, its top three hypotheses and their evidence, its proposed action and what happened
> to it, its time to root cause against the human `[rca]`. Any unsafe proposal, and what blocked it.

## Root cause and trigger

> - **Trigger:** what started it (a change, a fault, a preemption, a traffic change).
> - **Root cause:** why the system let the trigger hurt shoppers.
> - **Contributing factors:** what made it bigger, longer or harder to see. Look at process too: alerting,
>   runbooks, review, tooling. A person's action is never the root cause.

## Hypothesis and outcome (game days only)

| | Expected | Observed |
|---|---|---|
| Journeys hit | | |
| Alerts | | |
| Time to detect | | |
| Recovery | | |
| Budget used | | |

> Was the hypothesis confirmed or disproved? A disproved hypothesis is a result, not a failure.

## What went well

## What went poorly

## Where we got lucky

## Action items

> Each item starts with a verb, is specific and has a clear end ("Add a 2 s timeout to the frontend's recommendation
> calls", not "improve resilience"). One owner each. Every item is a GitHub issue labelled `postmortem` that links
> back here; the issue is the tracker, this table is the summary. Priorities: P0 before the next game day or within
> a week; P1 within the current milestone; P2 later. "A postmortem without subsequent action is indistinguishable
> from no postmortem" ([SRE workbook](https://sre.google/workbook/postmortem-culture/)).

| # | Action | Type | Priority | Owner | Issue | Status |
|---|---|---|---|---|---|---|
| 1 | <verb ...> | prevent / detect / mitigate / process | P0 / P1 / P2 | `<handle>` | #<n> | open / done |

## Lessons

> What would you tell someone running a similar lab? One to three points.

## Supporting data

> Links and exact queries used above: PromQL, log queries, dashboards, the game-day timeline file, commits.
