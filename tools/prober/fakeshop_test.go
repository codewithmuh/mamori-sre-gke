package main

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// The fake shop stands in for the Istio ingress gateway in front of the Online Boutique v0.10.7
// frontend. Its pages copy the parts of src/frontend/templates (Apache-2.0) the prober reads:
// product links, the add-to-cart form, the cart's checkout form with the demo's pre-filled card,
// the order page and the error page. Routes and status codes follow src/frontend/handlers.go.

const testHost = "shop.test"

var testProducts = []string{"OLJCESPC7Z", "66VCHSJNUP", "1YMWWN1N4O"}

// fault changes how one route answers. Routes: "GET /", "GET /product", "POST /cart",
// "GET /cart", "POST /cart/checkout", "POST /cart/empty".
type fault struct {
	delay       time.Duration // wait first (or until the request is cancelled)
	status      int           // answer with this status and an error page...
	contentType string        // ...or, when set, with body as is (like Envoy's local replies)
	body        string
	page        string // answer 200 with this page instead of the real one
	location    string // POST /cart: redirect here instead of /cart
	rateLimited bool   // add the header the ingress gateway's local rate limit sets
}

type fakeRequest struct {
	route     string
	session   string
	userAgent string
	form      url.Values
}

type fakeShop struct {
	srv    *httptest.Server
	prefix string // path prefix, like the frontend's BASE_URL

	mu       sync.Mutex
	faults   map[string]fault
	carts    map[string][]string
	requests []fakeRequest
	nextID   int

	inflight    atomic.Int32 // requests in progress on GET /
	maxInflight atomic.Int32
}

func newFakeShop(t *testing.T) *fakeShop {
	t.Helper()
	return newFakeShopWithPrefix(t, "")
}

func newFakeShopWithPrefix(t *testing.T, prefix string) *fakeShop {
	t.Helper()
	f := &fakeShop{prefix: prefix, faults: map[string]fault{}, carts: map[string][]string{}}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeShop) setFault(route string, fl fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults[route] = fl
}

func (f *fakeShop) routes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests {
		out = append(out, r.route)
	}
	return out
}

func (f *fakeShop) recorded() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeRequest(nil), f.requests...)
}

func (f *fakeShop) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = nil
}

func (f *fakeShop) cartItems() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, items := range f.carts {
		n += len(items)
	}
	return n
}

func (f *fakeShop) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The Istio Gateway accepts one host; Envoy answers 404 for any other.
	if r.Host != testHost {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	path, ok := strings.CutPrefix(r.URL.Path, f.prefix)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	route := r.Method + " " + path
	productID := ""
	if id, ok := strings.CutPrefix(path, "/product/"); ok && r.Method == http.MethodGet {
		route, productID = "GET /product", id
	}
	if route == "GET /" {
		n := f.inflight.Add(1)
		defer f.inflight.Add(-1)
		for {
			m := f.maxInflight.Load()
			if n <= m || f.maxInflight.CompareAndSwap(m, n) {
				break
			}
		}
	}

	// ensureSessionID in src/frontend/middleware.go.
	session := ""
	if c, err := r.Cookie("shop_session-id"); err == nil {
		session = c.Value
	} else {
		f.mu.Lock()
		f.nextID++
		session = fmt.Sprintf("session-%d", f.nextID)
		f.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "shop_session-id", Value: session, MaxAge: 60 * 60 * 48})
	}
	_ = r.ParseForm()

	f.mu.Lock()
	f.requests = append(f.requests, fakeRequest{route: route, session: session, userAgent: r.UserAgent(), form: r.PostForm})
	fl := f.faults[route]
	f.mu.Unlock()

	if fl.delay > 0 {
		select {
		case <-time.After(fl.delay):
		case <-r.Context().Done():
			return
		}
	}
	switch {
	case fl.status != 0 && fl.contentType != "":
		if fl.rateLimited {
			w.Header().Set("x-local-rate-limit", "true")
		}
		w.Header().Set("Content-Type", fl.contentType)
		w.WriteHeader(fl.status)
		_, _ = io.WriteString(w, fl.body)
		return
	case fl.status != 0:
		f.errorPage(w, fl.status, fl.body)
		return
	case fl.page != "":
		f.page(w, fl.page)
		return
	}

	switch route {
	case "GET /":
		f.page(w, f.homePage())
	case "GET /product":
		if !contains(testProducts, productID) {
			f.errorPage(w, http.StatusInternalServerError, "could not retrieve product: rpc error: code = NotFound")
			return
		}
		f.page(w, f.productPage(productID))
	case "POST /cart":
		qty, _ := strconv.Atoi(r.PostForm.Get("quantity"))
		id := r.PostForm.Get("product_id")
		if qty < 1 || qty > 10 || id == "" {
			f.errorPage(w, http.StatusUnprocessableEntity, "Field 'Quantity' is invalid: required")
			return
		}
		if !contains(testProducts, id) {
			f.errorPage(w, http.StatusInternalServerError, "could not retrieve product")
			return
		}
		f.mu.Lock()
		f.carts[session] = append(f.carts[session], id)
		f.mu.Unlock()
		loc := f.prefix + "/cart"
		if fl.location != "" {
			loc = fl.location
		}
		w.Header().Set("location", loc)
		w.WriteHeader(http.StatusFound)
	case "GET /cart":
		f.mu.Lock()
		items := append([]string(nil), f.carts[session]...)
		f.mu.Unlock()
		f.page(w, f.cartPage(items))
	case "POST /cart/checkout":
		if msg := validateOrder(r.PostForm); msg != "" {
			f.errorPage(w, http.StatusUnprocessableEntity, msg)
			return
		}
		f.mu.Lock()
		items := f.carts[session]
		delete(f.carts, session)
		f.mu.Unlock()
		if len(items) == 0 {
			f.errorPage(w, http.StatusInternalServerError, "failed to complete the order: cart is empty")
			return
		}
		f.page(w, f.orderPage())
	case "POST /cart/empty":
		f.mu.Lock()
		delete(f.carts, session)
		f.mu.Unlock()
		w.Header().Set("location", f.prefix+"/")
		w.WriteHeader(http.StatusFound)
	default:
		http.NotFound(w, r)
	}
}

