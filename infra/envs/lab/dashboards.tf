# Platform dashboards in Cloud Monitoring (docs/observability.md has the reading guide):
#   - "Mamori mesh": golden signals per service from the Envoy sidecars, and the public path from
#     the load balancer through the Istio ingress gateway.
#   - "Mamori platform": nodes, capacity against the 70 % request budget, Spot preemptions, the
#     control planes (Argo CD, istiod), and what the telemetry itself costs.
# The journey SLOs (compliance, error budget, burn rate) have their own generated dashboard,
# "Mamori CUJ SLOs" (#18, slo_generated.tf.json).
#
# Mesh and control-plane panels use PromQL on Managed Prometheus series (deploy/observability
# keeps only what these panels, the SLOs and the incident agent use). Google Cloud metrics
# (GKE, load balancer, billing, the log-based metric) use metric filters. GKE system metrics,
# load balancer and billing metrics cost nothing to ingest.
#
# tf-apply needs monitoring.dashboards.* (roles/monitoring.editor, requested from infra/bootstrap
# by #19).

locals {
  # Common label filters.
  observability_mesh_out = "cluster=\"${local.cluster_name}\", reporter=\"source\", destination_service_namespace=\"boutique\""
  observability_gateway  = "cluster=\"${local.cluster_name}\", reporter=\"source\", source_workload=\"istio-ingressgateway\""
  observability_k8s_node = "monitored_resource=\"k8s_node\", cluster_name=\"${local.cluster_name}\""
  observability_k8s_ctr  = "monitored_resource=\"k8s_container\", cluster_name=\"${local.cluster_name}\""

  # "Bad" as the PRD starts it: HTTP 5xx or no response (code 0), or a gRPC status other than OK.
  # The two selectors don't overlap, so "or" is a plain union.
  observability_mesh_errors = join(" or ", [
    "rate(istio_requests_total{${local.observability_mesh_out}, response_code=~\"5..|0\"}[5m])",
    "rate(istio_requests_total{${local.observability_mesh_out}, response_code!~\"5..|0\", grpc_response_status!~\"0|\"}[5m])",
  ])

  # Building blocks for chart widgets.
  observability_chart_defaults = {
    chartOptions = { mode = "COLOR" }
  }
}

