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
| Pull request that touches `tools/`, `services/`, `agent/` or the workflow | For each image: Go tests with the race detector if it is a `services/` Go module (`tools/` modules are tested by `ci.yml`), `docker build`, Trivy scan of the image. Nothing is pushed and no job can get cloud or signing credentials. |
| Push to `main` touching the same paths | Build and scan (the tests ran on the pull request), then publish: push, sign, SBOM (below). Only build and scan while the repository variable `GCP_WIF_PROVIDER` is unset. |
| Release (release-please merges its release PR) | `release.yml` calls this workflow with the version (requested in #28); it builds tag `v<version>` and publishes it as `:<version>`. |
| By hand: Actions → images → Run workflow, from `main` | With a version: publish that release (for example the first time, see below). Without: publish `main` as `:<git sha>`. |

A version is only published if its tag points at a commit on `main` (`git merge-base
--is-ancestor`), so a tag on an unreviewed branch can't produce a version image. The build uses
the commit the tag pointed to when the run started, even if the tag moves during the run.

The run has two kinds of jobs. **Build** (one per image) checks out the code, runs the tests on
pull requests, builds, scans and, on `main`, writes the SBOM and saves the image as a run artifact.
It has no `id-token` permission. **Publish** (one per image, `main` only) has `id-token: write`,
but checks out no code and runs no tests or scanners: it checks the artifact's checksums, loads
the image the build job scanned, pushes it, signs it and attaches the SBOM. Code under test, the
build and third-party scanners therefore never run where a token for `ci-images` or Sigstore can
be had.

The Trivy scan fails the job on any HIGH or CRITICAL finding that has a fix (vulnerabilities and
secrets in the image). On `main` that stops the publish, so a vulnerable image never reaches the
registry. Fix it by updating the base image or the dependency; if there is no fix, the finding
does not fail the job.

## Tags

Images go to `asia-northeast1-docker.pkg.dev/<project>/mamori/<name>`:

| Tag | Set by |
|---|---|
| `<git sha>` (40 hex characters) | a publish from `main` (push, or a manual run without a version) |
| `<X.Y.Z>` | a release publish, built from tag `vX.Y.Z` |

The repository has **immutable tags** (`infra/bootstrap/registry.tf`): once pushed, a tag names the
same digest for good. The workflow works with that:

- A release pushes only `:<X.Y.Z>`, a fresh build of the tagged commit; the job fails if the
  version tag already exists (cut a new release instead). The same commit may also exist as
  `:<git sha>` from `main`, as a separate build with its own digest.
- A run for a commit that is already published as `:<git sha>` (a re-run, or a manual run) pushes
  nothing and signs the existing image again. That only adds a signature, and it also repairs a
  publish that failed between its push and its signature.

`deploy/` refers to version tags, for example
`asia-northeast1-docker.pkg.dev/PROJECT_ID/mamori/frontend:0.1.0`. A new image reaches the
cluster through a release and a version bump in `deploy/`, reviewed like any change, and Argo CD
syncs it. There is no `latest`. Because tags are immutable, a version tag is as exact as a digest.

The registry's cleanup policy keeps every version tag (prefixes `0.`, `1.`, `2.`) and the ten
newest versions of each image, and deletes other tagged images after 30 days, so old `:<git sha>`
images go and released ones stay. It never deletes untagged manifests, which is how signatures and
SBOM attestations are stored (below).

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
- `ci-images` holds Artifact Registry Writer on the repository `mamori` only, granted on the
  repository in `ci_images.tf` (the repository is in `infra/bootstrap` too). Writer can push, tag
  and attach signatures, not delete. Artifact Registry doesn't support IAM conditions on resource
  names, so a project-level grant could not be narrowed to one repository; `ci_images.tf`
  explains.
- Only the publish job gets an OIDC token (`id-token: write`), and it runs no repository code
  (above).
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
2. Takes the **SBOM** (SPDX JSON) that the build job generated with Trivy from the same image.
3. **Attaches the SBOM as a signed attestation** (`cosign attest --type spdxjson`), so the SBOM is
   tied to the image and to the same identity.

cosign v3 stores the signature and the attestation as OCI 1.1 referrers of the image: untagged
manifests that point at it. Artifact Registry keeps them as attachments; its cleanup policies
don't apply to attachments directly, and attachments are deleted when their image is. This matters
with immutable tags: cosign's older scheme (a `sha256-<digest>.sig` tag, rewritten for every new
signature) would be refused by the registry on the second signature. To check on the first real
publish: `gcloud artifacts attachments list --repository=mamori --location=asia-northeast1` lists
the signature and the attestation, and no `sha256-*.sig` or `*.att` tags appear.

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

## The registry outlives the lab

The repository `mamori` is created by `infra/bootstrap`, not by the lab root, so `make down`
leaves it and its images in place (a few hundred MB, inside Artifact Registry's free storage, an
estimate) and `make up` needs no republish. Deployments find their images the moment Argo CD
creates them.

## First time: the order

The very first `make up` has nothing in the registry yet, and the manifests still say
`PROJECT_ID`. Deployments that use our images (`prober`, `executor`, and `frontend` once its
kustomization points at the fork) would wait in `ImagePullBackOff` and hold up their Argo CD sync
wave, which is why `prober` and `executor` ship with `replicas: 0`. Once, in this order:

1. `make bootstrap` (creates the repository and `ci-images`), then set the repository variables
   it prints.
2. `make up`. The shop runs on upstream images; `prober` and `executor` exist with 0 replicas.
3. Publish the images: merge the release-please PR (release.yml publishes the version), or run
   ```sh
   gh workflow run images.yml --ref main -f version=0.1.0   # the version deploy/ refers to
   gh run watch "$(gh run list --workflow images.yml --limit 1 --json databaseId --jq '.[0].databaseId')"
   ```
4. `scripts/set-registry.sh --apply <project-id>`, review `git diff -- deploy`, and commit (through
   a pull request, like any change).
5. Raise `replicas` of `prober` and `executor` from 0 (and switch `frontend` to the fork) in a
   pull request; Argo CD rolls them out.

After that, `make down` and `make up` need none of this: the images and the manifests stay valid.

## Locally

```sh
docker build --platform linux/amd64 -t frontend:dev services/frontend
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock aquasec/trivy:0.76.1 \
  image --scanners vuln,secret --severity HIGH,CRITICAL --ignore-unfixed frontend:dev
```
