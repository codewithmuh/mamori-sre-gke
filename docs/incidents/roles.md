# Incident roles

Status: draft, M3 (#20), 2026-11-13. Not yet used on a real incident.

Google's incident management (IMAG) and PagerDuty's incident response guide both split an incident into roles, so
that nobody both fixes and coordinates. This lab has one person. The roles still matter: they are the jobs that must
get done, and naming them makes it obvious when one is being skipped.

## The four roles

| Role | Does | Comes from |
|---|---|---|
| **Incident commander (IC)** | Declares the incident and sets its severity. Holds the overall picture, decides the plan, decides when to escalate and when it is over, assigns the postmortem. Holds every role not handed to someone else | IMAG's incident commander ([workbook](https://sre.google/workbook/incident-response/)); PagerDuty's IC, who "is NOT a resolver" ([roles](https://docs.pagerduty.com/ops-guides/incident-response-guide/different-roles)) |
| **Operations (ops)** | The only role that changes the system during the incident. Mitigates, then fixes. Reports each change and its effect | IMAG's operations lead ([SRE book](https://sre.google/sre-book/managing-incidents/)) |
| **Communications (comms)** | Posts the declaration, regular updates and the resolution in the incident thread ([communication.md](communication.md)) | IMAG's communications lead |
| **Scribe** | Keeps the timeline: times, findings, actions, decisions | PagerDuty's scribe |

IMAG also has a planning role (bugs, handoffs, tracking what was changed from normal). Here the IC covers it: it is
small, and most of it happens after the incident in the postmortem.

## One person, four roles

IMAG says the commander holds every role not yet delegated. In this lab nothing can be delegated, so the on-call
person holds all four. What keeps that workable:

1. **A fixed order when the page arrives.**
   1. *IC:* acknowledge the page; read the alert's journey and tier; pick a first severity
      ([severity.md](severity.md)), higher if unsure.
   2. *Comms:* post the one-line declaration in `#incidents` ([communication.md](communication.md)). Under a minute;
      it starts the clock everyone (and the postmortem) will use.
   3. *Ops:* follow the runbook's "first five minutes".
   4. *Comms:* set a phone timer for the update cadence (15 minutes for SEV1, 30 for SEV2) and post when it rings.
   5. *IC:* at every timer, step back for one minute: is the mitigation working? Should severity change? Is it time
      for a bigger mitigation ([on-call.md](on-call.md), escalation)? A separate commander would ask exactly these
      questions; the timer stands in for them.
2. **One change at a time.** Without a separate commander, the classic solo mistake is stacking changes until
   nobody can tell which one helped. Ops makes one change, waits for the journey's SLI to move (a few minutes),
   posts the result, then decides the next one.
3. **Machines do most of the scribing.** Slack timestamps every message; PagerDuty logs every notification and
   acknowledgement; Cloud Monitoring keeps each alert's open and close times; Argo CD keeps sync history; GKE sends
   Kubernetes events to Cloud Logging; the `gameday` CLI writes a timeline file per run; from M5 the agent and the
   executor write JSON audit lines. The scribe only has to post what leaves no trace: decisions and their reasons.
4. **Tag what you post** so the thread can be read as a timeline later:

   | Tag | Use it for | Example |
   |---|---|---|
   | `[decision]` | A choice and why | `[decision] no rollback yet: last deploy was 3 days ago` |
   | `[action]` | A change made to the system | `[action] removed VirtualService f01-currency-abort` |
   | `[finding]` | A fact learned | `[finding] currencyservice returns UNAVAILABLE on every call` |
   | `[rca]` | The root cause, once confident | `[rca] Istio abort on currencyservice, 100 % of calls` |
   | `[update]` | The cadence update (template) | see [communication.md](communication.md) |

   The first `[rca]` message is the human "time to root cause" that the agent evaluation compares against (#26).
5. **Know when to stop.** After two hours on a SEV1, or at night once the journey is mitigated, stop. Leave the
   remaining fix for a rested person (tomorrow's you), with a handoff note. The SRE book's advice to watch your own
   state and ask for support applies doubly when there is nobody to hand over to.

### What one person cannot replace

- **A real-time second opinion.** Nobody checks the commander's reasoning during the incident. From M5 the agent's
  hypotheses help, but they are advice, not review.
- **Two-person approval.** The executor (M5) only runs an action after a human approves it in Slack. Here that human
  is the same person who is fixing the incident. It still stops the agent from acting alone; it does not give a
  second human check.
- **Rested responders.** There is no next shift. On-call load is tracked for this reason ([on-call.md](on-call.md)).

## The AI incident agent: an advisor, never the commander

From M5 (#24, #25). Every alert also reaches the agent through Pub/Sub. It investigates with read-only tools and
posts in the incident's Slack thread: the journeys hit, an impact summary, a proposed severity, ranked root-cause
hypotheses with evidence, and at most one proposed action, which the executor turns into an approval request.

| The agent may | The agent never |
|---|---|
| Investigate with read-only access (metrics, logs, traces, Kubernetes, Argo CD, Istio config, past postmortems) | Holds write credentials (decision D9: privilege separation) |
| Propose a severity | Sets the severity, declares or resolves an incident |
| Rank hypotheses and show its evidence | Decides what is true; the IC checks the evidence |
| Propose one allow-listed action (restart, scale, rollback, remove a fault) | Runs it: the executor runs it only after a human approves, once per incident |
| Write a timestamped investigation record | Approves its own proposal, or acts outside the incident thread |

How the commander uses it:

- **Read its output as a hypothesis, not a finding.** Logs and alert text can carry prompt injection
  ([../threat-model.md](../threat-model.md)); the guardrails sit in RBAC and the executor, not in the prompt.
- **Record each decision on a proposal:** `[decision] approve proposal <proposalId>: evidence matches` or
  `[decision] reject proposal <proposalId>: wrong service, mesh shows cartservice healthy`.
- **Don't wait for it.** The runbooks work without the agent. If it is slow, down or wrong, the process is the same.
- **Score it in the postmortem:** was its top hypothesis right, how long did it take compared with the human
  `[rca]`, and was its proposed action the right one? Those numbers feed `EVALS.md` (#26).

Why it is never the commander: accountability stays with a person; the agent can be fed misleading input; and its
accuracy is only known after the evaluation. Low-risk actions may later run automatically and revert if the SLO
doesn't recover (PLAN.md section 4), but only after the evaluation shows it is safe, and the commander stays human
even then.

## Sources

- Google SRE book, [Managing Incidents](https://sre.google/sre-book/managing-incidents/): the four roles, "stop the
  bleeding", and the best-practice list (prioritize, prepare, trust, introspect, consider alternatives, practice).
- Google SRE workbook, [Incident Response](https://sre.google/workbook/incident-response/): IMAG, the 3Cs
  (coordinate, communicate, control), "declare incidents early and often".
- PagerDuty, [Different Roles](https://docs.pagerduty.com/ops-guides/incident-response-guide/different-roles) and
  [During an Incident](https://docs.pagerduty.com/ops-guides/incident-response-guide/during-an-incident): IC, deputy,
  scribe, subject-matter experts, liaisons.
