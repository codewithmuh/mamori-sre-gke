# Keys come only from the environment: the provider reads DD_API_KEY and DD_APP_KEY itself, so
# they never become Terraform values and never land in state, a saved plan or a command line.
# The site is not a secret, so it is a variable.
provider "datadog" {
  api_url = "https://api.${var.datadog_site}/"
}

# The maintainer's own credentials (gcloud application-default login), as for infra/bootstrap.
provider "google" {
  project = var.project_id
}