// validateOrder checks what PlaceOrderPayload and paymentservice check.
func validateOrder(form url.Values) string {
	for _, field := range checkoutFields {
		if form.Get(field) == "" {
			return fmt.Sprintf("Field '%s' is invalid: required", field)
		}
	}
	if !strings.Contains(form.Get("email"), "@") {
		return "Field 'Email' is invalid: email"
	}
	if !regexp.MustCompile(`^4[0-9]{15}$`).MatchString(form.Get("credit_card_number")) {
		return "Field 'CcNumber' is invalid: credit_card"
	}
	month, _ := strconv.Atoi(form.Get("credit_card_expiration_month"))
	year, _ := strconv.Atoi(form.Get("credit_card_expiration_year"))
	if month < 1 || month > 12 || year < time.Now().Year() {
		return "failed to charge card: expired"
	}
	return ""
}

func (f *fakeShop) page(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, body)
}

func (f *fakeShop) header() string {
	return `<!DOCTYPE html><html lang="en"><head><title>Online Boutique</title></head><body>
<header><div class="navbar"><div class="h-free-shipping">Demo shop, no real payments. Do not enter real card details.</div></div>
<div class="navbar sub-navbar"><a href="` + f.prefix + `/" class="navbar-brand">Online Boutique</a>
<a class="cart-link" href="` + f.prefix + `/cart" role="button"><img src="` + f.prefix + `/static/icons/Hipster_CartIcon.svg" alt="Cart icon" title="Cart"></a></div></header>
`
}

const fakeFooter = `<footer class="py-5"><div class="footer-top"><a href="https://github.com/GoogleCloudPlatform/microservices-demo">source</a></div></footer></body></html>`

func (f *fakeShop) homePage() string {
	var b strings.Builder
	b.WriteString(f.header())
	b.WriteString(`<main role="main" class="home"><div class="container-fluid"><div class="row hot-products-row px-xl-6">
<div class="col-12"><h3>Hot Products</h3></div>`)
	for _, id := range testProducts {
		fmt.Fprintf(&b, `
<div class="col-md-4 hot-product-card">
  <a href="%[1]s/product/%[2]s">
    <img loading="lazy" src="%[1]s/static/img/products/%[2]s.jpg">
    <div class="hot-product-card-img-overlay"></div>
  </a>
  <div><div class="hot-product-card-name">Product %[2]s</div><div class="hot-product-card-price">$19.99</div></div>
</div>`, f.prefix, id)
	}
	b.WriteString(`</div></div></main>` + fakeFooter)
	return b.String()
}

