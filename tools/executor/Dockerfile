# Static Go binary on distroless: no shell, no package manager, runs as non-root (65532) and
# works with a read-only root filesystem (it writes nothing to disk).
# Build from this directory: docker build -t executor:dev .
# Both base images are pinned by digest (checked 2026-11-13); bump them on purpose.

FROM golang:1.27.3-trixie@sha256:af1bfc2614ebb1dbd31af514b78740fd678850e76498762b425ecee4c5c6ea80 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/executor .

FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
COPY --from=build /out/executor /executor
USER 65532:65532
EXPOSE 8080 9090
ENTRYPOINT ["/executor"]
