package main

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestJourneysSucceed(t *testing.T) {
	tests := []struct {
		cuj       string
		steps     []string
		requests  []string
		cartAfter int
	}{
		{cujBrowse, []string{"home", "product"}, []string{"GET /", "GET /product"}, 0},
		{cujAddToCart, []string{"home", "add-to-cart"}, []string{"GET /", "POST /cart", "POST /cart/empty"}, 0},
		{cujViewCart, []string{"home", "add-to-cart", "view-cart"}, []string{"GET /", "POST /cart", "GET /cart", "POST /cart/empty"}, 0},
		// checkoutservice empties the cart, so no cleanup request.
		{cujCheckout, []string{"home", "add-to-cart", "view-cart", "place-order"}, []string{"GET /", "POST /cart", "GET /cart", "POST /cart/checkout"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.cuj, func(t *testing.T) {
			shop := newFakeShop(t)
			p, _ := testProber(t, shop)
			j, _ := journeyByID(tt.cuj)

			res := p.probe(t.Context(), j)

			if !res.ok || res.err != "" || res.failedStep != "" {
				t.Fatalf("probe failed: step %q: %s", res.failedStep, res.err)
			}
			var steps []string
			for _, st := range res.steps {
				steps = append(steps, st.name)
				if !st.ok || st.duration <= 0 {
					t.Errorf("step %s: ok=%v duration=%v", st.name, st.ok, st.duration)
				}
			}
			if !slices.Equal(steps, tt.steps) {
				t.Errorf("steps = %v, want %v", steps, tt.steps)
			}
			if got := shop.routes(); !slices.Equal(got, tt.requests) {
				t.Errorf("requests = %v, want %v", got, tt.requests)
			}
			if res.duration <= 0 {
				t.Errorf("duration = %v", res.duration)
			}
			if n := shop.cartItems(); n != tt.cartAfter {
				t.Errorf("items left in carts = %d, want %d", n, tt.cartAfter)
			}
		})
	}
}

func TestProbeActsLikeOneShopper(t *testing.T) {
	shop := newFakeShop(t)
	p, _ := testProber(t, shop)
	j, _ := journeyByID(cujCheckout)

	if res := p.probe(t.Context(), j); !res.ok {
		t.Fatalf("probe failed: %s", res.err)
	}
	reqs := shop.recorded()
	for _, r := range reqs[1:] {
		if r.session != reqs[0].session {
			t.Errorf("%s used session %q, want the home page's %q", r.route, r.session, reqs[0].session)
		}
	}
	for _, r := range reqs {
		if !strings.HasPrefix(r.userAgent, "mamori-prober/") {
			t.Errorf("%s: User-Agent = %q", r.route, r.userAgent)
		}
	}
	// The product comes from the home page (pick returns the last one), and the order uses the
	// cart page's pre-filled form.
	add := reqs[1].form
	if add.Get("product_id") != testProducts[2] || add.Get("quantity") != "1" {
		t.Errorf("add-to-cart form = %v", add)
	}
	order := reqs[3].form
	want := url.Values{
		"email":                        {"someone@example.com"},
		"street_address":               {"1600 Amphitheatre Parkway"},
		"zip_code":                     {"94043"},
		"city":                         {"Mountain View"},
		"state":                        {"CA"},
		"country":                      {"United States"},
		"credit_card_number":           {"4432801561520454"},
		"credit_card_expiration_month": {"1"},
		"credit_card_expiration_year":  {strconv.Itoa(time.Now().Year() + 1)},
		"credit_card_cvv":              {"672"},
	}
	for k, v := range want {
		if !slices.Equal(order[k], v) {
			t.Errorf("order form %s = %v, want %v", k, order[k], v)
		}
	}
	if len(order) != len(want) {
		t.Errorf("order form has %d fields, want %d: %v", len(order), len(want), order)
	}

	// The next probe is a new shopper.
	shop.reset()
	p.probe(t.Context(), j)
	if next := shop.recorded()[0].session; next == reqs[0].session {
		t.Errorf("second probe reused session %q", next)
	}
}

func TestProbeWithBasePath(t *testing.T) {
	shop := newFakeShopWithPrefix(t, "/shop")
	p, _ := testProber(t, shop)
	for _, j := range journeys {
		if res := p.probe(t.Context(), j); !res.ok {
			t.Errorf("%s failed at %s: %s", j.id, res.failedStep, res.err)
		}
	}
}

func TestStepFailures(t *testing.T) {
	tests := []struct {
		name       string
		cuj        string
		route      string
		fault      fault
		host       string
		failedStep string
		wantErr    string
	}{
		{
			name: "frontend error page", cuj: cujBrowse, route: "GET /",
			fault:      fault{status: 500, body: "could not retrieve currencies: rpc error: code = Unavailable desc = fault filter abort"},
			failedStep: "home",
			wantErr:    "GET /: status 500, want 200: could not retrieve currencies: rpc error: code = Unavailable desc = fault filter abort",
		},
		{
			name: "envoy local reply", cuj: cujBrowse, route: "GET /",
			fault:      fault{status: 503, contentType: "text/plain", body: "no healthy upstream"},
			failedStep: "home", wantErr: "GET /: status 503, want 200: no healthy upstream",
		},
		{
			name: "gateway rate limit", cuj: cujBrowse, route: "GET /",
			fault:      fault{status: 429, contentType: "text/plain", body: "local_rate_limited", rateLimited: true},
			failedStep: "home", wantErr: "GET /: status 429, want 200 (gateway rate limit): local_rate_limited",
		},
		{
			name: "home without products", cuj: cujAddToCart, route: "GET /",
			fault:      fault{page: "<html><h3>Hot Products</h3><a href=\"/cart\">cart</a></html>"},
			failedStep: "home", wantErr: "GET /: the page has no product links",
		},
		{
			name: "home cut short", cuj: cujBrowse, route: "GET /",
			fault:      fault{page: "<html><header>Online Boutique</header>"},
			failedStep: "home", wantErr: `GET /: the page has no "Hot Products"`,
		},
		{
			name: "product error", cuj: cujBrowse, route: "GET /product",
			fault:      fault{status: 500, body: "could not retrieve product"},
			failedStep: "product", wantErr: "GET /product/1YMWWN1N4O: status 500, want 200: could not retrieve product",
		},
		{
			name: "product page without form", cuj: cujBrowse, route: "GET /product",
			fault:      fault{page: "<html><h2>Product</h2></html>"},
			failedStep: "product", wantErr: "no add-to-cart form for product 1YMWWN1N4O",
		},
		{
			name: "add to cart fails", cuj: cujAddToCart, route: "POST /cart",
			fault:      fault{status: 500, body: "failed to add to cart: rpc error: code = Unavailable"},
			failedStep: "add-to-cart", wantErr: "POST /cart: status 500, want 302: failed to add to cart",
		},
		{
			name: "add to cart without redirect", cuj: cujAddToCart, route: "POST /cart",
			fault:      fault{page: "<html>ok</html>"},
			failedStep: "add-to-cart", wantErr: "POST /cart: status 200, want 302",
		},
		{
			name: "add to cart redirects elsewhere", cuj: cujAddToCart, route: "POST /cart",
			fault:      fault{location: "/login"},
			failedStep: "add-to-cart", wantErr: `POST /cart: redirected to "/login", want /cart`,
		},
		{
			name: "cart page error", cuj: cujViewCart, route: "GET /cart",
			fault:      fault{status: 500, body: "failed to get shipping quote"},
			failedStep: "view-cart", wantErr: "GET /cart: status 500, want 200: failed to get shipping quote",
		},
		{
			name: "cart lost the item", cuj: cujViewCart, route: "GET /cart",
			fault:      fault{page: "<html><h3>Your shopping cart is empty!</h3></html>"},
			failedStep: "view-cart", wantErr: "GET /cart: product 1YMWWN1N4O is not in the cart",
		},
		{
			name: "cart without checkout form", cuj: cujCheckout, route: "GET /cart",
			fault:      fault{page: "<html>SKU #1YMWWN1N4O</html>"},
			failedStep: "view-cart", wantErr: "GET /cart: the page has no checkout form",
		},
		{
			name: "checkout form incomplete", cuj: cujCheckout, route: "GET /cart",
			fault: fault{page: `<html>SKU #1YMWWN1N4O<form action="/cart/checkout" method="POST">
				<input name="email" value="someone@example.com"><input name="zip_code" value="">
				</form></html>`},
			failedStep: "view-cart",
			wantErr:    "the checkout form has no value for street_address, zip_code, city, state, country, credit_card_number",
		},
		{
			name: "order fails", cuj: cujCheckout, route: "POST /cart/checkout",
			fault:      fault{status: 500, body: "failed to complete the order: rpc error: code = Internal desc = failed to charge card"},
			failedStep: "place-order", wantErr: "POST /cart/checkout: status 500, want 200: failed to complete the order",
		},
		{
			name: "order page cut short", cuj: cujCheckout, route: "POST /cart/checkout",
			fault:      fault{page: "<html><header>Online Boutique</header>"},
			failedStep: "place-order", wantErr: `POST /cart/checkout: the page has no "Your order is complete!"`,
		},
		{
			name: "host the gateway does not serve", cuj: cujBrowse, host: "other.test",
			failedStep: "home", wantErr: "GET /: status 404, want 200",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shop := newFakeShop(t)
			if tt.route != "" {
				shop.setFault(tt.route, tt.fault)
			}
			p, _ := testProber(t, shop, func(c *config) {
				if tt.host != "" {
					c.host = tt.host
				}
			})
			j, _ := journeyByID(tt.cuj)

			res := p.probe(t.Context(), j)

			if res.ok || res.cancelled {
				t.Fatalf("probe ok=%v cancelled=%v, want a failure", res.ok, res.cancelled)
			}
			if res.failedStep != tt.failedStep {
				t.Errorf("failedStep = %q, want %q (error: %s)", res.failedStep, tt.failedStep, res.err)
			}
			if !strings.Contains(res.err, tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", res.err, tt.wantErr)
			}
			last := res.steps[len(res.steps)-1]
			if last.name != tt.failedStep || last.ok {
				t.Errorf("last step = %+v, want the failed step", last)
			}
			for _, st := range res.steps[:len(res.steps)-1] {
				if !st.ok {
					t.Errorf("step %s before the failure is not ok", st.name)
				}
			}
		})
	}
}

