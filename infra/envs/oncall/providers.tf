# The API token comes only from the environment (PAGERDUTY_TOKEN), so it is never in Git, in a
# tfvars file or in the state. An account in PagerDuty's EU region also needs
# PAGERDUTY_SERVICE_REGION=eu.
provider "pagerduty" {}
