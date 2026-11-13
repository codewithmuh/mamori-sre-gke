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
  │  notify: slow burn                                           → local.incident_ticket_notification_channels
  ▼
notification channels (infra/envs/lab/notifications.tf)
  ├─ "mamori-shop"              pagerduty, page only ──▶ PagerDuty service mamori-shop (infra/envs/oncall)
  │                                                       └▶ escalation policy mamori-default
  │                                                           └▶ schedule mamori-primary ─▶ the on-call person
  │                                                               push now, phone call after 5 min (own rules)
  │                                                       └▶ Slack #incidents: incident card
  ├─ "mamori alerts (email)"    email, page and notify ─▶ inbox (second path if PagerDuty delivery fails)
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

Why it is split this way:

- **Only pages reach PagerDuty.** Cloud Monitoring's PagerDuty channel shows every alert as
  `critical`, whatever its severity, unless PagerDuty orchestration rules remap it, so PagerDuty
  can't tell a slow burn from a fast one by default. Sending slow burn elsewhere keeps the free
  plan's 100 phone/SMS notifications a month for real pages.
- **Email gets everything.** Google recommends a second channel type next to PagerDuty, because
  PagerDuty, webhook and Slack notifications go through the same delivery service on Google's
  side. Email also keeps a record of the alerts that don't page.
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

Do these in order. Steps 1–4 can happen before the cluster exists.

1. **PagerDuty account.** Sign up for the free plan with the address you want paged. Install the
   mobile app, sign in, add your phone number, and set your notification rules (above).
2. **PagerDuty as code.** Create a REST API key (**Integrations > API Access Keys**), then apply
   `infra/envs/oncall` from your laptop with `PAGERDUTY_TOKEN` and `TF_VAR_oncall_user_email` set.
   Step by step, including the free-plan cleanup before the first apply:
   [infra/envs/oncall/README.md](../infra/envs/oncall/README.md). CI never touches this root.
3. **Slack.** Create a free workspace (or use one you own) and a public channel `#incidents`. In
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
4. **Lab inputs.** `infra/envs/lab` needs two values that are not in Git:

   | Terraform variable | Locally | GitHub (repository secret) |
   |---|---|---|
   | `pagerduty_integration_key` | `TF_VAR_pagerduty_integration_key` | `PAGERDUTY_INTEGRATION_KEY` |
   | `alert_email` | `TF_VAR_alert_email` | `ALERT_EMAIL` |

   ```sh
   key="$(terraform -chdir=infra/envs/oncall output -raw cloud_monitoring_integration_key)"
   printf '%s' "$key" | gh secret set PAGERDUTY_INTEGRATION_KEY     # read from stdin, not argv
   printf '%s' 'you@example.com' | gh secret set ALERT_EMAIL
   export TF_VAR_pagerduty_integration_key="$key" TF_VAR_alert_email='you@example.com'
   ```

   Both are secrets so that GitHub masks them in the public logs, and both variables are
   `sensitive`, so plans posted on pull requests show `(sensitive value)`. CI's plan and apply jobs
   pass them as `TF_VAR_*` (`ci.yml` job `terraform-plan`, `terraform-apply.yml`). Both variables
   are required on purpose: a missing secret fails the plan instead of silently applying a lab that
   can't page.
5. **CI permission.** `tf-apply` needs `roles/monitoring.notificationChannelEditor` on the project
   to manage the channels. It is granted in `infra/bootstrap/iam.tf`, so run `make bootstrap` once
   after that change, before the first CI apply. (The topic and its IAM are already covered by
   `roles/pubsub.admin`.)
6. **Apply the lab** (`make up`, or the `terraform-apply` workflow). This creates the three
   channels, topic `incident-alerts`, and the topic grant for Cloud Monitoring.

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

7. **Test** the whole path (next section).

`make down` deletes the channels and the topic with the rest of the lab; PagerDuty and Slack stay
(both free). The next `make up` recreates the channels with the same key.

## Test a page

Each test can cost one phone call or SMS if you don't acknowledge it within your rules' delay.

**1. PagerDuty alone (no cluster needed).** Sends one event to the service's Events API v2
integration. Your phone should get a push within a minute, and `#incidents` an incident card.

