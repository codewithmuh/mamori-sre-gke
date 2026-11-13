# Observability

Status: M2 (#19), 2026-11-13. Written before the cluster exists: every number here is an estimate
until it is measured, and the "To measure" section lists how. Design context: [design.md](design.md)
(telemetry path), [PLAN.md](../PLAN.md) D3 and section 6 (cost). SLOs and alerts:
[slo/slo-design.md](slo/slo-design.md) (#18). What happens when a budget burns:
[slo/error-budget-policy.md](slo/error-budget-policy.md).

## What is collected, and where it goes

| Signal | Source | Path | Where to look | Cost a month (estimate) |
|---|---|---|---|---|
| Mesh metrics (requests, errors, latency per service) | Envoy sidecars and the Istio ingress gateway, port 15090 | Managed Service for Prometheus scrapes every 30 s, keeps 3 metric families | Cloud Monitoring (PromQL), dashboard *Mamori mesh*, SLOs | $2.40–4.80 |
| Control-plane metrics | istiod (:15014), Argo CD application controller (:8082), the trace collector (:8888) | Managed Prometheus, every 60 s, a short keep-list | dashboard *Mamori platform* | about $0.30 |
| Journey probes | the prober (#16), `:8080/metrics` | Managed Prometheus (scrape config owned by #16) | *Mamori CUJ SLOs* (#18) | about $1–2 (rough guess from its metric list) |
| Node, pod and container metrics | GKE system metrics | built in | dashboard *Mamori platform* | $0 (free) |
| Load balancer and Cloud Armor | Google Cloud metrics | built in | dashboard *Mamori mesh* | $0 (free) |
| Traces | 7 of the shop's services (OpenTelemetry) | OTLP/gRPC → collector in namespace `observability` → tail sampling → Cloud Trace | Trace Explorer | $0 (inside the free 2.5 million spans); at most about $4.70 |
| Logs | every container's stdout/stderr, GKE and audit logs | GKE logging agent → Cloud Logging; 90 % of the shop's INFO/DEBUG lines excluded | Logs Explorer | $0 if under 50 GiB; see Logs |
| Spot preemptions | Compute Engine system-event audit log | log-based metric `spot_preemptions` | dashboard *Mamori platform* | about $0 |

Total for this issue's part: about **$3–5 a month**, against the ~$10 for Managed Prometheus in
[PLAN.md](../PLAN.md) section 6 (my math, below). Without the filtering and sampling, the same
telemetry would cost roughly $60–80 a month (about $37 of Prometheus samples, $25 of spans, and up to
$20 of logs over the free allotment).

Files:

| Path | What |
|---|---|
| `deploy/observability/` | Argo CD Application `observability` (wave 5): namespace, collector, scrape configs |
| `deploy/observability/online-boutique-tracing/` | kustomize Component that turns tracing on in the shop (included by `deploy/online-boutique`) |
| `infra/bootstrap/observability_identity.tf` | the collector's IAM grant (Cloud Trace Agent, to its Kubernetes service account) |
| `infra/envs/lab/dashboards.tf` | dashboards *Mamori mesh* and *Mamori platform* |
| `infra/envs/lab/observability.tf` | log-based metric `spot_preemptions`, log exclusion `shop-info-logs-sampled` |

## Metrics: Managed Service for Prometheus

Managed Prometheus runs Google's collectors on every node (GKE add-on, on in `infra/envs/lab/gke.tf`).
`PodMonitoring` and `ClusterPodMonitoring` objects say what to scrape. Pricing: $0.06 per million
samples, no free allotment. A sample is one point of a counter or gauge; a histogram point costs 2 plus
one per bucket that holds any requests (Google Cloud Observability pricing, 2026-11-13).

### Scrape configs (`deploy/observability/monitoring-*.yaml`)

| Object | Namespace | Selects | Port | Every | Keeps |
|---|---|---|---|---|---|
| ClusterPodMonitoring `istio-sidecars` | all | `security.istio.io/tlsMode: istio` (every injected sidecar) | 15090 `/stats/prometheus` | 30 s | `istio_requests_total`, `istio_request_duration_milliseconds_{bucket,sum,count}`, `istio_tcp_connections_{opened,closed}_total` |
| PodMonitoring `istio-ingressgateway` | `istio-ingress` | `istio: ingressgateway` | 15090 | 30 s | same |
| PodMonitoring `istiod` | `istio-system` | `app: istiod` | 15014 `/metrics` | 60 s | pushes, push triggers, inbound config updates, rejects, convergence time, validation, injection |
| PodMonitoring `argocd-application-controller` | `argocd` | `app.kubernetes.io/name: argocd-application-controller` | `metrics` (8082) | 60 s | `argocd_app_info`, `argocd_app_condition`, `argocd_app_sync_total`, `argocd_cluster_connection_status` |
| PodMonitoring `otel-collector` | `observability` | the collector | `metrics` (8888) | 60 s | spans received, refused, sent, failed; sampling decisions; memory and CPU |

Choices:

- **Envoy's own port (15090), not the merged one (15020).** The shop's services expose no Prometheus
  metrics of their own, so the merged endpoint would only add Istio agent metrics.
- **Every standard Istio label stays** (`reporter`, `source_*` and `destination_*` workload, service
  and namespace, `response_code`, `grpc_response_status`, `response_flags`, ...). Dropping a label
  that is not 1:1 with another would merge series and break counts; dropping one that is 1:1 saves
  nothing, because billing counts samples, not labels.
- **Both reporters.** `reporter="source"` (the calling sidecar) is the one that sees Istio fault
  injection: the fault is injected in the caller's sidecar, so the callee never sees the request. SLIs
  and the dashboards use it. `reporter="destination"` (the callee's sidecar) stays for comparison: a
  difference between the two points at the network path or an injected fault rather than the server.
- **All histogram buckets stay.** Empty buckets are free, so dropping some would save little, and a
  mistake in the drop rule could remove the 500 and 1,000 ms buckets the latency SLOs need.
- **Dropped:** every `envoy_*` series, the request and response size histograms, and the services'
  trace export to the collector (`destination_service_namespace="observability"`).
- **Numeric port 15090.** Istio 1.31 runs the sidecar as a native sidecar (an init container);
  Managed Prometheus scrapes a numeric port whether or not a container declares it.
- **The gateway is selected separately**, because Istio's gateway template doesn't set the
  `tlsMode` label; nothing is scraped twice.

### Estimate (my math)

30 s scrapes are 87,600 points per series a month; 60 s, 43,800.

| Source | Samples per scrape | Samples a month | $ a month |
|---|---|---|---|
| Mesh, steady state: 16 caller→callee edges × 2 reporters, about 40 label combinations, each 1 counter + 1 histogram (2 + about 8 non-empty buckets), plus 8 TCP series | about 450 | about 39 million | about $2.40 |
| Mesh after weeks of game days (error combinations pile up in Envoy until the pod restarts): about 80 combinations | about 900 | about 79 million | about $4.80 |
| istiod | about 60 | about 2.6 million | $0.16 |
| Argo CD (about 12 Applications) | about 40 | about 1.8 million | $0.11 |
| Collector | about 10 | about 0.4 million | $0.03 |
| **This issue** | | **45–85 million** | **$2.70–5.10** |
| Not ours: prober (#16), recording rules (#18) | | rough: 25–35 million | rough: $1.50–2 |

For comparison, scraping 15090 unfiltered: Envoy's default stats are several hundred `envoy_*` series per
proxy, plus the size histograms, about 7,000 samples per scrape for 13 proxies, about 600 million a
month or **about $37** (estimate; check with the command under "To measure").

## Traces: OpenTelemetry → Cloud Trace

```
frontend, checkout, productcatalog (Go) ┐
currency, payment (Node.js)             ├─ OTLP/gRPC, plain text ─▶ otel-collector.observability:4317
email, recommendation (Python)          ┘                            memory_limiter → tail_sampling → batch
                                                                     └▶ googlecloud exporter ─▶ Cloud Trace
```

- **What Online Boutique v0.10.7 needs** (read in its source at commit `5b608cb`): `ENABLE_TRACING=1`,
  `COLLECTOR_SERVICE_ADDR=<host>:4317` and `OTEL_SERVICE_NAME`. Upstream's `google-cloud-operations`
  component sets these, but it also adds a collector inside the shop's namespace (with an init container
  that fetches the project ID) and turns Cloud Profiler on. We use only its variables, in
  `deploy/observability/online-boutique-tracing/`, pointed at one collector outside the mesh.
- **Not traced:** adservice and shippingservice (their `initTracing` is an empty TODO in v0.10.7),
  cartservice (no OpenTelemetry package), redis-cart, the load generator. Calls to them still show up
  as their callers' client spans.
- **Sampling, in the collector.** Every service samples 100 % (Go uses `AlwaysSample`), so the
  collector decides, once a trace is complete (10 s): keep every trace with an error, every trace
  slower than 1,000 ms, and 1 % of the rest, capped at 10 spans/s in total.
- **Estimate (my math):** the load generator makes about 2.6 frontend requests/s (10 users, 1–10 s
  think time, 1.5 requests per task); a page fans out to about 10 gRPC calls, about 20 spans. About 50
  spans/s, 130 million a month: about $25 if all were kept. Sampled: about 0.5 spans/s, 1.3 million a
  month, inside the free 2.5 million. Worst case (everything failing all month, the cap reached): 26
  million, about $4.70.
- **Identity:** the Kubernetes service account `observability/otel-collector` is granted
  `roles/cloudtrace.agent` directly (Workload Identity Federation for GKE, no Google service account,
  no key). The exporter finds the project from the GKE metadata server.
- **Envoy's own spans are off.** Istio could also report a span per hop, including the services that
  can't trace. That needs mesh config changes (`istiod-values.yaml`) and doubles the span count; left
  for later if the app spans aren't enough.
- **Collector resources:** requests 20m CPU / 128 MiB, limits 500m / 256 MiB (measured locally at
  about 100 MiB). One replica, because tail sampling needs every span of a trace in one process.

## Logs: Cloud Logging

GKE's logging agent sends every container's stdout and stderr, plus system and audit logs. Pricing:
50 GiB per project a month free, then $0.50 per GiB, with 30 days of retention. Audit logs in the
`_Required` bucket are free.

- **The shop is chatty.** Most services log a line or two per request (currency conversions, cart
  reads, shipping quotes, the frontend's request log), and the load generator prints its stats table
  every few seconds. About 30 lines/s under the steady load; at 0.6–1 KiB per log entry with its
  Kubernetes metadata, about 45–75 GiB a month from the shop alone (estimate).
- **Exclusion `shop-info-logs-sampled`** (`infra/envs/lab/observability.tf`) drops 90 % of the shop's
  INFO and DEBUG lines before they are stored (excluded entries are not billed). It keeps every
  WARNING and above, every entry with no or an unparsed severity, and everything outside namespace
  `boutique`. That brings the shop to about 5–8 GiB. Set
  `observability_shop_info_log_keep_percent = 100` to keep everything (for example during a game day
  that needs full request logs); 0 drops all INFO/DEBUG.
- **Istio access logs stay off** (the Istio default). The mesh metrics already count every request.
- **Log-based metric `spot_preemptions`:** counts Compute Engine's `compute.instances.preempted`
  system events for the cluster's node VMs, with the node name as a label. The platform dashboard shows
  it next to GKE's `kubernetes.io/node/interruption_count`, because Compute Engine's docs say a
  preemption can also appear only as a `compute.instances.delete` by `system@google.com`.

## Dashboards (`infra/envs/lab/dashboards.tf`)

**Mamori mesh**

- *Public path:* load balancer requests by response class, Cloud Armor 429s, p95 latency at the load
  balancer and from the gateway to the frontend, gateway→frontend requests by code and error ratio.
- *Golden signals per service* (caller's view): traffic, error ratio, p95 and p50 latency, CPU against
  requests and memory against limits for the shop's containers.
- *Faults and dependencies:* requests carrying Envoy's fault flags (`FI` abort, `DI` delay), so a
  forgotten game-day fault is visible; TCP connections from cartservice to redis-cart by flag.

**Mamori platform**

- *Nodes and capacity:* ready nodes, requests against allocatable with the 70 % budget line, CPU and
  memory per node, CPU by namespace, container restarts.
- *Spot preemptions:* `spot_preemptions` by node; GKE interruptions by type and reason.
- *Control planes:* Argo CD applications by health, syncs per hour (deploy markers), istiod pushes
  and rejected config.
- *Telemetry cost:* Managed Prometheus samples per metric, Cloud Logging bytes per resource type, spans
  received and sent by the collector against the 10 spans/s cap.

Journey SLOs, error budgets and burn rates are on #18's *Mamori CUJ SLOs* dashboard.

## How to query

**PromQL** works in Metrics Explorer and in dashboards. Scripts and the incident agent use the
Prometheus HTTP API of Managed Prometheus (needs `roles/monitoring.viewer`):

```sh
curl -s -H "Authorization: Bearer $(gcloud auth print-access-token)" \
  "https://monitoring.googleapis.com/v1/projects/$PROJECT_ID/location/global/prometheus/api/v1/query" \
  --data-urlencode 'query=sum by (destination_service_name) (rate(istio_requests_total{reporter="source"}[5m]))'
```

Useful queries:

```promql
# Error ratio per service, caller's view (HTTP 5xx or no response, or a gRPC status other than OK)
sum by (destination_service_name) (
    rate(istio_requests_total{reporter="source", destination_service_namespace="boutique", response_code=~"5..|0"}[5m])
  or
    rate(istio_requests_total{reporter="source", destination_service_namespace="boutique", response_code!~"5..|0", grpc_response_status!~"0|"}[5m])
)
/ sum by (destination_service_name) (rate(istio_requests_total{reporter="source", destination_service_namespace="boutique"}[5m]))

# p95 latency per service (ms)
histogram_quantile(0.95, sum by (destination_service_name, le) (
  rate(istio_request_duration_milliseconds_bucket{reporter="source", destination_service_namespace="boutique"}[5m])))

# Is an Istio fault active? (FI = injected abort, DI = injected delay)
sum by (source_workload, destination_service_name, response_flags) (
  rate(istio_requests_total{reporter="source", response_flags=~".*(FI|DI).*"}[5m]))

# Caller saw failures the callee never saw: a fault or network problem between them.
# Keyed on destination_service_name: for a request the caller's sidecar answers itself (an injected
# abort), Istio may not know the destination workload and report it as "unknown".
sum by (destination_service_name) (rate(istio_requests_total{reporter="source", response_code=~"5..|0"}[5m]))
  - sum by (destination_service_name) (rate(istio_requests_total{reporter="destination", response_code=~"5..|0"}[5m]))

# What changed? Argo CD syncs in the last hour, and apps that aren't healthy
sum by (name, phase) (increase(argocd_app_sync_total[1h]))
argocd_app_info{health_status!="Healthy"}

# Did a proxy reject mesh config?
sum by (type) (increase(pilot_total_xds_rejects[15m]))
```

Google Cloud metrics in PromQL take the form `kubernetes_io:node_cpu_allocatable_utilization`
(first `/` becomes `:`, the rest `_`), with `monitored_resource="k8s_node"` where a metric fits more
than one resource type.

**Logs Explorer:**

```
# Errors from the shop
resource.type="k8s_container" resource.labels.namespace_name="boutique" severity>=WARNING

# Spot preemptions
logName="projects/PROJECT_ID/logs/cloudaudit.googleapis.com%2Fsystem_event"
protoPayload.methodName="compute.instances.preempted"

# Kubernetes events for the shop (pod kills, OOM, scheduling)
logName="projects/PROJECT_ID/logs/events" jsonPayload.involvedObject.namespace="boutique"
```

**Traces:** Trace Explorer in the console, filtered by `service.name` (the `OTEL_SERVICE_NAME`
values) or by status. Kept traces are the errors, the slow ones and a 1 % sample, so a missing trace
for a successful fast request is normal. Reading traces needs `roles/cloudtrace.user`.

## Apply order and prerequisites

1. Bootstrap enables `cloudtrace.googleapis.com` and gives `tf-apply` `roles/monitoring.editor` and
   `roles/logging.configWriter` (requested in #19's report; `bootstrap/apis.tf` and `bootstrap/iam.tf`).
2. `make up`: the lab root creates the cluster, the dashboards, the log-based metric and the
   exclusion; Argo CD syncs `deploy/observability` (wave 5) and the shop with tracing on.
3. After the first `make up`: set `gke_workload_pool_exists = true` in
   `infra/bootstrap/terraform.tfvars` and apply bootstrap again. That grants the collector Cloud Trace
   Agent; the workload identity pool only exists once the cluster does. Until then the collector logs
   `PermissionDenied` from Cloud Trace and drops the traces; nothing else is affected.

## To measure (M2)

| What | How | Replaces |
|---|---|---|
| Samples a month | *Mamori platform* → samples per metric (`monitoring.googleapis.com/billing/samples_ingested`) after a day, × 30 | the Prometheus estimate |
| Envoy series per proxy, unfiltered | `kubectl -n boutique exec deploy/frontend -c istio-proxy -- pilot-agent request GET stats/prometheus \| grep -vc '^#'` | the $37 "unfiltered" figure |
| Log volume | *Mamori platform* → Cloud Logging bytes, and the Logs Storage page | the 45–75 GiB estimate |
| Spans | collector panel (received vs sent) and `cloudtrace.googleapis.com/billing/spans_ingested` | the 50 spans/s estimate |
| Collector size | `kubectl -n observability top pod` | the 20m / 128 MiB requests |
| Dashboards | open both, check every panel has data; run `terraform plan` after the first apply and check it shows no change to the dashboards | assumption that the JSON round-trips |
| Spot metric | after the first preemption, check `spot_preemptions` counted it | the log filter |

## Not verified yet

- Nothing here has run on a cluster. The collector config passes `otelcol-contrib validate` and ran
  locally with test spans; the manifests pass `deploy/validate.sh`; the Terraform passes `validate`
  and tflint. Panels, PromQL and filters are checked only against documentation.
- Whether IAM refuses a grant to a principal in a pool that doesn't exist yet: assumed from reports of
  that error for the older member format, hence the flag.
- Whether the workload identity pool survives `make down` (cluster deleted). If it doesn't, the grant
  has to be applied again after the next `make up`.
- That `kubernetes.io/node/interruption_count` counts Spot preemptions (Google documents it for
  GPU/TPU disruption; its reasons include `Eviction` and `PreemptionEvent`).
