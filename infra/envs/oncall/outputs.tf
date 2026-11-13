output "service_url" {
  description = "PagerDuty web page of service mamori-shop; add its integrations there (docs/oncall.md)."
  value       = pagerduty_service.shop.html_url
}
