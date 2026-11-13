# Container images

How the lab's own images are built, checked, tagged, signed and published, and how to check a
signature. The workflow is [`.github/workflows/images.yml`](../.github/workflows/images.yml).

Nothing here has run against a real project yet (the project is created on 2026-11-14). Commands
that need the registry are written from the tools' documentation and are unverified until the
first publish.

## What gets built

Every directory with a `Dockerfile` in one of these places is an image, named after the directory:

| Dockerfile | Image name | Example |
|---|---|---|
| `tools/<name>/Dockerfile` | `<name>` | `tools/prober` → `prober` |
| `services/<name>/Dockerfile` | `<name>` | `services/frontend` → `frontend` (the [fork](../services/frontend/CHANGES.md)) |
| `agent/Dockerfile` | `agent` | |

A new directory needs no workflow change. Two directories with the same name (say `tools/x` and
`services/x`) fail the run, because they would share one registry path. Images are built for
`linux/amd64`, the architecture of the cluster's `e2-standard-2` nodes.

## When it runs

| Trigger | What happens |
|---|---|
| Pull request that touches `tools/`, `services/`, `agent/` or the workflow | For each image: Go tests with the race detector if it is a `services/` Go module (`tools/` modules are tested by `ci.yml`), `docker build`, Trivy scan of the image. Nothing is pushed and the job has no cloud credentials. |
| Push to `main` touching the same paths | The same checks, then push, sign and SBOM (below). Skipped, with only the checks run, while the repository variable `GCP_WIF_PROVIDER` is unset. |
| Release (release-please merges its release PR) | `release.yml` calls this workflow with the version (requested in #28); it builds tag `v<version>` and publishes it as `:<version>` as well. |
| By hand: Actions → images → Run workflow, from `main` | With a version: publish that release again (after `make up`, see below). Without: publish `main` as `:<git sha>`. |

The Trivy scan fails the job on any HIGH or CRITICAL finding that has a fix (vulnerabilities and
secrets in the image). On `main` that stops the publish, so a vulnerable image never reaches the
registry. Fix it by updating the base image or the dependency; if there is no fix, the finding
does not fail the job.

## Tags

Images go to `asia-northeast1-docker.pkg.dev/<project>/mamori/<name>`:

| Tag | Set by | Moves? |
|---|---|---|
| `<git sha>` (40 hex characters) | every publish; for a release, the tagged commit | only when the same commit is published again (a re-run) |
| `<X.Y.Z>` | release publishes only | **never**: the job fails if the tag already exists |

`deploy/` refers to version tags, for example
`asia-northeast1-docker.pkg.dev/PROJECT_ID/mamori/frontend:0.1.0`. A new image reaches the
cluster through a release and a version bump in `deploy/`, reviewed like any change, and Argo CD
syncs it. There is no `latest`. Manifests use tags rather than digests because the registry is
rebuilt with every `make up` (below): a digest written into Git would stop existing, while a version
tag is rebuilt from the same Git tag.

`PROJECT_ID` in those paths is a placeholder, because the project ID is not known when manifests
are written. Once the project exists, point them at it and commit:

```sh
scripts/set-registry.sh mamori-lab-123456          # dry run: shows every line it would change
scripts/set-registry.sh --apply mamori-lab-123456  # rewrites the files under deploy/
scripts/set-registry.sh --apply --from mamori-lab-123456 mamori-lab-654321  # moving to a new project
```

It only touches the registry path `asia-northeast1-docker.pkg.dev/PROJECT_ID/mamori/`, never a
`PROJECT_ID` used for anything else. `PROJECT_ID` from `.env` is used when no argument is given.

## Who may push

- The publish job authenticates through Workload Identity Federation (no keys) as the service
  account `ci-images`, created in [`infra/bootstrap/ci_images.tf`](../infra/bootstrap/ci_images.tf).
- Only jobs on the `main` branch of this repository can use it: the account trusts exactly the
  OIDC subject `<subject prefix>:ref:refs/heads/main`. Pull requests, other branches, tags and
  jobs in a GitHub environment get a different subject.
- `ci-images` holds Artifact Registry Writer on the repository `mamori` only, granted in
  `infra/envs/lab/iam.tf` next to the repository. Artifact Registry doesn't support IAM
  conditions on resource names, so a project-level grant could not be narrowed to one
  repository; `ci_images.tf` explains the options.
- Pull request jobs never get an OIDC token (`id-token: write` is only on the publish job), and
  images are built and scanned before any credentials exist on the runner.
- Residual risk: any workflow file on `main` that asks for `id-token: write` could act as
  `ci-images`. Workflow changes reach `main` only through a reviewed pull request (CODEOWNERS).

Repository variables it needs (from `make bootstrap`): `GCP_PROJECT_ID`, `GCP_WIF_PROVIDER`.
Optional `GCP_CI_IMAGES_SA` overrides the account email, which otherwise is
`ci-images@<project>.iam.gserviceaccount.com`.

## Signing and the SBOM

For each pushed image the job:

1. **Signs the digest** with `cosign sign` (cosign v3.1.4), keyless: Sigstore's Fulcio issues a
   short-lived certificate for this workflow's GitHub identity, and the signature is recorded in
   the public Rekor transparency log. No signing key exists anywhere. Signing the digest covers
   every tag that points at it.
2. **Generates an SBOM** (SPDX JSON) of the pushed image with Trivy.
3. **Attaches the SBOM as a signed attestation** (`cosign attest --type spdxjson`), so the SBOM is
   tied to the image and to the same identity.

cosign v3 stores the signature and the attestation as OCI 1.1 referrers of the image. Artifact
Registry keeps them as attachments: its cleanup policies don't apply to attachments directly, and
attachments are deleted when their image is. The registry's cleanup policy
(`infra/envs/lab/registry.tf`) therefore can't leave an image without its signature.

The Rekor entry is public and names the image, including the project ID. That ID is already public
in this repository once `scripts/set-registry.sh` has run.

## Verify a signature

You need [cosign](https://github.com/sigstore/cosign) v3 and read access to the registry, for
example `gcloud auth configure-docker asia-northeast1-docker.pkg.dev`. Always verify a digest; the
job summary of each publish run prints it.

```sh
IMAGE=asia-northeast1-docker.pkg.dev/<project>/mamori/frontend
# The digest behind a tag (or take it from the job summary of the publish run):
DIGEST=$(docker buildx imagetools inspect "$IMAGE:0.1.0" --format '{{json .Manifest}}' | jq -r .digest)

cosign verify "$IMAGE@$DIGEST" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity https://github.com/codewithmuh/mamori-sre-gke/.github/workflows/images.yml@refs/heads/main \
  --certificate-github-workflow-repository codewithmuh/mamori-sre-gke \
  --certificate-github-workflow-ref refs/heads/main
```

This passes only for an image that `images.yml` signed while running on `main` of this repository.
A signature made anywhere else (a fork, a pull request, another workflow) fails the identity check.

The SBOM:

```sh
cosign verify-attestation "$IMAGE@$DIGEST" --type spdxjson \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity https://github.com/codewithmuh/mamori-sre-gke/.github/workflows/images.yml@refs/heads/main \
  | jq -r .payload | base64 -d | jq .predicate > sbom.spdx.json
```

Unverified until the first publish: the exact certificate identity (it is built from GitHub's
`job_workflow_ref` claim; the repository uses GitHub's immutable OIDC subject format, which changes
`sub`, not that claim as far as the documentation says) and the output format of
`verify-attestation`. If the identity check fails on a genuine image, print what the certificate
says with `--certificate-identity-regexp '.*'` and compare it before loosening anything.

## The registry lives and dies with the lab

The repository `mamori` belongs to `infra/envs/lab`, so `make down` deletes it and every image in
it (`scripts/leftover-check.sh` expects it gone). After `make up` the repository is empty, and
every Deployment that uses one of our images is in `ImagePullBackOff` until the images are
published again:

```sh
gh workflow run images.yml --ref main -f version=0.1.0   # the version deploy/ refers to
gh run watch "$(gh run list --workflow images.yml --limit 1 --json databaseId --jq '.[0].databaseId')"
```

Kubernetes retries the pulls on its own, so the pods start once the images exist. The rebuilt
image comes from the same tag, but it is a new build with a new digest and a new signature.

While the lab is down, a push to `main` that touches images still runs the checks; the publish
step finds no repository and ends with a warning instead of failing. A release publish fails
instead, because `deploy/` would point at a tag that doesn't exist.

## Locally

```sh
docker build --platform linux/amd64 -t frontend:dev services/frontend
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock aquasec/trivy:0.76.1 \
  image --scanners vuln,secret --severity HIGH,CRITICAL --ignore-unfixed frontend:dev
```
