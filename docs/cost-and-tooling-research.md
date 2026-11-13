# Cost and tooling research (2026-11-13)

Inputs to `PLAN.md` sections 3 and 6. Prices are list prices for asia-northeast1 (Tokyo) on the date above.
"My math" means computed from the listed rates at 730 hours a month. Re-check before M1; Infracost replaces the
estimates once the Terraform exists.

## Google Cloud

- **Free trial:** $300 for 90 days. A trial (non-billable) account can't add GPUs, use Marketplace or request quota
  increases. The credit **can't** be used for partner generative-AI models such as Claude; those need a paid billing
  account. Eligible only if you have never been a paying user of Google Cloud, Maps Platform or Firebase and have never
  signed up for the trial. Upgrading to a paid account keeps unused credit, but it still expires 90 days after
  signup. https://docs.cloud.google.com/free/docs/free-cloud-features
- **GKE free tier:** $74.40 a month per billing account, covering the $0.10/hour fee of one Autopilot or zonal
  Standard cluster. Not regional clusters, not compute. https://cloud.google.com/kubernetes-engine/pricing
- **Autopilot, Tokyo:** $0.0571 per vCPU-hour, $0.0063215 per GiB-hour; Spot pods $0.0171 and $0.0018964 (same page).
- **E2, Tokyo:** e2-standard-2 $0.08597/hour on demand, $0.051584 Spot; e2-standard-4 $0.17193 and $0.103168.
  https://cloud.google.com/products/compute/pricing/general-purpose, https://cloud.google.com/spot-vms/pricing
- **Disks:** node boot disk default is 100 GB pd-balanced ($0.13/GiB-month in Tokyo); pd-standard is $0.052.
  https://cloud.google.com/compute/disks-image-pricing
- **Estimates (my math):** zonal Standard, 3 × e2-standard-2 Spot with 30 GB pd-standard disks ≈ $118/month;
  3 × e2-standard-4 Spot ≈ $231–265/month; Autopilot Spot for ~5 vCPU / 10 GiB ≈ $76/month.

## Online Boutique

- Apache-2.0; 10 app services + a Locust load generator, plus redis-cart; Go, C#, Node.js, Python, Java; gRPC between
  services. Latest release v0.10.7 (2026-09-18), last commit 2026-10-27, maintained (mostly dependency bumps).
  https://github.com/GoogleCloudPlatform/microservices-demo
- The 12 Deployments request 1.57 vCPU and 1.34 GiB in total; most services 100m/64Mi, the load generator 300m/256Mi.
  https://github.com/GoogleCloudPlatform/microservices-demo/blob/v0.10.7/release/kubernetes-manifests.yaml
- OpenTelemetry is off by default; the `google-cloud-operations` kustomize component adds a Collector exporting traces
  only to Cloud Trace. Other components: `service-mesh-istio`, `network-policies`, `without-loadgenerator`,
  `shopping-assistant`. https://github.com/GoogleCloudPlatform/microservices-demo/tree/main/kustomize/components

## Cloud Monitoring and Managed Prometheus

- SLOs on GKE services, Istio, Cloud Service Mesh, or custom services.
  https://docs.cloud.google.com/stackdriver/docs/solutions/slo-monitoring/ui/define-svc
- Request-based SLIs (good/total) and windows-based SLIs (needed for latency percentiles; can hide bursty failures).
  https://docs.cloud.google.com/stackdriver/docs/solutions/slo-monitoring
- Burn-rate alerts via `select_slo_burn_rate`; Google suggests ~10× over 1–2 h (fast) and ~2× over 24 h (slow);
  lookback ≤ 24 h. https://docs.cloud.google.com/stackdriver/docs/solutions/slo-monitoring/alerting-on-budget-burn-rate
- Managed Prometheus: $0.06 per million samples (first 50 billion), no free allotment. Alerting-policy charges start
  2027-09-01. https://cloud.google.com/stackdriver/pricing
