locals {
  # On every Datadog object this root (and slo_generated.tf.json) creates, so the window's objects
  # are easy to filter, count and delete.
  datadog_tags = ["app:mamori", "env:lab", "managed-by:terraform"]

  # Same cluster name as infra/envs/lab and datadog.clusterName in deploy/optional/datadog/values.yaml.
  # The Agent puts it on every metric as kube_cluster_name.
  cluster_name = "mamori"

  # Runbooks: docs/runbooks/cuj-<id>.md for the journeys, sections of docs/datadog-window.md for
  # the Kubernetes monitors.
  runbook_base_url = "https://github.com/codewithmuh/mamori-sre-gke/blob/main/docs/runbooks"
  window_doc_url   = "https://github.com/codewithmuh/mamori-sre-gke/blob/main/docs/datadog-window.md"

  # Appended to every message. Empty means the alert is visible in Datadog only.
  notify = join(" ", var.notification_handles)
}
