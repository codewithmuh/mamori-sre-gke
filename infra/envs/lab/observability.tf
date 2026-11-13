# Cloud Logging and alerting pieces of the observability setup (docs/observability.md):
#   - a log-based metric that counts Spot preemptions of the cluster's nodes (no free system
#     metric says "Compute Engine took this VM back"), shown on the platform dashboard;
#   - an exclusion that keeps only a sample of the shop's routine INFO/DEBUG lines, the biggest
#     and least useful part of the log volume;
#   - edge cost guards: an exclusion that keeps only a sample of load-balancer log entries for
#     requests Cloud Armor denied, and an alert on the load balancer's request rate.
#
# tf-apply needs roles/logging.configWriter for the metric and exclusions, and
# roles/monitoring.editor for the alert (both requested from infra/bootstrap by #19).
#
# Merge order: the alert uses local.incident_ticket_notification_channels from notifications.tf
# (#21), so #21 must be merged before this file.

# ---------------------------------------------------------------------------------------------
# Spot preemptions
# ---------------------------------------------------------------------------------------------

# Compute Engine writes a system-event audit log entry, method compute.instances.preempted, when it
# reclaims a Spot VM. Those entries land in the _Required bucket, which costs nothing and can't be
# excluded. A preemption can also show up as compute.instances.delete by system@google.com without
# the preempted entry (Compute Engine troubleshooting docs), so the dashboard shows GKE's own
# kubernetes.io/node/interruption_count next to this metric.
resource "google_logging_metric" "observability_spot_preemptions" {
  name        = "spot_preemptions"
  description = "Spot VMs of GKE cluster ${local.cluster_name} preempted by Compute Engine (system event compute.instances.preempted)."

  filter = join(" AND ", [
    "resource.type=\"gce_instance\"",
    "logName=\"projects/${var.project_id}/logs/cloudaudit.googleapis.com%2Fsystem_event\"",
    "protoPayload.methodName=\"compute.instances.preempted\"",
    # GKE names node VMs gke-<cluster>-<pool>-<hash>-<suffix>.
    "protoPayload.resourceName:\"/instances/gke-${local.cluster_name}-\"",
  ])

  metric_descriptor {
    metric_kind  = "DELTA"
    value_type   = "INT64"
    unit         = "1"
    display_name = "Spot preemptions (${local.cluster_name})"

    labels {
      key         = "node"
      value_type  = "STRING"
      description = "Name of the preempted VM, which is also the Kubernetes node name."
    }
  }

  label_extractors = {
    node = "REGEXP_EXTRACT(protoPayload.resourceName, \"/instances/([^/]+)$\")"
  }
}

# ---------------------------------------------------------------------------------------------
# Log volume: sample the shop's routine lines
# ---------------------------------------------------------------------------------------------

variable "observability_shop_info_log_keep_percent" {
  description = "Share (0-100) of the shop's INFO and DEBUG container log lines (namespace boutique) kept in Cloud Logging. WARNING and above, and every line with no or an unknown severity, are always kept. 100 turns the exclusion off, for example during a game day."
  type        = number
  default     = 10

  validation {
    condition     = var.observability_shop_info_log_keep_percent >= 0 && var.observability_shop_info_log_keep_percent <= 100
    error_message = "observability_shop_info_log_keep_percent must be between 0 and 100."
  }
}

# Why: Online Boutique logs a line or two per request in most services (currency conversions,
# cart reads, shipping quotes, the frontend's request log) and the load generator prints its
# stats table every few seconds. Under the steady load that is about 30 lines a second, roughly
# 45-75 GiB a month (estimate, docs/observability.md), against Cloud Logging's free 50 GiB a month
# per project; above it, $0.50 per GiB. Keeping 10 % of INFO/DEBUG brings the shop down to about
# 5-8 GiB and leaves room for everything else.
#
# What is kept: every WARNING, ERROR and CRITICAL line (the ones game days, postmortems and the
# incident agent look for), every line whose severity is missing or unparsed (DEFAULT), all other
# namespaces, and all audit and system logs. sample(insertId, f) matches a fraction f of entries,
# and matched entries are excluded, so f is the share dropped.
resource "google_logging_project_exclusion" "observability_shop_info_logs" {
  count = var.observability_shop_info_log_keep_percent < 100 ? 1 : 0

  name        = "shop-info-logs-sampled"
  description = "Drop ${100 - var.observability_shop_info_log_keep_percent} % of INFO/DEBUG container logs of namespace boutique on cluster ${local.cluster_name} (infra/envs/lab/observability.tf)."

  filter = join(" AND ", [
    "resource.type=\"k8s_container\"",
    "resource.labels.cluster_name=\"${local.cluster_name}\"",
    "resource.labels.namespace_name=\"boutique\"",
    "(severity=\"DEBUG\" OR severity=\"INFO\")",
    "sample(insertId, ${(100 - var.observability_shop_info_log_keep_percent) / 100})",
  ])
}

