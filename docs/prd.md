# Product requirements: mamori-sre-gke

Status: draft, M0 (2026-11-13). Nothing runs yet. Every target here is a starting point, and every number is an
estimate until it is measured.

## What the product is

The product is not the shop. [Online Boutique](https://github.com/GoogleCloudPlatform/microservices-demo) v0.10.7 is
the given workload. The product is the *reliability* of that shop: journeys that keep working, alerts that mean user
pain, a practiced incident response, and an incident agent that helps without being able to do harm. See
[PLAN.md](../PLAN.md) sections 1–2 for why.

## Who it serves

| Who | What they need from the project |
|---|---|
| **The on-call SRE** running the shop (one person, the maintainer) | Alerts that fire only when a journey's error budget is at risk; for each alert, which journey is burning and how fast; a runbook; dashboards; later, a triage and root-cause summary from the agent with one proposed action to approve or reject |
| **Shoppers**, whose journeys define reliability | Browse, add to cart and check out without errors or long waits. In this lab they are the Locust load generator, the prober and the odd visitor to the public demo; no real payments, no accounts |
| **Reviewers** reading the repo | Claims they can check: every number marked measured or estimated, linked to its source (dashboards, postmortems, evaluation results); decisions with their reasons ([ADRs](adr/README.md)); a way to rebuild it all (`make up`) |

## Critical user journeys (CUJs)

[PLAN.md](../PLAN.md) section 4 fixes the first three and allows a fourth; the prober (#16) runs all four. The
frontend paths are Online Boutique's own.

| CUJ | What the shopper does | Frontend requests |
|---|---|---|
| **CUJ-1 Browse** | Opens the home page, then a product page | `GET /`, `GET /product/{id}` |
| **CUJ-2 Add to cart** | Adds a product to the cart | `POST /cart` (redirects to the cart page) |
| **CUJ-3 Checkout** | Places an order with the demo's pre-filled fake card | `POST /cart/checkout` |
| **CUJ-4 View cart** | Looks at the cart, with shipping cost and recommendations | `GET /cart` |

Which backend services each journey truly depends on is **not** written down by hand. The M2 discovery run (D4,
`tools/cuj-discovery`) breaks one upstream service at a time (an Istio abort; `redis-cart`, which is plain TCP, is
scaled to zero) and runs every journey through the prober's on-demand API; the services whose failure breaks a
journey are its *critical* dependencies. The result is a generated `slo/cuj-map.yaml`
([method](slo/cuj-discovery.md)). A dependency that turns out to be critical but shouldn't be becomes hardening work
in M4.

### Starting SLO targets (provisional)

**Provisional until the M2 fault-injection discovery** and the first weeks of data. They are guesses made before any
measurement, chosen so a lab on Spot VMs in one zone can meet them most weeks. M2 resets each one from the measured
baseline and records the change in the error-budget policy.

| CUJ | Availability target | Latency target (starting threshold) | Window |
|---|---|---|---|
| CUJ-1 Browse | 99.5 % | 95 % of requests under 500 ms | rolling 28 days |
| CUJ-2 Add to cart | 99.5 % | 95 % of requests under 500 ms | rolling 28 days |
| CUJ-3 Checkout | 99.0 % | 95 % of requests under 1,000 ms | rolling 28 days |
| CUJ-4 View cart | 99.5 % | 95 % of requests under 500 ms | rolling 28 days |

- **Why checkout starts lower:** checkout calls the most services, and under D4 a journey's availability is the
  *product* of its critical APIs' success rates. The same per-service quality therefore gives checkout a lower
  number. The lower start reflects that; it does not mean checkout matters less.
- **Error budget, in prober minutes (my math):** a 28-day window has 40,320 one-minute probes. 99.5 % allows about
  202 failed minutes (about 3.4 hours); 99.0 % allows about 403 (about 6.7 hours).
- **First full window (my math):** if the operated period starts on the M2 target date, 2026-12-05, the first full
  28-day window closes around 2027-01-02.

### How each journey is measured

Two independent signals, so that one can catch what the other misses.

1. **Request-based SLIs from mesh metrics.** Every request between services passes an Istio (Envoy) sidecar, which
   records its result and latency (`istio_requests_total`, `istio_request_duration_milliseconds`). Managed Service
   for Prometheus collects them. Per D4, for each journey:
   - *Availability* = the product, over the journey's critical APIs, of good requests ÷ all requests.
   - *Latency* = the lowest share, among the critical APIs, of requests faster than that API's threshold.
   - *Good* starts as: HTTP status below 500, gRPC status `OK`. M2 refines this (for example, whether `NOT_FOUND`
     counts as good).
   - The thresholds in the table above apply to the frontend request; M2 sets per-API thresholds from measured
     latency.
2. **A Go prober, every minute.** It runs each journey end to end against the public hostname, so it also covers DNS,
   the TLS certificate, the load balancer and Cloud Armor, which the mesh never sees. It records success and step
   latency per journey. Because it runs whether or not anyone shops, it also gives each journey a steady floor of
   traffic. Where it runs, and how the alerting notices when the prober itself stops reporting, are designed in M2.

During the 14-day Datadog window (M3), Datadog synthetics add a third view from outside Google Cloud.

## Error-budget policy (outline)

M2 writes the full policy. The outline:

- **Budget:** 1 − target, per journey, over the rolling 28 days.
- **Alerts:** burn-rate alerts on each journey's budget. A fast burn of a prober SLO pages the on-call phone; mesh
  SLOs and slow burns notify without paging, so junk public traffic can't page anyone. Google's suggested starting
  points are about 10× burn over 1–2 hours (fast) and about 2× over 24 hours (slow)
  ([research](cost-and-tooling-research.md)); M2 sets the exact windows.
- **Budget left:** normal work. Changes and game days go ahead.
- **Budget spent for a journey:** no game days and no risky changes that touch that journey until its budget is back
  above zero. Postmortem follow-ups come first. Exceptions: security fixes, cost fixes, and changes that stop the
  burn.
- **Big incidents:** any single incident that uses more than 20 % of a journey's 28-day budget gets a postmortem (a
  rule from the [example policy](https://sre.google/workbook/error-budget-policy/) in Google's SRE Workbook).
- **Game days count.** They spend budget on purpose, so they are scheduled when the budget allows, and their burn is
  counted, not excluded: shoppers would have felt it. The postmortem says how much burn was planned.
- **The spend guard counts too.** If the budget alarm scales the node pool to zero, the shop is down on purpose. That
  downtime burns the error budget, and the postmortem names it as a cost decision.
- **Every use of the policy is logged**, so the case study can show it was actually invoked, not only written.
- **Who decides:** the on-call SRE, who is also the service owner in this one-person lab.

## Non-goals

From [PLAN.md](../PLAN.md) section 2, plus what follows from it:

- **Writing a product.** No shop features. The shop changes only for reliability (for example, letting non-critical
  dependencies fail open in M4).
- **Multi-region and disaster recovery.** One zone, one cluster. It would double the cost.
- **Spanner or AlloyDB.** Too expensive for a lab; Cloud SQL or Memorystore only if a decision needs one.
- **A 10K QPS claim.** The capacity test reports what the cluster actually holds, and the cost to scale it.
- **Real payments, real accounts, real customer data.**
- **Production claims.** Every number comes from a lab with synthetic load and says so.
- **A staffed 24/7 rotation.** There is one on-call person. Time to acknowledge a page, day or night, is measured,
  not promised.

## Success measures

None of these exist yet. Each is reported as measured, with the date range it covers.

| Measure | What is reported | When |
|---|---|---|
| Weeks operated | Weeks with the cluster always on, steady load and SLO data accumulating; about 7 weeks is planned (estimate) | From M2 |
| SLO results | Per journey and window: SLI, budget spent, burn-rate alerts fired | From M2 |
| Rebuild and teardown | `make up` from nothing works; `make down` leaves nothing billable (leftover check passes) | M1 |
| Game days | 5–6 run; a blameless postmortem for each, built from real telemetry; follow-ups closed | M3 |
| Alert quality | Alerts and pages per week; share that were actionable, before and after tuning; pages per month against PagerDuty's free limit of 100 | M3 |
| Agent evaluation (`EVALS.md`) | On 20–30 injected incidents: detection time; top-1 and top-3 root-cause accuracy; time to root cause against the human baseline from the M3 game days; correct-action rate; unsafe actions attempted and blocked (unsafe actions *executed* must be zero); cost and time per investigation | M5 |
| Capacity | Highest request rate that still meets the SLOs; cost per 1K requests per second | M4 |
| Cost | Cost per day, and total spend against the estimate of about $280–360 for the whole plan ([PLAN.md](../PLAN.md) section 6) | From M1 |
