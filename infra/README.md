# infra

Terraform for the mamori lab on Google Cloud: one GKE cluster in Tokyo with its network, image
registry, public edge, budget and spend guard. Two flat roots, no modules:

| Root | State | Applied by | Owns |
|---|---|---|---|
| `bootstrap/` | local file (`bootstrap/terraform.tfstate`, gitignored) | the maintainer, once, with own credentials | APIs, state bucket, Workload Identity Federation, all service accounts, the spend-guard custom role, the spend-guard source bucket, the `mamori` image repository, CI permissions |
| `envs/lab/` | GCS bucket from bootstrap, prefix `lab` | `make up` locally, or CI from the GitHub environment `lab` on `main` | everything billable that `make down` removes: VPC, NAT, firewall rules, GKE, static IP, Cloud Armor, certificate, budget, spend guard (with its own image repository), and the role bindings of the lab's service accounts |

Why identities sit in bootstrap: the CI apply account (`tf-apply`) then can't create service
accounts, edit roles or grant arbitrary roles. Widening what CI may do takes a bootstrap apply
by the maintainer. The image repository sits there too, so images survive `make down`.

Files are split by concern. Bootstrap: `apis.tf`, `storage.tf`, `registry.tf`, `wif.tf`,
`iam.tf`. Lab: `network.tf`, `gke.tf`, `edge.tf`, `budget.tf`, `spend_guard.tf`, `iam.tf`. Both
also have `versions.tf`, `providers.tf`, `variables.tf`, `locals.tf` and `outputs.tf`.

## Apply order

1. **By hand, once:** create the project and link the billing account (trial signup). Run
   `gcloud auth application-default login` with an account that is project Owner and Billing
   Account Administrator.
2. **Bootstrap:** `make bootstrap`, or:
   ```sh
   cd infra/bootstrap
   export TF_VAR_project_id=... TF_VAR_billing_account_id=...
   terraform init && terraform apply
   ```
   Keep `infra/bootstrap/terraform.tfstate` safe; it is the only record of this root.
   A copy in the state bucket (`gcloud storage cp terraform.tfstate gs://<state_bucket>/bootstrap/`)
   is enough.
3. **GitHub settings, by hand** (from `terraform output` in `infra/bootstrap`):

   | Repository variable | Value |
   |---|---|
   | `GCP_PROJECT_ID` | the project ID |
   | `GCP_WIF_PROVIDER` | output `wif_provider` |
   | `GCP_TF_PLAN_SA` | output `tf_plan_sa` |
   | `GCP_TF_APPLY_SA` | output `tf_apply_sa` |
   | `TF_STATE_BUCKET` | output `state_bucket` |

   Also add the billing account ID as the repository secret `GCP_BILLING_ACCOUNT_ID`. Then make
   sure the environment **`lab`** exists with a deployment branch rule that allows only `main`,
   and yourself as required reviewer (`make bootstrap` sets this up). `tf-apply` itself already
   refuses any job that isn't both in `lab` and on `main` (see "IAM"). The environment rules add
   the human approval step.