- Terraform: `google_monitoring_slo` (`basic_sli`, `request_based_sli`, `windows_based_sli`); provider v8.11.0
  (2026-11-10). No official Terraform example of a burn-rate alert policy found; `google_monitoring_alert_policy` with a
  `select_slo_burn_rate` filter should work (verify in M2).
  https://registry.terraform.io/providers/hashicorp/google/latest/docs/resources/monitoring_slo

## Datadog

- Free tier: up to 5 hosts, 1-day metric retention, infrastructure only. https://www.datadoghq.com/pricing/
- Free trial: 14 days, whole platform (APM, logs, Synthetics, alerting), no card. Extension not confirmed.
  https://www.datadoghq.com/free-datadog-trial/
- Open-source programme needs a 1-year-old project with 5+ regular contributors from 2+ companies: not us.
  https://www.datadoghq.com/partner/open-source/
- Terraform provider v4.30.0: `datadog_monitor`, `datadog_service_level_objective`, `datadog_slo_correction`,
  `datadog_synthetics_test`, `datadog_integration_gcp_sts`.
  https://github.com/DataDog/terraform-provider-datadog/tree/master/docs/resources
- **Bits AI SRE**, now **Bits Investigation** (GA Dec 2025): an autonomous RCA agent, billed in AI credits, ~$4 per
  investigation on average (Datadog, 2026-11-02). A useful benchmark for our agent's cost per investigation.
  https://docs.datadoghq.com/bits_ai/bits_investigation/,
  https://www.datadoghq.com/blog/bits-investigations-cost-speed-accuracy/

## Claude on Vertex AI

- Claude is in Model Garden. The **global** endpoint is recommended; regional endpoints cost 10% more and serve only
  older models. **asia-northeast1 is not offered**; use `region="global"` from Tokyo.
  https://platform.claude.com/docs/en/build-with-claude/claude-models-on-google-cloud/google-managed
- Auth: `roles/aiplatform.user` (needs `aiplatform.endpoints.predict`); `AnthropicVertex` uses Application Default
  Credentials. From GKE, Workload Identity Federation (always on in Autopilot, must be enabled on Standard).
  https://docs.cloud.google.com/kubernetes-engine/docs/how-to/workload-identity

## AI SRE agents to know (prior art)

| Project | What it is | Status |
|---|---|---|
| Gemini Cloud Assist investigations | RCA over logs, config, metrics; supports GKE | Preview; needs Premium Support since 2026-04-10, out of reach for individuals. https://docs.cloud.google.com/cloud-assist/investigations |
| GKE MCP server | Managed remote MCP, read-only endpoint (`get_k8s_logs`, `list_k8s_events`); OSS `gke-mcp` v0.15.0 | https://docs.cloud.google.com/kubernetes-engine/docs/reference/mcp, https://github.com/GoogleCloudPlatform/gke-mcp |
| kagent | Kubernetes-native agent framework on Google ADK; CNCF Sandbox; v1.0.1 (2026-11-12) | https://www.cncf.io/projects/kagent/ |
| HolmesGPT | RCA agent over Prometheus/Kubernetes with runbooks and MCP; CNCF Sandbox; v0.46.0 | https://www.cncf.io/blog/2026/01/07/holmesgpt-agentic-troubleshooting-built-for-the-cloud-native-era/ |
| k8sgpt | Rule-based cluster scanner with LLM explanations; CNCF Sandbox | https://www.cncf.io/projects/k8sgpt/ |
| Google SRE on agentic AI | Concepts and whitepaper, not an architecture | https://cloud.google.com/blog/products/devops-sre/how-google-sre-is-using-agentic-ai-to-improve-operations |

Our agent's place: these tools investigate. Few publish **guarded remediation with a measured evaluation**; that is
the gap the project targets. Consider running HolmesGPT on the same fault library as a comparison in M5 (optional).