func TestFailedProbeEmptiesItsCart(t *testing.T) {
	shop := newFakeShop(t)
	shop.setFault("POST /cart/checkout", fault{status: 500, body: "failed to charge card"})
	p, _ := testProber(t, shop)
	j, _ := journeyByID(cujCheckout)

	if res := p.probe(t.Context(), j); res.ok {
		t.Fatal("probe succeeded, want a failure")
	}
	routes := shop.routes()
	if routes[len(routes)-1] != "POST /cart/empty" {
		t.Errorf("requests = %v, want the cart emptied last", routes)
	}
}

func TestCleanupFailureDoesNotFailProbe(t *testing.T) {
	shop := newFakeShop(t)
	shop.setFault("POST /cart/empty", fault{status: 500, body: "failed to empty cart"})
	p, logs := testProber(t, shop)
	j, _ := journeyByID(cujAddToCart)

	if res := p.probe(t.Context(), j); !res.ok {
		t.Fatalf("probe failed: %s", res.err)
	}
	if !strings.Contains(logs.String(), "could not empty the probe's cart") {
		t.Errorf("log = %s, want the cleanup failure at debug level", logs)
	}
}

func TestStepTimeout(t *testing.T) {
	shop := newFakeShop(t)
	shop.setFault("GET /cart", fault{delay: time.Second})
	p, _ := testProber(t, shop, func(c *config) { c.stepTimeout = 100 * time.Millisecond })
	j, _ := journeyByID(cujViewCart)

	res := p.probe(t.Context(), j)

	if res.ok || res.failedStep != "view-cart" {
		t.Fatalf("ok=%v failedStep=%q, want view-cart to fail", res.ok, res.failedStep)
	}
	if want := "GET /cart: no complete response within 100ms"; res.err != want {
		t.Errorf("error = %q, want %q", res.err, want)
	}
	if d := res.steps[2].duration; d < 100*time.Millisecond || d > time.Second {
		t.Errorf("timed-out step took %v", d)
	}
}

