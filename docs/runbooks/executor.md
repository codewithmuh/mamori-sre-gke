# Runbook: executor (kill switch, stuck proposals, audit)

The executor runs the incident agent's proposed actions after a human approves them in Slack: restart a Deployment,
scale one within bounds, or delete an injected fault object, at most one action per incident. Design:
[executor.md](../executor.md). Code: [tools/executor](../../tools/executor/README.md).

Everything lives in namespace `mamori-agent`: Deployment `executor`, ConfigMaps `executor-config` (allow-list and kill
switch) and `executor-state` (proposals), Secret `executor-slack` (tokens).

## Stop it now (kill switch)

Use it when an action looks wrong, the agent is proposing nonsense, or you are not sure. It is always safe: the shop
does not need the executor.

```bash
make executor-kill                 # enabled: "false" in executor-config
make executor-status               # shows enabled: false, the pod, open proposals
```

- New proposals get 423 `kill-switch`. A click on Approve now ends as `blocked` (reason `kill-switch`); nothing runs.
- The executor reads the switch again right before each Kubernetes call, so an approval racing the switch is blocked
  too. An action already in its API call (well under a second) finishes.
- Argo CD does not undo it: the Application ignores that one key.

Hard stop, if the process itself misbehaves. `replicas` is in Git, so pause the Application first or self-heal
scales it back up within seconds:

```bash
kubectl -n argocd patch application executor --type merge -p '{"spec":{"syncPolicy":{"automated":null}}}'
kubectl -n mamori-agent scale deploy/executor --replicas=0
# afterwards: a PR that sets replicas: 0 in deploy/executor/deployment.yaml makes it permanent;
# to resume, scale back to 1 and restore the Application's syncPolicy (re-apply deploy/apps/executor.yaml).
```

If you suspect the executor's own identity is being misused: hard stop as above (auto-sync off), then
`kubectl -n boutique delete rolebinding executor`.

## Check what it did

```bash
make executor-audit SINCE=2h       # audit lines from the running pod
```

Audit lines carry `event` (`proposal_received`, `approved`, `rejected`, `executed`, `blocked`, `failed`;
`approval_denied` for clicks by someone not on the list), `proposalId`, `incidentId`, `action`, `status`, `actor`,
`reason`, `error`, `result`. Undo by hand if needed:

| Action | Undo |
|---|---|
| restart | Nothing to undo: pods were replaced. If the new pods fail, look at why (`kubectl -n boutique describe deploy/<name>`); the old ReplicaSet is still there |
| scale | `kubectl -n boutique scale deploy/<name> --replicas=<previous>` (the result line says "from A to B") |
| remove-fault | Re-inject only through the game-day CLI (`gameday start <id>`), and only on purpose |

## Audit queries

Logs Explorer (Cloud Logging keeps them past pod restarts):

```text
resource.type="k8s_container"
resource.labels.namespace_name="mamori-agent"
resource.labels.container_name="executor"
jsonPayload.event=("proposal_received" OR "approved" OR "rejected" OR "executed" OR "blocked" OR "failed" OR "approval_denied")
```

Narrow it with one more line:

| Question | Add |
|---|---|
| Everything for one incident | `jsonPayload.incidentId="<id>"` |
| Every action that ran | `jsonPayload.event="executed"` |
| Unsafe or refused attempts | `jsonPayload.event="blocked"` (see `jsonPayload.reason`) |
| Who approved what | `jsonPayload.event="approved"` (`jsonPayload.actor` is the Slack user id) |
| Clicks by people not on the list | `jsonPayload.event="approval_denied"` |
| Failures needing a human | `jsonPayload.event="failed"` |

From a shell:

```bash
gcloud logging read 'resource.type="k8s_container" AND resource.labels.namespace_name="mamori-agent"
  AND resource.labels.container_name="executor" AND jsonPayload.event:*' --freshness=7d \
  --format='table(timestamp, jsonPayload.event, jsonPayload.incidentId, jsonPayload.action.type,
  jsonPayload.action.target, jsonPayload.actor, jsonPayload.reason)'
```

Metrics (Managed Prometheus): `mamori_executor_events_total{event,action,reason}`,
`mamori_executor_enabled`, `mamori_executor_pending_proposals`, `mamori_executor_slack_connected`,
`mamori_executor_action_duration_seconds`.

## A proposal is stuck

| What you see | Cause | Do |
|---|---|---|
| `pending`, no Slack message | Slack failed after the proposal was stored, or the message was deleted | Nothing: it expires after `proposalTTL` (15 min). The agent can propose again after that |
| `pending`, message there, clicks do nothing | Socket Mode is down (`mamori_executor_slack_connected` is 0), or you are not on `slack.approvers` (you got a private note) | Check the pod log for `slack_disconnected`; check the Secret (below); add yourself to `slack.approvers` by PR |
| Executor pod restarting, or in CrashLoopBackOff | Socket Mode stopped for good (`slack_disconnected` with `invalid_auth`, `token_revoked`, ...): the liveness probe fails on purpose so the pod restarts | Fix the tokens (`make executor-slack-secret`); pending proposals simply expire meanwhile |
| `approved` for more than a minute | The pod died during the action | It turns `failed` (reason `interrupted`) when the pod starts. If the pod does not start: `kubectl -n mamori-agent describe pod -l app.kubernetes.io/name=executor`. Check the cluster by hand: the action may or may not have run |
| `409 already-acted` but the incident needs another action | One action per incident, by design | Do it by hand, through the normal change path. Do not edit the state to get around it |

