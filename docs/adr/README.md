# Architecture decision records

One file per significant decision: hard to reverse, or affecting more than one component
([0001](0001-record-architecture-decisions.md)).

## How ADRs work here

1. Copy [0000-template.md](0000-template.md) to `NNNN-short-title.md`, using the next free number.
2. The maintainer writes every ADR from scratch. The input is [../decision-facts.md](../decision-facts.md): the
   question, the options, sourced facts and open questions for each decision. It holds no decisions.
3. A new ADR starts as `proposed` and moves to `accepted` when the maintainer accepts it. An ADR that is replaced stays
   in place with status `superseded by NNNN` and a link to the new one.
4. Update the table below in the same pull request.

## Index

ADRs 0002–0014 cover decisions D1–D13 in [PLAN.md](../../PLAN.md) section 3.

| ADR | Topic | Status |
|---|---|---|
| [0000](0000-template.md) | Template | template |
| [0001](0001-record-architecture-decisions.md) | Record architecture decisions | accepted |
| 0002 | D1 — workload | to write |
| 0003 | D2 — cluster type | to write |
| 0004 | D3 — telemetry | to write |
| 0005 | D4 — CUJ SLI method | to write |
| 0006 | D5 — delivery | to write |
| 0007 | D6 — paging and incident channel | to write |
| 0008 | D7 — chaos | to write |
| 0009 | D8 — agent model and auth | to write |
| 0010 | D9 — agent action safety | to write |
| 0011 | D10 — policy engine | to write |
| 0012 | D11 — frontend exposure | to write |
| 0013 | D12 — spend guard | to write |
| 0014 | D13 — where the agent code lives | to write |