# Tiles as {x, y, w, h, widget} on a 48-column mosaic; the resources below turn them into the API's
# tile format.
locals {
  observability_mesh_tiles = [
    {
      x = 0, y = 0, w = 48, h = 7
      widget = {
        title = "About this dashboard"
        text = {
          format  = "MARKDOWN"
          content = <<-EOT
            Mesh view of Online Boutique, from the Envoy sidecars (Managed Service for Prometheus, scraped every 30 s).
            **Caller's view** (`reporter="source"`): the only view that sees Istio fault injection, so game-day faults show here.
            Errors are HTTP 5xx, no response, or a gRPC status other than OK. Journey SLOs: dashboard *Mamori CUJ SLOs*.
            Guide and queries: `docs/observability.md`.
          EOT
        }
      }
    },

    # --- Public path -----------------------------------------------------------------------
    {
      x = 0, y = 7, w = 48, h = 4
      widget = {
        title         = "Public path: load balancer and Istio ingress gateway"
        sectionHeader = { subtitle = "Cloud Armor rate limits answer 429 at the load balancer; the mesh never sees those requests.", dividerBelow = true }
      }
    },
    {
      x = 0, y = 11, w = 16, h = 16
      widget = {
        title = "Load balancer requests/s by response class"
        xyChart = merge(local.observability_chart_defaults, {
          dataSets = [{
            plotType           = "STACKED_AREA"
            targetAxis         = "Y1"
            minAlignmentPeriod = "60s"
            timeSeriesQuery = {
              timeSeriesFilter = {
                filter = "metric.type=\"loadbalancing.googleapis.com/https/request_count\" resource.type=\"https_lb_rule\""
                aggregation = {
                  alignmentPeriod    = "60s"
                  perSeriesAligner   = "ALIGN_RATE"
                  crossSeriesReducer = "REDUCE_SUM"
                  groupByFields      = ["metric.label.\"response_code_class\""]
                }
              }
            }
          }]
          yAxis = { label = "requests/s", scale = "LINEAR" }
        })
      }
    },
    {
      x = 16, y = 11, w = 16, h = 16
      widget = {
        title = "Rate-limited by Cloud Armor (429/s)"
        xyChart = merge(local.observability_chart_defaults, {
          dataSets = [{
            plotType           = "LINE"
            targetAxis         = "Y1"
            minAlignmentPeriod = "60s"
            timeSeriesQuery = {
              timeSeriesFilter = {
                filter = "metric.type=\"loadbalancing.googleapis.com/https/request_count\" resource.type=\"https_lb_rule\" metric.label.\"response_code\"=429"
                aggregation = {
                  alignmentPeriod    = "60s"
                  perSeriesAligner   = "ALIGN_RATE"
                  crossSeriesReducer = "REDUCE_SUM"
                }
              }
            }
          }]
          yAxis = { label = "requests/s", scale = "LINEAR" }
        })
      }
    },
    {
      x = 32, y = 11, w = 16, h = 16
      widget = {
        title = "Latency p95 (ms): load balancer total vs gateway to frontend"
        xyChart = merge(local.observability_chart_defaults, {
          dataSets = [
            {
              plotType           = "LINE"
              targetAxis         = "Y1"
              minAlignmentPeriod = "60s"
              legendTemplate     = "load balancer (client to last byte)"
              timeSeriesQuery = {
                timeSeriesFilter = {
                  filter = "metric.type=\"loadbalancing.googleapis.com/https/total_latencies\" resource.type=\"https_lb_rule\""
                  aggregation = {
                    alignmentPeriod    = "60s"
                    perSeriesAligner   = "ALIGN_DELTA"
                    crossSeriesReducer = "REDUCE_PERCENTILE_95"
                  }
                }
              }
            },
            {
              plotType           = "LINE"
              targetAxis         = "Y1"
              minAlignmentPeriod = "60s"
              legendTemplate     = "gateway to frontend (mesh)"
              timeSeriesQuery = {
                prometheusQuery = "histogram_quantile(0.95, sum by (le) (rate(istio_request_duration_milliseconds_bucket{${local.observability_gateway}}[5m])))"
              }
            },
          ]
          yAxis = { label = "ms", scale = "LINEAR" }
        })
      }
    },
    {
      x = 0, y = 27, w = 24, h = 16
      widget = {
        title = "Gateway to frontend requests/s by response code"
        xyChart = merge(local.observability_chart_defaults, {
          dataSets = [{
            plotType           = "STACKED_AREA"
            targetAxis         = "Y1"
            minAlignmentPeriod = "60s"
            timeSeriesQuery = {
              prometheusQuery = "sum by (response_code) (rate(istio_requests_total{${local.observability_gateway}}[5m]))"
            }
          }]
          yAxis = { label = "requests/s", scale = "LINEAR" }
        })
      }
    },
    {
      x = 24, y = 27, w = 24, h = 16
      widget = {
        title = "Gateway to frontend error ratio (5xx or no response)"
        xyChart = merge(local.observability_chart_defaults, {
          dataSets = [{
            plotType           = "LINE"
            targetAxis         = "Y1"
            minAlignmentPeriod = "60s"
            timeSeriesQuery = {
              prometheusQuery = "sum(rate(istio_requests_total{${local.observability_gateway}, response_code=~\"5..|0\"}[5m])) / sum(rate(istio_requests_total{${local.observability_gateway}}[5m]))"
            }
          }]
          thresholds = [{ value = 0.005, label = "0.5 % (99.5 % target)", color = "YELLOW", direction = "ABOVE", targetAxis = "Y1" }]
          yAxis      = { label = "ratio", scale = "LINEAR" }
        })
      }
    },

    # --- Golden signals per service ---------------------------------------------------------
    {
      x = 0, y = 43, w = 48, h = 4
      widget = {
        title         = "Golden signals per service (caller's view)"
        sectionHeader = { subtitle = "Traffic, errors and latency as the calling sidecar saw them; saturation from GKE container metrics.", dividerBelow = true }
      }
    },
    {
      x = 0, y = 47, w = 24, h = 16
      widget = {
        title = "Traffic: requests/s by destination service"
        xyChart = merge(local.observability_chart_defaults, {
          dataSets = [{
            plotType           = "LINE"
            targetAxis         = "Y1"
            minAlignmentPeriod = "60s"
            timeSeriesQuery = {
              prometheusQuery = "sum by (destination_service_name) (rate(istio_requests_total{${local.observability_mesh_out}}[5m]))"
            }
          }]
          yAxis = { label = "requests/s", scale = "LINEAR" }
        })
      }
    },
    {
      x = 24, y = 47, w = 24, h = 16
      widget = {
        title = "Errors: share of failed requests by destination service"
        xyChart = merge(local.observability_chart_defaults, {
          dataSets = [{
            plotType           = "LINE"
            targetAxis         = "Y1"
            minAlignmentPeriod = "60s"
            timeSeriesQuery = {
              prometheusQuery = "sum by (destination_service_name) (${local.observability_mesh_errors}) / sum by (destination_service_name) (rate(istio_requests_total{${local.observability_mesh_out}}[5m]))"
            }
          }]
          thresholds = [{ value = 0.005, label = "0.5 %", color = "YELLOW", direction = "ABOVE", targetAxis = "Y1" }]
          yAxis      = { label = "ratio", scale = "LINEAR" }
        })
      }
    },
    {
      x = 0, y = 63, w = 24, h = 16
      widget = {
        title = "Latency: p95 (ms) by destination service"
        xyChart = merge(local.observability_chart_defaults, {
          dataSets = [{
            plotType           = "LINE"
            targetAxis         = "Y1"
            minAlignmentPeriod = "60s"
            timeSeriesQuery = {
              prometheusQuery = "histogram_quantile(0.95, sum by (destination_service_name, le) (rate(istio_request_duration_milliseconds_bucket{${local.observability_mesh_out}}[5m])))"
            }
          }]
          thresholds = [{ value = 500, label = "500 ms", color = "YELLOW", direction = "ABOVE", targetAxis = "Y1" }]
          yAxis      = { label = "ms", scale = "LINEAR" }
        })
      }
    },
    {
      x = 24, y = 63, w = 24, h = 16
      widget = {
        title = "Latency: p50 (ms) by destination service"
        xyChart = merge(local.observability_chart_defaults, {
          dataSets = [{
            plotType           = "LINE"
            targetAxis         = "Y1"
            minAlignmentPeriod = "60s"
            timeSeriesQuery = {
              prometheusQuery = "histogram_quantile(0.5, sum by (destination_service_name, le) (rate(istio_request_duration_milliseconds_bucket{${local.observability_mesh_out}}[5m])))"
            }
          }]
          yAxis = { label = "ms", scale = "LINEAR" }
        })
      }
    },
    {
      x = 0, y = 79, w = 24, h = 16
      widget = {
        title = "Saturation: CPU used / requested, shop containers"
        xyChart = merge(local.observability_chart_defaults, {
          dataSets = [{
            plotType           = "LINE"
            targetAxis         = "Y1"
            minAlignmentPeriod = "60s"
            timeSeriesQuery = {
              prometheusQuery = "max by (pod_name) (kubernetes_io:container_cpu_request_utilization{${local.observability_k8s_ctr}, namespace_name=\"boutique\", container_name!=\"istio-proxy\"})"
            }
          }]
          thresholds = [{ value = 1, label = "at request", color = "YELLOW", direction = "ABOVE", targetAxis = "Y1" }]
          yAxis      = { label = "ratio", scale = "LINEAR" }
        })
      }
    },
    {
      x = 24, y = 79, w = 24, h = 16
      widget = {
        title = "Saturation: memory used (non-evictable) / limit, shop containers"
        xyChart = merge(local.observability_chart_defaults, {
          dataSets = [{
            plotType           = "LINE"
            targetAxis         = "Y1"
            minAlignmentPeriod = "60s"
            timeSeriesQuery = {
              prometheusQuery = "max by (pod_name) (kubernetes_io:container_memory_limit_utilization{${local.observability_k8s_ctr}, namespace_name=\"boutique\", container_name!=\"istio-proxy\", memory_type=\"non-evictable\"})"
            }
          }]
          thresholds = [{ value = 0.9, label = "90 %: OOM kill near", color = "RED", direction = "ABOVE", targetAxis = "Y1" }]
          yAxis      = { label = "ratio", scale = "LINEAR" }
        })
      }
    },

    # --- Faults and dependencies ------------------------------------------------------------
    {
      x = 0, y = 95, w = 48, h = 4
      widget = {
        title         = "Injected faults and the Redis dependency"
        sectionHeader = { subtitle = "Envoy flags FI (fault abort) and DI (fault delay) mark Istio fault injection: is a game-day fault active?", dividerBelow = true }
      }
    },
    {
      x = 0, y = 99, w = 24, h = 16
      widget = {
        title = "Requests/s with an injected fault, by service and flag"
        xyChart = merge(local.observability_chart_defaults, {
          dataSets = [{
            plotType           = "STACKED_BAR"
            targetAxis         = "Y1"
            minAlignmentPeriod = "60s"
            timeSeriesQuery = {
              prometheusQuery = "sum by (destination_service_name, response_flags) (rate(istio_requests_total{${local.observability_mesh_out}, response_flags=~\".*(FI|DI).*\"}[5m]))"
            }
          }]
          yAxis = { label = "requests/s", scale = "LINEAR" }
        })
      }
    },
    {
      x = 24, y = 99, w = 24, h = 16
      widget = {
        title = "cartservice to redis-cart: TCP connections closed/s by flag"
        xyChart = merge(local.observability_chart_defaults, {
          dataSets = [{
            plotType           = "STACKED_BAR"
            targetAxis         = "Y1"
            minAlignmentPeriod = "60s"
            timeSeriesQuery = {
              prometheusQuery = "sum by (response_flags) (rate(istio_tcp_connections_closed_total{cluster=\"${local.cluster_name}\", reporter=\"source\", destination_service_name=\"redis-cart\"}[5m]))"
            }
          }]
          yAxis = { label = "connections/s", scale = "LINEAR" }
        })
      }
    },
  ]

  observability_platform_tiles = [
    {
      x = 0, y = 0, w = 48, h = 6
      widget = {
        title = "About this dashboard"
        text = {
          format  = "MARKDOWN"
          content = <<-EOT
            Cluster `${local.cluster_name}`: 3 × e2-standard-2 Spot nodes. Requests should stay under **70 %** of allocatable (`deploy/README.md`, resource budget).
            Spot preemptions are real incidents; they count against the journeys' error budgets. Control planes: Argo CD and istiod.
            Last row: what this telemetry costs. Estimates to compare against: `docs/observability.md`.
          EOT
        }
      }
    },

    # --- Nodes and capacity ------------------------------------------------------------------
    {
      x = 0, y = 6, w = 48, h = 4
      widget = {
        title         = "Nodes and capacity"
        sectionHeader = { subtitle = "GKE system metrics (free).", dividerBelow = true }
      }
    },
    {
      x = 0, y = 10, w = 12, h = 16
      widget = {
        title = "Ready nodes"
        scorecard = {
          timeSeriesQuery = {
            timeSeriesFilter = {
              filter = "metric.type=\"kubernetes.io/node/status_condition\" resource.type=\"k8s_node\" resource.label.\"cluster_name\"=\"${local.cluster_name}\" metric.label.\"condition\"=\"Ready\" metric.label.\"status\"=\"True\""
              aggregation = {
                alignmentPeriod    = "60s"
                perSeriesAligner   = "ALIGN_COUNT_TRUE"
                crossSeriesReducer = "REDUCE_SUM"
              }
            }
          }
          sparkChartView = { sparkChartType = "SPARK_LINE" }
          thresholds     = [{ value = 3, color = "YELLOW", direction = "BELOW" }]
        }
      }
    },
    {
      x = 12, y = 10, w = 18, h = 16
      widget = {
        title = "Requests / allocatable, whole cluster"
        xyChart = merge(local.observability_chart_defaults, {
          dataSets = [
            {
              plotType           = "LINE"
              targetAxis         = "Y1"
              minAlignmentPeriod = "60s"
              legendTemplate     = "CPU"
              timeSeriesQuery = {
                prometheusQuery = "sum(kubernetes_io:container_cpu_request_cores{${local.observability_k8s_ctr}}) / sum(kubernetes_io:node_cpu_allocatable_cores{${local.observability_k8s_node}})"
              }
            },
            {
              plotType           = "LINE"
              targetAxis         = "Y1"
              minAlignmentPeriod = "60s"
              legendTemplate     = "memory"
              timeSeriesQuery = {
                prometheusQuery = "sum(kubernetes_io:container_memory_request_bytes{${local.observability_k8s_ctr}}) / sum(kubernetes_io:node_memory_allocatable_bytes{${local.observability_k8s_node}})"
              }
            },
          ]
          thresholds = [{ value = 0.7, label = "70 % budget line", color = "YELLOW", direction = "ABOVE", targetAxis = "Y1" }]
          yAxis      = { label = "ratio", scale = "LINEAR" }
        })
      }
    },
    {
      x = 30, y = 10, w = 18, h = 16
      widget = {
        title = "CPU used / allocatable, per node"
        xyChart = merge(local.observability_chart_defaults, {
          dataSets = [{
            plotType           = "LINE"
            targetAxis         = "Y1"
            minAlignmentPeriod = "60s"
            timeSeriesQuery = {
              timeSeriesFilter = {
                filter = "metric.type=\"kubernetes.io/node/cpu/allocatable_utilization\" resource.type=\"k8s_node\" resource.label.\"cluster_name\"=\"${local.cluster_name}\""
                aggregation = {
                  alignmentPeriod    = "60s"
                  perSeriesAligner   = "ALIGN_MEAN"
                  crossSeriesReducer = "REDUCE_MEAN"
                  groupByFields      = ["resource.label.\"node_name\""]
                }
              }
            }
          }]
          yAxis = { label = "ratio", scale = "LINEAR" }
        })
      }
    },
    {
      x = 0, y = 26, w = 16, h = 16
      widget = {
        title = "Memory used (non-evictable) / allocatable, per node"
        xyChart = merge(local.observability_chart_defaults, {
          dataSets = [{
            plotType           = "LINE"
            targetAxis         = "Y1"
            minAlignmentPeriod = "60s"
            timeSeriesQuery = {
              timeSeriesFilter = {
                # Non-evictable only: page cache (evictable) would make every node look full.
                filter = "metric.type=\"kubernetes.io/node/memory/allocatable_utilization\" resource.type=\"k8s_node\" resource.label.\"cluster_name\"=\"${local.cluster_name}\" metric.label.\"memory_type\"=\"non-evictable\""
                aggregation = {
                  alignmentPeriod    = "60s"
                  perSeriesAligner   = "ALIGN_MEAN"
                  crossSeriesReducer = "REDUCE_SUM"
                  groupByFields      = ["resource.label.\"node_name\""]
                }
              }
            }
          }]
          yAxis = { label = "ratio", scale = "LINEAR" }
        })
      }
    },
    {
      x = 16, y = 26, w = 16, h = 16
      widget = {
        title = "CPU used (cores) by namespace"
        xyChart = merge(local.observability_chart_defaults, {
          dataSets = [{
            plotType           = "STACKED_AREA"
            targetAxis         = "Y1"
            minAlignmentPeriod = "60s"
            timeSeriesQuery = {
              prometheusQuery = "sum by (namespace_name) (rate(kubernetes_io:container_cpu_core_usage_time{${local.observability_k8s_ctr}}[5m]))"
            }
          }]
          yAxis = { label = "cores", scale = "LINEAR" }
        })
      }
    },
    {
      x = 32, y = 26, w = 16, h = 16
      widget = {
        title = "Container restarts per hour by namespace"
        xyChart = merge(local.observability_chart_defaults, {
          dataSets = [{
            plotType           = "STACKED_BAR"
            targetAxis         = "Y1"
            minAlignmentPeriod = "60s"
            timeSeriesQuery = {
              prometheusQuery = "sum by (namespace_name) (increase(kubernetes_io:container_restart_count{${local.observability_k8s_ctr}}[1h]))"
            }
          }]
          yAxis = { label = "restarts", scale = "LINEAR" }
        })
      }
    },

    # --- Spot ------------------------------------------------------------------------------------
    {
      x = 0, y = 42, w = 48, h = 4
      widget = {
        title         = "Spot preemptions"
        sectionHeader = { subtitle = "Compute Engine's preemption audit event (log-based metric spot_preemptions) and GKE's node interruption count.", dividerBelow = true }
      }
    },
    {
      x = 0, y = 46, w = 24, h = 16
      widget = {
        title = "Spot VMs preempted per hour, by node"
        xyChart = merge(local.observability_chart_defaults, {
          dataSets = [{
            plotType           = "STACKED_BAR"
            targetAxis         = "Y1"
            minAlignmentPeriod = "3600s"
            timeSeriesQuery = {
              timeSeriesFilter = {
                filter = "metric.type=\"logging.googleapis.com/user/${google_logging_metric.observability_spot_preemptions.name}\" resource.type=\"gce_instance\""
                aggregation = {
                  alignmentPeriod    = "3600s"
                  perSeriesAligner   = "ALIGN_SUM"
                  crossSeriesReducer = "REDUCE_SUM"
                  groupByFields      = ["metric.label.\"node\""]
                }
              }
            }
          }]
          yAxis = { label = "preemptions", scale = "LINEAR" }
        })
      }
    },
    {
      x = 24, y = 46, w = 24, h = 16
      widget = {
        title = "GKE node interruptions per hour, by type and reason"
        xyChart = merge(local.observability_chart_defaults, {
          dataSets = [{
            plotType           = "STACKED_BAR"
            targetAxis         = "Y1"
            minAlignmentPeriod = "3600s"
            timeSeriesQuery = {
              timeSeriesFilter = {
                filter = "metric.type=\"kubernetes.io/node/interruption_count\" resource.type=\"k8s_node\" resource.label.\"cluster_name\"=\"${local.cluster_name}\""
                aggregation = {
                  alignmentPeriod    = "3600s"
                  perSeriesAligner   = "ALIGN_SUM"
                  crossSeriesReducer = "REDUCE_SUM"
                  groupByFields      = ["metric.label.\"interruption_type\"", "metric.label.\"interruption_reason\""]
                }
              }
            }
          }]
          yAxis = { label = "interruptions", scale = "LINEAR" }
        })
      }
    },

    # --- Control planes ----------------------------------------------------------------------
    {
      x = 0, y = 62, w = 48, h = 4
      widget = {
        title         = "Control planes: Argo CD and istiod"
        sectionHeader = { subtitle = "What changed, and did the mesh accept it? Sync events are deploy markers for an incident timeline.", dividerBelow = true }
      }
    },
    {
      x = 0, y = 66, w = 16, h = 16
      widget = {
        title = "Argo CD applications by health"
        xyChart = merge(local.observability_chart_defaults, {
          dataSets = [{
            plotType           = "STACKED_AREA"
            targetAxis         = "Y1"
            minAlignmentPeriod = "60s"
            timeSeriesQuery = {
              prometheusQuery = "sum by (health_status) (argocd_app_info{cluster=\"${local.cluster_name}\"})"
            }
          }]
          yAxis = { label = "applications", scale = "LINEAR" }
        })
      }
    },
    {
      x = 16, y = 66, w = 16, h = 16
      widget = {
        title = "Argo CD syncs per hour by application and result"
        xyChart = merge(local.observability_chart_defaults, {
          dataSets = [{
            plotType           = "STACKED_BAR"
            targetAxis         = "Y1"
            minAlignmentPeriod = "60s"
            timeSeriesQuery = {
              prometheusQuery = "sum by (name, phase) (increase(argocd_app_sync_total{cluster=\"${local.cluster_name}\"}[1h]))"
            }
          }]
          yAxis = { label = "syncs", scale = "LINEAR" }
        })
      }
    },
    {
      x = 32, y = 66, w = 16, h = 16
      widget = {
        title = "istiod: config pushes/s and rejected config"
        xyChart = merge(local.observability_chart_defaults, {
          dataSets = [
            {
              plotType           = "LINE"
              targetAxis         = "Y1"
              minAlignmentPeriod = "60s"
              timeSeriesQuery = {
                prometheusQuery = "sum by (type) (rate(pilot_xds_pushes{cluster=\"${local.cluster_name}\"}[5m]))"
              }
            },
            {
              plotType           = "LINE"
              targetAxis         = "Y2"
              minAlignmentPeriod = "60s"
              legendTemplate     = "rejected by a proxy (right axis)"
              timeSeriesQuery = {
                prometheusQuery = "sum by (type) (increase(pilot_total_xds_rejects{cluster=\"${local.cluster_name}\"}[5m]))"
              }
            },
          ]
          yAxis  = { label = "pushes/s", scale = "LINEAR" }
          y2Axis = { label = "rejects", scale = "LINEAR" }
        })
      }
    },

    # --- Telemetry cost ----------------------------------------------------------------------
    {
      x = 0, y = 82, w = 48, h = 4
      widget = {
        title         = "What the telemetry costs"
        sectionHeader = { subtitle = "Managed Prometheus: $0.06 per million samples. Cloud Logging: 50 GiB a month free, then $0.50/GiB. Cloud Trace: 2.5 million spans a month free, then $0.20 per million.", dividerBelow = true }
      }
    },
    {
      x = 0, y = 86, w = 16, h = 16
      widget = {
        title = "Managed Prometheus samples per hour, by metric"
        xyChart = merge(local.observability_chart_defaults, {
          dataSets = [{
            plotType           = "STACKED_BAR"
            targetAxis         = "Y1"
            minAlignmentPeriod = "3600s"
            timeSeriesQuery = {
              timeSeriesFilter = {
                filter = "metric.type=\"monitoring.googleapis.com/billing/samples_ingested\" resource.type=\"global\""
                aggregation = {
                  alignmentPeriod    = "3600s"
                  perSeriesAligner   = "ALIGN_SUM"
                  crossSeriesReducer = "REDUCE_SUM"
                  groupByFields      = ["metric.label.\"metric_type\""]
                }
              }
            }
          }]
          yAxis = { label = "samples", scale = "LINEAR" }
        })
      }
    },
    {
      x = 16, y = 86, w = 16, h = 16
      widget = {
        title = "Cloud Logging bytes ingested per 2 h, by resource type"
        xyChart = merge(local.observability_chart_defaults, {
          dataSets = [{
            plotType           = "STACKED_BAR"
            targetAxis         = "Y1"
            minAlignmentPeriod = "7200s"
            timeSeriesQuery = {
              timeSeriesFilter = {
                filter = "metric.type=\"logging.googleapis.com/billing/bytes_ingested\" resource.type=\"global\""
                aggregation = {
                  alignmentPeriod    = "7200s"
                  perSeriesAligner   = "ALIGN_SUM"
                  crossSeriesReducer = "REDUCE_SUM"
                  groupByFields      = ["metric.label.\"resource_type\""]
                }
              }
            }
          }]
          yAxis = { label = "bytes", scale = "LINEAR" }
        })
      }
    },
    {
      x = 32, y = 86, w = 16, h = 16
      widget = {
        title = "Spans: received by the collector vs sent to Cloud Trace (per s)"
        xyChart = merge(local.observability_chart_defaults, {
          dataSets = [
            {
              plotType           = "LINE"
              targetAxis         = "Y1"
              minAlignmentPeriod = "60s"
              legendTemplate     = "received"
              timeSeriesQuery = {
                prometheusQuery = "sum(rate(otelcol_receiver_accepted_spans{cluster=\"${local.cluster_name}\"}[5m]))"
              }
            },
            {
              plotType           = "LINE"
              targetAxis         = "Y1"
              minAlignmentPeriod = "60s"
              legendTemplate     = "sent to Cloud Trace"
              timeSeriesQuery = {
                prometheusQuery = "sum(rate(otelcol_exporter_sent_spans{cluster=\"${local.cluster_name}\"}[5m]))"
              }
            },
            {
              plotType           = "LINE"
              targetAxis         = "Y1"
              minAlignmentPeriod = "60s"
              legendTemplate     = "export failed"
              timeSeriesQuery = {
                prometheusQuery = "sum(rate(otelcol_exporter_send_failed_spans{cluster=\"${local.cluster_name}\"}[5m]))"
              }
            },
          ]
          thresholds = [{ value = 10, label = "sampling cap (10 spans/s)", color = "YELLOW", direction = "ABOVE", targetAxis = "Y1" }]
          yAxis      = { label = "spans/s", scale = "LINEAR" }
        })
      }
    },
  ]
}

# Zero coordinates are left out of each tile, as the API leaves them out of what it returns, so a
# plan after apply shows no difference (not verified against the API yet).
resource "google_monitoring_dashboard" "observability_mesh" {
  dashboard_json = jsonencode({
    displayName = "Mamori mesh"
    labels      = { mamori = "" }
    mosaicLayout = {
      columns = 48
      tiles = [for t in local.observability_mesh_tiles : merge(
        { width = t.w, height = t.h, widget = t.widget },
        { for k, v in { xPos = t.x, yPos = t.y } : k => v if v != 0 },
      )]
    }
  })
}

resource "google_monitoring_dashboard" "observability_platform" {
  dashboard_json = jsonencode({
    displayName = "Mamori platform"
    labels      = { mamori = "" }
    mosaicLayout = {
      columns = 48
      tiles = [for t in local.observability_platform_tiles : merge(
        { width = t.w, height = t.h, widget = t.widget },
        { for k, v in { xPos = t.x, yPos = t.y } : k => v if v != 0 },
      )]
    }
  })
}
