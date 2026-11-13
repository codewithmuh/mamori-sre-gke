# Changes from upstream

Upstream: [GoogleCloudPlatform/microservices-demo](https://github.com/GoogleCloudPlatform/microservices-demo)
`src/frontend` at commit `5b608cb1dfe317d05e4f1bb8c99b7618452fde87` (tag `v0.10.7`, the version
`deploy/online-boutique` runs). Copied on 2026-11-13 from the GitHub archive of that commit
(`git get-tar-commit-id` on the archive printed that commit).

Every file changed here says so in its header, as the Apache License 2.0 (section 4b) asks.

## 1. The currency service fails open on browse pages

The currency service only changes how prices are displayed: the product catalog and the shipping
service already price everything in USD. Upstream still answers **500** on the home, product and
cart pages when it fails, so a currency outage takes down browsing and the cart (and add-to-cart
looks broken, because it redirects to the cart): the "dependency that should not be critical but
is" from PLAN.md section 4.

| Page | Upstream when currency fails | This fork |
|---|---|---|
| `GET /` (home) | 500 | 200, prices in USD, notice |
| `GET /product/{id}` | 500 | 200, price in USD, notice |
| `GET /cart` | 500 | 200, item prices, shipping and total in USD, notice, checkout warning |
| `POST /cart/checkout` | 500 | 500, unchanged (see section 2) |
| Order confirmation, after a successful order | 500 | 200 (see section 2) |
| `GET /assistant` | 500 | 200 (the currency list is not shown there anyway) |

How it works (`currency_fallback.go`):

- Each page makes its currency calls through one `currencyView`. The **first failure switches the
  whole page to USD**: later currency calls are skipped (they would most likely fail too and only
  add latency), and prices already converted are dropped. A page never mixes currencies, which also
  keeps `money.Sum` (it panics on mixed currencies) safe in the cart total.
- A conversion result that can't be added up (no amount, a different currency than asked for, an
  invalid amount) counts as a failure.
- The header shows a notice with the stable id `currency-fallback-notice`
  (`templates/header.html`); the cart adds `currency-fallback-checkout` next to *Place Order*
  (`templates/cart.html`). Probes can look for these ids to count degraded pages.
- Each fallback logs one warning, `currency service failed; falling back to USD`, with the
  error and the user's currency.
- The user's currency cookie is left alone, so prices come back in their currency once the
  service recovers.
- Optional `CURRENCY_TIMEOUT` (a Go duration such as `300ms`): the time budget for all currency
  calls of one page. When it runs out, the page falls back to USD. Unset or `0` means no budget,
  as upstream. Upstream sets no deadline on currency calls, and per-host Istio VirtualServices
  are reserved for fault injection in this lab, so this is the only place a slow currency service
  can be cut off. Not set in the manifests yet: pick a value from measured latency (M4).
- Every other dependency behaves as upstream: without the product catalog, the cart or the
  shipping quote the page still fails, because there is nothing correct to show.

Files: `currency_fallback.go` (new), `handlers.go`, `rpc.go` (currency calls go through
`fe.currency`, a `pb.CurrencyServiceClient`, so tests can use a fake; `getShippingQuote` returns USD
and the cart converts it with the item prices), `main.go` (creates the client, reads
`CURRENCY_TIMEOUT`), `templates/header.html`, `templates/cart.html`.

## 2. Checkout: still fails when currency fails (decided)

`POST /cart/checkout` keeps failing while the currency service is down. Reasons:

- The order is placed by `checkoutservice` (not forked), which converts every item price and the
  shipping cost into the user's currency **before** it charges the card, and calls the currency
  service even for USD. With the currency service down it returns an error and charges nothing.
- Charging in USD instead would change the currency of a payment the user did not agree to, and
  would need a change in `checkoutservice`. A failed checkout is the honest outcome, and the
  checkout SLO counts it.

What the fork does change: once `PlaceOrder` has succeeded, the confirmation page no longer turns
into a 500 because the currency list for the (hidden) currency selector failed. Upstream did, which
told a user whose order had gone through that it failed, inviting a second payment. The cart page
warns, while in USD, that placing an order may fail until currency conversion is back.

## 3. Recommendations get a deadline

The product and cart pages (and the order confirmation) already render without recommendations
when `ListRecommendations` fails, but upstream waits for it with no deadline. So a *slow*
recommendationservice (CPU stress, injected latency) slowed every product and cart page, although
the result is optional.

