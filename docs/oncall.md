# On-call wiring: how an alert reaches a phone and the agent

Status: planned for M3 (issue #21); nothing here has been applied yet. This page is the wiring:
what sends what where, how to set it up, and how to test it. The policy (on-call hours, severities,
handoff, how the phone/SMS allowance is spent) is in [incidents/on-call.md](incidents/on-call.md)
and [incidents/severity.md](incidents/severity.md). Alert thresholds are in
[slo/slo-design.md](slo/slo-design.md). Decision: D6 in [PLAN.md](../PLAN.md) (PagerDuty free plan,
Slack free workspace).

## The path

```
SLO burn-rate alert policies (Cloud Monitoring; generated into infra/envs/lab/slo_generated.tf.json)
  │  page:   fast burn, medium burn, journey without probe data  → local.incident_notification_channels
  │  notify: slow burn, spend-guard watchdog                     → local.incident_ticket_notification_channels
  ▼
notification channels (infra/envs/lab/notifications.tf)
  ├─ "mamori-shop"              pagerduty, page only ──▶ PagerDuty service mamori-shop (infra/envs/oncall)
  │                                                       └▶ escalation policy mamori-default
  │                                                           └▶ schedule mamori-primary ─▶ the on-call person
  │                                                               push now, phone call after 5 min (own rules)
  │                                                       └▶ Slack #incidents: incident card
  ├─ "mamori alerts (email)"    email, page and notify ─▶ inbox (second path if PagerDuty delivery fails;
  │                                                        only if alert_email is set)
  └─ "incident-alerts (Pub/Sub)" pubsub, page and notify ─▶ topic incident-alerts
                                                           └▶ subscription mamori-agent-incidents (M5)
                                                               └▶ incident agent ─▶ Slack: summary, root cause,
                                                                  one proposed action to approve
```

| Alert | Severity | Goes to | Phone? |
|---|---|---|---|
| Fast or medium burn of a journey's error budget | `CRITICAL` | PagerDuty, email, Pub/Sub | yes |
| A journey's prober stopped reporting | `CRITICAL` | PagerDuty, email, Pub/Sub | yes |
| Slow burn | `WARNING` | email, Pub/Sub | no |
| Spend guard logged an error, or handled no budget message for 23.5 hours | `WARNING` | email, Pub/Sub | no |

Why it is split this way:

- **Only pages reach PagerDuty.** Cloud Monitoring's PagerDuty channel shows every alert as
  `critical`, whatever its severity, unless PagerDuty orchestration rules remap it, so PagerDuty
  can't tell a slow burn from a fast one by default. Sending slow burn elsewhere keeps the free
  plan's 100 phone/SMS notifications a month for real pages.
- **Email gets everything.** Google recommends a second channel type next to PagerDuty, because
  PagerDuty, webhook and Slack notifications go through the same delivery service on Google's
  side. Email also keeps a record of the alerts that don't page. It is optional in Terraform
  (`alert_email`), but without it only the agent sees the alerts that don't page.
- **Pub/Sub gets everything.** The agent (M5) sees every alert, slow burns included; it only
  proposes.

### Inside PagerDuty

| Object | Setting | Effect |
|---|---|---|
| Service `mamori-shop` | Urgency high (PagerDuty's default) | Every incident pages |
| | Acknowledgement timeout 30 min | An acknowledged incident still open after 30 minutes pages again |
| | Auto-resolve off | Cloud Monitoring resolves the incident when its alert closes |
| Escalation policy `mamori-default` | One rule: the schedule; 15 min; repeat twice | Unacknowledged pages notify again after 15 and 30 minutes, then stop |
| Schedule `mamori-primary` | One weekly shift (Monday 09:00 Tokyo), one person | Always the maintainer today; up to 5 people on the free plan |

**Acknowledge in PagerDuty; don't resolve there.** Cloud Monitoring sends the resolve when its
alert closes (the condition clears, or no data for 30 minutes with `auto_close`). If you resolve in
PagerDuty while the alert is still open, Monitoring reopens it in PagerDuty. You can't close a
Monitoring alert from PagerDuty.

**One failure can open several incidents.** A dependency outage burns several journeys and both of
their signals at once (prober and mesh), so expect up to about six incidents together (an estimate
from the generated policies; see slo/slo-design.md). Each has its own notifications.

### Your notification rules (set in the PagerDuty app, not in Terraform)

Phone numbers and notification rules are personal, so each person sets them in **My Profile >
Notification Rules**. Suggested for high urgency:

1. Push notification immediately (unlimited on the free plan). In the mobile app, allow PagerDuty
   to override Do Not Disturb for high-urgency notifications, if your phone supports it.
2. Phone call after 5 minutes, only if still unacknowledged (counts against the 100 a month).

Acknowledging every open incident from the app within 5 minutes then costs no phone call. The
policy page decides the final rules; the count of pages a month is an alert-quality measure
([prd.md](prd.md)).

## Set up, once

Do these in order. Steps 1–4 can happen before the cluster exists. No step puts a PagerDuty key in
Git, GitHub or Terraform state.

1. **PagerDuty account.** Sign up for the free plan with the address you want paged. Install the
   mobile app, sign in, add your phone number, and set your notification rules (above).
2. **PagerDuty as code.** Create a REST API key (**Integrations > API Access Keys**), then apply
   `infra/envs/oncall` from your laptop with `PAGERDUTY_TOKEN` and `TF_VAR_oncall_user_email` set.
   Step by step, including the free-plan cleanup before the first apply:
   [infra/envs/oncall/README.md](../infra/envs/oncall/README.md). CI never touches this root.
3. **Cloud Monitoring integration, by hand.** In PagerDuty: **Services > mamori-shop > Integrations
   > Add an integration**, type **Events API v1**, name "Google Cloud Monitoring". Its integration
   key stays visible on that page; you need it in step 7. (Why by hand, and when to add an Events
   API v2 integration too: [infra/envs/oncall/README.md](../infra/envs/oncall/README.md).)
4. **Slack.** Create a free workspace (or use one you own) and a public channel `#incidents`. In
   PagerDuty, **Integrations > Extensions > Slack** (or **Configuration > Integrations >
   Extensions**), **Add Workspace**, then **Allow** in Slack (needs a workspace owner or admin:
   you). Link your PagerDuty and Slack accounts when asked. In `#incidents`, run `/pd connect`,
   choose the PagerDuty account, service `mamori-shop` and a responder notification (the one with
   acknowledge and resolve buttons). What the free plans allow, as of 2026-11-13:
   - PagerDuty Free lists a "basic chat experience" for Slack. Creating, archiving or posting to
     Slack channels from PagerDuty needs Professional, so incidents share `#incidents`. Whether the
     free plan's incident cards carry the acknowledge and resolve buttons is not verified yet.
   - Slack Free shows 90 days of history and allows 10 apps. PagerDuty takes one; the agent's
     approval app (M5) takes another; Datadog's, if used in its window, a third.
   - Slack is a convenience, not the page: the phone is paged by PagerDuty whether or not Slack
     works.
5. **Alert email and CI permissions.**
   - Put your address in `.env` as `ALERT_EMAIL` (or export `TF_VAR_alert_email`), and in the
     GitHub repository secret `ALERT_EMAIL`, which CI passes as `TF_VAR_alert_email`. The variable
     is optional: empty means no email channel, so plan and `make down` never need it. But a CI
     apply without the secret removes the email channel that `make up` created, so set both.
   - `tf-apply` needs `roles/monitoring.notificationChannelEditor` (channels),
     `roles/monitoring.alertPolicyEditor` and `roles/logging.configWriter` (the spend-guard
     watchdog) on the project. They are granted in `infra/bootstrap/iam.tf`, so run
     `make bootstrap` once after that change, before the first CI apply. The topic and its IAM
     are already covered by `roles/pubsub.admin`.
6. **Apply the lab** (`make up`, or the `terraform-apply` workflow). This creates the channels
   (the PagerDuty one with a placeholder key), topic `incident-alerts`, its grant for Cloud
   Monitoring, and the spend-guard watchdog.

   If the apply stops at `google_pubsub_topic_iam_member.incident_alerts_monitoring` with "service
   account ... does not exist": Cloud Monitoring creates its notification service agent
   (`service-<project number>@gcp-sa-monitoring-notification.iam.gserviceaccount.com`) around the
   project's first Pub/Sub channel, and Google documents that only for the console. Create it, then
   apply again. Google's documented way: **Monitoring > Alerting > Edit notification channels >
   Pub/Sub > Add new**, then cancel (Google: the form doesn't have to be finished for the agent to
   be created). Or from a shell, since Google lists this account as the primary service agent of
   `monitoring.googleapis.com` (not tested yet):

   ```sh
   gcloud beta services identity create --service=monitoring.googleapis.com --project="$PROJECT_ID"
   ```

7. **Set the PagerDuty key on the channel, by hand.** Terraform creates the channel with a
   placeholder key and then leaves it alone (`ignore_changes = all`), so the real key is set once
   here and stays out of the state. Until it is set, **nothing pages**. Either:
   - Console: **Monitoring > Alerting > Edit notification channels > PagerDuty**, edit
     `mamori-shop`, paste the key from step 3 into **Service key**, save.
   - Shell (the key is read without echo and stays out of your shell history; it is visible in
     the process list while gcloud runs):

     ```sh
     channel="$(terraform -chdir=infra/envs/lab output -raw pagerduty_notification_channel)"
     read -rs -p 'PagerDuty integration key: ' PD_KEY; echo
     gcloud beta monitoring channels update "$channel" \
       --update-channel-labels="service_key=${PD_KEY}" --project="$PROJECT_ID"
     unset PD_KEY
     ```

   **Repeat this after every `make up` that follows a `make down`**: `make down` deletes the
   channel, and the new one starts with the placeholder again. An apply on a running lab doesn't
   touch the key. To change the channel itself, run
   `terraform apply -replace=google_monitoring_notification_channel.pagerduty` and set the key
   again.
8. **Test** the whole path (next section).

`make down` deletes the channels (even if a hand-made policy still uses them), the topic and the
watchdog with the rest of the lab; PagerDuty, its integrations and Slack stay (all free).

## Test a page

Each test can cost one phone call or SMS if you don't acknowledge it within your rules' delay.

**1. PagerDuty alone (no cluster needed).** In the PagerDuty web or mobile app, create an incident
by hand on service `mamori-shop` (**Incidents > New Incident**). Your phone should get a push within
a minute, and `#incidents` an incident card. Acknowledge it, then resolve it.

If you have added an Events API v2 integration for another tool, you can test it the same way that
tool will call it:

```sh
read -rs -p 'Events API v2 integration key: ' ROUTING_KEY; echo
curl -sS https://events.pagerduty.com/v2/enqueue -H 'Content-Type: application/json' -d @- <<EOF
{"routing_key": "${ROUTING_KEY}", "event_action": "trigger", "dedup_key": "mamori-test-page",
 "payload": {"summary": "mamori test page: acknowledge me", "source": "docs/oncall.md", "severity": "critical"}}
EOF
# Expect: {"status":"success","message":"Event processed","dedup_key":"mamori-test-page"}
```

Resolve it with the same request and `"event_action": "resolve"` (the `payload` can be left out).

**2. Cloud Monitoring end to end (cluster up; run after every step 7).** A temporary alert policy
that fires at once (the cluster has nodes) and sends to the paging channels. Expect, within a few
minutes: a PagerDuty incident for the policy, an email (if `alert_email` is set), and a Pub/Sub
message. No PagerDuty incident while the email arrives means the key is still the placeholder.

```sh
channels="$(terraform -chdir=infra/envs/lab output -json incident_notification_channels)"
policy="$(mktemp)"
cat > "$policy" <<EOF
{
  "displayName": "mamori test page (delete me)",
  "combiner": "OR",
  "severity": "CRITICAL",
  "notificationChannels": ${channels},
  "alertStrategy": {"autoClose": "1800s"},
  "documentation": {"mimeType": "text/markdown",
    "content": "Paging test from docs/oncall.md. Acknowledge in PagerDuty, then delete this policy."},
  "conditions": [{
    "displayName": "The cluster has nodes",
    "conditionThreshold": {
      "filter": "resource.type = \"k8s_node\" AND metric.type = \"kubernetes.io/node/cpu/allocatable_cores\"",
      "comparison": "COMPARISON_GT",
      "thresholdValue": 0,
      "duration": "0s",
      "aggregations": [{"alignmentPeriod": "60s", "perSeriesAligner": "ALIGN_MEAN", "crossSeriesReducer": "REDUCE_SUM"}]
    }
  }]
}
EOF
gcloud monitoring policies create --policy-from-file="$policy" --project="$PROJECT_ID"
```

Clean up in this order: acknowledge in PagerDuty; delete the policy; if its incident is still open
in **Monitoring > Alerting > Incidents**, close it there; resolve the PagerDuty incident if it is
still open. A forgotten test policy doesn't block `make down`: the channels are deleted with
`force_delete`.

```sh
gcloud monitoring policies list --project="$PROJECT_ID" \
  --filter='displayName="mamori test page (delete me)"' --format='value(name)' |
  xargs -n1 gcloud monitoring policies delete --project="$PROJECT_ID" --quiet
```

**3. Pub/Sub only.** To see what the agent receives, attach a short-lived subscription before
test 2 and pull from it afterwards (once the agent runs, it gets the test incident too):

```sh
gcloud pubsub subscriptions create incident-alerts-test --topic=incident-alerts \
  --expiration-period=1d --message-retention-duration=10m --project="$PROJECT_ID"
# ... run test 2 ...
gcloud pubsub subscriptions pull incident-alerts-test --auto-ack --limit=5 --project="$PROJECT_ID"
gcloud pubsub subscriptions delete incident-alerts-test --project="$PROJECT_ID"
```

The message is Cloud Monitoring's incident JSON (`{"incident": {...}, "version": "1.2"}`). The
Pub/Sub channel also has **Send test notification** in **Monitoring > Alerting > Edit notification
channels**.

## Spend-guard watchdog

The spend guard is the only thing that stops spending when the budget is gone, and it fails
quietly: when a retry can't help, it logs an error and acknowledges the message. Two policies in
`notifications.tf` notify (email and Pub/Sub, no page):

| Policy | Fires when | Notes |
|---|---|---|
| `spend-guard logged an error` | any spend-guard log entry at `ERROR` or above: permission denied, node pool not found, a bad or non-USD message | log-based; at most one notification an hour while errors go on |
| `spend-guard handled no budget message for 23.5 hours` | log-based metric `spend-guard-outcomes` (one line per handled message) has no data for 23.5 hours | Cloud Billing sends several messages a day; 23.5 hours is Monitoring's longest absence window; it starts counting only after the first message, so a fresh lab stays quiet |

Both link to the runbook [runbooks/spend-guard.md](runbooks/spend-guard.md).

## When paging breaks

| Symptom | Likely cause | Check |
|---|---|---|
| Email arrives, no PagerDuty incident | The channel still has the placeholder key (a new channel after `make down` / `make up`), or a wrong key | Set the key again (step 7), then run test 2 |
| PagerDuty incident, but no push or call | Notification rules, phone settings, or nobody on the schedule | PagerDuty **My Profile > Notification Rules**; **People > On-Call Schedules** |
| No Pub/Sub messages | Cloud Monitoring can't publish | the topic's IAM has `roles/pubsub.publisher` for the notification service agent (step 6) |
| No alerts at all while the shop is down | Prober or collectors down with the cluster | the per-journey "no probe data" alerts page; Cloud Monitoring keeps working outside the cluster |
| The email channel disappeared after a CI apply | `ALERT_EMAIL` secret missing, so CI applied an empty `alert_email` | step 5 |

## Where each secret lives

| Secret | Where it is | Where it never is |
|---|---|---|
| PagerDuty REST API key | Your shell (`PAGERDUTY_TOKEN`) while applying `infra/envs/oncall`; then deleted or in a password manager | Git, GitHub, Terraform state |
| Cloud Monitoring integration key (Events API v1) | PagerDuty (integration page); the Cloud Monitoring channel, set by hand | Git, GitHub, Terraform state, shell history |
| Events API v2 integration key (only if added) | PagerDuty; the tool that uses it | Git, Terraform state |
| Alert email address | `.env` (gitignored) or your shell; GitHub secret `ALERT_EMAIL`; state of `infra/envs/lab` | Git, plan output |

Terraform state holds only the placeholder `placeholder-set-by-hand-see-docs` for the PagerDuty
channel. A leaked integration key lets someone page you or resolve an open page; it can't read or
change the PagerDuty account. To rotate it, delete the integration in PagerDuty, add it again, and
set the new key on the channel (step 7).

## Limits and risks

- **Free plans.** PagerDuty Free: 5 users, 1 schedule, 1 escalation policy, 100 phone/SMS
  notifications a month, no noise reduction, no per-incident Slack channels. A noisy alert that
  spends the allowance is an alert-fatigue finding to fix, not a reason to upgrade.
- **The key step after `make up`.** A rebuilt lab doesn't page until step 7 is done again. That
  is the price of keeping the key out of the state; test 2 shows whether it was done.
- **One person on call.** Nobody to escalate to; the policy repeats instead. Time to acknowledge is
  measured, not promised.
- **Events API v1.** Cloud Monitoring's PagerDuty channel speaks v1, so the service has a v1
  integration for it. PagerDuty calls v1 "legacy"; no end date was published when this was
  written.
- **Who can publish to `incident-alerts`.** Cloud Monitoring's notification agent (topic grant),
  project Owners and Editors, and `tf-apply` (Pub/Sub Admin). A fake message makes the agent
  investigate a fake incident; it can only propose, and the executor acts only after a human
  approves in Slack.
- **Not verified yet** (no accounts exist): that Cloud Monitoring accepts the placeholder service
  key when it creates the channel, and that `gcloud beta monitoring channels update
  --update-channel-labels` sets the key as the console does; that Cloud Monitoring's PagerDuty
  channel delivers to an Events API v1 integration key as Google's guide describes; that the free
  plan accepts the shift-based schedule and a repeating escalation policy through the API; whether
  the notification service agent appears with the first Pub/Sub channel created by API (step 6 has
  the fix); the watchdog's filters against real spend-guard logs; the exact PagerDuty and Slack
  menu names, which change often.
