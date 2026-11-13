# Paging for the lab (D6). PagerDuty's free plan allows 5 users, 1 on-call schedule, 1 escalation
# policy and 100 phone/SMS notifications a month, so this root creates exactly one schedule and one
# escalation policy, and one service that every lab alert pages through. How alerts reach it and
# how to test it: docs/oncall.md.

locals {
  # One shift a week, Monday 09:00 to Monday 09:00 in Tokyo, written in UTC as the API stores it.
  # The first shift is in the past on purpose: PagerDuty moves a past effective_since to "now" and
  # the provider keeps the configured value, so coverage starts at the first apply. A future date
  # would leave nobody on call until then.
  shift_start = "2026-11-09T00:00:00Z"
  shift_end   = "2026-11-16T00:00:00Z"
}

# Looked up, not created: the user exists once the account does, and managing users here would put
# names and contact details into a public repository. Phone numbers and notification rules are set
# by each user in the PagerDuty app (see docs/oncall.md).
data "pagerduty_user" "oncall" {
  email = var.oncall_user_email
}

# Shift-based schedule (v3 Schedules API). The provider deprecates the older pagerduty_schedule.
resource "pagerduty_schedulev2" "primary" {
  name        = "mamori-primary"
  description = "On call for the mamori lab: one person, all week."
  time_zone   = var.time_zone

  rotation {
    event {
      name            = "Weekly, around the clock"
      start_time      = local.shift_start
      end_time        = local.shift_end
      effective_since = local.shift_start
      recurrence      = ["RRULE:FREQ=WEEKLY"]

      # A second person (the free plan allows 5 users) is one more member block; members then take
      # turns, one week each.
      assignment_strategy {
        type              = "rotating_member_assignment_strategy"
        shifts_per_member = 1

        member {
          type    = "user_member"
          user_id = data.pagerduty_user.oncall.id
        }
      }
    }
  }
}

# With one person there is no one to escalate to, so the policy repeats instead: an unacknowledged
# page notifies the on-call person again after 15 and 30 minutes, then stops. Each round can cost a
# phone call from the 100 a month, which is why it stops at three.
# escalation_rule_assignment_strategy is left out on purpose: setting it needs round-robin
# scheduling, which the free plan doesn't have (the API answers 403).
resource "pagerduty_escalation_policy" "default" {
  name        = "mamori-default"
  description = "Notify whoever is on call in mamori-primary; repeat twice, 15 minutes apart."
  num_loops   = 2

  rule {
    escalation_delay_in_minutes = 15

    target {
      type = "schedule_reference"
      id   = pagerduty_schedulev2.primary.id
    }
  }
}

# Urgency is left at PagerDuty's default, high, so everything that reaches this service pages.
# Alerts that must not page (slow burn) never reach PagerDuty: infra/envs/lab/notifications.tf
# sends them to email and Pub/Sub only.
resource "pagerduty_service" "shop" {
  name              = "mamori-shop"
  description       = "Online Boutique on the mamori GKE lab (synthetic traffic). Pages come from Cloud Monitoring SLO burn-rate alerts."
  escalation_policy = pagerduty_escalation_policy.default.id

  # An acknowledged incident that is still open after 30 minutes pages again.
  acknowledgement_timeout = "1800"

  # Off: Cloud Monitoring resolves the incident when its alert closes. A timer that resolves an
  # incident whose alert is still open only makes Monitoring reopen it, which pages again.
  auto_resolve_timeout = "null"
}

# Cloud Monitoring's built-in PagerDuty channel speaks Events API v1, and Google's setup guide asks
# for an "Events API v1" integration. PagerDuty's own guides tie the key to the API version of its
# integration, so this integration is for Cloud Monitoring only.
resource "pagerduty_service_integration" "cloud_monitoring" {
  name    = "Google Cloud Monitoring (Events API v1)"
  type    = "generic_events_api_inbound_integration"
  service = pagerduty_service.shop.id
}

# Everything else that sends events: the test page in docs/oncall.md now, and later Alertmanager,
# the Datadog window or the incident agent if they ever page.
resource "pagerduty_service_integration" "events_v2" {
  name    = "Events API v2"
  type    = "events_api_v2_inbound_integration"
  service = pagerduty_service.shop.id
}