4. **Lab:** `make up`, or:
   ```sh
   cd infra/envs/lab
   terraform init -backend-config="bucket=$TF_STATE_BUCKET" -backend-config="prefix=lab"
   terraform apply
   ```
   This needs `tools/spend-guard` (the function's Go code) in the checkout; plan zips it. Do the
   first apply before relying on pull request plans: `tf-plan` can't write the empty state
   object that the first `init` of a new prefix creates.
5. **DNS, by hand,** where codewithmuh.com's DNS is hosted. Add the two records from
   `terraform output dns_records_to_add`:
   - `A` for the frontend hostname, pointing to the static IP. The IP is new after every
     `make down` / `make up`, so update this record each time.
   - `CNAME` for `_acme-challenge.<hostname>` from the DNS authorization. The certificate stays
     `PROVISIONING` until it resolves. Check the value after a fresh `make up`; whether it stays
     the same for a re-created authorization isn't documented.

## Inputs

`bootstrap/`:

| Variable | Default | Notes |
|---|---|---|
| `project_id` | none | |
| `billing_account_id` | none | sensitive |
| `region` | `asia-northeast1` | buckets |
| `github_repository` | `codewithmuh/mamori-sre-gke` | the only repository the WIF pool accepts |
| `github_repository_id` | `1456357674` | must match too, so a re-created repository with the same name is refused |
| `github_subject_prefix` | `repo:codewithmuh@51082957/mamori-sre-gke@1456357674` | GitHub's immutable `sub` prefix for this repository (`gh api repos/<owner>/<repo>/actions/oidc/customization/sub`) |
| `github_apply_environment` | `lab` | |

`envs/lab/` (CI sets the first two; everything else has a default):

| Variable | Default | Notes |
|---|---|---|
| `project_id` | none | |
| `billing_account_id` | none | sensitive, because PR plans are posted on a public repository |
| `zone` | `asia-northeast1-a` | the region is derived from it |
| `frontend_hostname` | `mamori.codewithmuh.com` | |
| `master_authorized_networks` | `[]` | see "Control-plane access" below |
| `armor_rate_limit_per_minute` | `600` | requests per client IP per minute before a 429 |
| `monthly_budget_usd` | `200` | cost after free-tier credits, before the trial credit; keep below the remaining trial credit until the account is upgraded in M5 |
| `spend_guard_threshold` | `1.0` | lower it briefly to test the guard with a real notification |
| `spend_guard_dry_run` | `false` | |

Examples: `terraform.tfvars.example` in each root.

## Outputs

`bootstrap/`: `wif_provider`, `tf_plan_sa`, `tf_apply_sa`, `state_bucket`,
`artifact_registry_url`, `spend_guard_source_bucket`.

`envs/lab/`: `cluster_name`, `cluster_location`, `get_credentials_command` (IP endpoint),
`get_credentials_command_dns` (DNS endpoint), `node_pool_name`, `artifact_registry_url`,
`frontend_ip`, `dns_records_to_add` (map of `{name, type, data}`), `cert_map_name`,
`armor_policy_name`, `budget_alerts_topic`.

Names other parts of the repository depend on: cluster `mamori`, node pool `spot-pool`, static IP
`mamori-frontend-ip`, certificate map `mamori-cert-map`, Cloud Armor policy
`mamori-frontend-armor`, topic `budget-alerts`, function `spend-guard`, node network tag
`mamori-node`.

## Design notes

### Cluster

Zonal GKE Standard cluster on the REGULAR channel, with no default node pool. The only pool is
`spot-pool`: 3 × `e2-standard-2` Spot nodes with 30 GB `pd-standard` disks. It has a fixed size
(no autoscaling in M1) and surge upgrades (`max_surge = 1`, `max_unavailable = 0`). Other
settings:

- Private nodes, Workload Identity, `GKE_METADATA` on nodes, Shielded nodes with secure boot.
- Dataplane V2, so NetworkPolicy is enforced without Calico. It can only be chosen at creation.
- Gateway API standard channel, Managed Service for Prometheus.
- `deletion_protection = false`, so `make down` works.
- The node service account `gke-nodes` holds only `roles/container.defaultNodeServiceAccount`,
  plus read access to the `mamori` image repository (granted in `bootstrap/registry.tf`).

**Quota:** 6 vCPU steady, 8 vCPU during a surge upgrade. A trial account can't request quota
increases, so check the region's `CPUS` and the global `CPUS_ALL_REGIONS` quota in M1.

**Spend guard vs Terraform:** the node pool sets `ignore_node_count_changes = true`. An apply after
the spend guard fired leaves the pool at 0 nodes instead of silently restarting the spend.
`make resume` resizes it back to 3 with gcloud.

**Firewall rules** (target tag `mamori-node`):

- Control plane `172.16.0.0/28` to TCP 15017, for Istio's webhooks. GKE opens only 443 and
  10250 to private nodes. Gatekeeper (M6) will need 8443 added to the same rule.
- Google's load balancer and health-check ranges `35.191.0.0/16` and `130.211.0.0/22` to TCP 80
  and 15021 on the Istio ingress gateway pods.

### Control-plane access

The Kubernetes API server has two public endpoints:

- **IP endpoint** (`get_credentials_command`). With `master_authorized_networks = []` (the
  default), it accepts TLS connections from any IP address. Setting it to your own address
  (`[{ cidr_block = "x.x.x.x/32", display_name = "home" }]`) turns authorized networks on, with
  Google Cloud public ranges excluded.
- **DNS endpoint** (`get_credentials_command_dns`, i.e. `get-credentials --dns-endpoint`).
  Authorized networks don't apply to it. Google checks IAM (`container.clusters.connect`, in
  `roles/container.viewer` and up) before a request reaches the API server.

On both, every request needs Google IAM credentials and passes Kubernetes RBAC, so access control
holds either way. With the default empty list, what's lost is network-level filtering on the IP
endpoint: the API server is exposed to scanning and to any unauthenticated vulnerability that
might appear. Trivy flags this (`AVD-GCP-0061`); the finding is ignored inline as a lab
trade-off.

