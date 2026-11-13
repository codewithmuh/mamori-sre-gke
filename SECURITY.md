# Security policy

## Supported versions

The project is pre-1.0. Only the latest commit on `main` is supported.

## Reporting a vulnerability

Please don't open a public issue for a security problem, such as a leaked credential, an over-broad IAM grant, a CI
injection path, or a way to abuse the public demo shop.

1. Report it privately through
   [GitHub private vulnerability reporting](https://github.com/codewithmuh/mamori-sre-gke/security/advisories/new)
   (preferred).
2. Or email [contact@codewithmuh.com](mailto:contact@codewithmuh.com) with the subject `[SECURITY] mamori-sre-gke`.

Include what you found, how to reproduce it, and the impact you expect. You'll get an acknowledgement within 48 hours
and a status update within 5 business days. Fixes are disclosed after they ship.

## How the repo keeps secrets out

- No long-lived credentials: CI reaches Google Cloud through Workload Identity Federation, and workloads use GKE
  Workload Identity.
- gitleaks runs in pre-commit and in CI on the full history; trivy scans for secrets and misconfigurations.
- `.gitignore` blocks `.env` files, keys, Terraform state and plans, and `*.tfvars` (only `*.tfvars.example` is
  committed).

See [docs/threat-model.md](docs/threat-model.md) for the threats this lab is designed against.
