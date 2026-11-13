# A few Kubernetes monitors next to the journey SLOs. The SLOs (Cloud Monitoring, and Datadog's
# from slo_generated.tf.json) say *that* shoppers are hurt; these say what is broken when the
# SLOs can't: a non-critical service down (no journey burns budget for it), or the cluster
# running short of nodes before anything fails.
#
# Each one waits long enough to ignore what heals itself (a Spot preemption replaced in a few
# minutes, one restart), notifies without paging (pages come from the SLO burn-rate alerts), and
# links a runbook. Metrics come from kubernetes_state_core in the Cluster Agent, which tags them
# with kube_cluster_name (datadog-agent 7.84.6, pkg/collector/corechecks/cluster/ksm).

locals {
  k8s_monitor_runbooks = {
    crash_loop  = "${local.window_doc_url}#runbook-boutique-crash-loop"
    unavailable = "${local.window_doc_url}#runbook-boutique-replicas-unavailable"
    nodes       = "${local.window_doc_url}#runbook-ready-nodes-below-pool-size"
  }
}

# Restarts with back-off (10 s, 20 s, 40 s, ...) give about six in 15 minutes; one OOM kill or a
# single flaky start stays below the threshold. A deleted pod (Chaos Mesh pod-kill) starts a new
# series at 0, so it doesn't count here.
resource "datadog_monitor" "boutique_crash_loop" {
  name     = "[mamori] boutique pod {{pod_name.name}} is crash-looping"
  type     = "query alert"
  priority = "3"
  query    = "change(max(last_15m),last_15m):max:kubernetes_state.container.restarts{kube_cluster_name:${local.cluster_name},kube_namespace:boutique} by {pod_name} > 3"

  message = trimspace(<<-EOT
    {{#is_alert}}Pod {{pod_name.name}} in boutique restarted more than 3 times in 15 minutes.{{/is_alert}}
    {{#is_recovery}}Pod {{pod_name.name}} stopped restarting.{{/is_recovery}}
    Check whether a journey SLO is burning; if not, the service is probably non-critical and this is a ticket, not an incident.
    Runbook: ${local.k8s_monitor_runbooks.crash_loop}
    ${local.notify}
    EOT
  )

  monitor_thresholds {
    critical = 3
  }

  # A count that jumps between evaluations: evaluate whatever data there is.
  require_full_window = false
  renotify_interval   = 0
  include_tags        = false

  assets {
    name     = "Runbook"
    category = "runbook"
    url      = local.k8s_monitor_runbooks.crash_loop
  }

  tags = concat(local.datadog_tags, ["signal:kubernetes"])
}

# The most user-facing of the three: a Deployment in the shop has had fewer ready pods than it
# wants for 15 minutes. Covers crash loops, failing readiness probes, image pull errors and pods
# left Pending after a node went away. Every shop service runs one replica, so 1 is a whole
# service down.
resource "datadog_monitor" "boutique_replicas_unavailable" {
  name     = "[mamori] boutique {{kube_deployment.name}} has had no ready pod for 15 minutes"
  type     = "query alert"
  priority = "3"
  query    = "min(last_15m):max:kubernetes_state.deployment.replicas_unavailable{kube_cluster_name:${local.cluster_name},kube_namespace:boutique} by {kube_deployment} > 0"

  message = trimspace(<<-EOT
    {{#is_alert}}Deployment {{kube_deployment.name}} in boutique has had unavailable replicas for 15 minutes.{{/is_alert}}
    {{#is_recovery}}Deployment {{kube_deployment.name}} is fully available again.{{/is_recovery}}
    If a journey SLO is burning, the SLO alert leads and this explains it; if not, the service is non-critical for every journey (see slo/cuj-map.yaml).
    Runbook: ${local.k8s_monitor_runbooks.unavailable}
    ${local.notify}
    EOT
  )

  monitor_thresholds {
    critical = 0
  }

  # The value is reported every check run, also when it is 0, so a full window is available.
  require_full_window = true
  renotify_interval   = 0
  include_tags        = false

  assets {
    name     = "Runbook"
    category = "runbook"
    url      = local.k8s_monitor_runbooks.unavailable
  }

  tags = concat(local.datadog_tags, ["signal:kubernetes"])
}

# Counts ready nodes instead of watching each node's condition, so it also catches a Spot VM that
# is gone and not replaced (no capacity in the zone), not only one stuck NotReady. A preemption
# normally comes back within minutes; 20 minutes below the pool size means it didn't.
#
# No data for 30 minutes alerts too: the Cluster Agent reports this metric, so silence means the
# Agent is down, the nodes are scaled to zero (spend guard), or the cluster is gone. Otherwise a
# silent Datadog would look like a healthy one.
resource "datadog_monitor" "ready_nodes" {
  name     = "[mamori] cluster has had fewer than ${var.expected_ready_nodes} ready nodes for 20 minutes"
  type     = "query alert"
  priority = "3"
  query    = "max(last_20m):sum:kubernetes_state.node.by_condition{kube_cluster_name:${local.cluster_name},condition:ready,status:true} < ${var.expected_ready_nodes}"

  message = trimspace(<<-EOT
    {{#is_alert}}Only {{value}} of ${var.expected_ready_nodes} nodes in cluster ${local.cluster_name} have been ready for the last 20 minutes.{{/is_alert}}
    {{#is_no_data}}Datadog has had no node data from cluster ${local.cluster_name} for 30 minutes: Agent down, nodes scaled to zero by the spend guard, or the cluster deleted.{{/is_no_data}}
    {{#is_recovery}}All ${var.expected_ready_nodes} nodes are ready again.{{/is_recovery}}
    Runbook: ${local.k8s_monitor_runbooks.nodes}
    ${local.notify}
    EOT
  )

  monitor_thresholds {
    critical = var.expected_ready_nodes
  }

  require_full_window = true
  notify_no_data      = true
  no_data_timeframe   = 30
  renotify_interval   = 0
  include_tags        = false

  assets {
    name     = "Runbook"
    category = "runbook"
    url      = local.k8s_monitor_runbooks.nodes
  }

  tags = concat(local.datadog_tags, ["signal:kubernetes"])
}
