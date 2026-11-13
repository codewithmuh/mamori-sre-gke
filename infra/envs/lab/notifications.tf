# Where Cloud Monitoring alerts go (D6). Alert policies, such as the SLO burn-rate alerts in
# slo_generated.tf.json, don't name channels; they use one of the two lists below:
#
#   local.incident_notification_channels         PagerDuty + email + Pub/Sub   pages a phone
#   local.incident_ticket_notification_channels  email + Pub/Sub               no page
#
# Email is in both lists as the second path Google recommends next to PagerDuty: PagerDuty, webhook
# and Slack notifications go through one shared delivery service on Google's side. Pub/Sub topic
# incident-alerts feeds the incident agent. The PagerDuty side (service, schedule, escalation
# policy) is the separate root infra/envs/oncall. The whole path, and how to test it:
# docs/oncall.md.
#
# No PagerDuty key and no required input here: plan, apply and destroy (make down) run without any
# paging secret, and the key never reaches Git, CI or the state. The key is set on the channel by
# hand once it exists (docs/oncall.md).
#
# Who can apply this: tf-apply needs roles/monitoring.notificationChannelEditor for the channels,
# roles/monitoring.alertPolicyEditor and roles/logging.configWriter for the spend-guard watchdog
# (granted in infra/bootstrap); the topic and its IAM are covered by roles/pubsub.admin.

variable "alert_email" {
  description = "Email address that receives every lab alert, paging or not. Empty means no email channel. Personal data: set TF_VAR_alert_email (or ALERT_EMAIL in .env) and the GitHub secret ALERT_EMAIL; never commit it."
  type        = string
  default     = ""
  sensitive   = true # keeps the address out of the plan comments CI posts on a public repository

  validation {
    condition     = var.alert_email == "" || can(regex("^[^@\\s]+@[^@\\s]+\\.[^@\\s]+$", var.alert_email))
    error_message = "alert_email must be an email address, or empty for no email channel."
  }
}

locals {
  # Whether the address is set is not secret, and count can't take a sensitive value.
  alert_email_enabled = nonsensitive(var.alert_email != "")

  # Cloud Monitoring needs some service key to create the channel. The real one is set by hand
  # (docs/oncall.md), so only this placeholder is ever in Git or the state.
  pagerduty_placeholder_key = "placeholder-set-by-hand-see-docs"
}

# Every alert as JSON, for the incident agent (its subscription is in agent.tf). No schema, retention
# or message storage policy: Google warns that a storage policy can stop Monitoring from publishing.
resource "google_pubsub_topic" "incident_alerts" {
  name = "incident-alerts"
}

resource "google_monitoring_notification_channel" "pagerduty" {
  # Google's guide asks for the PagerDuty service's name as the channel's name.
  display_name = "mamori-shop"
  type         = "pagerduty"
  description  = "Pages whoever is on call through PagerDuty service mamori-shop (infra/envs/oncall). Service key set by hand: docs/oncall.md."
  user_labels  = local.labels

  # Delete even if a hand-made alert policy (the test in docs/oncall.md) still uses the channel, so
  # that it can't block `make down`.
  force_delete = true

  sensitive_labels {
    service_key = local.pagerduty_placeholder_key
  }

  # Terraform only creates and deletes this channel. The provider updates a channel with a PATCH
  # that resends every label, including the placeholder key from the state, which would overwrite
  # the real key set by hand. Monitoring never returns the key, so there is no drift to see either.
  # To change the channel: terraform apply -replace=google_monitoring_notification_channel.pagerduty,
  # then set the key again.
  lifecycle {
    ignore_changes = all
  }
}

resource "google_monitoring_notification_channel" "email" {
  count = local.alert_email_enabled ? 1 : 0

  display_name = "mamori alerts (email)"
  type         = "email"
  description  = "Every lab alert, paging or not; the fallback if PagerDuty delivery fails."
  user_labels  = local.labels
  force_delete = true

  labels = {
    email_address = var.alert_email
  }
}

resource "google_monitoring_notification_channel" "incident_alerts" {
  display_name = "incident-alerts (Pub/Sub)"
  type         = "pubsub"
  description  = "Every lab alert as JSON on topic incident-alerts, for the incident agent."
  user_labels  = local.labels
  force_delete = true

  labels = {
    topic = google_pubsub_topic.incident_alerts.id
  }
}

# Monitoring publishes as its notification service agent. Google creates that agent when a
# project's first Pub/Sub channel is set up (or when it is requested; see docs/oncall.md), so the
# grant waits for the channel. Topic-level, so the agent can publish to this topic only.
resource "google_pubsub_topic_iam_member" "incident_alerts_monitoring" {
  topic  = google_pubsub_topic.incident_alerts.id
  role   = "roles/pubsub.publisher"
  member = "serviceAccount:service-${data.google_project.this.number}@gcp-sa-monitoring-notification.iam.gserviceaccount.com"

  depends_on = [google_monitoring_notification_channel.incident_alerts]
}