`getRecommendations` (`rpc.go`) now has a 250 ms deadline, the way `getAd` has 100 ms. It covers
the whole lookup: the recommendation call and the catalog lookups of the products it returns.
When it runs out, the page renders without the "You May Also Like" section; the existing warning
`failed to get product recommendations` is logged. 250 ms is half of the browse journey's 500 ms
latency threshold (`docs/prd.md`), leaving the other half for the calls the page can't do without;
healthy calls take far less (an estimate until M4 measures it). It is a constant, not a setting,
like upstream's ad deadline; change it in `rpc.go` if the measured latency calls for it.

## 4. Image runs as non-root

`Dockerfile`: the runtime image is `gcr.io/distroless/static-debian13:nonroot`, pinned by digest
(upstream used the unpinned `gcr.io/distroless/static`, which runs as root), with `USER
65532:65532` so Kubernetes can enforce `runAsNonRoot`. The server only reads `./templates` and
`./static`, so it also works with a read-only root filesystem. The build stage is unchanged.

## 5. Dependency update

`golang.org/x/net` v0.58.0 → v0.62.0 for two HIGH advisories Trivy reports on upstream's version
(CVE-2026-78669, fixed in 0.60.0; CVE-2026-78661, fixed in 0.62.0). `go get` and `go mod tidy`
with the Dockerfile's Go (1.27.0) also moved `golang.org/x/crypto` v0.55.0 → v0.58.0, `x/sync`
v0.22.0 → v0.24.0, `x/sys` v0.47.0 → v0.49.0 and `x/text` v0.41.0 → v0.43.0, and raised the `go`
directive from 1.25.0 to 1.26.0, which x/net v0.62.0 requires. All five releases were at least two weeks old on
2026-11-13 (proxy.golang.org).

Go toolchain: the build image moves from `golang:1.27.0-alpine` to `golang:1.27.3-alpine` (pinned by digest) and
`toolchain` in `go.mod` from `go1.27.1` to `go1.27.3`. The binary embeds the Go standard library, and Trivy found four
HIGH CVEs in 1.27.0 (CVE-2026-111943 crypto/x509, CVE-2026-78667 and CVE-2026-78669 net/http, CVE-2026-97031
crypto/tls), all fixed in 1.27.3.

## 6. Tests

- `currency_fallback_test.go`: the fallback rules with a fake `pb.CurrencyServiceClient`: healthy
  conversion, list failure, failure halfway through a page, unusable results, the time budget,
  `CURRENCY_TIMEOUT` parsing.
- `currency_fallback_handlers_test.go`: the real handlers and templates against in-memory gRPC fakes
  (bufconn) for catalog, cart, shipping, checkout and recommendations: each page with the currency service down and
  healthy, a cart where only a later conversion fails, checkout failing, a placed order confirmed
  despite the currency failure, and a catalog failure that must still be a 500. Also a fake
  recommendation service: shown when fast; when it blocks past the deadline, the product and cart
  pages still render (without recommendations) within about 250 ms.

No new dependencies: `bufconn` and logrus's test hook come from modules upstream already requires.

## 7. Housekeeping

- `LICENSE` copied from the upstream repository root (`src/frontend` has none of its own);
  `NOTICE` and this file added; `README.md` replaced (upstream's described a `dep` workflow that
  no longer applies).
- A final newline added to `genproto.sh`, `templates/error.html` and `static/styles/cart.css`
  (the repository's pre-commit hooks require it); the empty `.gitkeep` left out.

## Left as upstream on purpose

- The Go module path stays `github.com/GoogleCloudPlatform/microservices-demo/src/frontend`.
  Renaming it would touch every import and make upstream updates harder; nothing imports this
  module.
- `genproto.sh` is kept for reference only: it expects `../../protos`, which this repository does
  not vendor. `genproto/` is upstream's generated code.
- Untouched upstream code is not reformatted (`gofmt` would change three lines in `handlers.go`
  and the import order in `middleware.go`).
- Known upstream issues not fixed here: `homeHandler` rewrites the global `plat` on every request,
  and `deployment_details.go` fills `deploymentDetailsMap` from a background goroutine while
  requests read it. Both are data races; the second matters only in the first seconds after start.

## Updating from upstream

1. Download the new upstream commit's archive, check it with `git get-tar-commit-id`.
2. Diff its `src/frontend` against the commit above to see what upstream changed, and apply that
   to this directory; keep the changes listed here.
3. Run the tests (README.md), update the commit and dates in this file and NOTICE, and bump the
   image version used in `deploy/`.