func (f *fakeShop) productPage(id string) string {
	return f.header() + fmt.Sprintf(`<main role="main"><div class="h-product container"><div class="row">
<div class="col-md-6"><img class="product-image" alt="" src="%[1]s/static/img/products/%[2]s.jpg" /></div>
<div class="product-info col-md-5"><div class="product-wrapper">
  <h2>Product %[2]s</h2><p class="product-price">$19.99</p><p>A product.</p>
  <form method="POST" action="%[1]s/cart">
    <input type="hidden" name="product_id" value="%[2]s" />
    <div class="product-quantity-dropdown">
      <select name="quantity" id="quantity">
        <option>1</option><option>2</option><option>3</option><option>4</option><option>5</option><option>10</option>
      </select>
      <img src="%[1]s/static/icons/Hipster_DownArrow.svg" alt="">
    </div>
    <button type="submit" class="cymbal-button-primary">Add To Cart</button>
  </form>
</div></div></div></div>
<div class="ad"><div class="container py-3 px-lg-5 py-lg-5"><div role="alert"><strong>Ad</strong>
<a href="%[1]s/product/66VCHSJNUP" rel="nofollow" target="_blank">Hairdryer for sale. 50%% off.</a></div></div></div>
</main>`, f.prefix, id) + fakeFooter
}

func (f *fakeShop) cartPage(items []string) string {
	var b strings.Builder
	b.WriteString(f.header())
	b.WriteString(`<main role="main" class="cart-sections">`)
	if len(items) == 0 {
		b.WriteString(`<section class="empty-cart-section"><h3>Your shopping cart is empty!</h3>
<a class="cymbal-button-primary" href="` + f.prefix + `/" role="button">Continue Shopping</a></section></main>` + fakeFooter)
		return b.String()
	}
	fmt.Fprintf(&b, `<section class="container"><div class="row"><div class="col-lg-6 col-xl-5 offset-xl-1 cart-summary-section">
<div class="row mb-3 py-2"><div class="col-4 pl-md-0"><h3>Cart (%d)</h3></div>
<div class="col-8 pr-md-0 text-right"><form method="POST" action="%s/cart/empty">
<button class="cymbal-button-secondary cart-summary-empty-cart-button" type="submit">Empty Cart</button></form></div></div>`, len(items), f.prefix)
	for _, id := range items {
		fmt.Fprintf(&b, `
<div class="row cart-summary-item-row"><div class="col-md-8 pr-md-0">
  <div class="row"><div class="col"><h4>Product %[1]s</h4></div></div>
  <div class="row cart-summary-item-row-item-id-row"><div class="col">
      SKU #%[1]s
  </div></div>
  <div class="row"><div class="col">Quantity: 1</div><div class="col pr-md-0 text-right"><strong>$19.99</strong></div></div>
</div></div>`, html.EscapeString(id))
	}
	b.WriteString(`</div><div class="col-lg-5 offset-lg-1 col-xl-4">`)
	b.WriteString(f.checkoutForm())
	b.WriteString(`</div></div></section></main>` + fakeFooter)
	return b.String()
}