locals {
  # Pages: fast and medium burn, and journeys with no probe data.
  incident_notification_channels = concat(
    [google_monitoring_notification_channel.pagerduty.id],
    google_monitoring_notification_channel.email[*].id,
    [google_monitoring_notification_channel.incident_alerts.id],
  )

  # Notifies without paging: slow burn, the spend-guard watchdog. Keeps the free plan's 100
  # phone/SMS a month for pages. Without alert_email, only the agent sees these.
  incident_ticket_notification_channels = concat(
    google_monitoring_notification_channel.email[*].id,
    [google_monitoring_notification_channel.incident_alerts.id],
  )
}

# ---------------------------------------------------------------------------------------------
# Spend-guard watchdog. The guard (spend_guard.tf) is the only thing that stops spending, and it
# fails quietly: when a retry can't help, it logs an error and acknowledges the message so that
# Pub/Sub doesn't loop. These two policies notify (no page) when it fails or when it stops hearing
# from the budget.
# Runbook: docs/runbooks/spend-guard.md.
# ---------------------------------------------------------------------------------------------

locals {
  spend_guard_logs    = "resource.type=\"cloud_run_revision\" AND resource.labels.service_name=\"${google_cloudfunctions2_function.spend_guard.name}\""
  spend_guard_runbook = "https://github.com/codewithmuh/mamori-sre-gke/blob/main/docs/runbooks/spend-guard.md"
}

# Every failure (PermissionDenied, NotFound, a bad or non-USD message) is logged at ERROR.
resource "google_monitoring_alert_policy" "spend_guard_errors" {
  display_name          = "spend-guard logged an error"
  combiner              = "OR"
  severity              = "WARNING"
  notification_channels = local.incident_ticket_notification_channels
  user_labels           = merge(local.labels, { severity = "ticket" })

  conditions {
    display_name = "spend-guard log entry at ERROR or above"

    condition_matched_log {
      filter = "${local.spend_guard_logs} AND severity>=ERROR"
    }
  }

  alert_strategy {
    # Log-based policies must limit notifications: at most one an hour while errors go on.
    notification_rate_limit {
      period = "3600s"
    }
    auto_close = "1800s"
  }

  documentation {
    mime_type = "text/markdown"
    content   = "Runbook: ${local.spend_guard_runbook}\n\nThe spend guard logged an error, so it may not be able to scale the node pool to zero when the budget is spent. Read the entry's `error` and `outcome` fields in Logs Explorer."
  }
}

# Each budget message the guard handles ends in one log line with an `outcome` field; Cloud Billing
# sends several messages a day. Counting those lines shows the guard is still being called and still
# reading the messages.
resource "google_logging_metric" "spend_guard_outcomes" {
  name        = "spend-guard-outcomes"
  description = "Budget messages the spend guard handled (one log line with an outcome each)."
  filter      = "${local.spend_guard_logs} AND jsonPayload.outcome:*"

  metric_descriptor {
    metric_kind = "DELTA"
    value_type  = "INT64"
    unit        = "1"
  }
}

resource "google_monitoring_alert_policy" "spend_guard_silent" {
  display_name          = "spend-guard handled no budget message for 23.5 hours"
  combiner              = "OR"
  severity              = "WARNING"
  notification_channels = local.incident_ticket_notification_channels
  user_labels           = merge(local.labels, { severity = "ticket" })

  conditions {
    display_name = "No spend-guard outcome log for 23.5 hours"

    condition_absent {
      filter = "resource.type = \"cloud_run_revision\" AND metric.type = \"logging.googleapis.com/user/${google_logging_metric.spend_guard_outcomes.name}\""
      # 23.5 hours is the longest absence window Cloud Monitoring allows. It only starts counting
      # once the metric has had data, so a fresh lab doesn't alert before the first message.
      duration = "84600s"

      aggregations {
        alignment_period = "3600s"
        # Summed over revisions: a redeploy starts a new revision, and the old one's series going
        # quiet must not count as silence.
        per_series_aligner   = "ALIGN_SUM"
        cross_series_reducer = "REDUCE_SUM"
      }
    }
  }

  alert_strategy {
    auto_close = "1800s"
  }

  documentation {
    mime_type = "text/markdown"
    content   = "Runbook: ${local.spend_guard_runbook}\n\nThe spend guard has handled no budget notification for 23.5 hours. Either Cloud Billing stopped publishing to `budget-alerts`, the trigger is broken, or the function fails before it logs. Until this is fixed, nothing scales the node pool to zero when the budget is spent."
  }
}

output "incident_notification_channels" {
  description = "IDs of the channels that page (PagerDuty, email if set, Pub/Sub), e.g. for a test alert policy (docs/oncall.md)."
  value       = local.incident_notification_channels
}

output "incident_ticket_notification_channels" {
  description = "IDs of the channels that notify without paging (email if set, Pub/Sub)."
  value       = local.incident_ticket_notification_channels
}

output "incident_alerts_topic" {
  description = "Pub/Sub topic that receives every alert for the incident agent."
  value       = google_pubsub_topic.incident_alerts.name
}

output "pagerduty_notification_channel" {
  description = "Resource name of the PagerDuty channel. Set its service key by hand after it is created (docs/oncall.md)."
  value       = google_monitoring_notification_channel.pagerduty.name
}