# ---------------------------------------------------------------------------------------------
# Edge cost: what keeps billing when Cloud Armor says no
# ---------------------------------------------------------------------------------------------
#
# Cloud Armor charges $0.75 per million requests it evaluates, denied or throttled ones included,
# and the load balancer writes a log entry for each request at its backend logging sample rate.
# Neither lives in the cluster, so the spend guard (node pool to zero) can't stop them, and
# budget alerts arrive hours late. One client at 1,000 requests/s would cost about $65 a day in
# Cloud Armor requests plus about $40-80 a day in load-balancer logs (estimate,
# docs/observability.md "Edge cost"). The two resources below cut the logs and say early that it
# is happening; `make edge-off` stops the rest.

variable "observability_edge_denied_log_keep_percent" {
  description = "Share (0-100) of load-balancer request log entries for requests Cloud Armor denied (including rate-limit throttles) kept in Cloud Logging, as evidence. 100 turns the exclusion off."
  type        = number
  default     = 1

  validation {
    condition     = var.observability_edge_denied_log_keep_percent >= 0 && var.observability_edge_denied_log_keep_percent <= 100
    error_message = "observability_edge_denied_log_keep_percent must be between 0 and 100."
  }
}

variable "observability_edge_request_rate_alert_rps" {
  description = "Load-balancer requests per second (all responses, denied ones included) above which, for 5 minutes, an alert notifies without paging. Normal public traffic is the prober plus the odd visitor."
  type        = number
  default     = 50

  validation {
    condition     = var.observability_edge_request_rate_alert_rps > 0
    error_message = "observability_edge_request_rate_alert_rps must be positive."
  }
}

# Cloud Armor adds jsonPayload.enforcedSecurityPolicy to the load balancer's request log entries
# (resource type http_load_balancer). A rate-limit throttle and a rate-based ban both log
# outcome DENY (Cloud Armor request logging docs). Allowed requests are untouched here; their
# sample rate is the backend logging sampleRate in deploy/edge.
resource "google_logging_project_exclusion" "observability_edge_denied_requests" {
  count = var.observability_edge_denied_log_keep_percent < 100 ? 1 : 0

  name        = "edge-denied-requests-sampled"
  description = "Drop ${100 - var.observability_edge_denied_log_keep_percent} % of load-balancer log entries for requests Cloud Armor denied (infra/envs/lab/observability.tf)."

  filter = join(" AND ", [
    "resource.type=\"http_load_balancer\"",
    "jsonPayload.enforcedSecurityPolicy.outcome=\"DENY\"",
    "sample(insertId, ${(100 - var.observability_edge_denied_log_keep_percent) / 100})",
  ])
}

# Early warning on edge traffic. Counts every request the load balancer answered, including Cloud
# Armor's 429s, so an attack shows here even when nothing reaches the mesh. The only load balancer
# in the project is the Gateway's, so the filter needs no forwarding-rule name (the Gateway
# controller generates it). Load-balancer metrics arrive up to about 3.5 minutes late, so expect
# the alert 5-10 minutes after the traffic starts.
resource "google_monitoring_alert_policy" "observability_edge_request_rate" {
  display_name = "Edge: load balancer above ${var.observability_edge_request_rate_alert_rps} requests/s"
  combiner     = "OR"
  severity     = "WARNING"

  conditions {
    display_name = "Load balancer requests/s above ${var.observability_edge_request_rate_alert_rps} for 5 minutes"

    condition_threshold {
      filter          = "metric.type=\"loadbalancing.googleapis.com/https/request_count\" AND resource.type=\"https_lb_rule\""
      comparison      = "COMPARISON_GT"
      threshold_value = var.observability_edge_request_rate_alert_rps
      duration        = "300s"

      aggregations {
        alignment_period     = "60s"
        per_series_aligner   = "ALIGN_RATE"
        cross_series_reducer = "REDUCE_SUM"
      }

      trigger {
        count = 1
      }
    }
  }

  alert_strategy {
    auto_close = "1800s"
  }

  # Email and Pub/Sub (and so the incident agent), no page: this is a cost problem first. If it
  # also hurts shoppers, the journey SLO alerts page on their own.
  notification_channels = local.incident_ticket_notification_channels

  documentation {
    mime_type = "text/markdown"
    subject   = "Edge traffic above ${var.observability_edge_request_rate_alert_rps} requests/s: Cloud Armor and load-balancer cost is running"
    content   = <<-EOT
      The public load balancer is answering more than ${var.observability_edge_request_rate_alert_rps} requests/s. Cloud Armor bills every request it evaluates ($0.75 per million), denied ones too, and the node-pool spend guard can't stop that.

      1. Dashboard *Mamori mesh*, "Public path": is it 429s (Cloud Armor throttling one client) or real traffic reaching the gateway?
      2. Logs Explorer: `resource.type="http_load_balancer"`, group by `httpRequest.remoteIp` (denied requests are sampled at ${var.observability_edge_denied_log_keep_percent} %).
      3. If it is an attack and doesn't stop: `make edge-off` pauses Argo CD on the edge, deletes the Gateway and waits until the load balancer is gone, which stops the per-request charges. The shop is then down for outside users and the prober: log it in the error-budget policy as a cost decision. `make edge-on` brings the edge back.

      Guide: docs/observability.md, "Edge cost".
    EOT
  }

  user_labels = local.labels
}