// checkoutForm copies the form in templates/cart.html, with the template's year list
// (this year to this year + 4, the second one selected).
func (f *fakeShop) checkoutForm() string {
	year := time.Now().Year()
	var years strings.Builder
	for i := range 5 {
		selected := ""
		if i == 1 {
			selected = `
                                            selected="selected"`
		}
		fmt.Fprintf(&years, `<option value="%d"%s
                                    >%d</option>`, year+i, selected, year+i)
	}
	return `<form class="cart-checkout-form" action="` + f.prefix + `/cart/checkout" method="POST">
    <div class="row"><div class="col"><h3>Shipping Address</h3></div></div>
    <div class="form-row"><div class="col cymbal-form-field"><label for="email">E-mail Address</label>
        <input type="email" id="email"
            name="email" value="someone@example.com" required></div></div>
    <div class="form-row"><div class="col cymbal-form-field"><label for="street_address">Street Address</label>
        <input type="text" name="street_address"
            id="street_address" value="1600 Amphitheatre Parkway" required></div></div>
    <div class="form-row"><div class="col cymbal-form-field"><label for="zip_code">Zip Code</label>
        <input type="text"
            name="zip_code" id="zip_code" value="94043" required pattern="\d{4,5}"></div></div>
    <div class="form-row"><div class="col cymbal-form-field"><label for="city">City</label>
        <input type="text" name="city" id="city"
            value="Mountain View" required></div></div>
    <div class="form-row">
        <div class="col-md-5 cymbal-form-field"><label for="state">State</label>
            <input type="text" name="state" id="state"
                value="CA" required></div>
        <div class="col-md-7 cymbal-form-field"><label for="country">Country</label>
            <input type="text" id="country"
                placeholder="Country Name"
                name="country" value="United States" required></div>
    </div>
    <div class="row"><div class="col"><h3 class="payment-method-heading">Payment Method</h3></div></div>
    <div class="form-row"><div class="col cymbal-form-field"><label for="credit_card_number">Credit Card Number</label>
        <input type="text" id="credit_card_number"
            name="credit_card_number"
            placeholder="0000000000000000"
            value="4432801561520454"
            required pattern="\d{16}"></div></div>
    <div class="form-row">
        <div class="col-md-5 cymbal-form-field"><label for="credit_card_expiration_month">Month</label>
            <select name="credit_card_expiration_month" id="credit_card_expiration_month">
                <option value="1">January</option>
                <option value="2">February</option>
                <option value="3">March</option>
                <option value="12">January</option>
            </select>
            <img src="` + f.prefix + `/static/icons/Hipster_DownArrow.svg" alt="" class="cymbal-dropdown-chevron"></div>
        <div class="col-md-4 cymbal-form-field">
                <label for="credit_card_expiration_year">Year</label>
                <select name="credit_card_expiration_year" id="credit_card_expiration_year">
                ` + years.String() + `
                </select></div>
        <div class="col-md-3 cymbal-form-field"><label for="credit_card_cvv">CVV</label>
            <input type="password" id="credit_card_cvv"
                name="credit_card_cvv" value="672" required pattern="\d{3}"></div>
    </div>
    <div class="form-row justify-content-center"><div class="col text-center">
        <button class="cymbal-button-primary" type="submit">
            Place Order
        </button></div></div>
</form>`
}

func (f *fakeShop) orderPage() string {
	return f.header() + `<main role="main" class="order"><section class="container order-complete-section">
<div class="row"><div class="col-12 text-center"><h3>
                        Your order is complete!
                    </h3></div>
<div class="col-12 text-center"><p>We've sent you a confirmation email.</p></div></div>
<div class="row border-bottom-solid padding-y-24"><div class="col-6 pl-md-0">Confirmation #</div>
<div class="col-6 pr-md-0 text-right">2f1c6a3e-0b1d-4b8e-9d7e-3c2a1b0f9e8d</div></div>
</section></main>` + fakeFooter
}

// errorPage copies templates/error.html, which renderHTTPError serves with the error status.
func (f *fakeShop) errorPage(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, f.header()+fmt.Sprintf(`<main role="main"><div class="py-5">
<div class="container bg-light py-3 px-lg-5 py-lg-5"><h1>Uh, oh!</h1>
<p>Something has failed. Below are some details for debugging.</p>
<p><strong>HTTP Status:</strong> %d %s</p>
<pre class="border border-danger p-3"
    style="white-space: pre-wrap; word-break: keep-all;">%s</pre>
</div></div></main>`, status, http.StatusText(status), html.EscapeString(msg))+fakeFooter)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// syncBuffer is a log sink that tests can read while the prober writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// testProber returns a prober aimed at the fake shop, with short timings and a fixed product
// choice (the last product), and its log.
func testProber(t *testing.T, shop *fakeShop, change ...func(*config)) (*prober, *syncBuffer) {
	t.Helper()
	target, err := url.Parse(shop.srv.URL + shop.prefix)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config{
		targetURL:      target,
		host:           testHost,
		interval:       time.Minute,
		stepTimeout:    2 * time.Second,
		scheduled:      []string{},
		listenAddr:     "127.0.0.1:0",
		runConcurrency: 4,
		logLevel:       slog.LevelDebug,
	}
	for _, c := range change {
		c(&cfg)
	}
	logs := &syncBuffer{}
	p := newProber(cfg, newMetrics(prometheus.NewRegistry()), newLogger(logs, cfg.logLevel))
	p.pick = func(n int) int { return n - 1 }
	return p, logs
}

// waitFor polls cond until it holds or the time is up.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for !cond() {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func counterValue(t *testing.T, c *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.WithLabelValues(labels...).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

func histogramCount(t *testing.T, h *prometheus.HistogramVec, labels ...string) uint64 {
	t.Helper()
	var m dto.Metric
	if err := h.WithLabelValues(labels...).(prometheus.Metric).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetHistogram().GetSampleCount()
}
