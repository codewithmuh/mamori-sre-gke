package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

// Journey ids, fixed by the build contract (PRD CUJ-1 to CUJ-4).
const (
	cujBrowse    = "browse"
	cujAddToCart = "add-to-cart"
	cujViewCart  = "view-cart"
	cujCheckout  = "checkout"
)

// Text the Online Boutique v0.10.7 templates print only when the page rendered fully
// (src/frontend/templates/home.html and order.html).
const (
	markerHome  = "Hot Products"
	markerOrder = "Your order is complete!"
)

const (
	// A shop page is about 15 KB; anything far bigger is not the page we expect.
	maxBodyBytes = 2 << 20
	// Limit for emptying the probe's cart afterwards.
	cleanupTimeout = 3 * time.Second
)

// Fields the frontend's PlaceOrderPayload validator requires (src/frontend/validator/validator.go).
var checkoutFields = []string{
	"email", "street_address", "zip_code", "city", "state", "country",
	"credit_card_number", "credit_card_expiration_month", "credit_card_expiration_year", "credit_card_cvv",
}

// version is set at build time (see the Dockerfile) and sent in the User-Agent, so shop logs and
// mesh metrics can tell the prober from shoppers.
var version = "dev"

type step struct {
	name string
	run  func(*session, context.Context) error
}

var (
	stepHome       = step{"home", (*session).home}
	stepProduct    = step{"product", (*session).product}
	stepAddToCart  = step{"add-to-cart", (*session).addToCart}
	stepViewCart   = step{"view-cart", (*session).viewCart}
	stepPlaceOrder = step{"place-order", (*session).placeOrder}
)

type journey struct {
	id    string
	steps []step
}

// journeys in PRD order. Every probe is a new shopper: a new cookie jar and new connections, and
// it starts on the home page, because that is where a product id comes from. Any failed step
// fails the probe; the steps after it don't run.
var journeys = []journey{
	{cujBrowse, []step{stepHome, stepProduct}},
	// Stops at the 302 the frontend sends once cartservice has stored the item. Following it
	// would make this journey the same as view-cart.
	{cujAddToCart, []step{stepHome, stepAddToCart}},
	{cujViewCart, []step{stepHome, stepAddToCart, stepViewCart}},
	// Goes through the cart page like a shopper: the checkout form, with the demo's pre-filled
	// fake card, is read from there.
	{cujCheckout, []step{stepHome, stepAddToCart, stepViewCart, stepPlaceOrder}},
}

func journeyByID(id string) (journey, bool) {
	for _, j := range journeys {
		if j.id == id {
			return j, true
		}
	}
	return journey{}, false
}

func journeyIDs() []string {
	ids := make([]string, len(journeys))
	for i, j := range journeys {
		ids[i] = j.id
	}
	return ids
}

type stepResult struct {
	name     string
	ok       bool
	duration time.Duration
}

type probeResult struct {
	cuj        string
	ok         bool
	duration   time.Duration // all steps that ran, cleanup excluded
	failedStep string
	err        string
	steps      []stepResult
	// cancelled: the prober is shutting down or the API caller left. Such a probe says nothing
	// about the shop and is never recorded.
	cancelled bool
}

// session is one shopper: its cookie jar holds the frontend's shop_session-id cookie.
type session struct {
	client      *http.Client
	transport   *http.Transport
	base        *url.URL
	host        string
	stepTimeout time.Duration
	pick        func(n int) int

	productID    string
	checkoutForm url.Values
	cartFilled   bool // an item may have gone in, and no order took it out
}

