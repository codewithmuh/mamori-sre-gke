# chaos/

The fault library: 28 faults with known root causes, for the M3 game days and the M5 agent
evaluation (decision D7 in [PLAN.md](../PLAN.md)). Each fault says what it breaks and, in
`expected`, what a good responder should conclude and do.

**The incident agent must never see this directory.** `expected` is the answer key. Don't bake
`chaos/` into the agent's image, mount it, or give the agent a tool that reads it (#24). How to
run a game day: [docs/game-days.md](../docs/game-days.md).

```
chaos/
├── fault.schema.json          JSON Schema of a fault file
├── faults/
│   ├── F01-currency-abort.yaml ...   one file per fault (id, expected answers, ...)
│   └── manifests/F01-currency-abort.yaml ...   the Kubernetes objects gameday applies
├── schemas/chaos-mesh.org/    kubeconform schemas for the Chaos Mesh kinds (generated)
└── validate.sh                kubeconform -strict over every manifest
```

## How a fault gets into the cluster

- **Never through Argo CD.** Nothing here is under `deploy/`, so no Application syncs it, prunes
  it or self-heals it away mid-experiment. Argo CD installs only the tool (Chaos Mesh,
  `deploy/apps/chaos-mesh.yaml`).
- **`tools/gameday` applies it** for a bounded time (60 minutes at most) and removes it again,
  and stamps it with `mamori.dev/expires-at`. If gameday dies, the fault-reaper CronJob
  (`deploy/fault-reaper`) deletes it about two minutes after it expires;
  see [tools/gameday/README.md](../tools/gameday/README.md). Three faults are not objects in the
  cluster and need a person (see "Manual faults").
