# Capacity report: run TBD (measured)

Template for the M4 capacity report (PLAN.md R2). Copy it to `docs/capacity/YYYY-MM-DD-capacity.md`,
paste the numbers from `loadtest-results/<run id>/report.md` (made by `make loadtest-report`), and
write the analysis yourself. Method and limits: [README.md](README.md). Lab measurement with synthetic
load on three Spot nodes: not a production figure.

## Summary

| Measure | Value |
|---|---|
| Highest request rate that held every SLO | TBD (measured) req/s |
| Planned rate of that step | TBD (measured) RPS |
| Step that ended the search (fail, invalid or not-run), and why | TBD (measured) |
| Inconclusive steps (too few requests to judge), if any | TBD (measured) |
| What limited it | TBD (measured) |
| Cluster cost (nodes and disks, list price) | TBD (measured) an hour, TBD (measured) a month |
| Cost per 1K RPS | TBD (measured) an hour, TBD (measured) a month |
| Cost per 1K RPS without the load generator's share (estimate) | TBD (measured) |
| Suggested ingress gateway rate limit (90 % of the capacity, for #27's `tokens_per_fill`) | TBD (measured) req/s |

One or two sentences: what the number means for this lab, and what would raise it.

## Setup

| | |
|---|---|
| Run id and date (UTC) | TBD (measured) |
| Time range | TBD (measured) to TBD (measured) |
| Repository commit | TBD (measured) |
| Cluster | `mamori`: 3 × e2-standard-2 Spot, asia-northeast1-a, GKE TBD (measured) |
| Istio, Online Boutique | Istio TBD (measured), Online Boutique v0.10.7 |
| Resilience settings in place (HPA, PDBs, retries, timeouts) | TBD (measured) |
| HPA replicas at the highest passing step (maximum each) | TBD (measured) |
| Target and gateway rate limit | internal port 8080, outside the rate limit; 429s seen: TBD (measured) |
| Steps and step duration | TBD (measured) RPS, TBD (measured) each, first 60 s not measured |
| Journey mix | TBD (measured) |
| Background traffic during the run | Locust TBD (measured) users, prober every minute; frontend total TBD (measured) req/s |
| k6 placement | node TBD (measured); shared with TBD (measured) |
| Error budget spent by the test | TBD (measured), per journey |

## SLOs applied (from `slo/slos.yaml` at that commit)

| Journey | Availability target | Latency target |
|---|---|---|
| browse | TBD (measured) | TBD (measured) under TBD (measured) ms |
| add-to-cart | TBD (measured) | TBD (measured) under TBD (measured) ms |
| view-cart | TBD (measured) | TBD (measured) under TBD (measured) ms |
| checkout | TBD (measured) | TBD (measured) under TBD (measured) ms |

## Steps

Each journey cell: availability · share of requests under the latency threshold · p95 latency.

| Step | Planned RPS | Measured req/s | Delivered | browse | add-to-cart | view-cart | checkout | k6 CPU | Node CPU | Verdict |
|---:|---:|---:|---:|---|---|---|---|---:|---:|---|
| 1 | TBD (measured) | TBD (measured) | TBD (measured) | TBD (measured) | TBD (measured) | TBD (measured) | TBD (measured) | TBD (measured) | TBD (measured) | TBD (measured) |
| 2 | TBD (measured) | TBD (measured) | TBD (measured) | TBD (measured) | TBD (measured) | TBD (measured) | TBD (measured) | TBD (measured) | TBD (measured) | TBD (measured) |
| … | | | | | | | | | | |

Warnings from the report: TBD (measured).

## What limited it

Evidence for the bottleneck at the first failing step, from the dashboards and Managed Prometheus:

| Signal | At the highest passing step | At the first failing step |
|---|---|---|
| Node CPU utilisation (average, busiest node) | TBD (measured) | TBD (measured) |
| Frontend CPU (cores) and its limit | TBD (measured) | TBD (measured) |
| Ingress gateway CPU (cores) | TBD (measured) | TBD (measured) |
| Slowest upstream service (mesh p95) | TBD (measured) | TBD (measured) |
| Upstream errors (mesh, by service) | TBD (measured) | TBD (measured) |
| k6 CPU (cores) | TBD (measured) | TBD (measured) |
| Pods throttled, restarted or `Pending` | TBD (measured) | TBD (measured) |

Conclusion: TBD (measured).

## Cost

| | |
|---|---|
| Prices used | Spot e2-standard-2 TBD (measured)/hour, pd-standard TBD (measured)/GB-month, Tokyo list prices of TBD (measured) |
| Cluster cost | TBD (measured)/hour |
| Capacity | TBD (measured) RPS |
| Cost per 1K RPS | TBD (measured)/hour, TBD (measured)/month |
| Load generator's share of the allocatable CPU | TBD (measured) |
| Cost per 1K RPS without it (estimate) | TBD (measured) |

The figure assumes cost grows in proportion to load and leaves out the load balancer, Cloud Armor, NAT
and monitoring. See [README.md](README.md#cost-per-1k-rps).

## Before and after (M4)

If the run is repeated after a change (HPA, more gateway replicas, fail-open dependencies), compare:

| Run | Change | Capacity (req/s) | Cost per 1K RPS | First SLO to break |
|---|---|---|---|---|
| TBD (measured) | baseline | TBD (measured) | TBD (measured) | TBD (measured) |
| TBD (measured) | TBD (measured) | TBD (measured) | TBD (measured) | TBD (measured) |

## Caveats

- Lab with synthetic load on Spot nodes in one zone; not a production figure.
- The path leaves out the external load balancer, TLS and Cloud Armor (README, "Where the load comes
  from").
- k6 shared the nodes with the shop (README, "The load generator shares the nodes").
- TBD (measured): anything unusual in this run (preemptions, upgrades, alerts, retries).

## Follow-ups

| Follow-up | Issue |
|---|---|
| TBD (measured) | TBD (measured) |

## Raw data

- `loadtest-results/<run id>/` (not in Git): `k6.log`, `summary.json`, `report.md`, `report.json`, `run.env`.
- Managed Prometheus: `k6_mamori_cuj_*_total{run="<run id>"}` over the run's time range.
