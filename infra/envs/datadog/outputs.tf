output "synthetics_test_ids" {
  description = "Public ids of the four journey tests (as in the Datadog URL /synthetics/details/<id>)."
  value = {
    browse        = datadog_synthetics_test.cuj_browse.id
    "add-to-cart" = datadog_synthetics_test.cuj_add_to_cart.id
    "view-cart"   = datadog_synthetics_test.cuj_view_cart.id
    checkout      = datadog_synthetics_test.cuj_checkout.id
  }
}

output "synthetics_monitor_ids" {
  description = "Monitor ids of the journey tests; the monitor-based SLOs in slo_generated.tf.json use these."
  value = {
    browse        = datadog_synthetics_test.cuj_browse.monitor_id
    "add-to-cart" = datadog_synthetics_test.cuj_add_to_cart.monitor_id
    "view-cart"   = datadog_synthetics_test.cuj_view_cart.monitor_id
    checkout      = datadog_synthetics_test.cuj_checkout.monitor_id
  }
}

output "kubernetes_monitor_ids" {
  description = "Ids of the Kubernetes monitors, for counting their alerts during the window."
  value = {
    boutique_crash_loop           = datadog_monitor.boutique_crash_loop.id
    boutique_replicas_unavailable = datadog_monitor.boutique_replicas_unavailable.id
    ready_nodes                   = datadog_monitor.ready_nodes.id
  }
}

output "gcp_integration_service_account" {
  description = "Read-only service account Datadog impersonates for Google Cloud metrics."
  value       = google_service_account.datadog.email
}

output "monitors_url" {
  description = "Datadog page listing every monitor of the window (filtered by tag app:mamori)."
  value       = "https://app.${var.datadog_site}/monitors/manage?q=tag%3A%22app%3Amamori%22"
}
