# executor

A small Go service that runs the incident agent's proposed actions, one per incident, after a human approves each one
in Slack. The agent (#24) can only propose: it holds no write credentials. This program has its own narrow RBAC, an
allow-list with bounds, an audit log and a kill switch. Why it is built this way, the RBAC table and the rollback
decision: [docs/executor.md](../../docs/executor.md). Operating it: [docs/runbooks/executor.md](../../docs/runbooks/executor.md).

Nothing here has run against a real cluster or Slack workspace yet (no cluster exists on 2026-11-13). The tests use
client-go fakes and a fake Slack.

## Flow

```
agent ──POST /v1/proposals──▶ executor
                               0. under the rate limit?        10 at once, then one per 6 s, else 429 rate-limited
                               1. caller is the agent?        TokenReview of its projected token (audience "executor")
                               2. kill switch on?              executor-config enabled == "true", else 423 kill-switch
                               3. allow-list and bounds?       403 not-allowed / 422 out-of-bounds
                               4. live checks                  target exists; fault label; no HPA; Git doesn't set replicas
                               5. incident free?               no other open or acted proposal, else 409 already-acted;
                                                              at most 5 open in all, else 429 too-many-open
                               6. store as pending, post to Slack with Approve / Reject      ──▶ 202 {proposalId, pending}
human clicks Approve ─(Socket Mode)─▶ executor
                               7. clicker on the approver list? click from the proposal's own message, unedited? not expired?
                               8. kill switch, allow-list, bounds and live checks again
                               9. claim the incident (atomic write to executor-state), audit "approved"
                              10. kill switch once more, then the Kubernetes call     ──▶ executed / failed / blocked
                              11. result on the Slack message and in its thread
```

A proposal nobody decides on expires after `proposalTTL` (default 15 minutes).

## API (contract)

Service `executor.mamori-agent.svc:8080`. Only pods labelled `app.kubernetes.io/name: agent` can connect
(NetworkPolicy).

`POST /v1/proposals` needs `Authorization: Bearer <projected service account token>`: audience `executor`, user
`system:serviceaccount:mamori-agent:agent`. Body (unknown fields are refused):

```json
{"incidentId": "0.nqzj9ah0ksie", "investigationId": "inv-123",
 "action": {"type": "scale", "namespace": "boutique", "target": "deployment/cartservice", "params": {"replicas": 2}},
 "rationale": "cartservice CPU is saturated since 09:02; one more replica should take the load"}
```

| Response | `reason` | When |
|---|---|---|
| 202 `{"proposalId", "status": "pending"}` | | Accepted and posted to Slack. The same request again (agent retry) returns the same proposal |
| 400 | `invalid-request` | Bad JSON, unknown field, missing or malformed ids, bad `params` |
| 401 | `unauthorized` | No token, or not the agent's |
| 403 | `not-allowed` | Type not allowed (rollback always), other namespace, target not listed, not a labelled fault, HPA owns replicas, Git sets replicas, target missing |
| 409 | `already-acted` | The incident already had an action, or another proposal for it is open |
| 429 | `too-many-open` | 5 proposals are already pending or approved |
| 429 | `rate-limited` | More than 10 POSTs at once, then more than one per 6 s |
| 422 | `out-of-bounds` | `replicas` outside the bounds for that Deployment |
| 423 | `kill-switch` | Switched off, config unreadable or invalid, or no way to approve (no Slack tokens, channel or approvers) |
| 503 | `approval-unavailable` | Posting to Slack failed; nothing is pending |

Error bodies are `{"error": "<text>", "reason": "<reason>"}`.

`GET /v1/proposals/{id}` (no token needed) returns `proposalId`, `status`
(`pending|approved|rejected|executed|failed|expired|blocked`), `incidentId`, `investigationId`, `action`, `rationale`,
`createdAt`, `expiresAt`, `decidedAt`, `decidedBy`, `executedAt`, `reason`, `error`, `result`, `slackChannel`,
`slackTs`. Empty fields are left out. `rationale` keeps the first 1,000 bytes; the full text is in the
`proposal_received` audit line. Unknown id: 404 `not-found`.

## Actions

| Type | Target | What it does | Refused when |
|---|---|---|---|
| `restart` | `deployment/<name>` in `restart.deployments` | Sets `kubectl.kubernetes.io/restartedAt` on the pod template (what `kubectl rollout restart` does) | Not listed, missing, paused |
| `scale` | `deployment/<name>` in `scale`, `params.replicas` | Sets replicas through the `scale` subresource | Outside the bounds, an HPA targets it, Git sets `spec.replicas`, already at that count |
| `remove-fault` | `<kind>/<name>`, kind in `removeFault.kinds` | Deletes that one object, with a UID precondition | No label `mamori.dev/fault: "true"`, tracked by Argo CD, already deleting, missing |
| `rollback` | | Never runs: always 403 with advice to revert in Git | Always |

Fault kinds: `virtualservice`, `sidecar` (networking.istio.io/v1), `authorizationpolicy` (security.istio.io/v1),
`deployment` (apps/v1, only the fault library's own extra Deployments), `podchaos`, `networkchaos`, `stresschaos`
(chaos-mesh.org/v1alpha1).

## Configuration

ConfigMap `executor-config` in `mamori-agent` ([deploy/executor/configmap-config.yaml](../../deploy/executor/configmap-config.yaml)),
read on every check:

- `enabled`: the kill switch. Only `"true"` lets it act. Missing, invalid or anything else means off.
- `config.yaml`: `slack.channel` (channel id), `slack.approvers` (user ids), `proposalTTL` (1m to 1h), `restart`,
  `scale` (bounds `1 <= min <= max <= 5` per Deployment), `removeFault.kinds`. Unknown keys and a `rollback` section are
  errors, and an invalid config counts as off.

Environment (set in [deploy/executor/deployment.yaml](../../deploy/executor/deployment.yaml)):

| Variable | Default | Meaning |
|---|---|---|
| `POD_NAMESPACE` | `mamori-agent` | Where `executor-config` and `executor-state` live |
| `HTTP_ADDR` / `OPS_ADDR` | `:8080` / `:9090` | API / metrics and probes (`/metrics`, `/healthz`, `/readyz`). `/healthz` fails once Socket Mode has stopped for good (e.g. a revoked token), so the kubelet restarts the container |
| `AUTH_MODE` | `tokenreview` | `none` turns caller checks off: local runs only |
| `AUTH_AUDIENCE` | `executor` | Audience the agent's projected token must carry |
| `AUTH_ALLOWED_USERS` | `system:serviceaccount:mamori-agent:agent` | Comma-separated |
| `EXECUTOR_SLACK_APP_TOKEN`, `EXECUTOR_SLACK_BOT_TOKEN` | none | `xapp-...` (Socket Mode) and `xoxb-...` (`chat:write`) of the executor's own Slack app (never the agent's), from Secret `executor-slack`. Without them every proposal is refused |
| `EXECUTOR_SLACK_API_URL` | `https://slack.com/api/` | Another Slack host, or a local fake Slack for an end-to-end test |
| `KUBECONFIG` | none | Local runs outside the cluster |

State survives restarts in ConfigMap `executor-state` (key `state.json`): recent proposals, and the incidents that
already had an action. Final proposals are kept 30 days (at most 300).

## Logs and metrics

One JSON object per line on stdout (`severity`, `message`, `time`), read by Cloud Logging. Audit lines have `event`
`proposal_received`, `approved`, `rejected`, `executed`, `blocked` or `failed`, and carry `proposalId`, `incidentId`,
`investigationId`, `action` `{type, namespace, target, params}` and `status`; plus `actor`, `reason`, `error`,
`rationale`, `result` and `durationMs` where they apply. An expiry is `event=rejected` with `reason=expired`. A click
by someone not on the list is `event=approval_denied`. Queries: [runbook](../../docs/runbooks/executor.md#audit-queries).

| Metric | Labels |
|---|---|
| `mamori_executor_events_total` | `event`, `action`, `reason` |
| `mamori_executor_action_duration_seconds` | `action`, `result` (`executed`, `failed`, `blocked`) |
| `mamori_executor_enabled` | 1 when the kill switch allows actions |
| `mamori_executor_pending_proposals` | |
| `mamori_executor_slack_connected` | 1 while Socket Mode is connected |

## Develop and test

Go isn't needed on the host. From this directory:

```bash
GO_IMAGE=golang:1.27.3-trixie
LINT_IMAGE=golangci/golangci-lint:v2.15.0

# Tests with the race detector and coverage (CI fails below 80 %)
docker run --rm -v "$PWD":/src -w /src $GO_IMAGE \
  sh -c 'go test -race -covermode=atomic -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1'

# Vet, format, lint, known vulnerabilities
docker run --rm -v "$PWD":/src -w /src $GO_IMAGE sh -c 'go vet ./... && test -z "$(gofmt -l .)"'
docker run --rm -v "$PWD":/src -w /src $LINT_IMAGE golangci-lint run ./...
docker run --rm -v "$PWD":/src -w /src $GO_IMAGE go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

# Image (distroless, non-root)
docker build -t executor:dev .
```

The tests cover the allow-list, bounds, the label check, one action per incident, the kill switch at proposal and
right before execution, expiry, the approver check, the message check, restarts (state reload, interrupted actions),
caller authentication, the HTTP status codes, Slack message escaping and Socket Mode dispatch.

### Run it locally

Against a local cluster (kind or similar) with `executor-config` and `executor-state` created from
`deploy/executor/`, and without Slack (every proposal is then refused with `kill-switch`):

```bash
go run . # with KUBECONFIG=~/.kube/config AUTH_MODE=none POD_NAMESPACE=mamori-agent
```

## Dependencies

| Module | Version | Why |
|---|---|---|
| `k8s.io/client-go`, `k8s.io/api`, `k8s.io/apimachinery` | v0.36.6 (2026-10-28) | Kubernetes API; matches the cluster's 1.36 |
| `github.com/slack-go/slack` | v0.31.0 (2026-10-26) | Web API and Socket Mode |
| `github.com/prometheus/client_golang` | v1.25.0 (2026-10-08) | Metrics |
| `sigs.k8s.io/yaml` | v1.6.0 | Strict parsing of `config.yaml` |
| `golang.org/x/net` (indirect) | v0.62.0 (2026-10-27) | Raised above client-go's v0.59.0 for CVE-2026-78661 and CVE-2026-78669 (HIGH, Trivy) |

slack-go advisory GHSA-pj2x-fjjr-q84c (medium, fixed in v0.35.0 of 2026-11-05) affects `InteractionCallbackParse`, which
parses interactions sent over HTTP. The executor uses Socket Mode only, whose client decodes payloads with
`json.Unmarshal` and never calls that function. Move to v0.35.0 or later once it is two weeks old (after 2026-11-19);
v0.32 to v0.35 change some signatures, so run the tests.
