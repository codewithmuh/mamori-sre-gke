# Incident communication

Status: draft, M3 (#20), 2026-11-13. Templates not yet used on a real incident.

## Where

- **Slack channel `#incidents`** in the lab's free workspace. PagerDuty's Slack integration posts a card there for
  every incident of the `mamori-shop` service (#21, [../oncall.md](../oncall.md)). Whether the free plan's cards
  carry acknowledge and resolve buttons is not verified yet; acknowledge in the PagerDuty app either way. The free
  plan cannot create a channel per incident, so **each incident is one thread** in `#incidents`, started by the
  declaration below.
- From M5 the agent posts its investigation, and the executor its approval request, as new messages in their
  configured channels, not in the incident thread. **Paste both links into the incident thread**
  ([roles.md](roles.md)), so the thread stays the one record.
- **No status page.** PagerDuty's free plan has none, and the shoppers are synthetic. The Slack thread is the working
  record; the postmortem in Git is the public one.

## Rules

- **Times in UTC**, the same as the telemetry, so the thread can be pasted into the postmortem timeline as it is.
- **Facts, not guesses.** Write "unknown" rather than a guess presented as fact. Tag messages `[decision]`,
  `[action]`, `[finding]`, `[rca]`, `[update]` ([roles.md](roles.md)).
- **Everything in the thread**, with "also send to channel" for the declaration's first update, a severity change
  and the resolution.
- **Safe to publish.** The thread ends up in a public repo: no tokens, keys, internal IP lists or personal data.
- **Copy it out within a week.** Slack's free plan shows only 90 days of history
  ([../decision-facts.md](../decision-facts.md), D6). The postmortem keeps what matters.
- **Cadence:** SEV1 every 15 minutes, SEV2 every 30 minutes, also when nothing changed ("no change" is news). SEV3
  and SEV4 are not declared; a ticket gets one message when work starts and one when it ends.

## Declare

Post as soon as the page is acknowledged and the first severity is chosen. It starts the incident's clock.

```text
INCIDENT DECLARED: SEV<n>: <journey> <what shoppers see>
Started: <HH:MM> UTC (first bad probe or alert, whichever is earlier)
Impact: <who sees what, in numbers if known, e.g. "about 30 % of product pages return 500">
Journeys: <browse | add-to-cart | view-cart | checkout>
Alert: <policy display name, e.g. "CUJ browse: prober availability fast burn">, PagerDuty <incident number>
Game day: <no | yes, GD<n>, fault <F0x-id>, planned budget <x> %>
Roles: IC, ops, comms, scribe: <handle> (solo)
Runbook: docs/runbooks/cuj-<id>.md
Next update: <HH:MM> UTC
```

## Update

```text
[update] <n>: SEV<n>: <journey> <short title>: <HH:MM> UTC
Status: <investigating | mitigating | monitoring>
Impact now: <numbers from the SLI, better, worse or same since last update>
Since last update: <findings and actions, one line each>
Next: <the one thing being tried next>
Severity: <unchanged | changed from SEV<a> to SEV<b> because ...>
Next update: <HH:MM> UTC
```

## Resolve

Resolve when the journey's SLI has been within target for 15 minutes and the alert has closed.

```text
RESOLVED: SEV<n>: <journey> <short title>: <HH:MM> UTC
Impact: <start>–<end> UTC (<minutes> min); <journeys>; about <x> % of <journey>'s 28-day budget used
Mitigation: <what stopped the damage>
Cause (first view, may change): <one line>
Alerts: <n> pages, <m> tickets; phone/SMS used: <k>
Agent (from M5): top hypothesis <right | wrong>; proposal <approved | rejected | none>
Follow-ups: postmortem <required | not required> by <YYYY-MM-DD>; issues <#..>
```

## Game day start and stop

Game days run the same process ([game-days.md](game-days.md)). Announce them so a real incident during one is
not mistaken for the experiment.

```text
GAME DAY GD<n> START: <HH:MM> UTC
Fault: <F0x-id> (<title>), duration <m> min, run id <run-id>
Blind: <yes | no>
Hypothesis: <one line>
Abort if: <the abort criteria, one line>
Planned budget: <journey> about <x> %
```

```text
GAME DAY GD<n> STOP: <HH:MM> UTC (<ended on time | aborted because ...>)
Fault removed: <yes, make gameday-status empty and no mamori.dev/fault objects left>
```

## Handoff note to yourself

See the checklist in [on-call.md](on-call.md).

```text
HANDOFF: away <from HH:MM> to <until HH:MM> UTC
Faults active: none (make gameday-status, label check)
Argo CD: all Synced and Healthy
Open: <incidents and tickets, next step each>
Budget left: browse <x> %, add-to-cart <x> %, view-cart <x> %, checkout <x> %
Snoozes: none | <which, until when>
Executor kill switch (M5): <enabled | disabled>
Phone/SMS used this month: <n> of 100
```

## Sources

- PagerDuty, [During an Incident](https://docs.pagerduty.com/ops-guides/incident-response-guide/during-an-incident):
  role announcements, notes and follow-up markers in Slack, "if in doubt, post it out".
- Google SRE book, [Managing Incidents](https://sre.google/sre-book/managing-incidents/): the communications role
  and its periodic updates.