Open proposals: `make executor-status` (needs `jq` for a table). The raw state:
`kubectl -n mamori-agent get configmap executor-state -o jsonpath='{.data.state\.json}'`.

Only if the state itself is corrupt (the pod logs "decode state.json" and is not ready): save it, then empty it. This
forgets every proposal and which incidents already had an action, so do it with the kill switch on:

```bash
make executor-kill
kubectl -n mamori-agent get configmap executor-state -o yaml > executor-state-backup.yaml
kubectl -n mamori-agent patch configmap executor-state --type json -p '[{"op":"remove","path":"/data"}]'
kubectl -n mamori-agent rollout restart deploy/executor
```

## A proposal was refused

The agent's call gets a 4xx with `reason`; the same reason is in the `blocked` audit line.

| `reason` | Meaning | Do |
|---|---|---|
| `kill-switch` | Switched off; or `executor-config` is missing or invalid; or no Slack tokens, channel or approvers | `make executor-status`; pod log at start says which |
| `not-allowed` | Not on the allow-list, wrong namespace, `rollback`, not a labelled fault, HPA or Git owns replicas, target missing | Expected for unsafe proposals. For rollback: revert the commit in Git |
| `out-of-bounds` | Replicas outside the configured bounds | Change bounds by PR if they are wrong |
| `already-acted` | The incident already had an action, or another proposal is open | See above |
| `too-many-open` (429) | 5 proposals are already pending or approved | Decide them in Slack, or wait for them to expire |
| `rate-limited` (429) | More than 10 POSTs at once, then more than one per 6 s | The agent is looping: check its log; `make executor-kill` if in doubt |
| `unauthorized` | The caller's token is not the agent's | Check the agent's projected token (audience `executor`) |

`blocked` after approval with reason `conflict`: the target kept changing between the check and the write (checked
again and tried twice); nothing was changed and the incident is free for a new proposal.

A click answered privately with "the message no longer shows what this proposal would run" (`approval_denied`,
reason `content-mismatch`): the Slack message was edited after the executor posted it. Treat it as a security event:
something holding a bot token of the executor's Slack app changed it. Rotate the executor's bot token, check that the
agent has its own Slack app, and leave the kill switch on until then. If every click on unedited messages shows this,
Slack changed how it returns message text: the pod log line "the clicked message differs from the proposal" shows the
first difference.

`blocked` after approval with "denied by Kubernetes (RBAC or admission policy)": the API server refused the request.
That means the code and the cluster disagree; keep the kill switch on and look at the error text and
`deploy/executor/admission-policy.yaml`.

## Turn it on (first time, M5)

0. **The pod:** after the first image publish (see `docs/images.md`), set `replicas: 1` in
   `deploy/executor/deployment.yaml` by PR. It ships at 0 so it does not sit in ImagePullBackOff before then.
1. **Slack app, its own:** not the agent's app. At api.slack.com/apps create an app for the executor in the workspace. Turn on Socket Mode; it asks for an app-level
   token with scope `connections:write` (`xapp-...`). Under Interactivity, leave it on (Socket Mode needs no URL).
   Under OAuth, add bot scope `chat:write` and install; copy the bot token (`xoxb-...`). Invite the bot to the
   approval channel (`/invite @<app>`).
2. **Tokens into the cluster** (never into Git):

   ```bash
   export EXECUTOR_SLACK_APP_TOKEN=xapp-...  EXECUTOR_SLACK_BOT_TOKEN=xoxb-...   # from a password manager, not typed into history
   make executor-slack-secret
   ```

3. **Channel and approvers by PR:** in `deploy/executor/configmap-config.yaml`, set `slack.channel` (the channel id,
   from the channel's details) and `slack.approvers` (member ids from each profile, "Copy member ID"). Merge; Argo CD
   syncs it.
4. **Switch on:** `make executor-enable`, then `make executor-status`.
5. **Smoke test** with a harmless labelled object, from your machine (port-forward is authorised by your RBAC; you
   borrow the agent's identity for one short token):

   ```bash
   kubectl -n boutique apply -f - <<'EOF'
   apiVersion: networking.istio.io/v1
   kind: VirtualService
   metadata:
     name: executor-smoke
     labels: {mamori.dev/fault: "true", mamori.dev/fault-id: F00-executor-smoke}
   spec:
     hosts: [executor-smoke.invalid]
     http: [{route: [{destination: {host: executor-smoke.invalid}}]}]
   EOF
   kubectl -n mamori-agent port-forward svc/executor 8080:8080 &
   TOKEN=$(kubectl -n mamori-agent create token agent --audience executor --duration 10m)
   curl -sS localhost:8080/v1/proposals -H "Authorization: Bearer $TOKEN" -d '{"incidentId":"smoke-'"$(date +%s)"'",
     "investigationId":"manual","action":{"type":"remove-fault","namespace":"boutique",
     "target":"virtualservice/executor-smoke","params":{}},"rationale":"executor smoke test"}'
   ```

   Approve it in Slack; the thread should say "Executed: deleted virtualservice boutique/executor-smoke". Then
   `make executor-audit SINCE=10m`. Also check the kill switch sticks: `make executor-kill`, sync the `executor`
   Application in Argo CD, `make executor-status` still shows `enabled: false`.

## Escalate

The executor never needs to run for the shop to work. If in doubt, leave it killed and handle the incident by hand
(incident process: `docs/incidents/`). Changes to its rights or its allow-list go through a PR.