func (p *prober) newSession() *session {
	jar, _ := cookiejar.New(nil) // never fails without options
	// Proxy stays nil: the prober must reach the gateway directly, whatever the environment says.
	tr := &http.Transport{
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     30 * time.Second,
	}
	return &session{
		client: &http.Client{
			Transport: tr,
			Jar:       jar,
			// Redirects are checked by the steps themselves, not followed.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		transport:   tr,
		base:        p.cfg.targetURL,
		host:        p.cfg.host,
		stepTimeout: p.cfg.stepTimeout,
		pick:        p.pick,
	}
}

// probe runs one journey from a fresh session.
func (p *prober) probe(ctx context.Context, j journey) probeResult {
	s := p.newSession()
	defer s.transport.CloseIdleConnections()

	res := probeResult{cuj: j.id, ok: true}
	start := time.Now()
	for _, st := range j.steps {
		stepCtx, cancel := context.WithTimeout(ctx, p.cfg.stepTimeout)
		t0 := time.Now()
		err := st.run(s, stepCtx)
		elapsed := time.Since(t0)
		cancel()
		if ctx.Err() != nil {
			return probeResult{cuj: j.id, cancelled: true}
		}
		res.steps = append(res.steps, stepResult{name: st.name, ok: err == nil, duration: elapsed})
		if err != nil {
			res.ok, res.failedStep, res.err = false, st.name, err.Error()
			break
		}
	}
	res.duration = time.Since(start)

	// Fresh sessions mean a new cart in redis-cart every probe; empty it so they don't pile up.
	// Best effort, not part of the result, and short, so it adds little to on-demand calls.
	if s.cartFilled {
		cleanupCtx, cancel := context.WithTimeout(ctx, min(p.cfg.stepTimeout, cleanupTimeout))
		if err := s.emptyCart(cleanupCtx); err != nil && ctx.Err() == nil {
			p.log.Debug("could not empty the probe's cart", "cuj", j.id, "error", err.Error())
		}
		cancel()
	}
	return res
}

func (s *session) home(ctx context.Context) error {
	pg, err := s.do(ctx, http.MethodGet, "/", nil)
	if err != nil {
		return err
	}
	if err := pg.wantStatus(http.StatusOK); err != nil {
		return err
	}
	if !bytes.Contains(pg.body, []byte(markerHome)) {
		return pg.errorf("the page has no %q", markerHome)
	}
	ids := productIDs(pg.body)
	if len(ids) == 0 {
		return pg.errorf("the page has no product links")
	}
	s.productID = ids[s.pick(len(ids))]
	return nil
}

func (s *session) product(ctx context.Context) error {
	pg, err := s.do(ctx, http.MethodGet, "/product/"+s.productID, nil)
	if err != nil {
		return err
	}
	if err := pg.wantStatus(http.StatusOK); err != nil {
		return err
	}
	form, ok := findForm(pg.body, "/cart")
	if !ok || form.Get("product_id") != s.productID {
		return pg.errorf("the page has no add-to-cart form for product %s", s.productID)
	}
	return nil
}

func (s *session) addToCart(ctx context.Context) error {
	form := url.Values{"product_id": {s.productID}, "quantity": {"1"}}
	// Even a failed or timed-out add may have stored the item, so clean up either way.
	s.cartFilled = true
	pg, err := s.do(ctx, http.MethodPost, "/cart", form)
	if err != nil {
		return err
	}
	// The frontend answers 302 to /cart only after cartservice stored the item; every failure
	// renders an error page with a 4xx or 5xx status instead.
	if err := pg.wantStatus(http.StatusFound); err != nil {
		return err
	}
	if pg.location == nil || pg.location.Path != s.base.Path+"/cart" {
		return pg.errorf("redirected to %q, want %s/cart", pg.header.Get("Location"), s.base.Path)
	}
	return nil
}

func (s *session) viewCart(ctx context.Context) error {
	pg, err := s.do(ctx, http.MethodGet, "/cart", nil)
	if err != nil {
		return err
	}
	if err := pg.wantStatus(http.StatusOK); err != nil {
		return err
	}
	if !bytes.Contains(pg.body, []byte("SKU #"+s.productID)) {
		return pg.errorf("product %s is not in the cart", s.productID)
	}
	form, ok := findForm(pg.body, "/cart/checkout")
	if !ok {
		return pg.errorf("the page has no checkout form")
	}
	var missing []string
	for _, f := range checkoutFields {
		if form.Get(f) == "" {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return pg.errorf("the checkout form has no value for %s", strings.Join(missing, ", "))
	}
	s.checkoutForm = form
	return nil
}

func (s *session) placeOrder(ctx context.Context) error {
	pg, err := s.do(ctx, http.MethodPost, "/cart/checkout", s.checkoutForm)
	if err != nil {
		return err
	}
	if err := pg.wantStatus(http.StatusOK); err != nil {
		return err
	}
	if !bytes.Contains(pg.body, []byte(markerOrder)) {
		return pg.errorf("the page has no %q", markerOrder)
	}
	s.cartFilled = false // checkoutservice empties the cart after a placed order
	return nil
}

func (s *session) emptyCart(ctx context.Context) error {
	pg, err := s.do(ctx, http.MethodPost, "/cart/empty", url.Values{})
	if err != nil {
		return err
	}
	return pg.wantStatus(http.StatusFound)
}

// page is a response that has been read in full; its body is closed.
type page struct {
	method   string
	path     string // as sent, base path included
	status   int
	header   http.Header
	location *url.URL // the Location header resolved against the request URL; nil if absent or invalid
	body     []byte
}

// do sends one request with the configured Host header and reads and closes the response.
func (s *session) do(ctx context.Context, method, path string, form url.Values) (page, error) {
	target := *s.base
	target.Path = s.base.Path + path
	pg := page{method: method, path: target.Path}
	var reqBody io.Reader
	if form != nil {
		reqBody = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), reqBody)
	if err != nil {
		return pg, pg.wrap(err)
	}
	req.Host = s.host
	req.Header.Set("User-Agent", "mamori-prober/"+version)
	req.Header.Set("Accept", "text/html")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return pg, s.requestError(ctx, pg, err)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	// Read in full or abandoned: either way a close error changes nothing for the probe.
	_ = resp.Body.Close()
	if err != nil {
		return pg, s.requestError(ctx, pg, err)
	}
	if len(data) > maxBodyBytes {
		return pg, pg.errorf("response is larger than %d bytes", maxBodyBytes)
	}
	pg.status, pg.header, pg.body = resp.StatusCode, resp.Header, data
	if loc, err := resp.Location(); err == nil {
		pg.location = loc
	}
	return pg, nil
}

func (s *session) requestError(ctx context.Context, pg page, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return pg.errorf("no complete response within %s", s.stepTimeout)
	}
	// url.Error repeats the method and the full URL; keep only the cause.
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return pg.wrap(err)
}

