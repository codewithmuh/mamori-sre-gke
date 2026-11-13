# evidence

Assembles the weekly compliance evidence bundle: reads the raw inputs that
[`.github/workflows/evidence.yml`](../../.github/workflows/evidence.yml) collects and writes `bundle.json` (fixed
schema `mamori.evidence/v1`) and `summary.md`. The bundle, its schema and how to check it are described in
[docs/compliance/evidence-bundle.md](../../docs/compliance/evidence-bundle.md).

It reads files only and makes no network calls. It uses the standard library only, so the tool that summarises the
repository's supply chain adds no dependencies of its own.

## Usage

```bash
evidence -inputs bundle/inputs -out bundle -expect ci,github,trivy,gitleaks,sbom
```

| Flag | Default | Meaning |
|---|---|---|
| `-inputs` | required | directory with the input files below |
| `-out` | required | where `bundle.json` and `summary.md` go (created if missing) |
| `-expect` | none | section keys this run should collect, comma-separated, or `all`; an expected section that is missing needs a look. Keys: `ci`, `github`, `trivy`, `gitleaks`, `sbom`, `terraformDrift`, `cluster`, `gatekeeper`, `argocd`, `slos` |
| `-now` | current time | generation time, RFC 3339, for reproducible output |
| `-repository`, `-commit`, `-ref`, `-workflow`, `-event`, `-run-id`, `-run-attempt`, `-run-url` | GitHub Actions' `GITHUB_*` variables | where the bundle came from |

Exit codes: `0` written, `1` the inputs directory can't be opened or the output can't be written, `2` bad flags.
A missing or invalid input never fails the command; it becomes a `"not collected"` section with the reason.

## Inputs

Fixed file names. For an input it could not collect, the workflow writes `<name>.reason` instead: one line saying
why. Without either, the reason is "no input file and no reason recorded".

| File | Written by | Section |
|---|---|---|
| `ci-runs.json` | `gh api` (workflow runs on `main`) | `ci` |
| `github-settings.json` | read-only REST calls: `{"adminToken": bool, "responses": {"<name>": {"status": <HTTP>, "reason": "...", "body": <JSON>}}}` for `environment`, `environmentBranchPolicies`, `branch`, `branchRules`, `rulesets`, `branchProtection`, `workflowPermissions`, `actionsPermissions`; status 0 means not requested | `github` |
| `trivy.json` | Trivy `--format json` | `trivy` |
| `gitleaks.json` | gitleaks `--redact`, reduced with jq | `gitleaks` |
| `sbom.spdx.json` | syft `-o spdx-json` | `sbom` |
| `terraform-drift.json` | `terraform show -json` of a `plan -detailed-exitcode`, reduced on the runner with [`drift.jq`](drift.jq) to addresses, actions and attribute names | `terraformDrift` |
| `gke-cluster.json` | GKE API `clusters.get`, reduced with jq | `cluster` |
| `nodes.json` | `kubectl get nodes -o json` | `cluster` |
| `gatekeeper-constraints.json` | `kubectl get constraints -o json` | `gatekeeper` |
| `argocd-applications.json` | `kubectl get applications.argoproj.io -A -o json` | `argocd` |
| `slo-status.json` | Cloud Monitoring API (services, SLOs, `select_slo_*`), built with jq | `slos` |

Samples of every input are in [`testdata/inputs`](testdata/inputs); the bundle they produce is in
[`testdata/golden`](testdata/golden). `testdata/inputs/terraform-drift.json` is `drift.jq` applied to
[`testdata/plan/sample-plan.json`](testdata/plan/sample-plan.json); where `jq` is installed (GitHub's runners), a test
checks that, and that no attribute value from the plan survives the reduction:

```bash
jq --argjson code 2 -f drift.jq testdata/plan/sample-plan.json   # regenerate the sample input
```

## Develop and test

Go isn't needed on the host. From this directory:

```bash
GO_IMAGE=golang:1.27.3-trixie

# Tests with the race detector and coverage (CI fails below 80 %)
docker run --rm -v "$PWD":/src -w /src $GO_IMAGE \
  sh -c 'go test -race -covermode=atomic -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1'

# Lint, as CI does
docker run --rm -v "$PWD":/src -w /src golangci/golangci-lint:v2.15.0 golangci-lint run ./...

# Try it on the samples
docker run --rm -v "$PWD":/src -w /src $GO_IMAGE go run . -inputs testdata/inputs -out /tmp/bundle

# After a deliberate change to the schema or the summary, rewrite the golden files and review the diff
docker run --rm -v "$PWD":/src -w /src $GO_IMAGE go test -run TestGolden -update ./...
```

A change to `bundle.json` that renames, removes or changes the meaning of a field needs a new `schemaVersion` and an
update to the schema table in the docs. Adding a field doesn't.
