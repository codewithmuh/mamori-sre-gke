# Runbook: first run of the lab

The sequence for the first `make up` in the real lab project (issue
[#15](https://github.com/codewithmuh/mamori-sre-gke/issues/15)), planned for Saturday 2026-11-14. It goes from the
Google Cloud free-trial signup to the shop on HTTPS, the prober and the SLOs running, a tested spend guard, the CPU
quota written down, a measured cost per day, and a `make down` that leaves nothing billable.

Nothing in this repository has run against a real project yet. Every duration below is an estimate, and anything
marked **(unverified)** comes from documentation or reasoning, not from a run. When a step turns out different,
fix this page in the same PR as the issue's results.

| #15 checklist item | Steps |
|---|---|
| Free trial signed up; lab project created and linked to the trial billing account | 1-4 |
| `make bootstrap`, then repo variables set for CI | 5-7 |
| `make up` from nothing; DNS records added; the shop loads over HTTPS with the banner | 8-13 |
| Region CPU quota checked and written down | 8, 19 |
| Spend guard tested: dry run, then a real scale to zero and `make resume` | 16-17 |
| Cost per day measured from billing data | 20 |
| `make down` leaves nothing billable | 21 |

Steps 14, 15 and 18 (Workload Identity, our images and the prober, the SLOs) aren't on the checklist, but M2 starts
from them.

## How to use this page

- Run the commands from the repository root, on an up-to-date `main`.
- **Checkpoints.** `scripts/preflight.sh --phase <phase>` is read-only: it lists PASS, WARN and FAIL lines and exits
  1 on any FAIL. A FAIL line names what to do. Phases: `before-bootstrap` (step 4), `before-up` (step 8),
  `after-up` (steps 10-18). `make preflight PHASE=<phase>` runs the same once the Makefile loads `mk/*.mk`; until
  #32 is merged, run the script directly. It reads `.env` itself.
- **Settings** live in `.env` (from `.env.example`). Some commands below also need them in the shell; load them
  once per terminal with `set -a; . ./.env; set +a`.
- The output masks your e-mail address and the billing account ID, but shows the project ID and the static IP.
  Only the "Quota to write down" block is meant for the public issue.

## Overview

Active time is yours; waiting is for Google. Both are estimates.

| # | Step | Active | Waiting |
|---|---|---|---|
| 0 | Before Saturday | 20 min | |
| 1 | Sign up for the free trial | 15 min | |
| 2 | Install the tools | 15-30 min | |
| 3 | Log in | 5 min | |
| 4 | Create the project, link billing, fill in `.env` | 10 min | |
| 5 | `make bootstrap` (also sets up GitHub environment `lab`) | 5 min | 5-10 min |
| 6 | Keep the bootstrap state safe | 2 min | |
| 7 | GitHub variables and secrets | 10 min | |
| 8 | Preflight before `make up`; write down the quota | 5 min | |
| 9 | `make up` | 5 min | 30-50 min |
| 10 | Check the cluster and Argo CD | 10 min | |
| 11 | DNS records at Cloudflare | 5 min | minutes |
| 12 | Wait for the certificate | | 10-60 min **(unverified)** |
| 13 | Check the shop and the banner | 10 min | |
| 14 | Second bootstrap (Workload Identity) | 5 min | 2 min |
| 15 | Our images and the prober | 20 min | 15-30 min |
| 16 | Spend guard: dry run | 10 min | 10 min |
| 17 | Spend guard: real test, then `make resume` | 15 min | 20-75 min |
| 18 | Turn on the SLOs | 10 min | 10 min |
| 19 | Write down the CPU quota | 5 min | |
| 20 | Measure the cost per day | 15 min | one full billing day, then the data delay |
| 21 | When to `make down` | 5 min | 20-30 min |
| | Later: PagerDuty paging (optional for #15) | 30-45 min | |

Saturday covers steps 1-19: roughly 5-7 hours with the waits. Step 15 needs a first release of our images, which
may move it, and step 18 with it, to another day. Step 20 needs the lab up for one full billing day, so
`make down` (step 21) comes on Monday evening at the earliest.

## 0. Before Saturday

- Have ready: a payment card for the trial's identity check, your phone, a login to Cloudflare (codewithmuh.com's
  DNS), admin rights on the GitHub repository, and an hour without interruptions for steps 9-13.
- `git switch main && git pull`. Argo CD deploys from `main` on GitHub, not from your checkout, so anything the lab
  should run must be merged first.
- Install the tools (step 2) ahead of time.

## 1. Sign up for the free trial

**Do**

1. Sign in with the Google account that will own the lab. It becomes project Owner and Billing Account
   Administrator; use the same account for every `gcloud auth login` later.
2. Open <https://cloud.google.com/free> and start the free trial.
3. **Country:** where you live. It fixes the billing account's currency and tax setup, and can't be changed later.
4. **Currency:** if the form offers a choice, choose **USD**. `infra/envs/lab/budget.tf` creates the budget in USD,
   and Google refuses a budget whose currency differs from the account's (infra/README.md). Whether the form offers a
   choice for your country is **(unverified)**.
5. **Account type:** Individual, if asked.
6. **Payment:** a card. The trial uses it to verify you; it isn't charged unless you activate a full (paid)
   account. Read what the signup page says on the day, since Google has changed the trial flow before
   **(unverified)**.
7. Don't activate the full account. The plan upgrades at the start of M5 (PLAN.md, D8).

**Expect** (10-15 minutes)

- The console opens with a first project and a banner with the $300 credit and the days left.
- **The 90-day clock starts now.** The credit expires on 2027-02-12, or earlier if it is spent (PLAN.md section 6).
  When a trial ends without an upgrade, the account closes, resources stop, and after a 30-day grace period they are
  deleted (docs/decision-facts.md).
- Trial limits that matter here: no quota increases, no Marketplace, no GPUs, and the credit can't pay for Claude
  (docs/cost-and-tooling-research.md).
- Note the billing account ID: **Billing > Account management**, format `012345-6789AB-CDEF01`.

**If it fails**

- *Not eligible:* the account, or the card, has had a trial or a paid Cloud Billing account before (eligibility in
  docs/cost-and-tooling-research.md). Stop and replan: a paid account works the same way but has no credit.
- *The account bills in another currency than USD:* step 4's preflight FAILs on it. Before `make up`, the budget
  needs that currency and an amount in it (`budget.tf`, `monthly_budget_usd`), as a small PR. The currency can't be
  changed afterwards.

## 2. Install the tools

| Tool | Version | Why |
|---|---|---|
| gcloud + `gke-gcloud-auth-plugin` | any current release (the dev container has 591.0.0) | everything on Google Cloud; kubectl needs the plugin for GKE |
| terraform | >= 1.9, < 2 (1.9.8 locally, 1.17.x in CI) | both roots |
| kubectl | 1.35-1.37 (the cluster runs 1.36) | `make up`, checks |
| helm | 3.8 or later (4.x is fine) | installs Argo CD |
| gh | any 2.x | `make bootstrap` sets up environment `lab` with it; variables, workflow runs |
| docker | optional | local image builds and offline checks |

**Do**, either:

- **Dev container:** everything above, pinned (`.devcontainer/`). Logins live inside the container and are gone
  after a rebuild.
- **On the Mac:** the Google Cloud CLI from Google's installer (<https://cloud.google.com/sdk/docs/install>; macOS
  ARM archive, then `./google-cloud-sdk/install.sh`) or Homebrew (`brew install --cask gcloud-cli`; the cask's name
  is **(unverified)**, it used to be `google-cloud-sdk`). Then:

  ```sh
  gcloud components install gke-gcloud-auth-plugin
  ```

**Expect:** `scripts/preflight.sh --phase before-bootstrap` shows PASS for every line under "Local tools". Its other
lines FAIL until steps 3-4 are done. The scripts work with the Mac's own `/bin/bash` 3.2.

**If it fails:** `gke-gcloud-auth-plugin not on PATH`: it is installed in the SDK's `bin` directory; open a new
shell after the installer has updated your shell profile, or add
`"$(gcloud info --format='value(installation.sdk_root)')/bin"` to `PATH`.

## 3. Log in

```sh
gcloud auth login                       # the account from step 1
gcloud auth application-default login   # the credentials Terraform uses
gh auth login                           # as the repository's owner: make bootstrap needs admin rights
```

**Expect:** a browser sign-in for each (5 minutes). `application-default login` may warn that no quota project is
set; that is fine, because the Terraform providers name the lab project as the quota project themselves
(`user_project_override`).

**If it fails:** no browser (dev container): `gcloud auth login --no-browser`, and the same flag for
`application-default login`.

## 4. Create the project, link billing, fill in `.env`

Pick the project ID: `mamori-lab-` plus 6 digits, for example. 6-30 characters, lowercase letters, digits and
hyphens. It is globally unique and permanent (a deleted project's ID can't be reused), and it becomes public (image
paths in `deploy/`, signature entries; docs/images.md).

**Do**

1. Create the project and link the trial billing account. In the console: project picker > **New project**, edit
   the ID, choose the trial billing account, location "No organization". Or:

   ```sh
   gcloud projects create mamori-lab-123456 --name="mamori lab"
   gcloud billing accounts list               # the trial account, OPEN True
   gcloud billing projects link mamori-lab-123456 --billing-account=012345-6789AB-CDEF01
   ```

2. Point gcloud at it, and turn on the two APIs that Terraform needs before it can turn on the rest:

   ```sh
   gcloud config set project mamori-lab-123456
   gcloud services enable cloudresourcemanager.googleapis.com serviceusage.googleapis.com
   ```

   Why: bootstrap reads the project with the lab project as the quota project. On a new project the Resource
   Manager API may be off, which would stop the first plan **(unverified)**. Turning it on first is free, and
   bootstrap's `apis.tf` adopts it.
3. `cp .env.example .env` and set:
   - `PROJECT_ID` and `BILLING_ACCOUNT_ID`;
   - `ALERT_EMAIL`, optional: the address that gets every lab alert by e-mail. Empty means no e-mail channel.
   - Leave `TF_STATE_BUCKET` empty (bootstrap always names the bucket `<project>-tfstate`), keep the defaults for
     `ZONE` and `FRONTEND_HOSTNAME`, and leave `SLO_ENABLED` out until step 18.

   Until #32 is merged, the Makefile doesn't pass `ALERT_EMAIL` and `SLO_ENABLED` to Terraform. In the shell that
   runs `make`, do `set -a; . ./.env; set +a` and then
   `export TF_VAR_alert_email="${ALERT_EMAIL:-}" TF_VAR_slo_enabled="${SLO_ENABLED:-false}"`.
4. `scripts/preflight.sh --phase before-bootstrap`

**Expect:** every line PASS, the quota checks SKIP (the Compute Engine API is off until bootstrap), and
`Ready for: make bootstrap`. The GitHub lines check that you are admin on the repository and compare its OIDC subject
prefix with the one bootstrap writes into the identity pool; on 2026-11-13 GitHub returned
`repo:codewithmuh@51082957/mamori-sre-gke@1456357674`, which matches. The project the trial created on its own can
stay (empty projects cost nothing) or be deleted.

**If it fails**

- *Billing not enabled:* link it (above). If linking fails with a quota message, the trial may cap how many projects
  its billing account can pay for **(unverified)**: unlink the trial's first project
  (`gcloud billing projects unlink <its ID>`) and try again.
- *gcloud asks to enable `cloudbilling.googleapis.com` on a project:* answer yes **(unverified)**.
- *Currency FAIL:* step 1, "If it fails".
- *Not Owner, or not Billing Account Administrator:* you signed in with another account than the one that signed up
  (`gcloud auth list`; `gcloud config set account ...`).
- *Not admin on the repository:* `gh auth status`; log in as the owner.
- *OIDC subject prefix FAIL:* GitHub's subject for this repository differs from `github_subject_prefix` in
  `infra/bootstrap/variables.tf`. Fix the default in a PR before bootstrap, or no CI job can get Google credentials.

## 5. `make bootstrap`

**Do:** `make bootstrap`, read the Terraform plan, and answer `yes` (Terraform asks even with `-input=false`; checked
with Terraform 1.9.8).

**Expect** (10-15 minutes, most of it enabling APIs)

1. First, `scripts/github-lab-env.sh` creates or updates the GitHub environment `lab`: you as required reviewer,
   self-review allowed (you are the only reviewer), deployments from `main` only. It prints
   `GitHub environment lab in codewithmuh/mamori-sre-gke: required reviewer codewithmuh, deployments from main only.`
   It goes first because GitHub would create the environment unprotected the first time a workflow named it, and
   `tf-apply` accepts only jobs in environment `lab` on `main`.
2. The plan only creates: the APIs from `apis.tf`, the state and spend-guard source buckets, the image repository
   `mamori` (immutable tags; it outlives `make down`), the Workload Identity pools, the service accounts, the custom
   roles and their grants. Nothing to change or destroy.
3. The end prints the GitHub settings (`scripts/print-repo-vars.sh`; run it again with `PROJECT_ID` set if you lose
   the output). Keep it for step 7.

**If it fails** (Terraform continues where it stopped; fix and run `make bootstrap` again)

- *`gh` HTTP 403 or 404 before Terraform starts:* you aren't admin on the repository (`gh auth status`). To make
  someone else the reviewer, set `LAB_REVIEWER=<login>`.
- *"Cloud Resource Manager API has not been used in project ... or it is disabled":* step 4.2, wait a minute, rerun.
- *Another "API has not been used ... or it is disabled" partway through:* enabling an API takes a minute or two to
  reach every service. Wait and rerun.
- *Permission denied on a `google_billing_account_iam_member`:* you need Billing Account Administrator on the trial
  account (step 4, "Not Owner").
- *"Identity Pool does not exist":* `gke_workload_pool_exists` is set too early. Remove it from
  `infra/bootstrap/terraform.tfvars` (and the environment) until step 14.

## 6. Keep the bootstrap state safe

`infra/bootstrap/terraform.tfstate` is the only record of the bootstrap root (infra/README.md). Copy it to the state
bucket now and after every later bootstrap apply:

```sh
set -a; . ./.env; set +a
gcloud storage cp infra/bootstrap/terraform.tfstate "gs://$PROJECT_ID-tfstate/bootstrap/"
```

**Expect:** one object copied (seconds). The preflight shows "a copy of the bootstrap state is in gs://..." from now
on.

## 7. GitHub variables and secrets

**Do**

1. Paste the five `gh variable set` commands from step 5.
2. `gh secret set GCP_BILLING_ACCOUNT_ID --repo codewithmuh/mamori-sre-gke`, and paste the ID when asked.
3. If `ALERT_EMAIL` is set in `.env`: `gh secret set ALERT_EMAIL --repo codewithmuh/mamori-sre-gke`. It is a secret
   because it is personal data. Without it, a CI plan or apply has no e-mail address and would remove the e-mail
   channel that `make up` created.
4. Release PRs: pick one of the two options step 5 printed (a `RELEASE_PLEASE_TOKEN`, or the Actions setting). Step
   15 publishes our images from the first release.

Environment `lab` needs nothing by hand: step 5 set it up.

**Expect** (10 minutes): `scripts/preflight.sh` shows the GitHub lines PASS, including "environment lab has a required
reviewer" and "deploys from main only". The next pull request's Terraform plan is the first real test of keyless CI
**(unverified until then)**.

**If it fails:** `HTTP 403` from `gh`: your token or role lacks admin on the repository (`gh auth status` lists the
scopes; `gh auth refresh -s repo`).

## 8. Preflight before `make up`; write down the quota

**Do:** `scripts/preflight.sh` (the phase defaults to `before-up`).

**Expect:** no FAIL and `Ready for: make up`. Lines you can expect on the first day:

- INFO `deploy/ still names the registry project PROJECT_ID`: replaced in step 15.
- INFO `SLOs off (SLO_ENABLED unset)`: right for the first `make up`. If it says WARN `SLO_ENABLED=true before the
  lab has run`, take it out of `.env`: Cloud Monitoring rejects SLOs on metrics that don't exist yet.
- WARN on GitHub lines, if step 7 isn't done yet.

The output ends with a block "Quota to write down in #15". Copy it now, before the lab takes its share (step 19).

**If it fails** on `CPUS` or `CPUS_ALL_REGIONS`: [Quota](#quota). On anything else: the FAIL line's hint, or the
step it names.

## 9. `make up`

**Do:** note the time, run `make up` (in a shell with step 4's exports while #32 isn't merged), read the plan, and
answer `yes`.

**Expect** (35-60 minutes, mostly waiting; estimates)

1. `terraform init` against the state bucket (1 minute).
2. The plan only creates: network, Cloud NAT, firewall rules, cluster, node pool, the spend guard's image
   repository, static IP, Cloud Armor policy, DNS authorization, certificate and map, budget topic and budget,
   spend-guard function, notification channels (PagerDuty with a placeholder key, e-mail if `ALERT_EMAIL` is set),
   IAM bindings, dashboards and the other monitoring objects. No SLOs yet. The cluster takes about 5-10 minutes,
   the node pool 3-5, and the function's build 3-5 more (it waits for the node pool on purpose). 15-25 minutes in
   all.
3. Credentials for kubectl, then Argo CD with Helm (`--wait`, up to 10 minutes).
4. The root app, then `scripts/wait-for-shop.sh` (up to 30 minutes): "Waiting for Argo CD to create the shop",
   the frontend's rollout, then "Waiting for Gateway mamori-frontend to get its load balancer".
5. At the end:

   ```text
   Shop: https://mamori.codewithmuh.com  (load balancer IP: 34.x.x.x)
   HTTPS works once these DNS records exist where codewithmuh.com is hosted and the certificate is active:
   ```

   followed by `dns_records_to_add`: the `A` record and the certificate's `CNAME`. Keep them for step 11.

Write down the start and end times: the first measured `make up`.

**If it fails.** Terraform and the rest of `make up` can run again: fix the cause and rerun `make up`. If you are
stuck for a long time with the lab half built, `make down` stops the spend and works on a partial lab.

- *"Insufficient regional quota ... CPUS":* [Quota](#quota).
- *`ZONE_RESOURCE_POOL_EXHAUSTED`, or the node pool can't get Spot VMs:* Tokyo has no Spot capacity of that type at
  the moment. Wait 15-30 minutes and rerun. Moving to another zone (`ZONE=asia-northeast1-b`) recreates the zonal
  resources, so do it only after `make down`.
- *Budget errors:* a currency mismatch is step 1; a permission error means the applying account isn't Billing
  Account Administrator or Costs Manager on the account.
- *The function's build fails on a permission of `spend-guard-build`:* IAM can take minutes to propagate; rerun.
- *`google_pubsub_topic_iam_member.incident_alerts_monitoring`, "service account ... does not exist":*
  docs/oncall.md, "Set up, once", step 6, then rerun.
- *`google_pubsub_subscription_iam_member.agent_subscriber`, "Identity Pool does not exist":* the grant waits for
  the cluster, but the cluster's workload identity pool may lag behind it **(unverified)**. Wait a minute and rerun.
- *An error about SLO filters or metrics that don't exist:* `SLO_ENABLED` is on too early (step 8).
- *Helm times out:* `kubectl get nodes` (Ready?) and `kubectl -n argocd get pods`. Image pulls from quay.io and
  ghcr.io go through Cloud NAT.
- *`wait-for-shop.sh` times out:* step 10.

## 10. Check the cluster and Argo CD

```sh
kubectl get nodes                          # 3 nodes, Ready
kubectl -n argocd get applications         # see below
kubectl -n boutique get pods               # Running, 2/2 (the app and its Envoy sidecar)
scripts/preflight.sh --phase after-up      # the DNS and certificate lines FAIL until steps 11-12
```

**Expect:** every application Synced and Healthy, in sync-wave order. The apps on our own images ship with 0
replicas (`prober` until step 15, `executor` and `agent` until M5), so nothing waits for an image that isn't
published yet **(unverified on a cluster)**. The Argo CD UI is a port-forward away (deploy/README.md, "Argo CD
access"). The after-up preflight shows WARN for what steps 11-18 still have to do.

**If it fails**

- *Pods Pending:* `kubectl -n boutique describe pod <name>`. "Insufficient cpu" means the requests outgrew the nodes
  (deploy/README.md, "Resource budget").
- *`ImagePullBackOff` on upstream images:* Cloud NAT,
  `gcloud compute routers get-status mamori-router --region asia-northeast1`.
- *Pods in `boutique` never created, webhook timeouts in events:* the firewall rule for port 15017 (deploy/README.md,
  "What the cluster needs from Terraform").
- *An app stuck Progressing or Degraded:* `kubectl -n argocd get applications.argoproj.io <name> -o yaml`; the apps
  in later waves wait for it.

## 11. DNS records at Cloudflare

codewithmuh.com's DNS is on Cloudflare (its name servers were `*.ns.cloudflare.com` on 2026-11-13). The records are
in `terraform -chdir=infra/envs/lab output dns_records_to_add`; `make up` printed them.

**Do:** Cloudflare > codewithmuh.com > DNS > Records > Add record, twice:

| Type | Name | Content | Proxy status |
|---|---|---|---|
| A | `mamori` | the `frontend` record's IP | **DNS only** (grey cloud) |
| CNAME | `_acme-challenge.mamori` | the `certificate_validation` record's data, ending in `.authorize.certificatemanager.goog` | **DNS only** |

Both must be DNS only. Cloudflare proxies new records by default. A proxied `A` record resolves to Cloudflare's
addresses: Cloud Armor would then rate-limit Cloudflare's edge instead of each visitor, and the certificate check
below would never see the IP. codewithmuh.com has no CAA records (checked 2026-11-13); if one is added later, it must
allow `pki.goog`.

**Expect:** within minutes,

```sh
dig +short mamori.codewithmuh.com                          # the static IP
dig +short CNAME _acme-challenge.mamori.codewithmuh.com     # ...authorize.certificatemanager.goog.
```

and the preflight's DNS lines PASS. The static IP is new after every `make down` / `make up`, so update the `A`
record each time; check the `CNAME` too (whether it changes is **(unverified)**). `make edge-off` / `make edge-on`
keep the IP, so the records stay valid.

**If it fails:** `dig` shows `104.x` or `172.6x` addresses: the record is proxied, switch it to DNS only. Nothing:
check the name (Cloudflare adds `.codewithmuh.com` itself) and ask another resolver (`dig @1.1.1.1 ...`, or
`DNS_SERVER=1.1.1.1 scripts/preflight.sh --phase after-up`).

## 12. Wait for the certificate

```sh
gcloud certificate-manager certificates describe mamori-frontend-cert --location=global \
  --format='value(managed.state)'          # PROVISIONING, then ACTIVE
```

**Expect:** `ACTIVE` once Google has seen the `CNAME`; it doesn't need the `A` record. Reports put this between
minutes and an hour; Google gives no firm number **(unverified)**. Write down how long it took. Until then
`http://` redirects to `https://` and `https://` fails with a TLS error, as expected.

**If it fails** (still `PROVISIONING` after two hours): look at `managed.authorizationAttemptInfo` and
`managed.provisioningIssue` in the full `describe`, compare the `CNAME` with
`terraform -chdir=infra/envs/lab output dns_records_to_add` from the latest `make up`, and check for CAA records.

## 13. Check the shop and the banner

**Do**

1. Open <https://mamori.codewithmuh.com/>. Every page shows the banner "Demo shop, no real payments. Do not enter
   real card details." at the top. Browse, add to cart and check out with the pre-filled fake card.
2. From a shell:

   ```sh
   curl -sS -o /dev/null -w '%{http_code}\n' https://mamori.codewithmuh.com/               # 200
   curl -sS https://mamori.codewithmuh.com/ | grep -o 'Demo shop, no real payments[^<]*'    # the banner
   curl -sS -o /dev/null -w '%{http_code} %{redirect_url}\n' http://mamori.codewithmuh.com/  # 301 https://...
   scripts/preflight.sh --phase after-up
   ```

**Expect** (10 minutes): all of the above; the preflight's edge lines PASS.

**If it fails**

- *502 or 503 from Google:* the load balancer has no healthy backends yet. Give it a few minutes after the Gateway
  is Programmed, then check `kubectl -n istio-ingress get pods` and the firewall rule from Google's ranges
  (deploy/README.md, "What the cluster needs from Terraform").
- *404 from Istio:* the host isn't one the Istio Gateway and VirtualService accept (`FRONTEND_HOSTNAME` must match
  `deploy/`).
- *No banner:* `kubectl -n boutique get deploy frontend -o yaml | grep -A1 FRONTEND_MESSAGE`.

## 14. Second bootstrap: Workload Identity grants

The trace collector (and later the agent) gets Google access through the cluster's workload identity pool, which
exists only once the cluster does (infra/bootstrap/observability_identity.tf).

**Do**

```sh
printf 'gke_workload_pool_exists = true\n' >> infra/bootstrap/terraform.tfvars
make bootstrap
```

Keep only that line in the file (it is gitignored). Terraform prefers `terraform.tfvars` over the `TF_VAR_*` the
Makefile exports, so a copied `project_id` there would point bootstrap at the example project; the preflight FAILs on
it. Keep the line for good: a later `make bootstrap` without it removes the grants again.

**Expect** (5 minutes): `scripts/github-lab-env.sh` runs again and changes nothing; the plan adds the collector's
`roles/cloudtrace.agent` grant and the agent's grants, and changes nothing else. Copy the state again (step 6). The
preflight shows "Workload Identity grants applied".

**If it fails:** "Identity Pool does not exist": the cluster isn't finished; wait and rerun. Whether the pool
survives `make down` is **(unverified)**; if a rebuilt lab's bootstrap fails on these grants, run `make up` first.

## 15. Our images and the prober

The repository `mamori` comes from bootstrap and keeps its images through `make down`, so this is done once, not
after every `make up` (docs/images.md, "First time: the order").

**Do**

1. **Publish the images once**, at the version `deploy/` names for them (today `0.1.0`, the tag in
   `deploy/prober/deployment.yaml`). Either merge the release-please PR for that version (`release.yml` publishes
   it), or, once the tag `v<version>` is on `main`:

   ```sh
   gh workflow run images.yml --ref main -f version=0.1.0
   gh run watch "$(gh run list --workflow images.yml --limit 1 --json databaseId --jq '.[0].databaseId')"
   ```

2. **Point `deploy/` at the project:** `scripts/set-registry.sh` (a dry run that lists every line it would change),
   then `scripts/set-registry.sh --apply`, review `git diff -- deploy`, and merge it through a pull request.
3. **Start the prober:** in a pull request, raise `replicas` in `deploy/prober/deployment.yaml` from 0 to 1. The
   executor stays at 0 until M5, and the agent at 0 until M5's billing upgrade.

Argo CD picks up each merge within about three minutes.

**Expect** (20 minutes yours, plus the workflow and review time):

- The publish run pushes, signs and attaches an SBOM for each image (docs/images.md).
- `kubectl -n prober get pods`: one pod Running. `make prober-run CUJ=checkout RUNS=3` (once the Makefile loads
  `mk/*.mk`) shows each journey's result.
- The preflight's "Images and the prober" lines PASS: the image is in the registry, `deploy/` names the project, and
  the prober is Ready.
- The prober reaches the shop through the Istio ingress gateway's internal port 8080, which isn't public. It needs
  neither the DNS records nor the certificate, and keeps probing while the edge is off.

**If it fails**

- *`images.yml` refuses the version:* the tag `v<version>` isn't on `main` yet (docs/images.md); release first.
- *The publish fails because the tag already exists:* the repository's tags are immutable, so a version is
  published only once. Release a new version instead.
- *The prober's pod is in `ImagePullBackOff`:* the image isn't published, or the `set-registry` change isn't merged
  (`kubectl -n prober describe pod`).

## 16. Spend guard: dry run

The procedure is in [spend-guard.md](spend-guard.md), "Test it safely". Anything published to `budget-alerts` looks
like a budget notification to the guard.

**Do**, in a shell with step 4's exports while #32 isn't merged:

```sh
set -a; . ./.env; set +a
TF_VAR_spend_guard_dry_run=true make up
gcloud pubsub topics publish budget-alerts --project="$PROJECT_ID" \
  --message="$(cat tools/spend-guard/testdata/budget-notification.json)" \
  --attribute=schemaVersion=1.0,budgetId=manual-test
gcloud logging read 'resource.type="cloud_run_revision" AND resource.labels.service_name="spend-guard" AND jsonPayload.outcome:*' \
  --project="$PROJECT_ID" --freshness=1h \
  --format='table(timestamp, severity, jsonPayload.outcome, jsonPayload.spent_ratio, jsonPayload.message)'
```

**Expect** (20 minutes): the plan changes only the function's `DRY_RUN` (an in-place update); anything else in it,
stop and read. The rest of `make up` reruns and has nothing to do. Within a minute or two of the publish, a
`dry_run` line with `planned_steps`, and `kubectl get nodes` still lists 3 nodes. The preflight shows
`DRY_RUN=true`.

**If it fails:** no line after 5 minutes: check the trigger (`gcloud eventarc triggers list --location=asia-northeast1`)
and the function's ERROR logs. `outcome="failed"`: spend-guard.md, "If the guard failed".

## 17. Spend guard: real test, then `make resume`

The shop goes down on purpose until `make resume`. Do it before the SLOs are on (step 18), so the outage doesn't
burn error budget or fire alerts.

**Do**

```sh
make up                                    # DRY_RUN back to false; the plan changes only that
date -u                                    # write down the time
gcloud pubsub topics publish budget-alerts --project="$PROJECT_ID" \
  --message="$(cat tools/spend-guard/testdata/budget-notification.json)" \
  --attribute=schemaVersion=1.0,budgetId=manual-test
kubectl get nodes -w                       # watch them drain and go; Ctrl-C when none are left
```

**Expect:** `scaled_to_zero` in the logs (or `resize_running` while PodDisruptionBudgets hold the drain, which can
last up to an hour), no nodes, and the shop answering 502 or 503. The static IP and the DNS records stay. Write down
how long the drain took: a measured number for the case study.

Then bring it back:

```sh
make resume                                # spot-pool back to 3 nodes
kubectl get nodes -w                       # 3 Ready
kubectl -n argocd get applications         # Healthy again
scripts/preflight.sh --phase after-up
```

Write down the time from `make resume` to the shop answering 200 again. A manual test doesn't fire again by itself,
because the real notifications that follow are still below the budget (spend-guard.md).

**If it fails:** spend-guard.md, "If the guard failed". If the guard doesn't act, scale down by hand there, or skip
to `make resume`.

## 18. Turn on the SLOs

The generated Cloud Monitoring services, SLOs and burn-rate and no-data alerts exist only while `slo_enabled` is
true, because Cloud Monitoring rejects filters on metric types that don't exist yet. The prober's metrics exist once
it has run for a few minutes.

**Do**, once the prober (step 15) has run for about 10 minutes and Argo CD's `slo` app is Healthy:

1. Check the metrics are there: Metrics Explorer, PromQL `sum by (cuj) (mamori_cuj_probe_total)`.
2. Add `SLO_ENABLED=true` to `.env`. Every later `make up` needs it: one without it deletes the SLOs again. Until
   #32 is merged, also `export TF_VAR_slo_enabled=true` in the shell.
3. `gh variable set SLO_ENABLED --repo codewithmuh/mamori-sre-gke --body true`, so CI plans and applies keep them.
4. `make up`, read the plan (it only creates the generated services, SLOs and alert policies), answer `yes`.

**Expect** (10 minutes yours, about 10 minutes of prober time before): the preflight's "SLOs" section says
`SLOs are on: N SLOs in the lab` and that the repository variable exists. The journey SLO charts fill in from now on;
the operated period of M2 can start.

**If it fails:** an error naming a metric or filter: the metrics aren't there yet (back to 1, wait, rerun). The
preflight says `the lab has N SLOs, but SLO_ENABLED isn't true here`: the setting didn't reach Terraform (step 4's
note on #32).

## 19. Write down the CPU quota

**Do:** paste the "Quota to write down in #15" block from step 8 (taken before the lab used any CPU) into the issue,
with its date. Cross-check it in the console: **IAM & Admin > Quotas & System Limits**, filter "CPUs", region
asia-northeast1. The metric names the preflight reads come from `gcloud compute regions describe` and
`gcloud compute project-info describe`; which of them a trial account shows is **(unverified)**.

**Expect:** regional `CPUS` and global `CPUS_ALL_REGIONS` of at least 8, with room for the surge node (see
[Quota](#quota)).

## 20. Measure the cost per day

Cloud Billing reports count days in US Pacific time **(unverified)**: in November (UTC-8) one billing day runs from
17:00 to 17:00 JST. Cost data reach the reports with a delay of hours, sometimes more than a day.

The budget counts cost **after free-tier credits and before the trial credit**: the GKE free tier offsets the zonal
cluster fee (about $2.40 a day), while the $300 trial credit is a promotion and doesn't count
(`infra/envs/lab/budget.tf`; infra/README.md, "Budget and spend guard"). Read the day the same way.

**Do**

1. Keep the lab up and unchanged (3 nodes, the shop, the load generator, the prober, the edge on) through one full
   billing day: Sunday 2026-11-15 in Pacific time, from Sunday 17:00 JST to Monday 2026-11-16 17:00 JST. Saturday is
   a partial day with the spend-guard test in it, so it doesn't count.
2. On Tuesday or later: **Billing > Reports**, time range that one day, project the lab, group by SKU. In the
   credits filter keep the free-tier credits and leave out promotions (the trial credit); the exact labels in the
   report are **(unverified)**. Note the gross cost too (all credits left out): the difference is the cluster fee.
3. Write down both numbers and the top SKUs in #15, and compare them with [the estimates](#cost-while-the-lab-runs).

**Expect:** around $5-6 for the day after free-tier credits, about $2.40 more gross (estimates). Spot cores and
memory first, then the load balancer, Cloud Armor, Managed Prometheus samples and Cloud NAT.

## 21. When to `make down`

After the billing day of step 20 has ended (Monday 17:00 JST at the earliest), and whenever the next work session in
M1 is more than a day or two away: a day up costs about $5-6, a rebuild about an hour of your time and a new `A`
record (estimates). For a short break with the cluster still needed, `make edge-off` stops most of the public edge's
cost instead ([Cost while the lab runs](#cost-while-the-lab-runs)). From M2 the lab stays up (PLAN.md section 5).

**Do**

```sh
make down              # asks you to type the project ID; YES=1 skips that
```

Then delete the `A` record at Cloudflare. The static IP goes back to Google and can later belong to someone else, so a
record that still points at it would send `mamori.codewithmuh.com` there. The `CNAME` can stay.

**Expect** (20-30 minutes): the Argo CD apps and the Gateway are deleted, `wait-for-lb-gone.sh` waits until the
load balancer is gone (up to 15 minutes), `terraform destroy` runs, and the leftover check ends with
`Nothing billable is left in <project>.` That is the last #15 checklist item. These stay by design: the image
repository `mamori` and its images, the state bucket, the spend-guard source bucket, the `gcf-v2-sources-...` bucket,
the service accounts, the custom roles and the identity pools (a few hundred MB of images inside Artifact Registry's
free storage, or KBs, or free; estimates in docs/images.md and infra/README.md). The next `make up` needs no
republish, but a new `A` record and, if you use PagerDuty, the key on the new channel again.

**If it fails**

- *Leftover check exit 1:* the listed resources are still there and billed. Run `make down` again, or delete them by
  hand.
- *Exit 2:* a list call failed, so the project isn't known to be clean; fix the error and run
  `scripts/leftover-check.sh` again.
- *`terraform destroy` stops on the static IP, the Cloud Armor policy or the certificate map "in use":* the load
  balancer is still there; deploy/README.md, "Tear down", lists what to delete first.

## Later: PagerDuty paging (optional for #15)

`make up` needs nothing from PagerDuty: it creates the PagerDuty channel with a placeholder key that Terraform then
leaves alone, and nothing pages until you set the real key by hand. `ALERT_EMAIL` gives e-mail alerts without it.
When you want phone pages (M3 at the latest), follow [docs/oncall.md](../oncall.md), "Set up, once": the PagerDuty
account, `infra/envs/oncall`, the Cloud Monitoring integration, then step 7 there to set the key on the channel. The
key never goes into Git, CI or Terraform state. Set it again after every `make down` / `make up`, since the new
channel starts with the placeholder. The after-up preflight shows an INFO line as a reminder.

## Quota

What the lab needs (infra/envs/lab/gke.tf):

| Quota | Need | Why |
|---|---|---|
| `CPUS` in asia-northeast1 | 8 | 3 × e2-standard-2 = 6 vCPU, plus one surge node (2 vCPU) during upgrades |
| `CPUS_ALL_REGIONS` (global) | 8 | the same VMs |
| `IN_USE_ADDRESSES` in asia-northeast1 | 1 | Cloud NAT's automatic external IP |
| global static addresses | 1 | `mamori-frontend-ip` |

A trial account can't request increases. Google's free-trial terms have also capped trial accounts at 8 vCPUs running
at once; check the current page **(unverified)**. That would be exactly the surge peak, with no room for anything
else, such as a debug VM.

If the quota is short:

- **6 or 7 vCPU:** the lab runs, but a surge upgrade can't add its fourth node. Set `max_surge = 0` and
  `max_unavailable = 1` in `gke.tf` (a PR): upgrades then replace one node at a time, and the pods still fit on two
  nodes (deploy/README.md, "Resource budget": about 61 % of three nodes' CPU is requested).
- **Less than 6:** the stack doesn't fit; stop and replan with #15, or wait for the M5 upgrade, when increases can be
  requested.

## Cost while the lab runs

From PLAN.md section 6 (list prices, 2026-11-13; per day is my math at 30.4 days a month), counted the way the budget
counts: after free-tier credits, so the cluster fee is $0:

| Item | Per month | Per day |
|---|---|---|
| GKE cluster fee | $0 (offset by the free-tier credit; ~$74 gross) | $0 (~$2.40 gross) |
| 3 × e2-standard-2 Spot + 30 GB pd-standard disks | ~$118 | ~$3.90 |
| Managed Prometheus | ~$10 | ~$0.33 |
| Load balancer + Cloud Armor | ~$20-30 (estimate) | ~$0.65-1.00 |
| Cloud NAT, logging | to measure in M1 | |
| **Total** | **~$150-190** | **~$5-6.25** |

infra/README.md puts it at about $160 a month (~$5.30 a day). The budget is $200 a month on this basis, below the
$300 credit; the spend guard stops the nodes at 100 %. For the whole plan, PLAN.md estimates $280-360.

Ways to spend less without `make down`:

- **`make edge-off`** deletes the Gateway and waits until its load balancer is gone: no forwarding rules, no Cloud
  Armor or load-balancer charges per request, no public shop. The static IP, the certificate and the Cloud Armor
  policy stay, so DNS needs no change, and the reserved but unused IP costs about $0.24 a day. The prober keeps
  running, since it uses the gateway's internal port 8080. `make edge-on` brings the edge back. Saves roughly the
  load balancer and Cloud Armor line above plus any per-request charges (estimate).
- **While the spend guard holds the pool at zero,** the load balancer, Cloud Armor and the NAT IP still cost about
  $1 a day (my math from infra/README.md). The guard only scales nodes; `make edge-off` stops most of the rest.
- **After `make down`,** almost nothing: the image repository's storage and a few KB of buckets.

## What to write down in #15

| What | Step |
|---|---|
| Durations of `make bootstrap`, `make up` and `make down` | 5, 9, 21 |
| Time from the `CNAME` to an active certificate | 12 |
| CPU and address quotas | 8, 19 |
| Spend guard: publish to `scaled_to_zero`, drain time, `make resume` to 200 | 16, 17 |
| Cost per day after free-tier credits and gross, and the top SKUs | 20 |
| The leftover check's last line | 21 |
| Every place this runbook was wrong | all |

## Not verified yet

- The whole sequence: nothing here has run against a real project, and every duration is an estimate.
- The trial signup screens: the currency choice, the account type, whether the card is charged, the project cap.
- Whether the first `make bootstrap` needs the Resource Manager API turned on first (step 4.2).
- `scripts/preflight.sh` was tested with fake `gcloud`, `gh`, `kubectl`, `terraform`, `dig` and `curl`, not against
  Google Cloud. The gcloud output it parses (`--flatten=quotas` CSV, the billing account's `currencyCode`, the
  function's `serviceConfig`, Certificate Manager fields, `artifacts docker images list --include-tags`) is written
  from the documentation; the GitHub environment fields from GitHub's API reference. The OIDC subject check ran
  against the live API on 2026-11-13.
- The quota metric names a trial account shows, whether Spot VMs draw on `PREEMPTIBLE_CPUS`, and the 8-vCPU trial
  cap.
- How long the certificate takes, and whether the `CNAME` changes after `make down` / `make up`.
- That Cloud Billing reports use Pacific-time days, how late the data arrive, and the labels of the credit filters.
- That every Argo CD app ends Healthy with the prober, executor and agent at 0 replicas.
- How long the prober must run before Cloud Monitoring accepts the SLOs (about 10 minutes is the plan's figure).
- The Homebrew cask name.
