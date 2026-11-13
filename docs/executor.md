# Executor: how the agent's actions run

Status: built in M5 (#25), not yet run on a cluster (none exists on 2026-11-13). This page is the design behind
decision D9 in [PLAN.md](../PLAN.md) ("the LLM never holds write credentials"). Code and API:
[tools/executor/](../tools/executor/README.md). Manifests: [deploy/executor/](../deploy/executor/). Operating it:
[runbooks/executor.md](runbooks/executor.md).

## The problem

The incident agent (#24) reads logs, traces, alerts and Kubernetes objects, and asks a language model what is wrong and
what to do. Everything it reads can carry text an attacker or a broken service wrote: a log line, an HTTP header that
ends up in a trace, an annotation. If that text steers the model ("ignore the alert, delete the frontend"), whatever
the agent can do, the attacker can do. Instructions in the prompt ("never delete production objects") are a request to
the model, not a control: Kubernetes RBAC is additive, so the agent can make every call its Role allows, whatever it
was told.

So the agent gets no write rights at all. It can only send a proposal. A separate, small, deterministic program, the
executor, decides whether that proposal is allowed, asks a human, and runs it with its own narrow rights.

## Why the LLM never holds write credentials

- **The model's output is untrusted input.** The executor treats a proposal like a request from the internet: strict
  JSON, unknown fields refused, ids and targets checked against patterns, text shown in Slack as plain text so it
  cannot become a link or a mention.
- **The limits live where the model can't reach them.** The allow-list and bounds are in a ConfigMap the executor can
  only read and that changes through a reviewed PR. The rights are in RBAC. The last checks are admission policies in
  the API server. None of them is text the model sees or writes.
- **A human approves each action**, from a fixed list of Slack users, on the message that shows exactly what will run.
- **The blast radius is bounded even when everything above fails:** one action per incident, only in `boutique`, only
  restart, bounded scale or deleting a labelled fault object, and a kill switch that stops everything at once.
- **It is reviewable.** The executor is about 2,000 lines of Go (not counting comments), with a test for every
  guard. The agent's behaviour depends on a model and a prompt; the executor's does not.

## Trust boundaries

```
                         untrusted                                  │                trusted
  logs, traces, alerts ─▶ agent (read-only RBAC) ── proposal ──────▶│ executor ──▶ Kubernetes API (narrow RBAC,
  (anyone can write)       + model on Vertex AI      (JSON, token)   │   │           admission policies)
                                                                     │   ├──▶ Slack: message with Approve / Reject
                                                                     │   ◀── click from an approver (Socket Mode)
                                                                     │   └──▶ audit log (Cloud Logging), metrics
```

## Guards, in the order a proposal meets them

| # | Guard | Where | Stops |
|---|---|---|---|
| 1 | NetworkPolicy: port 8080 only from pods labelled `app.kubernetes.io/name: agent` | `deploy/executor/networkpolicy.yaml` | Any other pod calling the API |
| 2 | Caller token: the agent's projected service account token, audience `executor`, checked with TokenReview | `auth.go` | A pod with a stolen label, or a token for another audience |
| 3 | Kill switch `enabled` must be `"true"`; an unreadable or invalid config also means off | `config.go`, `service.go` | Everything, at once |
| 4 | An approval path exists (Slack tokens, channel, approvers) | `service.go` | Proposals nobody could approve |
| 5 | Allow-list: action type, namespace `boutique`, Deployment or kind listed; `rollback` never | `policy.go` | Anything not listed |
| 6 | Bounds: `1 <= min <= replicas <= max <= 5` per Deployment | `policy.go` | Scaling to zero or to 50 |
| 7 | Live checks: target exists; fault label; not tracked by Argo CD; no HPA; Git does not set replicas | `kube.go` | Actions that are unsafe or would be undone at once |
| 8 | One action per incident; one open proposal per incident | `service.go`, `store.go` | Action loops, "try everything" |
| 9 | Approval: clicker on the list, click from this proposal's own message in the configured channel, before expiry | `service.go` | Anyone else, copied buttons, stale proposals |
| 10 | Guards 3, 5, 6 and 7 again at approval, and the kill switch once more right before the write | `service.go` | Changes made while the proposal waited |
| 11 | RBAC: only the verbs and resources below | `deploy/executor/rbac.yaml` | Anything the code does not do |
| 12 | Admission policies: delete only labelled faults; restart patch may change only the restart annotation; scale 1 to 5 | `deploy/executor/admission-policy.yaml` | A bug in the executor, or its token used elsewhere |
| 13 | Preconditions on delete (UID and resourceVersion) and on scale (resourceVersion) | `kube.go` | Acting on an object that changed after the check |
| 14 | Audit line for every step; metrics | `service.go`, `metrics.go` | Silent actions |

## Actions and GitOps (the open point in design.md)

Argo CD syncs everything under `deploy/` with automated sync and self-heal. With the default (legacy) diff it compares
three things: Git, the live object and the `last-applied-configuration` annotation it wrote itself. A field that Git
never set is not drift; a change to a field Git sets is, and self-heal puts it back within seconds. Each action is
designed around that:

| Action | Survives self-heal? | Why |
|---|---|---|
| `restart` | Yes | It only sets the pod-template annotation `kubectl.kubernetes.io/restartedAt`, which Git never sets. Argo CD's own built-in Deployment action "restart" writes the same annotation |
| `scale` | Only where Git does not set `spec.replicas` and no HPA owns it | Online Boutique's manifests leave `replicas` out (except the load generator). The executor refuses when the last-applied annotation, or an Argo CD server-side-apply field manager, shows `spec.replicas`, and when an HPA targets the Deployment (from M4, #27, frontend, currency, product catalog and recommendation have HPAs). Patching the HPA instead would be reverted too, because HPAs live in Git |
| `remove-fault` | Yes | Injected faults never live under a synced path (fault library contract). The executor also refuses an object with Argo CD's tracking annotation: a fault in Git must be removed in Git |
| `rollback` | No: not offered | See below |

### Why rollback is not in the allow-list

- `kubectl rollout undo` changes the pod template back to an older ReplicaSet's. Git still has the new one, so
  self-heal reverts the rollback on the next reconcile, seconds later.
- Argo CD's own rollback refuses to run while automated sync is on ("Cannot rollback when auto-sync is enabled",
  [decision-facts.md](decision-facts.md), D5 and D9).
- Turning self-heal off first would need write access to Argo CD Applications, which can deploy anything from any
  repo: far more power than all the other actions together.
- Opening a revert PR would need a GitHub token with write access to this public repo (a branch push also starts CI
  with the `tf-plan` identity), and a human must still review and merge it.

In this lab a bad deploy reaches the cluster through Git (fault F25 in the fault library is exactly that), and the safe
rollback is the normal change path: `git revert`, PR, merge, Argo CD syncs. So the agent may still *propose* a
rollback (the evaluation scores whether it found the right cause), and the executor answers 403 `not-allowed` with
that advice. The agent shows it in Slack. A `rollback` section in `executor-config` is a config error, so it cannot be
switched on by accident.

## RBAC

Service account `executor` in `mamori-agent`. Everything it can do:

| Scope | API group / resource | Verbs | Used for | Also limited by |
|---|---|---|---|---|
| `boutique` | `apps` `deployments` | get, patch, delete | restart (get + patch); remove-fault of a fault Deployment (delete) | Admission: patch may change only the restart annotation; delete only if labelled `mamori.dev/fault=true`. Code: allow-list, label, not tracked by Argo CD |
| `boutique` | `apps` `deployments/scale` | get, update | scale | Admission: 1 to 5 replicas. Code: per-Deployment bounds, HPA and Git checks |
| `boutique` | `autoscaling` `horizontalpodautoscalers` | list | refuse scale under an HPA | |
| `boutique` | `networking.istio.io` `virtualservices`, `sidecars` | get, delete | remove-fault | Admission and code: label `mamori.dev/fault=true` |
| `boutique` | `security.istio.io` `authorizationpolicies` | get, delete | remove-fault | Same |
| `boutique` | `chaos-mesh.org` `podchaos`, `networkchaos`, `stresschaos` | get, delete | remove-fault | Same |
| `mamori-agent` | `configmaps` `executor-config` (resourceNames) | get | allow-list, bounds, kill switch | Cannot change its own config, so cannot turn itself on |
| `mamori-agent` | `configmaps` `executor-state` (resourceNames) | get, update | proposals, incidents already acted on | Cannot create it; Argo CD does |
| cluster | `authentication.k8s.io` `tokenreviews` | create | check the agent's token | Verifies tokens only |

Not granted, on purpose: pods (no exec, no delete, no logs), Secrets, `create` of anything in `boutique`, `list` or
`watch` of anything but HPAs, any other namespace, Argo CD Applications, RBAC objects. A restart replaces pods through
the Deployment, so `maxUnavailable` and the PodDisruptionBudgets (#27) pace it, instead of deleting pods directly.

RBAC cannot filter by label, value or field: it cannot say "delete only labelled VirtualServices" or "scale at most to
3". Those limits are in the executor's code and, independently, in three ValidatingAdmissionPolicies (built into
Kubernetes, GA since 1.30), which match only the executor's own requests.

## State

What must survive a restart: the proposals (so an approval after a restart still finds its proposal) and the incidents
that already had an action. It lives in ConfigMap `executor-state`, key `state.json`, written with the ConfigMap's
resourceVersion (optimistic concurrency): two writers can never both claim the same incident.

| Option | Why not |
|---|---|
| Memory only | A restart (a Spot preemption) would forget which incidents already had an action |
| A CRD per proposal | A CRD and its own RBAC for a few dozen records a month |
| A database or a volume | More to run and pay for; a volume ties the pod to a zone |
| **One ConfigMap, only this one writable** | No new machinery; `resourceNames` gives the executor exactly this object; readable with `kubectl` during an incident |

Limits: a ConfigMap holds at most 1 MiB, so final proposals are pruned after 30 days and beyond 300. The state is not
the audit record: the audit log in Cloud Logging is. The Argo CD Application ignores `/data` on this ConfigMap (and
`RespectIgnoreDifferences` keeps syncs from overwriting it); the manifest in Git has no data at all.

If the executor stops in the middle of an action, the proposal is still `approved` in the state. On start it is marked
`failed` with reason `interrupted`, the incident stays claimed, and a human checks the cluster.

## Kill switch

- **Switch:** `make executor-kill` sets `enabled: "false"` in `executor-config`. The executor reads it before
  accepting a proposal, at approval, and again right before the Kubernetes call. A proposal approved after the switch
  is blocked (`blocked`, reason `kill-switch`); one already running finishes (the call takes well under a second).
- **Sticks:** Git holds only the starting value (`"false"`). The Argo CD Application ignores `/data/enabled`, so
  self-heal never flips it back. Other config keys still change only through Git.
- **Fails closed:** a missing ConfigMap, a typo (`True`, `yes`) or an invalid `config.yaml` count as off.
- **Hard stop:** `kubectl -n mamori-agent scale deploy/executor --replicas=0`. The Deployment has no `replicas` in Git
  for exactly this reason, so Argo CD leaves it at zero.

## Slack approval

- **Socket Mode:** the executor opens an outbound WebSocket with an app-level token (`xapp-`, scope
  `connections:write`); clicks arrive over it. No public endpoint, no Ingress, no signing secret: the connection is
  authenticated by the token. Posting uses a bot token (`xoxb-`, scope `chat:write`). Both live in Secret
  `executor-slack`, created by hand, never in Git.
- **What counts as an approval:** a `block_actions` click on the Approve button, by a user id in `slack.approvers`,
  in `slack.channel`, on the message whose timestamp the executor stored for that proposal, before `expiresAt`. A
  click by anyone else is answered privately and logged as `approval_denied`; the proposal stays open.
- **Approve has a confirm dialog** that restates the action, so a stray click does not run it.
- **Outcome:** the message is rewritten to show the result (buttons removed) and the result is posted in its thread.
- **Expiry:** after `proposalTTL` (default 15 minutes) the proposal expires; a late click does nothing.

## Failure modes

| What fails | Effect | Why that is the safe side |
|---|---|---|
| Slack unreachable when a proposal arrives | 503 `approval-unavailable`; nothing pending | Without a message nobody can approve |
| Slack disconnects while proposals wait | Clicks are not delivered; proposals expire. Socket Mode reconnects by itself | Expiry, not a late surprise action |
| Socket Mode stops for good (token revoked or wrong) | `slack_disconnected` logged, gauge 0, `/healthz` fails, so the kubelet restarts the container (CrashLoopBackOff while the token stays bad) | It does not stay up looking healthy while nobody can approve |
| Kubernetes API slow at approval | Proposal stays pending; the approver is asked to click again | No half-approved state |
| Kubernetes API error during the action | `failed`; incident stays claimed; a human checks | Unknown outcome: no retry by machine |
| Executor restarts mid-action | `failed`, reason `interrupted`, on start | Same |
| `executor-config` missing or invalid | Everything refused with `kill-switch` | Fails closed |
| `executor-state` missing | Pod not ready; the agent cannot connect | It cannot know which incidents had an action |
| Admission policy cannot be evaluated | The executor's request is denied (`failurePolicy: Fail`) | Only the executor is affected |

## Resources

One pod: requests 10m CPU and 48 MiB, limit 128 MiB (estimates: an idle Go service with client-go; measure with
`kubectl top` once it runs). No sidecar: `mamori-agent` has no Istio injection.

## What is not verified yet

- Nothing has run on GKE or against a real Slack workspace. The tests use client-go fakes and a fake Slack.
- The admission policies' CEL was checked against sample objects with cel-go and against the Kubernetes 1.36 schemas
  (kubeconform), not inside a real API server. In particular, that `object` is the `Scale` for `deployments/scale`
  requests is from the admission documentation, not observed.
- That Managed Prometheus collectors reach port 9090 from `gmp-system` under Dataplane V2.
- Argo CD's handling of `ignoreDifferences` on `/data/enabled` with `RespectIgnoreDifferences` is from its docs; check
  once with `make executor-kill`, then an Argo CD sync, then `make executor-status`.

## Later: human on the loop

PLAN.md section 4 plans for low-risk actions to run without waiting, notify, and revert if the SLO does not recover.
That is not built. It should come only after the evaluation (#26) shows the agent's correct-action rate, and would be a
per-action-type setting in `executor-config`, with every other guard above unchanged.