| Caller | Talks to | Effect of `master_authorized_networks` |
|---|---|---|
| Maintainer's laptop (`make up`, kubectl) | IP endpoint; DNS endpoint when away from the listed address | must be listed for the IP endpoint |
| GitHub-hosted runners that run kubectl (the weekly evidence workflow) | DNS endpoint | none; their addresses change every run |
| Terraform (local and CI), `make resume`, the spend guard | GKE API (`container.googleapis.com`), not the cluster | none |
| Argo CD and everything else in the cluster | in-cluster service | none |

So pinning the IP endpoint to your address breaks nothing, and you can't lock yourself out:
Terraform and gcloud change authorized networks through the GKE API, and the DNS endpoint stays
available. Google recommends the DNS endpoint over IP allow-lists. The IP endpoint stays on
because it's the default path for kubectl and `make up`.

### Edge

The GKE Gateway in `deploy/edge` creates the load balancer. Terraform creates what that Gateway
refers to, so those pieces exist first:

- the global static IP `mamori-frontend-ip`;
- Cloud Armor policy `mamori-frontend-armor`;
- a Google-managed certificate validated by DNS authorization, served through certificate map
  `mamori-cert-map`.

Cloud Armor throttles each client IP to 600 requests a minute and answers 429 above that; all
other traffic is allowed. The reasoning is in `envs/lab/edge.tf`, and every number there is an
estimate. In short: a page view is about 20 requests, and a fast human with a cold cache stays
around 400 a minute.

### Budget and spend guard

- Budget `mamori-lab` on this project, in USD. Alerts at 50/90/100 % of current spend. Every
  update goes to the Pub/Sub topic `budget-alerts`.
- It counts cost after **free-tier credits** but before the **trial credit**
  (`credit_types_treatment = "INCLUDE_SPECIFIED_CREDITS"`, `credit_types = ["FREE_TIER"]`):
  - The GKE free tier offsets the zonal cluster fee (about $74 a month). Without it, an ordinary
    always-on month (about $160 plus $74) reaches $200 around the 27th, and the guard stops the
    cluster for nothing.
  - The $300 trial is a `PROMOTION` credit and doesn't count. Otherwise it would hide all spend
    until it ran out, which is when the trial stops everything anyway.
- The topic triggers the Cloud Run function `spend-guard` (runtime `go127`, entry point
  `ScaleToZero`, code in `tools/spend-guard`). At `THRESHOLD` it turns autoscaling off and sets
  `spot-pool`'s size to 0.
- The function runs as `spend-guard`, which holds a custom role with only
  `container.clusters.get`, `container.clusters.update` and `container.operations.get`. Those are
  the permissions GKE checks for `nodePools.get`, `nodePools.setAutoscaling`,
  `nodePools.setSize` and `operations.get`. GKE has no node-pool-level permissions, so this is
  as narrow as it gets.
- The function is built by its own account, `spend-guard-build`, not the default compute
  account. That account can write logs, read the source zip, and push to the `spend-guard` image
  repository only.
- Anyone who can publish to `budget-alerts` can stop the nodes with a fake over-budget message.
  The runbook uses exactly that to test the guard. Today the publishers are:
  - Cloud Billing;
  - project Owners and Editors (the maintainer, and the default compute account, which nothing
    here runs as);
  - `tf-apply`, through Pub/Sub Admin.

  Don't grant Pub/Sub publisher on this project to anything else. The failure mode is safe:
  nothing is deleted, and `make resume` undoes it.

**How applying the budget authenticates.** The Billing Budget API refuses user credentials
unless the request names a quota project. The lab provider therefore sets
`user_project_override = true` with `billing_project = project_id`. That works for a local
`make up`, and the service accounts get `roles/serviceusage.serviceUsageConsumer` for it.

Permissions are checked on the **billing account**:

- Creating or changing a budget needs Billing Account Costs Manager (or Administrator).
- Linking the topic needs `pubsub.topics.setIamPolicy`, because Cloud Billing grants itself
  publish rights on the topic.

Bootstrap gives `tf-apply` **Costs Manager** on the billing account, so CI can apply the whole lab
root. Costs Manager is the narrowest role that manages budgets: it can't link projects, change
payment settings or grant access. Its extra reach (every budget and cost report of the account)
is small on a personal billing account that pays only for this lab. If the billing account is
shared, remove that grant from `bootstrap/iam.tf` and apply `budget.tf` locally. `tf-plan` gets
Billing Account Viewer so that plan can refresh the budget.

