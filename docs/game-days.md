# Game days: running a fault safely

A game day breaks the lab on purpose, with a known cause, so that detection, paging, response and
the postmortem are practised on a real incident. This page covers the mechanics: preparing,
injecting, stopping and cleaning up. The incident process itself (roles, severity, the channel,
the postmortem) is in [docs/incidents/](incidents/) (#20).

The faults are in [chaos/](../chaos/README.md) and the tool is
[tools/gameday](../tools/gameday/README.md). Plan: M3 runs 5–6 game days around the six game-day
faults F01–F06; M5 reuses the whole library (28 faults) to evaluate the incident agent.

> **Don't read the fault file before you respond.** `chaos/faults/<id>.yaml` holds the expected
> answer. Whoever responds (a person on a game day, the agent in the evaluation) works from the
> telemetry only. The facilitator picks the fault; `gameday list` shows only ids and titles.

## How the tooling keeps it safe

- Faults are never synced by Argo CD. `gameday` applies them and removes them again.
- Only namespace `boutique` is touched, and only objects labelled `mamori.dev/fault: "true"`.
  Chaos Mesh itself is installed namespace-scoped to `boutique`, so even a wrong Chaos object
  cannot reach Argo CD, Istio or the system pods.
- One fault at a time. `start` refuses while another fault object exists, or while another
  VirtualService routes the same host (for example a #17 discovery run).
- Every run ends on its own: at its duration (30 minutes at most), on Ctrl-C, or when the objects
  are removed by someone else. Chaos Mesh objects also carry the duration, so they stop injecting
  even if the laptop running `gameday` goes to sleep.
- The big red button: `make gameday-stop-all` removes every fault object in `boutique`.

What the tooling does **not** protect against: a manual fault (F05 preemption, F25 bad deploy
through Git, F26 node drain) only ends when a person reverts it, and an Istio fault outlives a
`gameday start` process that died (the VirtualService has no built-in expiry). `gameday status`
shows such leftovers as EXPIRED.

## Before

1. **The error budget allows it.** The error-budget policy ([prd.md](prd.md), and #19's
   `docs/slo/error-budget-policy.md`) blocks game days on a journey whose budget is spent. A game
   day spends budget on purpose; estimate how much (journeys affected x duration) and write it in
   the plan.
2. **The lab is healthy and quiet.**
   ```sh
   make gameday-status                       # "no fault objects in boutique"
   kubectl -n boutique get pods              # all Running and Ready
   kubectl -n argocd get applications        # all Synced and Healthy
   kubectl -n chaos-mesh get pods            # for Chaos Mesh faults: controller and 3 daemons Ready
   ```
   No upgrade, node repair or spend-guard action running; no other experiment (discovery run,
   load test) planned for the same window.
3. **Paging works.** On-call is set in PagerDuty, the phone is at hand, and the incident channel
   exists. For M3, the Datadog window is open if the plan uses it.
4. **Write the plan** (the template is #20's): fault id, start time, duration, journeys expected
   to suffer (from the plan, not the fault file, if the responder is also the facilitator),
   abort criteria, who facilitates and who responds.
5. **Abort criteria**, decided before the start. Stop at once (`make gameday-stop-all`) if:
   anything outside `boutique` is affected; the public site is down longer than the plan allows;
   the budget burn exceeds the plan; a real incident starts; or the facilitator is unsure what is
   happening.
6. **Announce it** in the incident channel: "Game day starting at HH:MM, about N minutes,
   expect alerts."

## During

Run the fault in its own terminal and leave it open: it holds the fault, then removes it.

```sh
make gameday-start FAULT=F01-currency-abort              # the fault's own duration
make gameday-start FAULT=F01-currency-abort DURATION=15m # override, at most 30m
make gameday-status                                      # from another terminal
```

`gameday` prints the run id and the timeline file, `gameday-runs/<run-id>.json`. From here the
incident process takes over: detect, page, triage, mitigate, communicate.

Mitigating a game-day fault counts as mitigation. If the responder removes it (`make gameday-stop
FAULT=...`, or the executor's `remove-fault` once M5 is in), the running `gameday start` notices
within 5 seconds and records the time as `removed-externally`: that is the time to mitigate.

### Manual faults

F05, F25 and F26 are not objects in `boutique`. `gameday start <id>` prints their steps and exits
with code 3. To record a timeline while following them:

```sh
bin/gameday start F05-node-preemption --manual
```

Press Enter once the fault is injected; the run lasts its duration (or until Ctrl-C), then the
revert steps are printed and Enter marks the revert. F05 needs `gcloud` and `PROJECT_ID`/`ZONE`
from `.env`; F25 needs two pull requests (inject, then revert), so plan for the reviews.

## Stopping early

In order of preference:

1. Ctrl-C in the `gameday start` terminal. It removes the fault and closes the timeline.
2. `make gameday-stop FAULT=<id>` or `make gameday-stop-all`, from anywhere.
3. If a Chaos Mesh object will not go away (`still present after 3m`): Chaos Mesh is recovering
   the pods behind a finalizer. Check `kubectl -n chaos-mesh get pods` and
   `kubectl -n boutique describe <kind>/<name>`. Removing the finalizer by hand is the last
   resort, because it can leave the fault in the pod (a tc rule, a stress process); then restart
   the affected Deployment (`kubectl -n boutique rollout restart deploy/<name>`).
4. For a manual fault, its revert steps (`kubectl uncordon`, `git revert`).

## After

1. **Check recovery**: `make gameday-status` is empty, every pod in `boutique` is Ready, the
   prober is green for every journey, and the burn rate is falling. Faults that kill
   `redis-cart` (F03, F05, F26) empty every cart: that is expected, not a leftover.
2. **Keep the timeline.** `gameday-runs/<run-id>.json` has the exact injection and removal times;
   attach it to the postmortem (#20). It is gitignored; copy what the postmortem needs.
3. **Compare with the fault file**, now that the response is over. If reality differed from
   `expected` or an estimate in `notes` (how slow, how long), fix the fault file in the
   postmortem's pull request: the agent evaluation uses it as ground truth.
4. Announce the end in the incident channel.

## Game-day faults (M3)

| Id | Title | How |
|---|---|---|
| F01-currency-abort | Currency service returns errors | gameday |
| F02-productcatalog-latency | Product catalog answers 300 ms late | gameday |
| F03-redis-cart-pod-kill | The cart database pod is killed | gameday |
| F04-checkout-bad-config | A mesh config change cuts checkout off from payment | gameday |
| F05-node-preemption | A Spot node is preempted | manual (`gcloud`) |
| F06-recommendation-cpu-stress | Recommendation service starved of CPU | gameday (Chaos Mesh) |