// wantStatus fails on any other status and adds what the response says about the failure: the
// frontend's error page detail, or Envoy's plain-text reason (for example "no healthy upstream"
// or "fault filter abort").
func (pg page) wantStatus(want int) error {
	if pg.status == want {
		return nil
	}
	msg := fmt.Sprintf("status %d, want %d", pg.status, want)
	if pg.header.Get("X-Local-Rate-Limit") == "true" {
		msg += " (gateway rate limit)" // the ingress gateway's Envoy local rate limit, not the shop
	}
	if detail := responseDetail(pg.header, pg.body); detail != "" {
		msg += ": " + detail
	}
	return pg.errorf("%s", msg)
}

func (pg page) errorf(format string, args ...any) error {
	return fmt.Errorf("%s %s: %s", pg.method, pg.path, fmt.Sprintf(format, args...))
}

func (pg page) wrap(err error) error {
	return fmt.Errorf("%s %s: %w", pg.method, pg.path, err)
}

// responseDetail returns a short, single-line reason from an error response.
func responseDetail(header http.Header, body []byte) string {
	mediaType, _, _ := mime.ParseMediaType(header.Get("Content-Type"))
	text := ""
	if mediaType == "text/html" {
		text = preText(body)
	} else {
		text = string(body)
	}
	return truncate(strings.Join(strings.Fields(text), " "), 200)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
