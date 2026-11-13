Closes #

## What changed and why
<!-- The change in a few lines, and the reason for it. -->

## How it was tested
<!-- Commands and results. For anything that touches Google Cloud, give the `terraform plan` summary
     (CI posts it as a comment) or say it was validated offline only. -->

## Risks
<!-- What could break, the cost impact, and how to roll back. -->

## Checklist
- [ ] `make check` passes locally (same lint and tests as CI).
- [ ] No secrets, keys or credentials in the change (gitleaks runs in pre-commit and CI).
- [ ] Every new version is pinned (providers, modules, charts, images, actions with a full commit SHA).
- [ ] Terraform is formatted (`make fmt`) and Kubernetes manifests validate (`make lint`).
- [ ] Docs, runbooks and ADRs are updated where the change needs it.
