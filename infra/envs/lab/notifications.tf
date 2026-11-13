# Where Cloud Monitoring alerts go (D6). Alert policies, such as the SLO burn-rate alerts in
# slo_generated.tf.json, don't name channels; they use one of the two lists at the end:
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
# Who can apply this: tf-apply needs roles/monitoring.notificationChannelEditor for the channels
# (granted in infra/bootstrap); the topic and its IAM are covered by roles/pubsub.admin.

variable "pagerduty_integration_key" {
  description = "Key of the Google Cloud Monitoring integration on PagerDuty service mamori-shop: output cloud_monitoring_integration_key of infra/envs/oncall. CI reads it from the GitHub secret PAGERDUTY_INTEGRATION_KEY."
  type        = string
  # Hidden from plans, which CI posts on a public repository. It is still stored in the state:
  # Cloud Monitoring never returns it, so the provider keeps the value it sent.
  sensitive = true

  validation {
    condition     = can(regex("^[A-Za-z0-9]{32}$", var.pagerduty_integration_key))
    error_message = "pagerduty_integration_key must be the 32-character key from `terraform output -raw cloud_monitoring_integration_key` in infra/envs/oncall."
  }
}

variable "alert_email" {
  description = "Email address that receives every lab alert, paging or not. Personal data: set TF_VAR_alert_email locally and the GitHub secret ALERT_EMAIL for CI; never commit it."
  type        = string
  sensitive   = true # keeps the address out of the plan comments CI posts on a public repository

  validation {
    condition     = can(regex("^[^@\\s]+@[^@\\s]+\\.[^@\\s]+$", var.alert_email))
    error_message = "alert_email must be an email address."
  }
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
  description  = "Pages whoever is on call through PagerDuty service mamori-shop (infra/envs/oncall)."
  user_labels  = local.labels

  sensitive_labels {
    service_key = var.pagerduty_integration_key
  }
}

resource "google_monitoring_notification_channel" "email" {
  display_name = "mamori alerts (email)"
  type         = "email"
  description  = "Every lab alert, paging or not; the fallback if PagerDuty delivery fails."
  user_labels  = local.labels

  labels = {
    email_address = var.alert_email
  }
}

resource "google_monitoring_notification_channel" "incident_alerts" {
  display_name = "incident-alerts (Pub/Sub)"
  type         = "pubsub"
  description  = "Every lab alert as JSON on topic incident-alerts, for the incident agent."
  user_labels  = local.labels

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
  incident_notification_channels = [
    google_monitoring_notification_channel.pagerduty.id,
    google_monitoring_notification_channel.email.id,
    google_monitoring_notification_channel.incident_alerts.id,
  ]

  # Notifies without paging: slow burn. Keeps the free plan's 100 phone/SMS a month for pages.
  incident_ticket_notification_channels = [
    google_monitoring_notification_channel.email.id,
    google_monitoring_notification_channel.incident_alerts.id,
  ]
}

output "incident_notification_channels" {
  description = "IDs of the channels that page (PagerDuty, email, Pub/Sub), e.g. for a test alert policy (docs/oncall.md)."
  value       = local.incident_notification_channels
}

output "incident_ticket_notification_channels" {
  description = "IDs of the channels that notify without paging (email, Pub/Sub)."
  value       = local.incident_ticket_notification_channels
}

output "incident_alerts_topic" {
  description = "Pub/Sub topic that receives every alert for the incident agent."
  value       = google_pubsub_topic.incident_alerts.name
}
