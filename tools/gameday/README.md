# gameday

A small CLI that runs one fault from the fault library ([chaos/](../../chaos/README.md)) against
the lab cluster, for a bounded time, and records what happened when. How to run a game day
around it: [docs/game-days.md](../../docs/game-days.md).

```sh
make gameday-build                       # bin/gameday (Go on the host, or Go in Docker)
bin/gameday list                         # the library: ids, categories, game-day faults, manual ones
bin/gameday start F01-currency-abort     # apply, hold for the fault's duration, remove
bin/gameday start F01-currency-abort --duration 15m -o json
bin/gameday status                       # fault objects in the cluster now
bin/gameday stop F01-currency-abort      # remove one fault's objects (safe to repeat)
bin/gameday stop --all                   # remove every fault object in boutique
bin/gameday start F05-node-preemption --manual   # timeline for a fault a person injects
```

`go -C tools/gameday run . <args>` works too: the CLI finds the repository by walking up from the
current directory to `chaos/faults` (or takes `--repo`). Every command takes `-o text|json`,
`--repo`, `--kubeconfig` and `--context`.

## What `start` does

1. Loads the whole library and checks every file against `chaos/fault.schema.json` and the rules
   below. One bad file stops it: a half-loaded library would hide mistakes until a game day.
2. **Preflight.** Refuses (exit 2) if any fault object already exists in `boutique` (one fault at
   a time), if a stopped discovery run (#17) left its marker ConfigMap (labelled
   `mamori.dev/fault: "true"`; redis-cart may still be scaled to zero: run `make discovery-cleanup`),
   or if another mesh VirtualService routes the same host or another Sidecar selects the same pods
   (Istio's result would be undefined). A #17 discovery run is the likely clash.
3. Writes `gameday-runs/<run-id>.json` (run id `20261205T100000Z-F01-currency-abort`) and applies
   the objects with the dynamic client. Each always gets `mamori.dev/run-id` and
   `mamori.dev/expires-at` (UTC, RFC 3339), which the fault-reaper uses. Chaos Mesh objects get
   `spec.duration` set to the run's duration, so Chaos Mesh stops injecting by itself even if
   this process dies. If any object fails, the ones already applied are deleted again (exit 1).
4. **Holds** the fault in the foreground until the duration ends (default: the fault's own; at
   most 60 minutes), a signal arrives (Ctrl-C, SIGTERM, or SIGHUP when the terminal is closed),
   or someone else removes the objects
   (for example the executor's `remove-fault`), which it notices within 5 seconds and records as
   `removed-externally`, with the time.
5. **Removes** the objects and waits until they are gone. A failed delete (the API server is
   unreachable during a control-plane upgrade, the network drops, the login token expired) is
   retried with backoff (5 s, doubling, at most 30 s apart) and logged as `delete_retry` each time,
   for up to 5 minutes in all. A Chaos Mesh object only goes once Chaos Mesh has recovered its pods
   (a finalizer), so "gone" means "recovered". If it still fails after 5 minutes, it says so loudly
   (the fault may still be active) and exits 1: run `gameday stop <id>` again, and check
   `kubectl -n chaos-mesh get pods`. A second Ctrl-C does not interrupt the cleanup.

With `-o json`, `start` prints exactly two lines on stdout: `{"event":"started","runId",
"faultId","startedAt","endsAt","objects":[...],"timeline"}` once everything is applied, and
`{"event":"finished","runId","finishedAt","reason"}` at the end (`duration`, `signal` or
`removed-externally`). Logs are JSON lines on stderr (`severity`, `message`, `event`, ...).

### Safety rules (enforced in code)

- Writes only to namespace `boutique`; refuses any manifest object elsewhere.
- Applies only objects labelled `mamori.dev/fault: "true"` and `mamori.dev/fault-id: <its id>`,
  named after the fault, of an allowed kind (VirtualService, Sidecar, AuthorizationPolicy,
  Deployment, PodChaos, NetworkChaos, StressChaos). The resource names are fixed in code, not
  discovered.
- Chaos Mesh selectors may only name `boutique` (also enforced by Chaos Mesh's namespace-scoped
  install).
- `stop` deletes by label selector, so it can only ever delete fault objects.
- Every run is capped at 60 minutes, and `gameday status` flags objects that outlived their
  `expires-at` (a `start` that died). The **fault-reaper** CronJob (`deploy/fault-reaper`, every
  minute) deletes them about two minutes after they expire, so a fault without a TTL of its own
  (VirtualService, Sidecar, AuthorizationPolicy, Deployment) cannot outlive its run by much. Its
  RBAC allows get, list and delete on the seven kinds in `boutique` only, and a
  ValidatingAdmissionPolicy lets it delete only labelled objects that carry `expires-at`.
- `status` and `stop --all` also report discovery markers (ConfigMaps labelled as faults, see
  preflight) and print `run make discovery-cleanup`. gameday never deletes them: only the
  discovery tool knows how to restore redis-cart's replicas.

Not covered: the three manual faults (`F05`, `F25`, `F26`) change things outside `boutique` or go
through Git. `start` prints their steps and exits 3; `--manual` records the timeline while a
person follows them.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | Done (for `start`: applied, held and removed; for `stop`: removed, or nothing to remove) |
| 1 | Error (apply or cleanup failed, API unreachable, invalid library) |
| 2 | Usage error, or refused by a safety check |
| 3 | The fault needs a manual procedure |

### Timeline

`gameday-runs/<run-id>.json` (gitignored), rewritten after every event by the `start` process
that owns the run:

```json
{"runId": "20261205T100000Z-F01-currency-abort", "faultId": "F01-currency-abort", "mode": "cli",
 "kubeContext": "gke_...", "duration": "10m0s", "objects": ["virtualservice/f01-currency-abort"],
 "startedAt": "...", "endsAt": "...", "finishedAt": "...", "status": "finished", "endReason": "duration",
 "events": [{"time": "...", "event": "run_started"}, {"time": "...", "event": "object_applied",
 "object": "virtualservice/f01-currency-abort"}, {"time": "...", "event": "fault_active"}, "..."]}
```

Events: `run_started`, `object_applied`, `apply_failed`, `fault_active`,
`object_deletion_started` (someone else deleted it; a Chaos Mesh object lingers while its pods are
recovered), `object_removed_externally`, `removing`, `fault_removed`, `cleanup_failed`, `run_finished`; for
manual runs `fault_injected`, `revert_requested`, `fault_reverted` / `revert_not_confirmed`.
The incident's own timeline (detection, acknowledgement, mitigation) belongs to the postmortem
(#20); this file gives it the exact injection and removal times.

## Permissions

The person (or CI job) running `start` and `stop` needs, in namespace `boutique` only: create,
get, list, delete on the allowed kinds, and list on ConfigMaps (discovery markers). `status`
needs get and list. Chaos Mesh's own admission
webhook also checks that whoever creates a Chaos object may create that kind in `boutique`.

## Develop and test

No Go needed on the host; from the repository root:

```sh
make gameday-test   # scripts/go-test.sh tools/gameday: race detector, 80 % coverage gate
docker run --rm -v "$PWD":/src -w /src/tools/gameday golangci/golangci-lint:v2.15.0 golangci-lint run ./...
```

The tests use client-go's fake dynamic client: the apply-hold-remove cycle, external removal,
signals, refusals (another fault active, a discovery marker, a host or Sidecar clash), a failed
apply rolled back, deletes retried while the API server is unreachable, a finalizer that never
clears, the manual flow, `stop`, `status`, and kubeconfig loading.
`library_test.go` loads the real `chaos/faults`: every file against the schema, every manifest
object's labels, namespace and kind, the six game-day ids, 20–30 faults, every category, no
orphaned manifests.
