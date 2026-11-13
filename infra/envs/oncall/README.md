# infra/envs/oncall

PagerDuty as code for the lab (D6, issue #21): the one service that pages, the one schedule and the
one escalation policy that PagerDuty's free plan allows. How alerts get here from Cloud Monitoring,
the Slack setup and the test page are in [docs/oncall.md](../../../docs/oncall.md).

| Object | Name | Notes |
|---|---|---|
| User | looked up by `oncall_user_email` | Not created here: the account owner already exists, and phone numbers stay out of Git |
| Schedule | `mamori-primary` | Shift-based (`pagerduty_schedulev2`); one weekly shift, Monday 09:00 Tokyo, one member |
| Escalation policy | `mamori-default` | Notifies whoever is on call; repeats after 15 and 30 minutes if nobody acknowledges |
| Service | `mamori-shop` | Urgency high (PagerDuty's default); acknowledgement timeout 30 minutes; auto-resolve off |
| Integration | `Google Cloud Monitoring (Events API v1)` | Its key goes into `infra/envs/lab` as `pagerduty_integration_key` |
| Integration | `Events API v2` | Test pages; any later tool that speaks Events API v2 |

Free-plan limits this root stays within: 5 users, 1 on-call schedule, 1 escalation policy, 100
phone/SMS notifications a month (push and email are not counted).

## Applied by hand, not by CI

The maintainer applies this root from a laptop. CI never plans or applies it:

- The PagerDuty token can change the whole PagerDuty account. Keeping it off GitHub means a
  workflow can't page, silence or rewire on-call.
- It changes rarely: once at setup, then when someone joins the schedule.
- `make down` doesn't touch it. PagerDuty costs nothing while the lab is down, and the next
  `make up` recreates the Cloud Monitoring channel with the same key.

## Apply

Before the first apply:

1. Create the PagerDuty account (free plan) and finish its own setup: install the mobile app, add
   your phone, set your notification rules ([docs/oncall.md](../../../docs/oncall.md)).
2. The free plan allows one escalation policy and one schedule. If the new account already has
   one (a sample service with a "Default" policy, for example), delete the sample service and the
   policy in the web app first, or the apply fails.
3. Create a REST API key: **Integrations > API Access Keys > Create New API Key** (account owner
   or admin; not read-only). It is shown once.

Then, with Application Default Credentials for the state bucket (`gcloud auth application-default
login`) and `TF_STATE_BUCKET` set as for the lab:

```sh
export PAGERDUTY_TOKEN='<the REST API key>'     # read by the provider; never in a file or the state
export TF_VAR_oncall_user_email='<your PagerDuty login>'

cd infra/envs/oncall
terraform init -backend-config="bucket=$TF_STATE_BUCKET" -backend-config="prefix=oncall"
terraform apply

unset PAGERDUTY_TOKEN
```

An account in PagerDuty's EU region also needs `export PAGERDUTY_SERVICE_REGION=eu`. Delete the API
key afterwards, or keep it in a password manager; this root needs it only when it changes.

Then hand the Cloud Monitoring key to the lab (details in docs/oncall.md):

```sh
terraform output -raw cloud_monitoring_integration_key | gh secret set PAGERDUTY_INTEGRATION_KEY
export TF_VAR_pagerduty_integration_key="$(terraform output -raw cloud_monitoring_integration_key)"
```

## Inputs and outputs

| Variable | Default | Notes |
|---|---|---|
| `oncall_user_email` | none | sensitive; your PagerDuty login |
| `time_zone` | `Asia/Tokyo` | how the schedule is shown; full IANA name (`Etc/UTC`, not `UTC`) |

| Output | Sensitive | Use |
|---|---|---|
| `cloud_monitoring_integration_key` | yes | `pagerduty_integration_key` of `infra/envs/lab` |
| `events_api_v2_integration_key` | yes | `routing_key` for the test page in docs/oncall.md |
| `service_url` | no | the service in the PagerDuty web app |

Example: `terraform.tfvars.example`.

## Why two integrations

Cloud Monitoring's PagerDuty channel sends Events API v1 events with a "service key", and Google's
setup guide asks for an integration of type **Events API v1**. PagerDuty's guides tie a key to the
API version of its integration. So Cloud Monitoring gets its own v1 integration, and everything
else uses the v2 one. PagerDuty's docs call v1 "legacy", but no end-of-life date was published
when this was checked (2026-11-13).

## State and secrets

- State: the lab's state bucket, prefix `oncall`. It holds both integration keys in plain text, as
  the lab's state holds the Cloud Monitoring key. The bucket is private, but every identity that
  can read the lab's state can read this too (`tf-plan`, which any workflow run of the repository
  can use, and `tf-apply`).
- An integration key can only send events to `mamori-shop`: someone holding it could page you or
  resolve an open page, not read or change the account. To rotate one:
  `terraform apply -replace=pagerduty_service_integration.cloud_monitoring`, then update the
  GitHub secret and apply the lab again.
- The API token is read from `PAGERDUTY_TOKEN` only, so it never reaches the state.

## Checks

The same as the other roots (see [infra/README.md](../../README.md)): `terraform fmt`,
`terraform init -backend=false`, `terraform validate`, tflint with `infra/.tflint.hcl`, Trivy.
`.terraform.lock.hcl` pins the provider for `darwin_arm64` and `linux_amd64`.

Not verified yet (no PagerDuty account exists): that the free plan accepts a shift-based schedule
through the API (PagerDuty says shift-based schedules are on all plans; if the API refuses, the
deprecated `pagerduty_schedule` with one weekly layer is the fallback), and that a new account comes
with a sample escalation policy.