func TestConnectionRefused(t *testing.T) {
	shop := newFakeShop(t)
	p, _ := testProber(t, shop)
	shop.srv.Close()
	j, _ := journeyByID(cujBrowse)

	res := p.probe(t.Context(), j)

	if res.ok || res.failedStep != "home" {
		t.Fatalf("ok=%v failedStep=%q", res.ok, res.failedStep)
	}
	if !strings.HasPrefix(res.err, "GET /: dial tcp") || strings.Contains(res.err, shop.srv.URL+"/") {
		t.Errorf("error = %q, want the dial error without the repeated URL", res.err)
	}
}

func TestOversizedPage(t *testing.T) {
	shop := newFakeShop(t)
	shop.setFault("GET /", fault{page: strings.Repeat("x", maxBodyBytes+1)})
	p, _ := testProber(t, shop)
	j, _ := journeyByID(cujBrowse)

	res := p.probe(t.Context(), j)
	if !strings.Contains(res.err, "larger than") {
		t.Errorf("error = %q, want the size limit", res.err)
	}
}

func TestCancelledProbeIsNotAResult(t *testing.T) {
	shop := newFakeShop(t)
	shop.setFault("GET /cart", fault{delay: 5 * time.Second})
	p, _ := testProber(t, shop)
	j, _ := journeyByID(cujViewCart)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan probeResult)
	go func() { done <- p.probe(ctx, j) }()
	waitFor(t, "the cart request", func() bool { return slices.Contains(shop.routes(), "GET /cart") })

	cancel()
	res := <-done

	if !res.cancelled || res.ok || res.failedStep != "" || len(res.steps) != 0 {
		t.Errorf("result = %+v, want only cancelled", res)
	}
}

