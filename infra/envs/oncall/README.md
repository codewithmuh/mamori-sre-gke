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
| Integrations | added by hand, not here | "Events API v1" for Cloud Monitoring; "Events API v2" only when a tool needs it (see below) |

Free-plan limits this root stays within: 5 users, 1 on-call schedule, 1 escalation policy, 100
phone/SMS notifications a month (push and email are not counted).

## Applied by hand, not by CI

The maintainer applies this root from a laptop. CI never plans or applies it:

- The PagerDuty token can change the whole PagerDuty account. Keeping it off GitHub means a
  workflow can't page, silence or rewire on-call.
- It changes rarely: once at setup, then when someone joins the schedule.
- `make down` doesn't touch it. PagerDuty costs nothing while the lab is down; after the next
  `make up`, the same integration key goes onto the new Cloud Monitoring channel by hand.

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

Then add the Cloud Monitoring integration by hand (next section), and set its key on the lab's
PagerDuty channel once the lab is up ([docs/oncall.md](../../../docs/oncall.md), step 7).

## Inputs and outputs

| Variable | Default | Notes |
|---|---|---|
| `oncall_user_email` | none | sensitive; your PagerDuty login |
| `time_zone` | `Asia/Tokyo` | how the schedule is shown; full IANA name (`Etc/UTC`, not `UTC`) |

| Output | Use |
|---|---|
| `service_url` | the service in the PagerDuty web app, where its integrations are added |

Example: `terraform.tfvars.example`.

## Integrations, by hand

Integrations hold the keys that send events to `mamori-shop`. Managed here, a key would sit in this
root's state, in the state bucket that `tf-plan` (any workflow run of the repository) can read. So
they are added in the web app: **Services > mamori-shop > Integrations > Add an integration**.

| Integration | When | Why |
|---|---|---|
| **Events API v1**, named "Google Cloud Monitoring" | at setup | Cloud Monitoring's PagerDuty channel sends Events API v1 events with a "service key", and Google's setup guide asks for this type. PagerDuty's guides tie a key to its integration's API version |
| **Events API v2** | only when a tool needs one (Datadog's window, Alertmanager) | that tool's routing key |

The key stays visible on the integration's page, so there is nothing to store. PagerDuty's docs call
v1 "legacy", but no end-of-life date was published when this was checked (2026-11-13).

## State and secrets

- State: the lab's state bucket, prefix `oncall`. It holds names, IDs and settings only: no
  integration key and no token.
- The API token is read from `PAGERDUTY_TOKEN` only, so it never reaches the state.
- An integration key can only send events to `mamori-shop`: someone holding it could page you or
  resolve an open page, not read or change the account. To rotate one, delete the integration and
  add it again, then set the new key on the Cloud Monitoring channel ([docs/oncall.md](../../../docs/oncall.md)).

## Checks

The same as the other roots (see [infra/README.md](../../README.md)): `terraform fmt`,
`terraform init -backend=false`, `terraform validate`, tflint with `infra/.tflint.hcl`, Trivy.
`.terraform.lock.hcl` pins the provider for `darwin_arm64` and `linux_amd64`.

Not verified yet (no PagerDuty account exists): that the free plan accepts a shift-based schedule
through the API (PagerDuty says shift-based schedules are on all plans; if the API refuses, the
deprecated `pagerduty_schedule` with one weekly layer is the fallback), and that a new account comes
with a sample escalation policy.
