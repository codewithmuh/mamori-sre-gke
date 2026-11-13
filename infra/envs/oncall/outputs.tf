output "cloud_monitoring_integration_key" {
  description = "Key of the Google Cloud Monitoring integration. It becomes infra/envs/lab's pagerduty_integration_key (TF_VAR_pagerduty_integration_key locally, the GitHub secret PAGERDUTY_INTEGRATION_KEY in CI)."
  value       = pagerduty_service_integration.cloud_monitoring.integration_key
  sensitive   = true
}

output "events_api_v2_integration_key" {
  description = "Routing key of the Events API v2 integration, for test pages and any other tool that sends Events API v2."
  value       = pagerduty_service_integration.events_v2.integration_key
  sensitive   = true
}

output "service_url" {
  description = "PagerDuty web page of service mamori-shop."
  value       = pagerduty_service.shop.html_url
}