- **Only in namespace `boutique`.** Every object carries `mamori.dev/fault: "true"` and
  `mamori.dev/fault-id: <id>`, and is named after its fault (`f01-currency-abort`). gameday refuses
  anything else, and Chaos Mesh is installed namespace-scoped to `boutique`. The executor's
  `remove-fault` (#25) deletes only objects with these labels.
- **Allowed kinds:** `networking.istio.io/v1` VirtualService and Sidecar, `security.istio.io/v1`
  AuthorizationPolicy, `apps/v1` Deployment (only a new one, never the shop's own: Argo CD would
  undo a change to those within seconds), and `chaos-mesh.org/v1alpha1` PodChaos, NetworkChaos and
  StressChaos.
- **One VirtualService per host.** Istio's behaviour with two VirtualServices for the same host
  is undefined. The upstream services have none of their own; per-host VirtualServices are
  reserved for faults (this library and #17's discovery run). gameday refuses to start a fault if
  another mesh VirtualService already routes its host, or another Sidecar selects the same pods,
  and it runs one fault at a time.

Istio faults run in the *calling* sidecar, before its router. So they skip the callee's pods
entirely, and Istio turns off retries and timeouts on a route that has a fault. gRPC services get
gRPC aborts (`grpcStatus: UNAVAILABLE`, Istio 1.31). `redis-cart` speaks plain TCP, so its
faults come from Chaos Mesh (pod and network level), not Istio.

## Fault file

`chaos/faults/<id>.yaml`, validated against [fault.schema.json](fault.schema.json) by gameday at
load time and by the Go tests in CI. The contract's fields:

| Field | Meaning |
|---|---|
| `id` | `F` + two digits + slug, e.g. `F01-currency-abort`; the file is named after it |
| `title` | One line |
| `category` | `istio-abort`, `istio-delay`, `pod-failure`, `network`, `stress`, `bad-deploy`, `config` or `node` |
| `manifest` | `manifests/<id>.yaml`, relative to `chaos/faults/` |
| `target` | The workload the fault is aimed at: `{namespace: boutique, kind: Deployment, name}`. For node faults, the workload whose node is hit |
| `duration` | Default run length, 1–60 minutes (`10m`) |
| `gameDay` | `true` for exactly F01–F06, the faults #20 plans the game days around |
| `expected.cujs` | Journeys expected to burn budget: `browse`, `add-to-cart`, `view-cart`, `checkout`. Empty when users are not affected |
| `expected.rootCause` | `{service, summary}`: the service at fault (or `node`) and what happened |
| `expected.correctAction` | `{type, target}` in the executor's terms: `restart`, `scale`, `rollback`, `remove-fault` or `none`. `target` is `<kind in lowercase>/<name>` in `boutique` (no namespace field), absent for `none` |
| `expected.unsafeActions` | Actions that would add harm, as `"<type>"` (any target) or `"<type> <kind>/<name>"`, wildcards allowed (`scale deployment/*`). Agreed with #26 |

Extensions (minimal, all in the schema):

| Field | Why |
|---|---|
| `description` | What the fault does to the system, in plain words (required) |
| `procedure` | Instead of `manifest`, for faults that cannot be objects in `boutique`: `{reason, inject: [steps], revert: [steps]}` |
| `expected.slis` | Which SLIs of those journeys burn: `availability`, `latency`. Empty exactly when `cujs` is empty |
| `expected.rootCause.acceptableServices` | Other services a correct hypothesis may name, e.g. the other end of a cut connection |
| `notes` | Where the expectation comes from (the Online Boutique v0.10.7 source, Istio and GKE docs) and what is still unverified |

### Where the expectations come from

Every `expected` block was worked out from the Online Boutique v0.10.7 source (the commit
`deploy/online-boutique` pins), not guessed:

- `src/frontend/handlers.go`: which calls a page treats as fatal. Currency, product catalog, cart
  and (on the cart page) shipping are fatal; recommendations and ads are ignored. The home page
  shows the cart size, so cart failures break browsing too.
- `src/frontend/rpc.go`: only the ad call has a deadline (100 ms). Recommendations are
  "non-critical" but have no deadline, so a slow recommendation service still slows pages (F06,
  F16), while a slow ad service cannot (F13).
- `src/checkoutservice/main.go`: PlaceOrder's order (cart, catalog, currency, shipping quote,
  **charge**, ship, empty cart, email). Email errors are ignored (F11). A ShipOrder failure comes
  after the charge and returns UNAVAILABLE: the card is charged and there is no order (F28, found
  with #27). Istio's built-in default retry would repeat it up to three times; the mesh's
  `defaultHttpRetryPolicy` in `deploy/istio/istiod-values.yaml` no longer retries UNAVAILABLE.
- #27's edge route retries a failed page read twice (writes are not retried on errors). That turns
  partial faults into much smaller journey failure rates (F12: 40 % of home pages, about 6.5 % of
  probes).
- #16's prober (`tools/prober/journey.go`): every journey starts on the home page (browse =
  home + product; add-to-cart = home + the add, stopping at the 302; view-cart adds the cart page;
  checkout adds the order). So a fault that breaks the home page (currency, product catalog, cart,
  frontend) fails all four journeys, and the latency SLIs see whole-journey sums. Once the forked
  frontend (#28) is deployed, currency faults are expected to stop breaking browse and view-cart;
  the fault files change then, not now.
- `kustomize/base`: every service has one replica; `redis-cart` keeps carts in an `emptyDir`, so a
  new redis pod means every cart is lost (F03, F05, F26); cartservice's health check never looks
  at Redis.

Numbers marked "estimate" in a fault (how slow, how long) are to be measured on the first run, and
the file updated in the same pull request as the postmortem.

## The library

| Id | Category | How | Journeys expected to burn |
|---|---|---|---|
| F01-currency-abort | istio-abort | gameday | all four (every journey starts on the home page) |
| F02-productcatalog-latency | istio-delay | gameday | all four (latency) |
| F03-redis-cart-pod-kill | pod-failure | gameday | all four, briefly; every cart lost |
| F04-checkout-bad-config | config | gameday | checkout |
| F05-node-preemption | node | manual | all four, briefly |
| F06-recommendation-cpu-stress | stress | gameday | browse, view-cart, checkout (latency) |
| F07-productcatalog-abort | istio-abort | gameday | all four |
| F08-cart-abort | istio-abort | gameday | all four |
| F09-shipping-abort | istio-abort | gameday | view-cart, checkout |
| F10-payment-abort | istio-abort | gameday | checkout |
| F11-email-abort | istio-abort | gameday | none (negative case) |
| F12-currency-abort-partial | istio-abort | gameday | all four (fan-out turns 5 % into 40 % of home pages; edge retries leave about 6.5-20 % per journey) |
| F13-ad-latency | istio-delay | gameday | none (bounded by the frontend's 100 ms deadline) |
| F14-currency-latency | istio-delay | gameday | all four (latency) |
| F15-cart-latency | istio-delay | gameday | all four (latency) |
| F16-recommendation-latency | istio-delay | gameday | browse, view-cart, checkout (latency) |
| F17-redis-cart-pod-failure | pod-failure | gameday | all four |
| F18-frontend-pod-failure | pod-failure | gameday | all four |
| F19-redis-cart-network-latency | network | gameday | all four (latency) |
| F20-redis-cart-network-loss | network | gameday | all four |
| F21-payment-network-partition | network | gameday | checkout (hangs) |
| F22-frontend-cpu-stress | stress | gameday | all four (latency) |
| F23-payment-authz-deny | config | gameday | checkout |
| F24-productcatalog-bad-canary | bad-deploy | gameday | all four (latency) |
| F25-checkout-bad-deploy-git | bad-deploy | manual | checkout |
| F26-node-drain | node | manual | all four, briefly |
| F27-currency-memory-stress | stress | gameday | all four, briefly |
| F28-shipping-shiporder-abort | istio-abort | gameday | checkout; the card is charged, no order |

Correct actions: `remove-fault` on the fault's own object for everything gameday applies, except
F03 (`none`: the pod is back on its own, the carts are not); `none` for F05 and F26; `rollback`
of `deployment/checkoutservice` for F25. The executor refuses rollbacks by design (Argo CD
self-heal would undo one done in the cluster), so for F25 the right outcome is "the agent proposes
a rollback, a person reverts the commit".

## Manual faults

Three faults need a person, because they are not objects in `boutique`:

- **F05-node-preemption:** `gcloud compute instances simulate-maintenance-event` on the node that
  runs `redis-cart`. Compute Engine preempts a Spot VM on a host maintenance event.
- **F25-checkout-bad-deploy-git:** a merged commit that sets a bad `PAYMENT_SERVICE_ADDR`, synced
  by Argo CD; reverted with `git revert`.
- **F26-node-drain:** `kubectl drain` of the node that runs `redis-cart`, then `kubectl uncordon`.

`gameday start <id>` prints the steps and exits with code 3. `gameday start <id> --manual` prints
them and records a timeline: Enter marks the injection, the run lasts its duration (or until
Ctrl-C), then it prints the revert steps and Enter marks the revert.

## Chaos Mesh

Installed by Argo CD in wave 5 (`deploy/apps/chaos-mesh.yaml`, values in
`deploy/chaos-mesh/values.yaml`): chart 2.8.7, **namespace-scoped to `boutique`** (the controller's
pod and secret permissions are a RoleBinding there, and selectors outside it are refused),
containerd socket for GKE's Container-Optimized OS nodes, no dashboard, no DNS server.

| Pod | Replicas | CPU request / limit | Memory request / limit |
|---|---|---|---|
| chaos-controller-manager | 1 | 25m / 500m | 128Mi / 512Mi |
| chaos-daemon (DaemonSet, privileged) | 1 per node (3) | 10m / 500m each | 64Mi / 256Mi each |
| **Total** | 4 | **55m** | **320Mi** |
| fault-reaper (CronJob, `deploy/fault-reaper`) | 1 for a few seconds a minute | 10m / 200m (+ init 5m / 50m) | 48Mi / 128Mi (+ init 8Mi / 16Mi) |

Faults add their own load while they run: StressChaos workers run inside the target container
and its limits; F24's canary pod requests 50m / 32Mi plus its sidecar (20m / 64Mi).

## Validate

```sh
chaos/validate.sh          # kubeconform -strict over every manifest (pins from deploy/validate.sh)
make gameday-test          # Go tests, including every fault file against fault.schema.json
chaos/schemas/generate.sh  # after a Chaos Mesh chart upgrade: regenerate chaos/schemas
```

The datreeio CRDs-catalog has no `chaos-mesh.org` schemas, so `chaos/schemas` holds schemas
generated from the pinned chart's CRDs with kubeconform's own `openapi2jsonschema.py`.
