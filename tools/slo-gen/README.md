# slo-gen

Generates every SLO artefact of the lab from two inputs, so journey SLOs are written once and never by hand
(PLAN.md section 4, P1):

| Input | Owner | What it says |
|---|---|---|
| [`slo/slos.yaml`](../../slo/slos.yaml) | this tool's area (#18) | targets per journey, how each signal is measured, alert tiers |
| `slo/cuj-map.yaml` | [cuj-discovery](../cuj-discovery) (#17) | which services each journey depends on, found by fault injection. Until the first real run, `slo/cuj-map.example.yaml` |

| Output | Used by |
|---|---|
| `deploy/slo/` (`Rules`, `ClusterRules`, kustomization) | Argo CD Application `deploy/apps/slo.yaml`; Managed Service for Prometheus evaluates the recording rules |
| `infra/envs/lab/slo_generated.tf.json` | Terraform root `infra/envs/lab`: Cloud Monitoring services, SLOs, burn-rate and no-data alert policies, the "Mamori CUJ SLOs" dashboard |
| `infra/envs/datadog/slo_generated.tf.json` | Terraform root `infra/envs/datadog`: Datadog SLOs and burn-rate monitors (14-day window, M3) |
| `slo/generated/slo-catalog.md` | people, runbooks and the incident agent: every SLO, budget, alert and query per journey |

Why the SLIs and alerts look the way they do: [docs/slo/slo-design.md](../../docs/slo/slo-design.md).

## Use

From the repository root:

```bash
make slo-generate   # write the files that changed
make slo-check      # fail if any generated file is missing, stale or left over (CI runs this)
```

Both use `slo/cuj-map.yaml` when it exists and `slo/cuj-map.example.yaml` otherwise. Directly:

```bash
cd tools/slo-gen
go run . -root ../.. -cuj-map slo/cuj-map.example.yaml          # generate
go run . -root ../.. -cuj-map slo/cuj-map.example.yaml -check   # check only
```

| Flag | Default | Meaning |
|---|---|---|
| `-root` | `.` | repository root; the other paths are relative to it |
| `-slos` | `slo/slos.yaml` | the SLO spec |
| `-cuj-map` | `slo/cuj-map.yaml` | the CUJ map |
| `-check` | off | write nothing; exit 1 if anything would change |

Exit status: 0 done (or up to date), 1 out of date (`-check`), 2 bad flags or invalid input. Logs are JSON lines on
stderr (`severity`, `message`), one per file: `written`, `unchanged`, `removed`, or with `-check` `stale`, `missing`,
`extra`.

The tool owns `deploy/slo/` and `slo/generated/` entirely: a file there that it did not generate is removed (or, with
`-check`, reported). In `infra/envs/*` it only touches its own `slo_generated.tf.json`.

Input checks: the spec is decoded strictly (an unknown key is an error, so a typo can't fall back to a default) and
validated as a whole, with every problem listed. The CUJ map is decoded leniently, so cuj-discovery can add fields.
A journey in the spec that is missing from the map is an error; a journey only in the map is a warning.

Output is deterministic: same inputs, same bytes. Map keys are sorted, journeys keep the spec's order, services are
sorted, and nothing depends on the time of the run.

## Change an SLO

1. Edit `slo/slos.yaml` (target, threshold, alert tiers, signals per tier).
2. `make slo-generate`, review the diff of the generated files, commit all of it together.
3. Merge. Argo CD syncs `deploy/slo`; `terraform-apply` (by hand) applies the Cloud Monitoring part. The Datadog part
   is applied by hand during the Datadog window.

A target change is an error-budget-policy decision: record it in `docs/slo/error-budget-policy.md`.

## Develop and test

Go isn't needed on the host. From this directory:

```bash
GO_IMAGE=golang:1.27.3-trixie
LINT_IMAGE=golangci/golangci-lint:v2.15.0
docker run --rm -v "$PWD/../..":/repo -w /repo/tools/slo-gen $GO_IMAGE \
  sh -c 'go test -race -covermode=atomic -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1'
docker run --rm -v "$PWD":/src -w /src $LINT_IMAGE golangci-lint run ./...
```

The repository is mounted because one test checks that the committed `slo/slos.yaml` is valid (it skips outside
the repository).

Golden tests: `testdata/repo/slo/` holds a fixture spec and CUJ map (not a measurement), `testdata/golden/` the
expected output. After an intended change, rewrite the golden files and review their diff:

```bash
go test ./... -run TestGolden -update
```
