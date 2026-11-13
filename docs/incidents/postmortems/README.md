# Postmortems

Status: draft, M3 (#20), 2026-11-13. **No postmortems yet:** nothing has run.

One blameless postmortem per incident that needs one ([../severity.md](../severity.md) lists the triggers), and one
for every game day ([../game-days.md](../game-days.md)). Start from
[../postmortem-template.md](../postmortem-template.md).

## Index

| Date | Postmortem | Severity | Journeys | Fault | Status |
|---|---|---|---|---|---|
| | *None yet* | | | | |

## File names and front matter

- **Path:** `docs/incidents/postmortems/YYYY-MM-DD-<slug>.md`. The date is the UTC day the impact started; the slug
  is short, lowercase, words joined by hyphens (`2026-12-08-cart-errors`). Two incidents on one day get
  different slugs.
- **Front matter:** YAML at the top of the file. The first six keys are fixed, because the AI agent (#24) reads them
  to answer "has this happened before?". Keep every value a quoted string (or a list of them, or `null`), so YAML
  doesn't turn dates and times into other types.

| Key | Required | Type and format | Example |
|---|---|---|---|
| `id` | yes | string, the file name without `.md` | `"2026-12-08-cart-errors"` |
| `date` | yes | string, `YYYY-MM-DD`, UTC day the impact started | `"2026-12-08"` |
| `faultId` | yes | string, the game-day fault id, or `null` for a real incident | `"F01-currency-abort"` |
| `services` | yes | list of strings, Deployment names in `boutique` at the root of it, sorted; may be empty | `["cartservice"]` |
| `rootCause` | yes | string, one line | `"cartservice ran out of memory under load and restarted in a loop"` |
| `severity` | yes | string, final severity | `"SEV1"` |
| `cujs` | no | list of CUJ ids hit | `["browse", "add-to-cart", "view-cart", "checkout"]` |
| `timeline` | no | map of RFC3339 UTC strings or `null`: `impactStart`, `faultStart`, `detected`, `acknowledged`, `rootCauseIdentified`, `mitigated`, `resolved` | `detected: "2026-12-08T05:14:30Z"` |

Readers must ignore keys they don't know, so the optional keys can grow without breaking the agent.

**Who reads these files.** People, and from M5 the agent. The agent reads only files in this folder whose names
start with a date (this README is skipped) and never `chaos/faults/`, which holds the expected answers. A game-day
postmortem does carry its `faultId` and root cause, so the agent evaluation (#26) can leave out postmortems of the
fault it is testing; the agent can filter on `faultId`.

## Writing and review

Adapted from PagerDuty's [postmortem process](https://docs.pagerduty.com/ops-guides/postmortem-best-practices/step-by-step)
and Google's [postmortem culture](https://sre.google/workbook/postmortem-culture/), for one person:

1. **Owner.** The incident commander, which here is always the on-call person.
2. **Draft soon.** Within 2 days for SEV1, 5 days for anything else (PagerDuty: 3 calendar days for Sev-1, 5 business
   days for Sev-2). Copy the Slack thread out first: the free plan shows only 90 days.
3. **Timeline from telemetry**, with a source on every row, built forward from before the impact.
4. **Pause, then re-read.** Wait at least 24 hours, then check the draft against the checklist below. The pause
   stands in for the reviewers a team would have.
5. **Pull request.** Status `In review`. The repo is public, so anyone can comment. Merging sets `Reviewed`.
6. **Follow-ups.** Each action item becomes a GitHub issue labelled `postmortem` that links back to the postmortem.
   When every P0 and P1 item is done, set the status to `Closed` and update the index above.

**Review checklist:**

- [ ] Blameless: no person is a cause; no "should have"; roles, not names.
- [ ] Every timeline row has a source; times are UTC; the timeline starts before the impact.
- [ ] Impact has numbers (requests, probes, minutes, budget), each with its query.
- [ ] The root cause goes below the trigger, and contributing factors include process (alerts, runbooks, review).
- [ ] Each action item starts with a verb, is specific, has an end, an owner, a priority and an issue.
- [ ] Every alert that fired is classified in [../alert-log.md](../alert-log.md).
- [ ] Game days: hypothesis against outcome, planned against actual budget.
- [ ] Nothing that shouldn't be public: tokens, keys, personal data.
- [ ] Front matter keys and formats as above; `id` matches the file name.
