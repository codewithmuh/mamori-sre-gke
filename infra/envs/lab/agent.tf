# The incident agent's trigger (#24, docs/agent.md): a pull subscription on the incident-alerts
# topic that Cloud Monitoring's Pub/Sub notification channel publishes to (#21,
# notifications.tf). The agent pulls; nothing pushes into the cluster.
#
# Its project roles (read-only, plus the model call) are in infra/bootstrap/agent_identity.tf.

locals {
  # The Kubernetes service account mamori-agent/agent, through Workload Identity for GKE. Keep
  # in step with infra/bootstrap/agent_identity.tf and deploy/agent/serviceaccount.yaml.
  agent_ksa_principal = join("/", [
    "principal://iam.googleapis.com/projects/${data.google_project.this.number}",
    "locations/global/workloadIdentityPools/${var.project_id}.svc.id.goog",
    "subject/ns/mamori-agent/sa/agent",
  ])
}

resource "google_pubsub_subscription" "agent_incidents" {
  name  = "mamori-agent-incidents"
  topic = google_pubsub_topic.incident_alerts.id

  # The agent acknowledges a message as soon as it has queued the incident (seconds), not after
  # the investigation (minutes), so the default-sized deadline is enough.
  ack_deadline_seconds = 60

  # Alerts older than a day are history, not incidents to investigate.
  message_retention_duration = "86400s"
  retain_acked_messages      = false

  # Don't let Pub/Sub delete the subscription after 31 idle days (a lab that is down for a while).
  expiration_policy {
    ttl = ""
  }

  retry_policy {
    minimum_backoff = "10s"
    maximum_backoff = "300s"
  }
}

# Pull and acknowledge on this subscription only. A resource-level grant, which tf-apply can
# set (it holds Pub/Sub Admin); no project-level role is involved.
resource "google_pubsub_subscription_iam_member" "agent_subscriber" {
  subscription = google_pubsub_subscription.agent_incidents.id
  role         = "roles/pubsub.subscriber"
  member       = local.agent_ksa_principal

  # The principal's workload identity pool appears with the cluster.
  depends_on = [google_container_cluster.mamori]
}
