# Alert log

Status: empty, M3 (#20), 2026-11-13. No alert has fired yet.

Every alert that fires gets a row here, classified, so the lab can report how many alerts were worth acting on,
before and after tuning (goal G5). How the numbers are computed and what "before" and "after" mean:
[../alerting.md](../alerting.md#counting-actionability-before-and-after-tuning). Fill it in at the morning review
([on-call.md](on-call.md)) and after every incident or game day.

## Classes

| Class | Meaning | Counts as actionable? |
|---|---|---|
| `A` actionable | Real or imminent journey damage, a person had to act (or check and decide), and this was the first signal for that incident | Yes |
| `D` duplicate | Same incident as an earlier alert (another policy, tier or journey for the same cause) | No |
| `T` transient | Closed before anyone acted, with no action needed and no lasting budget damage | No |
| `F` false | No shopper impact at all (a prober bug, a metric gap, a telemetry delay) | No |
| `R` wrong route | Real, but at the wrong urgency: a page that should have been a ticket, or the reverse | No |

Game-day alerts are classified the same way and flagged with `GD<n>`, so the report can show the numbers with and
without game days. A **missed** incident (journey damage that no alert caught, found by a person or a dashboard)
gets a row with class `M` and no policy.

## Log

| Opened (UTC) | Source | Policy or monitor | Tier | Journey | Incident | Class | Acked after | Phone/SMS | Note |
|---|---|---|---|---|---|---|---|---|---|
| | | *None yet* | | | | | | | |

`Source` is `Cloud Monitoring` or `Datadog` (the 14-day window, #22, [../datadog-window.md](../datadog-window.md)).
`Incident` is the postmortem id, the Slack thread link, or `none`. `Acked after` is minutes from the first page to
acknowledgement (pages only). The weekly summary counts Cloud Monitoring alerts, the only ones that page; Datadog
rows feed the comparison between the two tools during the window.

## Weekly summary

| Week (Mon, UTC) | Alerts | Pages | Tickets | Actionable pages | Actionable share (pages) | Pages per incident | Night pages | Missed | Median ack (min) | Phone/SMS this month | Tuning in force |
|---|---|---|---|---|---|---|---|---|---|---|---|
| | | | | | | | | | | | |

`Tuning in force` names the alert configuration that week: `baseline` (as generated) or the pull request that
changed it.
