# Production-readiness controls

Status: M6 (#30), written 2026-11-13. Every policy runs in **dryrun**: Gatekeeper reports, it blocks
nothing. No cluster exists yet, so the findings below come from checking the Git manifests offline;
the cluster's audit will replace them.

The readiness checks of a public checklist become admission policies for OPA Gatekeeper (D10, ADR
0011). Gatekeeper's audit re-checks everything running every minute, and the weekly evidence bundle
([evidence-bundle.md](evidence-bundle.md)) records the results.

| Where | What |
|---|---|
| `deploy/apps/gatekeeper.yaml` (wave 5), `deploy/gatekeeper/` | Gatekeeper chart 3.23.1, values, namespace exemptions (`config/config.yaml`), metrics scrape |
| `deploy/apps/policy.yaml` (wave 6), `deploy/policy/` | 14 ConstraintTemplates from the gatekeeper-library (`templates/`), one Constraint per control (`constraints/`) |
| `deploy/policy/tests/`, `deploy/policy/check.sh` | gator test suite and the offline check of the rendered manifests |
| `infra/envs/lab/firewall_gatekeeper.tf` | Lets the GKE control plane reach the webhook on TCP 8443 |

## The checklist

Two public sources, both checked on 2026-11-13:

- **[GKE]** Google Cloud Architecture Center, [Best practices for running cost-optimized Kubernetes
  applications on GKE](https://docs.cloud.google.com/architecture/best-practices-for-running-cost-effective-kubernetes-applications-on-gke),
  section "Prepare cloud-based Kubernetes applications" and its "Summary of best practices" table. The
  page itself suggests enforcing these practices with Policy Controller, Google's managed build of
  Gatekeeper.
- **[K8s]** Kubernetes documentation: [Pod Security Standards](https://kubernetes.io/docs/concepts/security/pod-security-standards/)
  (Baseline and Restricted profiles), and for the items that page doesn't cover,
  [Images](https://kubernetes.io/docs/concepts/containers/images/) (the `:latest` note),
  [Recommended Labels](https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/)
  and the [Application Security Checklist](https://kubernetes.io/docs/concepts/security/application-security-checklist/).

## Controls

Each Constraint carries its ID in the annotation `mamori.dev/control-id`, plus the checklist item and
source URL. "Git today" is what `deploy/policy/check.sh` reports for the M1 manifests (see
[What would be flagged today](#what-would-be-flagged-today)).

| ID | Checklist item | Source | Template → Constraint | Matches | Status | Git today |
|---|---|---|---|---|---|---|
| PR-01 | Set appropriate resource requests and limits: CPU and memory requests, a memory limit. No CPU limit required (GKE: "set an unbounded CPU limit"; K8s: a CPU limit "might be set on sensitive workloads") | GKE; K8s app security checklist | `k8srequiredresources` → `pr-01-resources` | Pods | dryrun | 2 |
| PR-02 | Set meaningful readiness and liveness probes (HTTP, TCP, gRPC or exec) | GKE | `k8srequiredprobes` → `pr-02-probes` | Pods | dryrun | 4 |
| PR-03 | Avoid the `:latest` tag; name a tag and/or a digest | K8s Images | `k8sdisallowedtags` → `pr-03-no-latest-tag` | Pods | dryrun | 0 |
| PR-04 | Image security: only images from registries we know and scan | K8s app security checklist | `k8sallowedreposv2` → `pr-04-allowed-registries` | Pods | dryrun | 0 |
| PR-05 | No privileged containers | K8s PSS Baseline | `k8spspprivilegedcontainer` → `pr-05-no-privileged` | Pods | dryrun | 0 |
| PR-06 | No host PID or IPC namespace | K8s PSS Baseline "Host Namespaces" | `k8spsphostnamespace` → `pr-06-no-host-pid-ipc` | Pods | dryrun | 0 |
| PR-07 | No host network, no host ports | K8s PSS Baseline "Host Namespaces", "Host Ports" | `k8spsphostnetworkingports` → `pr-07-no-host-network-ports` | Pods | dryrun | 0 |
| PR-08 | Run as non-root | K8s PSS Restricted "Running as Non-root" | `k8spspallowedusers` → `pr-08-run-as-non-root` | Pods | dryrun | 0 |
| PR-09 | `allowPrivilegeEscalation: false` | K8s PSS Restricted "Privilege Escalation" | `k8spspallowprivilegeescalationcontainer` → `pr-09-no-privilege-escalation` | Pods | dryrun | 0 |
| PR-10 | Drop ALL capabilities, add back at most `NET_BIND_SERVICE` | K8s PSS Restricted "Capabilities" | `k8spspcapabilities` → `pr-10-drop-capabilities` | Pods | dryrun | 0 |
| PR-11 | Recommended labels `app.kubernetes.io/name` and `app.kubernetes.io/part-of` | K8s Recommended Labels | `k8srequiredlabels` → `pr-11-recommended-labels` | Pods | dryrun | 12 |
| PR-12 | No `LoadBalancer` Services: use container-native load balancing through the Gateway (D11) | GKE | `k8sblockloadbalancer` → `pr-12-no-loadbalancer` | Services | dryrun | 0 |
| PR-13 | No `NodePort` Services, same reason | GKE | `k8sblocknodeport` → `pr-13-no-nodeport` | Services | dryrun | 0 |
| PR-14 | Add a Pod Disruption Budget: the policy half. A PDB must leave room for one eviction, or node drains (GKE upgrades, Spot replacement) wait on it | GKE | `k8spoddisruptionbudget` → `pr-14-pdb-allows-drain` | Deployments, StatefulSets, PDBs | dryrun | 0 |

Templates are copied unchanged (apart from trailing spaces) from
[gatekeeper-library](https://github.com/open-policy-agent/gatekeeper-library) commit `a8d4799`
(2026-10-21); `deploy/policy/templates/kustomization.yaml` lists each upstream path and version.

### Checklist items no policy covers yet

| Item | Source | Why not a policy now | Where it is handled |
|---|---|---|---|
| Memory request equal to the memory limit (GKE: "Set the same amount of memory for the request and limit") | GKE | Upstream Online Boutique sets every memory limit above its request; `k8scontainerratios` could check it later | Sizing review in M4 (#29, #27) |
| A PDB for every Deployment with more than one replica | GKE "Add Pod Disruption Budget" | The library has no template that requires one (only PR-14's sanity check) | Manual review; PDBs and replicas arrive with M4 (#27) |
| Shut down in line with Kubernetes expectations (`terminationGracePeriodSeconds`, preStop) | GKE | Not in the gatekeeper-library. Google's Policy Controller library has one for Spot nodes (`gkespotvmterminationgrace`, at most 15 s); not adopted | Review; M4 resilience work |
| Lean containers | GKE | Image size is not visible at admission | Image builds in CI (#28) |
| Read-only root filesystem, seccomp `RuntimeDefault`, no automounted service-account token | K8s app security checklist, PSS Restricted "Seccomp" | Templates exist; left out of the first set. Online Boutique sets no seccomp profile, so that one would flag all 12 shop pods | Next candidates, once the first set is clean |
| NetworkPolicies; image scanning and signature checks | K8s app security checklist | Not a Pod field: NetworkPolicy is its own object; signatures need Binary Authorization or an external-data provider | Not planned in M6 |

## Exemptions

Every exemption is in Git and listed here; change both together.

**Namespaces** (`deploy/gatekeeper/config/config.yaml`, `processes: ["*"]`: no admission check, no
audit, no cache):

| Namespace | Reason |
|---|---|
| `kube-system` | GKE's own system workloads (kube-dns, konnectivity, Dataplane V2, logging and metrics agents, CSI drivers). Google configures and upgrades them, several need host access by design, and a policy that ever blocked them could break the cluster. |
| `gke-*` | GKE-managed add-on namespaces (for example `gke-managed-system`, `gke-gmp-system`); same reason. |
| `gmp-*` | Managed Service for Prometheus: GKE runs its operator and collectors in `gmp-system` and keeps their settings in `gmp-public`. |
| `istio-system` | istiod, from Istio's chart. If it could not start, no pod in the mesh could get a sidecar. Its settings are reviewed in `deploy/istio/istiod-values.yaml`. |
| `argocd` | Argo CD is how a wrong policy gets fixed; it must never be blocked by one. Installed by `make up`, not by Argo CD. |
| `gatekeeper-system` | The engine does not judge itself: a deny here could stop Gatekeeper from starting. The chart also keeps this namespace out of the webhook. |
| `chaos-mesh` | Chaos Mesh's chaos-daemon must run privileged with the host's PID and network namespaces to inject faults; every finding there would be permanent. |

What the namespace exemptions hide today (gator, M1 manifests): in `argocd`, 7 missing probes and 7
images from `quay.io/argoproj` and `ecr-public.aws.com`; in `istio-system`, istiod's missing liveness
probe.

Not exempt on purpose: `istio-ingress` (the gateway is our configuration), `boutique`, and every
namespace M2–M6 adds. The Datadog Agent (M3, namespace `datadog`) will show up in audit during its
14-day window: a node agent like that typically runs as root with host access. That is accurate, so
it stays visible.

**Images:**

| Image | Exempt from | Reason |
|---|---|---|
| `docker.io/istio/proxyv2:*` | PR-08, PR-10 | Without the Istio CNI node agent, istiod injects `istio-init`, which runs as root with `NET_ADMIN` and `NET_RAW` to write each pod's iptables rules. Accepted risk for the lab; installing Istio CNI removes it (it adds a privileged DaemonSet in `istio-system` instead). The sidecar itself runs as user 1337 and would pass. |
| `auto` | PR-03 (and allowed in PR-04) | Placeholder that Istio's gateway chart writes into its Deployment; the injection webhook swaps in the pinned proxy image before the Pod exists. Only offline checks of Git ever see it. |

**Engine settings that change what is checked** (`deploy/gatekeeper/values.yaml`):

- The webhook **fails open** (`failurePolicy: Ignore`, 3 s timeout), including the check that guards
  the `admission.gatekeeper.sh/ignore` namespace label. If Gatekeeper is down, nothing is checked at
  admission; audit catches up when it is back.
- **Rego only** (`enableK8sNativeValidation: false`). Six of these templates also carry CEL, which
  Gatekeeper prefers by default. At the pinned commit, gator showed the CEL code of two of them is
  weaker than their Rego: host-network-ports misses a host port on a container whose other ports have
  none (PR-07), and requiredlabels reports each missing label twice, the second time as
  "regex mismatch" (PR-11). All 14 templates have Rego. Worth an upstream issue.
- No ValidatingAdmissionPolicies are generated, no mutation, no external data.

## What would be flagged today

`deploy/policy/check.sh` on the manifests `deploy/validate.sh` renders (M1 base plus this change),
2026-11-13. Workloads are expanded into the Pods they would create; exempt namespaces are skipped.

| Control | Object | Finding | Proposed fix |
|---|---|---|---|
| PR-01 (×2) | `boutique/loadgenerator` | Init container `frontend-check` (busybox) has no CPU or memory request and no memory limit | Patch in `deploy/online-boutique/kustomization.yaml`: 10m / 16Mi, limit 32Mi. Adds nothing to the pod's effective request (requested from #32) |
| PR-02 (×2) | `boutique/loadgenerator` | Locust has no readiness or liveness probe | Decide before warn: it serves no traffic and a dead Locust exits, which restarts the container anyway. Either a documented exception (label selector in `pr-02-probes`) or an `exec` probe |
| PR-02 (×2) | `istio-ingress/istio-ingressgateway` | No probes in Git. On the cluster only the liveness probe is missing: injection adds a readiness probe on 15021 | Decide before warn: Istio's gateway template sets readiness only; if Envoy crashes, the container exits and the kubelet restarts it. Documented exception, or a liveness probe if the chart allows one |
| PR-11 (×12) | every Online Boutique Deployment | Upstream pods carry only `app: <service>` | Patch in `deploy/online-boutique/kustomization.yaml`: `app.kubernetes.io/part-of: mamori` on every object and pod template, `app.kubernetes.io/name: <service>` on each pod template (requested from #32). Checked: with it, PR-01 and PR-11 drop to 0 |

Expected on the cluster (audit sees the running Pods, Istio's injected containers included):
PR-01 2, PR-02 3, PR-11 12, everything else 0; after the requested patch, PR-02 3 only. Workloads that
M2–M6 add will show up as they arrive. Not yet measured.

## Reading the results

- `kubectl get constraints` lists every control with its enforcement action and total violations.
  `kubectl get k8srequiredlabels pr-11-recommended-labels -o yaml` shows `status.totalViolations`,
  the time of the last audit and the first 50 findings (`constraintViolationsLimit`).
- Cloud Logging, namespace `gatekeeper-system`: audit writes one JSON line per finding with
  `event_type="violation_audited"`; the webhook writes `event_type="violation"` for each admission
  request that breaks a constraint. Under dryrun that line still says "denied admission", and its
  `constraint_action` field says `dryrun`.
- Managed Service for Prometheus scrapes `gatekeeper_*` metrics: `gatekeeper_violations` by
  enforcement action, `gatekeeper_audit_last_run_time` (a stale value means audit stopped), webhook
  request counts and latency.
- The weekly evidence bundle ([evidence-bundle.md](evidence-bundle.md)) records each constraint's
  control ID, action and violation count.

## From dryrun to warn to deny

One constraint at a time, each step its own pull request:

1. **Clean up.** Fix every finding of the constraint, or add a narrow exemption with its reason to
   the tables above. Wait until audit shows 0 for that constraint for two weeks.
2. **Before the first `warn`: judge workloads at admission.** Today the Pod policies see Pods, and a
   Pod is created by a ReplicaSet controller, so a warning or a denial never reaches Argo CD or the
   person who changed the Deployment. Deploy the ExpansionTemplates from
   `deploy/policy/tests/expansion.yaml` (move them under `deploy/policy/`), so the Deployment itself
   is checked when Argo CD applies it. From then on audit reports most findings twice (workload and
   Pod); the evidence bundle should count by workload.
3. **`warn`** for one to two weeks: set `spec.enforcementAction: warn` in the constraint's file.
   `kubectl apply` and Argo CD sync results show the warning; nothing is blocked.
4. **Before the first `deny`: make Gatekeeper a dependable dependency.**
   - `replicas: 2`, `pdb.controllerManager.minAvailable: 1`, `priorityClassName`
     `system-cluster-critical` and `resourceQuota: true` (about +25m CPU and 256 MiB).
   - Choose the failure policy. `Ignore` keeps every write working when Gatekeeper is down, and the
     policy is skipped meanwhile. `Fail` guarantees the check but makes Gatekeeper part of every
     write: first exempt the system namespaces at the webhook too
     (`validatingWebhookExemptNamespacesLabels`, or `controllerManager.exemptNamespaces` plus the
     `admission.gatekeeper.sh/ignore` label), because the Config exemptions are applied inside
     Gatekeeper, after the API server has already called it.
   - Confirm the firewall rule works: the webhook's request count in the metrics rises when a Pod
     is created.
5. **`deny`**: start with the controls that have never had a finding and only one way to break them
   (PR-12, PR-13, PR-05, PR-06, PR-07), then the rest.

**Rollback:** revert the commit; Argo CD syncs the old `enforcementAction` within about three
minutes. In an emergency, a hand edit doesn't last: self-heal puts the constraint back, and `root`
re-creates a deleted Application. Turn off automated sync on `root` and `policy` first, then set the
constraint's `enforcementAction` to `dryrun` with `kubectl edit`; revert in Git and turn sync back on
afterwards.

## Test

```sh
deploy/policy/check.sh             # gator verify: 15 tests, 63 cases
deploy/policy/check.sh --render    # plus deploy/validate.sh and gator test on what it renders
deploy/policy/check.sh /tmp/render # gator test on an existing RENDER_DIR=/tmp/render deploy/validate.sh
```

It runs gator v3.23.1 from a digest-pinned image when `gator` is not installed, with the CEL engine
off like the cluster. Every template has passing and failing cases using this repo's own Constraints,
and each failing case asserts the expected message, so a case proves that the intended rule fired.
gator also rejects a Constraint whose parameters don't match its template's schema, which covers what
`deploy/validate.sh` can't: Constraint kinds have no published JSON schema, so kubeconform skips
them (they live in a plain directory that validate.sh does not render).

Failing cases, and the Istio exemption case, are written as AdmissionReview requests, the form the
API server sends to Gatekeeper. They are test inputs, not manifests to deploy, and CI's Trivy scan
doesn't read them as Kubernetes manifests; the passing cases are plain, hardened manifests that
Trivy does check.

To update the library: copy the new `template.yaml` over the file, update the commit in
`deploy/policy/templates/kustomization.yaml`, run the tests.

## Cost

Requests: 2 pods (webhook, audit) × 25m CPU / 256 MiB, memory limit 512 MiB each: **50m CPU,
512 MiB** in total. Sizes are estimates for 14 templates on a 3-node cluster; measure with
`kubectl top` and adjust. The firewall rule is free; the `gatekeeper_*` metrics (about a hundred
series at one sample a minute, estimate) cost cents a month.

## Not verified yet

Nothing here has run on a cluster. To check after `make up`:

- The webhook is reachable through `mamori-allow-control-plane-gatekeeper` (on current GKE the
  control plane may reach nodes through Private Service Connect; the Istio rule makes the same
  assumption). Symptom if not: API server logs "failed calling webhook", every write waits 3 s.
- Argo CD shows `gatekeeper` and `policy` Synced and Healthy, with no lasting drift on the webhook
  configuration or on fields Gatekeeper defaults in the ConstraintTemplates.
- Audit memory and CPU stay within the requests; the audit run time.
- The predicted findings above.
