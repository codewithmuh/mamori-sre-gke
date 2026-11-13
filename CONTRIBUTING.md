# Contributing

This is a one-maintainer SRE lab (see [README.md](README.md)). Issues and suggestions are welcome. Small pull
requests (typos, broken links, clearer docs) are welcome too; anything larger starts with an issue so we can agree on
scope first. By taking part you agree to the [Code of Conduct](CODE_OF_CONDUCT.md). Report security problems
privately, as described in [SECURITY.md](SECURITY.md), never in a public issue.

## How work flows

Every change, including the maintainer's own, follows the same path.

1. **Plan first.** Anything bigger than a small fix gets a written plan (scope, design, risks) in the issue or in
   [docs/](docs/). A significant decision gets an ADR in [docs/adr/](docs/adr/).
2. **Issue first.** Open an issue with the problem, the scope and acceptance criteria. No work without an issue
   number.
3. **One branch per issue**, from an up-to-date `main`: `<type>/<issue#>-<short-slug>`, for example
   `feat/6-terraform-foundation` or `docs/4-project-plan`. Never commit to `main` directly.
4. **Conventional Commits that reference the issue**, for example `feat: add the spend-guard function (#7)`. Types:
   `feat`, `fix`, `docs`, `ci`, `refactor`, `test`, `chore`, `ops`.
5. **Run `make check` before pushing.** It runs the same lint and tests as CI.
6. **Open a pull request** that closes the issue (`Closes #7`) and says:
   - **What changed**, and why.
   - **How it was tested**: commands and results. For anything that touches Google Cloud, include the `terraform
     plan` summary or say that it was validated offline only.
   - **Risks**: what could break, cost impact, and how to roll back.

   A draft PR is fine while work is in progress.
7. **Code review.** Fix review findings on the same branch. CI must be green.
8. **Squash merge.** The maintainer merges, so each PR becomes one Conventional Commit on `main`, and deletes the
   branch.

## Rules for every change

- **No secrets in Git.** gitleaks runs in pre-commit and CI. Configuration comes from the environment or a gitignored
  `.env` (template: `.env.example`).
- **No long-lived credentials.** CI reaches Google Cloud only through Workload Identity Federation.
- **Pin every version:** Terraform providers and modules, Helm charts, container images, GitHub Actions (full commit
  SHA plus a `# vX.Y.Z` comment), Go modules.
- **Infrastructure changes only through Terraform**, and Kubernetes changes only through Git and Argo CD. A manual
  change during an incident is written down and then made in Git.
- **Plain, short English** in docs and comments. Comments explain why, not what.
- **Numbers say what they are:** mark each one as measured (with the date and source) or as an estimate.