func TestRequestErrors(t *testing.T) {
	s := &session{base: &url.URL{Scheme: "http", Host: "x"}, stepTimeout: time.Second}
	if _, err := s.do(t.Context(), "BAD METHOD", "/", nil); err == nil || !strings.HasPrefix(err.Error(), "BAD METHOD /: ") {
		t.Errorf("do() with an invalid method: error = %v", err)
	}
	pg := page{
		method: http.MethodGet,
		path:   "/",
		status: 502,
		header: http.Header{"Content-Type": {"text/plain"}},
		body:   []byte(strings.Repeat("upstream reset ", 40)),
	}
	if err := pg.wantStatus(200); !strings.HasSuffix(err.Error(), "...") {
		t.Errorf("error = %q, want a truncated reason", err)
	}
	pg.body = nil
	if err := pg.wantStatus(200); err.Error() != "GET /: status 502, want 200" {
		t.Errorf("error = %q", err)
	}
}

func TestJourneyLookup(t *testing.T) {
	if got := journeyIDs(); !slices.Equal(got, []string{"browse", "add-to-cart", "view-cart", "checkout"}) {
		t.Errorf("journeyIDs() = %v", got)
	}
	if _, ok := journeyByID("search"); ok {
		t.Error("journeyByID(search) found a journey")
	}
}
