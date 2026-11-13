# Dev container image: Debian plus the tools that have no pinned dev container feature.
# Terraform, tflint, Go, golangci-lint, kubectl, Helm and gh come from features (devcontainer.json).
FROM mcr.microsoft.com/devcontainers/base:2.2.5-trixie

# Google Cloud CLI and the GKE auth plugin kubectl needs, from Google's apt repository at a fixed
# release. shellcheck and pre-commit come from Debian, so the base image's release fixes them.
ARG GCLOUD_VERSION=591.0.0
RUN set -eux; \
    curl -fsSL https://packages.cloud.google.com/apt/doc/apt-key.gpg \
      | gpg --dearmor -o /usr/share/keyrings/cloud.google.gpg; \
    echo "deb [signed-by=/usr/share/keyrings/cloud.google.gpg] https://packages.cloud.google.com/apt cloud-sdk main" \
      > /etc/apt/sources.list.d/google-cloud-sdk.list; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      "google-cloud-cli=${GCLOUD_VERSION}-0" \
      "google-cloud-cli-gke-gcloud-auth-plugin=${GCLOUD_VERSION}-0" \
      shellcheck pre-commit; \
    rm -rf /var/lib/apt/lists/*

# kubeconform for deploy/validate.sh, same version as its pinned image (it uses this binary when
# found, so the container needs no Docker). The checksums belong to this version: change together.
ARG KUBECONFORM_VERSION=v0.8.0
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
      amd64) sha=9bc2bffbf71f261128533edaf912153948b7ff238f9a531ae6d34466ec287883 ;; \
      arm64) sha=1f53fc8e81258197a35e8603054162a5af1de8c5af13746c71ab680d9534ed87 ;; \
      *) echo "unsupported architecture: $arch" >&2; exit 1 ;; \
    esac; \
    curl -fsSLo /tmp/kubeconform.tgz \
      "https://github.com/yannh/kubeconform/releases/download/${KUBECONFORM_VERSION}/kubeconform-linux-${arch}.tar.gz"; \
    echo "${sha}  /tmp/kubeconform.tgz" | sha256sum --check --strict; \
    tar -xzf /tmp/kubeconform.tgz -C /usr/local/bin kubeconform; \
    rm /tmp/kubeconform.tgz

# Day-to-day work runs as the image's non-root user (trivy flags images that run as root).
USER vscode
