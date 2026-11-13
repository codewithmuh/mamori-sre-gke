# CUJ prober image. Build from this directory:
#   docker build -t prober tools/prober
# The build stage runs on the build machine's platform and cross-compiles for the target, so a
# linux/amd64 image for GKE builds fast on an arm64 laptop too.

# golang:1.27.3-trixie (multi-arch index digest), the same Go as go.mod's toolchain line.
FROM --platform=$BUILDPLATFORM golang:1.27.3-trixie@sha256:af1bfc2614ebb1dbd31af514b78740fd678850e76498762b425ecee4c5c6ea80 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=0.1.0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/prober .

# Static binary on distroless: no shell, no package manager, CA certificates for an https target.
# gcr.io/distroless/static-debian13:nonroot, the multi-arch index published 2026-10-23.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
COPY --from=build /out/prober /prober
# nonroot (65532); writes nothing to disk, so it runs with a read-only root filesystem.
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/prober"]