**Currency:** the budget is in USD. If the billing account bills in another currency, the apply
fails rather than creating a budget of 200 yen.

**Confirm the credit split once the cluster has run for a day or two (M1):**

1. Billing → Reports, filtered to this project and the Kubernetes Engine service, grouped by SKU.
   Look at the credits on the cluster-management SKU: they should be the free tier, not the free
   trial. The trial shows up as a promotion credit across all services.
2. Billing → Budgets & alerts → `mamori-lab`. "Current spend" should be roughly gross cost minus
   the cluster fee. It must not be close to zero (that would mean the trial credit is subtracted)
   and must not include the cluster fee.
3. If the cluster fee isn't offset, the free-tier credit was booked under another type. If
   billing export to BigQuery is on, `credits.type` names it. Add that type to `credit_types`.

### Images

The `mamori` Docker repository (`asia-northeast1-docker.pkg.dev/<project>/mamori`) is in
bootstrap, so images survive `make down` and `make up` deploys the same ones without a rebuild.
images.yml pushes to it as the `ci-images` account (added to bootstrap with the image pipeline,
#28); nodes pull as `gke-nodes`.

- **Immutable tags:** a tag, once pushed, can't be moved to other content. images.yml pushes only
  new tags: `:<git sha>` from main, and `:<X.Y.Z>` for a release, which it refuses to overwrite.
  Two cases to watch on the first real publish (not testable offline):
  - Re-pushing an existing `:<git sha>`, for example a release built from a commit that main
    already published, fails if the rebuilt image differs.
  - Cosign signatures and attestations must be stored as OCI referrers, not as
    `sha256-<digest>.sig` tags, which a second signature would have to move.
- **Cleanup:** release tags (`0.`, `1.`, `2.` prefixes) are kept forever, and so are the 10 newest
  versions of each image. Other tagged builds go after 30 days. Untagged versions are never
  deleted: signatures, attestations and multi-arch child manifests are untagged, and with
  immutable tags no image is ever orphaned by a moved tag.

The spend-guard function's images stay in a separate `spend-guard` repository in the lab root.
They are rebuilt from source on every deploy and are worthless once the function is gone, so
`make down` removes them with it. Only the function's build account can push there.

### IAM

`tf-plan` can be impersonated by any workflow run of the repository. Bindings:

| Role | Where | Why |
|---|---|---|
| `roles/viewer` | project | refresh every resource during plan |
| `roles/iam.securityReviewer` | project | read IAM policies on resources (repository IAM, for example) |
| `roles/serviceusage.serviceUsageConsumer` | project | quota project for the Billing Budget API |
| `roles/storage.legacyBucketReader` | state bucket | list object names (`terraform init` looks up workspaces); no object content |
| `roles/storage.objectViewer`, only under `lab/` (IAM condition) | state bucket | read the lab's state, not other roots' (`oncall/` holds PagerDuty keys) |
| `roles/storage.objectUser`, only on `lab/*.tflock` (IAM condition) | state bucket | take and release the state lock without being able to overwrite state |
| `roles/storage.objectViewer` | spend-guard source bucket | refresh the uploaded zip |
| `roles/billing.viewer` | billing account | refresh the budget |

The readable prefixes are `tf_plan_state_prefixes` in `bootstrap/iam.tf`; add a root's prefix
there only if its state holds no secrets.

`tf-apply` can be impersonated only by jobs that run **in the GitHub environment `lab` and on
`refs/heads/main`**. The WIF provider maps every token to `attribute.lab_main`, and `tf-apply`
trusts only `principalSet://…/attribute.lab_main/true`. The attribute is `"true"` when both hold:

- `sub` is `repo:codewithmuh@51082957/mamori-sre-gke@1456357674:environment:lab` (GitHub's
  immutable subject format, the default for repositories created after 2026-07-15);
- `ref` is `refs/heads/main`.

The environment alone isn't enough. GitHub creates an environment, unprotected, the first time
any branch's workflow names it, so a pushed branch with `environment: lab` would otherwise get
`tf-apply` before anyone set up protection. Pull requests run on `refs/pull/<n>/merge` and other
branches on their own ref, so both get `"false"`. The mapping uses `sub` and `ref` because every
GitHub token has them. The `environment` claim is missing on jobs without one, and a mapping
error would break token exchange for every job.

One gap remains: a workflow on `main` triggered by `pull_request_target` also runs with
`ref = refs/heads/main`. Never give such a workflow `environment: lab`.

`tf-apply` bindings:

| Role | Where | Why |
|---|---|---|
| `roles/compute.networkAdmin` | project | VPC, subnet, router, NAT, static IP |
| `roles/compute.securityAdmin` | project | Cloud Armor policy and firewall rules |
| `roles/container.clusterAdmin` | project | cluster and node pool, without access to Kubernetes objects |
| `roles/artifactregistry.admin` | project | the `spend-guard` image repository, its cleanup policy and IAM |
| `roles/certificatemanager.editor` | project | DNS authorization, certificate, map, map entry |
| `roles/pubsub.admin` | project | topic, and topic IAM for the budget link |
| `roles/cloudfunctions.developer` | project | function and its Eventarc trigger |
| `roles/serviceusage.serviceUsageConsumer` | project | quota project for the Billing Budget API |
| `roles/resourcemanager.projectIamAdmin`, with an IAM condition | project | may grant or revoke only the five roles in `bootstrap/locals.tf` (`lab_grantable_roles`) |
| `roles/iam.serviceAccountUser` | `gke-nodes`, `spend-guard`, `spend-guard-build` only | attach these accounts to the node pool and the function |
| `roles/storage.objectUser` | state bucket, source bucket | write state, upload the zip |
| `roles/billing.costsManager` | billing account | the budget (see above) |

No Owner, Editor, Storage Admin, Service Account Admin or Role Admin. Adding a project-level role
binding to the lab root means adding it to `lab_grantable_roles` first (at most 10).

Hardening left for later: the default compute service account still has Editor (Google grants
it on projects without an organization). Nothing here runs as that account.

## Cost (estimates, list prices for Tokyo, 2026-11-13)

| Item | Per month | Source / note |
|---|---|---|
| GKE cluster fee | $0 | free tier covers one zonal cluster |
| 3 × e2-standard-2 Spot + 3 × 30 GB pd-standard | ~$118 | `docs/cost-and-tooling-research.md` |
| Load balancer forwarding rules (HTTP + HTTPS) | ~$18 + $0.008/GiB | $0.025/h for the first 5 rules |
| Cloud Armor (1 policy, 2 rules) | ~$7 + $0.75 per million requests | Standard tier |
| Cloud NAT (3 VMs) + 1 NAT IP | ~$3 + ~$3.65, + $0.045/GiB | $0.0014/VM-hour, $0.005/IP-hour |
| Managed Prometheus | ~$10 | depends on series; measure in M1 |
| Static IP | $0 while the Gateway uses it; $0.01/h while reserved and unused | |
| Certificate Manager | $0 | first 100 certificates free |
| Budget, Pub/Sub, spend guard, Artifact Registry, Logging | ~$0 | free tiers; logging is free up to 50 GiB a month |

About **$160 a month** while up, before logging and data volume. Infracost and the M1
measurement replace these numbers.

After `make down` these stay, by design:

- the state bucket and the spend-guard source bucket (a few KB each);
- the `mamori` image repository: Artifact Registry storage beyond the free 0.5 GB is about $0.10
  per GB a month (estimate), and the cleanup policy keeps it small;
- the service accounts, the custom role and the WIF pool (none billable).

Cloud Run functions also leaves its own `gcf-v2-sources-<project number>-asia-northeast1` bucket
behind (KB-sized).

## Checks

Run from the repository root; CI runs the same.

```sh
terraform fmt -check -recursive infra
for r in infra/bootstrap infra/envs/lab; do
  terraform -chdir=$r init -backend=false -input=false && terraform -chdir=$r validate
done

# tflint v0.65.0 with the google ruleset (pinned in infra/.tflint.hcl)
docker run --rm -v "$PWD":/src -w /src --entrypoint sh ghcr.io/terraform-linters/tflint:v0.65.0 -c \
  'tflint --init --config=/src/infra/.tflint.hcl &&
   tflint --config=/src/infra/.tflint.hcl --chdir=infra/bootstrap &&
   tflint --config=/src/infra/.tflint.hcl --chdir=infra/envs/lab'

docker run --rm -v "$PWD":/src -w /src aquasec/trivy:0.76.1 fs --scanners vuln,misconfig,secret \
  --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1 infra
```

Trivy findings below HIGH that are accepted on purpose:

- VPC flow logs off (cost).
- Buckets without customer-managed encryption keys (lab).

The `.terraform.lock.hcl` files pin provider checksums for `darwin_arm64` and `linux_amd64`.
After a provider upgrade, regenerate them with
`terraform providers lock -platform=darwin_arm64 -platform=linux_amd64`.
