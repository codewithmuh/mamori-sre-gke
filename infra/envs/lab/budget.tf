# Budget alerts at 50, 90 and 100 % of actual spend (D12). At 100 % the spend guard
# (spend_guard.tf) scales the node pool to zero; budgets alone never stop spending.
#
# Who can apply this: the Billing Budget API checks billing.budgets.* on the billing account, so
# the applier needs Billing Account Costs Manager (or Administrator) there; tf-apply gets Costs
# Manager in infra/bootstrap. Linking the topic also needs pubsub.topics.setIamPolicy, because
# Cloud Billing is granted publish rights on it. The quota project comes from providers.tf.

data "google_project" "this" {
  project_id = var.project_id
}

resource "google_pubsub_topic" "budget_alerts" {
  name = "budget-alerts"
}

resource "google_billing_budget" "lab" {
  billing_account = var.billing_account_id
  display_name    = "mamori-lab"

  budget_filter {
    projects        = ["projects/${data.google_project.this.number}"]
    calendar_period = "MONTH"

    # Gross cost: the trial credit would otherwise hide the spend until the credit is gone, which
    # is exactly when the trial would stop the cluster.
    credit_types_treatment = "EXCLUDE_ALL_CREDITS"
  }

  # Must match the billing account's currency; a non-USD account fails here instead of silently
  # setting a budget of 200 yen.
  amount {
    specified_amount {
      currency_code = "USD"
      units         = tostring(var.monthly_budget_usd)
    }
  }

  dynamic "threshold_rules" {
    for_each = [0.5, 0.9, 1.0]

    content {
      threshold_percent = threshold_rules.value
      spend_basis       = "CURRENT_SPEND"
    }
  }

  # Pub/Sub gets every update (several a day), not only threshold crossings; the function decides.
  # Billing admins still get the threshold emails.
  all_updates_rule {
    pubsub_topic   = google_pubsub_topic.budget_alerts.id
    schema_version = "1.0"
  }
}
