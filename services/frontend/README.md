# frontend (fork)

The Online Boutique frontend (Go, Apache-2.0), copied from upstream v0.10.7 so the lab can change
it and ship its own image. The main change: when the currency service fails, the home, product and
cart pages show prices in US dollars with a notice instead of an error page. Recommendations, which
are optional, also get a 250 ms deadline so a slow recommendation service can't slow those pages.

- What changed and why: [CHANGES.md](CHANGES.md). Licence and attribution: [LICENSE](LICENSE),
  [NOTICE](NOTICE).
- How the image is built, scanned, signed and published: [docs/images.md](../../docs/images.md).

Run the tests with the Go version the image is built with:

```sh
docker run --rm -v "$PWD":/src -w /src \
  golang:1.27.0-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc \
  go test ./...
```

CI (`.github/workflows/images.yml`) also runs them with the race detector.