```sh
ROUTING_KEY="$(terraform -chdir=infra/envs/oncall output -raw events_api_v2_integration_key)"
curl -sS https://events.pagerduty.com/v2/enqueue -H 'Content-Type: application/json' -d @- <<EOF
{"routing_key": "${ROUTING_KEY}", "event_action": "trigger", "dedup_key": "mamori-test-page",
 "payload": {"summary": "mamori test page: acknowledge me", "source": "docs/oncall.md", "severity": "critical"}}
EOF
# Expect: {"status":"success","message":"Event processed","dedup_key":"mamori-test-page"}
```

Acknowledge it on the phone, then resolve it with the same request and `"event_action": "resolve"`
(the `payload` can be left out).

**2. Cloud Monitoring end to end (cluster up).** A temporary alert policy that fires at once (the
cluster has nodes) and sends to the paging channels. Expect, within a few minutes: a PagerDuty
incident for the policy, an email, and a Pub/Sub message.

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
still open.

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

## When paging breaks

| Symptom | Likely cause | Check |
|---|---|---|
| Email arrives, no PagerDuty incident | Wrong or rotated key in `pagerduty_integration_key`; integration deleted in the web app | `terraform -chdir=infra/envs/oncall plan` shows the drift; set the secret again from the output (step 4) and apply the lab |
| PagerDuty incident, but no push or call | Notification rules, phone settings, or nobody on the schedule | PagerDuty **My Profile > Notification Rules**; **People > On-Call Schedules** |
| No Pub/Sub messages | Cloud Monitoring can't publish | the topic's IAM has `roles/pubsub.publisher` for the notification service agent (step 6) |
| No alerts at all while the shop is down | Prober or collectors down with the cluster | the per-journey "no probe data" alerts page; Cloud Monitoring keeps working outside the cluster |
| CI plan fails: "No value for required variable" | `PAGERDUTY_INTEGRATION_KEY` or `ALERT_EMAIL` secret missing | step 4 |

## Where each secret lives

| Secret | Where it is | Where it never is |
|---|---|---|
| PagerDuty REST API key | Your shell (`PAGERDUTY_TOKEN`) while applying `infra/envs/oncall`; then deleted or in a password manager | Git, GitHub, Terraform state |
| Cloud Monitoring integration key | PagerDuty; state of `infra/envs/oncall` and `infra/envs/lab`; GitHub secret `PAGERDUTY_INTEGRATION_KEY` | Git, plan output |
| Events API v2 integration key | PagerDuty; state of `infra/envs/oncall` | Git, GitHub |
| Alert email address | GitHub secret `ALERT_EMAIL`; state of `infra/envs/lab` | Git, plan output |

The integration keys sit in Terraform state because the providers keep what they create or send.
State is in the private state bucket; `tf-plan` (any workflow run of this repository) and
`tf-apply` can read it. A leaked integration key lets someone page you or resolve an open page;
it can't read or change the PagerDuty account. Rotate it with
`terraform -chdir=infra/envs/oncall apply -replace=pagerduty_service_integration.cloud_monitoring`,
update the GitHub secret, and apply the lab. Once the lab no longer has to run on Terraform 1.9,
the channel can use the provider's write-only `service_key_wo` (Terraform 1.11 or later) and the
key leaves the lab's state.

## Limits and risks

- **Free plans.** PagerDuty Free: 5 users, 1 schedule, 1 escalation policy, 100 phone/SMS
  notifications a month, no noise reduction, no per-incident Slack channels. A noisy alert that
  spends the allowance is an alert-fatigue finding to fix, not a reason to upgrade.
- **One person on call.** Nobody to escalate to; the policy repeats instead. Time to acknowledge is
  measured, not promised.
- **Events API v1.** Cloud Monitoring's PagerDuty channel speaks v1, so the service has a v1
  integration for it next to the v2 one. PagerDuty calls v1 "legacy"; no end date was published
  when this was written.
- **Who can publish to `incident-alerts`.** Cloud Monitoring's notification agent (topic grant),
  project Owners and Editors, and `tf-apply` (Pub/Sub Admin). A fake message makes the agent
  investigate a fake incident; it can only propose, and the executor acts only after a human
  approves in Slack.
- **Not verified yet** (no accounts exist): that Cloud Monitoring's PagerDuty channel delivers to
  an Events API v1 integration key as Google's guide describes; that the free plan accepts the
  shift-based schedule and a repeating escalation policy through the API; whether the notification
  service agent appears with the first Pub/Sub channel created by API (step 6 has the fix); the
  exact PagerDuty and Slack menu names, which change often.
