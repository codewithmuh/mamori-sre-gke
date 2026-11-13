# Incident response

Status: draft, M3 (#20), 2026-11-13. Written before anything runs: no page has been sent, no incident handled and no
game day run yet. Every target here is a starting point, and every number is an estimate until it is measured.

How this lab notices, handles and learns from incidents that hurt its critical user journeys (CUJs). One maintainer
runs it alone, so each page says how a team practice was adapted for one person, and where it can't be.

## Pages

| Page | What it covers |
|---|---|
| [severity.md](severity.md) | SEV1–SEV4, defined by journey impact and error-budget burn; when a postmortem is required |
| [roles.md](roles.md) | Incident commander, operations, communications, scribe; how one on-call person covers all four; the AI agent as an advisor, never the commander |
| [on-call.md](on-call.md) | Hours, what pages, the 100 phone/SMS notifications a month, handoff, escalation, on-call load |
| [communication.md](communication.md) | Slack templates: declare, update, resolve; game-day and handoff notes |
| [postmortem-template.md](postmortem-template.md) | The blameless postmortem to copy, with its YAML front matter |
| [postmortems/](postmortems/README.md) | Written postmortems, naming rules, review steps, index |
| [game-days.md](game-days.md) | The plan for the six game days (faults F01–F06): hypotheses, blast radius, abort criteria, measurements |
| [alert-log.md](alert-log.md) | Every alert classified as actionable or not, for the before/after tuning numbers |

Related pages owned elsewhere:

- [../alerting.md](../alerting.md): alert design, the alert inventory, how actionability is counted.
- [../runbooks/](../runbooks/README.md): one runbook per journey alert (`cuj-browse.md`, `cuj-add-to-cart.md`,
  `cuj-view-cart.md`, `cuj-checkout.md`).
- [../oncall.md](../oncall.md) (#21): how PagerDuty, the schedule, the escalation policy and the notification
  channels are wired in Terraform. This folder is the policy; that page is the wiring.
- [../game-days.md](../game-days.md) and `chaos/faults/` (#23): the fault library and the `gameday` CLI that starts
  and stops faults.
- [../slo/error-budget-policy.md](../slo/error-budget-policy.md) (#19): what happens when a journey's budget is spent.
- [../agent.md](../agent.md) (#24) and [../executor.md](../executor.md) (#25): the AI incident agent and the executor
  that runs approved actions (M5).

## An incident from start to end

```
burn-rate alert (Cloud Monitoring) ─┬─ page tier ──▶ PagerDuty "mamori-shop" ──▶ phone (push, then a call)
                                    ├─ ticket tier ─▶ email
                                    └─ both ────────▶ Pub/Sub incident-alerts ──▶ AI agent (from M5)
or a human notices something wrong ──────────────────────────────────────────▶ declare anyway
```

1. **Detect.** A journey's burn-rate alert fires ([../alerting.md](../alerting.md)), or someone notices a problem no
   alert caught (that is a finding in itself).
2. **Acknowledge** the page in PagerDuty and open the runbook linked in the alert.
3. **Declare** in Slack `#incidents` with the template, and set a first severity ([severity.md](severity.md)).
4. **Mitigate first, diagnose second.** Stop the damage to shoppers, then find the cause. Google's incident
   management chapter puts it as: stop the bleeding and restore service, while keeping evidence for root-cause
   analysis.
5. **Update** on a fixed cadence ([communication.md](communication.md)).
6. **Resolve** when the journey's SLI has been back within target for 15 minutes and the alert has closed.
7. **Postmortem** when severity.md says so: blameless, timeline from telemetry, follow-ups as GitHub issues.
8. **Classify every alert** that fired in [alert-log.md](alert-log.md): actionable or not.

## Team practice, adapted for one person

| Practice | In a team | In this lab |
|---|---|---|
| Incident roles | Separate people for commander, operations, communications and scribe | One person holds all four, in a fixed order, with timers; machines keep most of the timeline ([roles.md](roles.md)) |
| On-call rotation | Several people, shifts of 12 hours at most | One person, one schedule, pages 24/7; time to acknowledge is measured, not promised ([on-call.md](on-call.md)) |
| Escalation | To a secondary on-call, then to experts | PagerDuty repeats the page to the same person; "escalation" means bigger mitigations and outside status pages |
| Communication | Status page, stakeholder and executive updates | A Slack thread per incident is the record; no status page (PagerDuty's free plan has none, and shoppers are synthetic) |
| Postmortem review | A meeting with the people involved | A draft, a 24-hour pause, a re-read against a checklist, then a public pull request anyone can comment on |
| Game days | A game master injects faults the responders don't know about | The `gameday` CLI starts and stops faults on a timer; some runs are "blind" (fault and start time picked at random) |
| Second opinion | A colleague | From M5, the AI agent's ranked hypotheses, treated as advice to check |

## Sources

Practices here are adapted from these public sources (all checked 2026-11-13):

- Google SRE book: [Managing Incidents](https://sre.google/sre-book/managing-incidents/),
  [Being On-Call](https://sre.google/sre-book/being-on-call/),
  [Postmortem Culture](https://sre.google/sre-book/postmortem-culture/),
  [Monitoring Distributed Systems](https://sre.google/sre-book/monitoring-distributed-systems/),
  [Accelerating SREs to On-Call and Beyond](https://sre.google/sre-book/accelerating-sre-on-call/).
- Google SRE workbook: [Incident Response](https://sre.google/workbook/incident-response/),
  [On-Call](https://sre.google/workbook/on-call/),
  [Postmortem Culture: Learning from Failure](https://sre.google/workbook/postmortem-culture/),
  [Alerting on SLOs](https://sre.google/workbook/alerting-on-slos/),
  [Example error budget policy](https://sre.google/workbook/error-budget-policy/).
- Google: [Incident Management Guide](https://sre.google/resources/practices-and-processes/incident-management-guide/).
- PagerDuty's public incident response guide: [Severity Levels](https://docs.pagerduty.com/ops-guides/incident-response-guide/severity-levels),
  [Different Roles](https://docs.pagerduty.com/ops-guides/incident-response-guide/different-roles),
  [During an Incident](https://docs.pagerduty.com/ops-guides/incident-response-guide/during-an-incident),
  [Postmortem Best Practices](https://docs.pagerduty.com/ops-guides/postmortem-best-practices).
- [Principles of Chaos Engineering](https://principlesofchaos.org/) for the game-day method.
- PagerDuty and Slack free-plan limits: [decision-facts.md](../decision-facts.md) (D6), and
  [PagerDuty pricing](https://www.pagerduty.com/pricing/incident-management/).

## Not verified yet

- Nothing here has met a real page. The first game day (planned for M3) is the first test.
- The alert names, thresholds and routing follow what #18 (SLO generator) and #21 (paging) build; if they change,
  [../alerting.md](../alerting.md) and the runbooks change with them.
- Every duration and count is a target or an estimate until the alert log and the postmortems measure it.
