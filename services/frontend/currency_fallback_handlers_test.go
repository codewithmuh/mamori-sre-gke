// Copyright 2026 The mamori-sre-gke authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Added by the mamori-sre-gke project; not part of upstream Online Boutique. See CHANGES.md.

package main

// Page-level tests: the real handlers and templates, the fake currency client from
// currency_fallback_test.go, and in-memory gRPC fakes for the services the pages need. The ad
// service is not registered, so ad calls fail, which the upstream handlers already tolerate.

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/GoogleCloudPlatform/microservices-demo/src/frontend/genproto"
)

var testProducts = []*pb.Product{
	{Id: "OLJCESPC7Z", Name: "Sunglasses", Picture: "/static/img/products/sunglasses.jpg", PriceUsd: usd(19, 990000000)},
	{Id: "66VCHSJNUP", Name: "Tank Top", Picture: "/static/img/products/tank-top.jpg", PriceUsd: usd(18, 990000000)},
}

type fakeCatalog struct {
	pb.UnimplementedProductCatalogServiceServer
	err error
}

func (f *fakeCatalog) ListProducts(context.Context, *pb.Empty) (*pb.ListProductsResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &pb.ListProductsResponse{Products: testProducts}, nil
}

func (f *fakeCatalog) GetProduct(_ context.Context, in *pb.GetProductRequest) (*pb.Product, error) {
	if f.err != nil {
		return nil, f.err
	}
	for _, p := range testProducts {
		if p.GetId() == in.GetId() {
			return p, nil
		}
	}
	return nil, status.Errorf(codes.NotFound, "no product %q", in.GetId())
}

// fakeCart holds 2 × Sunglasses and 1 × Tank Top for every user.
type fakeCart struct {
	pb.UnimplementedCartServiceServer
}

func (fakeCart) GetCart(_ context.Context, in *pb.GetCartRequest) (*pb.Cart, error) {
	return &pb.Cart{UserId: in.GetUserId(), Items: []*pb.CartItem{
		{ProductId: "OLJCESPC7Z", Quantity: 2},
		{ProductId: "66VCHSJNUP", Quantity: 1},
	}}, nil
}

type fakeShipping struct {
	pb.UnimplementedShippingServiceServer
}

func (fakeShipping) GetQuote(context.Context, *pb.GetQuoteRequest) (*pb.GetQuoteResponse, error) {
	return &pb.GetQuoteResponse{CostUsd: usd(8, 990000000)}, nil
}

// fakeCheckout stands in for checkoutservice. err is what PlaceOrder returns; the real
// checkoutservice fails before charging when it can't convert prices.
type fakeCheckout struct {
	pb.UnimplementedCheckoutServiceServer
	err    error
	orders atomic.Int32
}

func (f *fakeCheckout) PlaceOrder(_ context.Context, in *pb.PlaceOrderRequest) (*pb.PlaceOrderResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.orders.Add(1)
	return &pb.PlaceOrderResponse{Order: &pb.OrderResult{
		OrderId:            "order-1",
		ShippingTrackingId: "track-1",
		ShippingCost:       &pb.Money{CurrencyCode: in.GetUserCurrency(), Units: 8, Nanos: 990000000},
		Items: []*pb.OrderItem{{
			Item: &pb.CartItem{ProductId: "OLJCESPC7Z", Quantity: 1},
			Cost: &pb.Money{CurrencyCode: in.GetUserCurrency(), Units: 19, Nanos: 990000000},
		}},
	}}, nil
}

// fakeRecommendations returns ids, or with block set waits until the caller's deadline has passed.
type fakeRecommendations struct {
	pb.UnimplementedRecommendationServiceServer
	ids         []string
	block       atomic.Bool
	calls       atomic.Int32
	sawDeadline atomic.Bool
}

func (f *fakeRecommendations) ListRecommendations(ctx context.Context, _ *pb.ListRecommendationsRequest) (*pb.ListRecommendationsResponse, error) {
	f.calls.Add(1)
	_, ok := ctx.Deadline()
	f.sawDeadline.Store(ok)
	if f.block.Load() {
		<-ctx.Done()
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	return &pb.ListRecommendationsResponse{ProductIds: f.ids}, nil
}

type shop struct {
	fe              *frontendServer
	currency        *fakeCurrency
	catalog         *fakeCatalog
	checkout        *fakeCheckout
	recommendations *fakeRecommendations
}

// newShop starts the fake services on an in-memory listener and returns a frontend wired to them.
func newShop(t *testing.T, currency *fakeCurrency) *shop {
	t.Helper()
	s := &shop{currency: currency, catalog: &fakeCatalog{}, checkout: &fakeCheckout{}, recommendations: &fakeRecommendations{}}

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pb.RegisterProductCatalogServiceServer(srv, s.catalog)
	pb.RegisterCartServiceServer(srv, fakeCart{})
	pb.RegisterShippingServiceServer(srv, fakeShipping{})
	pb.RegisterCheckoutServiceServer(srv, s.checkout)
	pb.RegisterRecommendationServiceServer(srv, s.recommendations)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	s.fe = &frontendServer{
		productCatalogSvcConn: conn,
		cartSvcConn:           conn,
		recommendationSvcConn: conn,
		checkoutSvcConn:       conn,
		shippingSvcConn:       conn,
		adSvcConn:             conn,
		currency:              currency,
	}
	return s
}

// serve runs one handler like the middleware in main.go would, and returns status and body.
func serve(t *testing.T, h http.HandlerFunc, r *http.Request, currency string) (int, string) {
	t.Helper()
	if currency != "" {
		r.AddCookie(&http.Cookie{Name: cookieCurrency, Value: currency})
	}
	logger := logrus.New()
	logger.Out = io.Discard
	ctx := context.WithValue(r.Context(), ctxKeySessionID{}, "test-session")
	ctx = context.WithValue(ctx, ctxKeyRequestID{}, "test-request")
	ctx = context.WithValue(ctx, ctxKeyLog{}, logrus.FieldLogger(logger))
	w := httptest.NewRecorder()
	h(w, r.WithContext(ctx))
	return w.Code, w.Body.String()
}

func currencyDown() *fakeCurrency {
	return &fakeCurrency{listErr: errCurrencyDown, convertErr: errCurrencyDown}
}

func healthyCurrency() *fakeCurrency {
	return &fakeCurrency{currencies: []string{"EUR", "USD"}}
}

const notice = `id="currency-fallback-notice"`

func TestBrowsePagesFallBackToUSDWhenCurrencyFails(t *testing.T) {
	product := mux.SetURLVars(httptest.NewRequest(http.MethodGet, "/product/OLJCESPC7Z", nil), map[string]string{"id": "OLJCESPC7Z"})
	pages := map[string]struct {
		handler func(*frontendServer) http.HandlerFunc
		req     *http.Request
		want    []string
	}{
		"home": {
			handler: func(fe *frontendServer) http.HandlerFunc { return fe.homeHandler },
			req:     httptest.NewRequest(http.MethodGet, "/", nil),
			want:    []string{"$19.99", "$18.99"},
		},
		"product": {
			handler: func(fe *frontendServer) http.HandlerFunc { return fe.productHandler },
			req:     product,
			want:    []string{"$19.99"},
		},
		"cart": {
			handler: func(fe *frontendServer) http.HandlerFunc { return fe.viewCartHandler },
			req:     httptest.NewRequest(http.MethodGet, "/cart", nil),
			// 2 × 19.99 + 18.99 + shipping 8.99, all in USD.
			want: []string{"$39.98", "$18.99", "$8.99", "$67.96", `id="currency-fallback-checkout"`},
		},
	}
	for name, page := range pages {
		t.Run(name, func(t *testing.T) {
			s := newShop(t, currencyDown())
			code, body := serve(t, page.handler(s.fe), page.req, "EUR")
			if code != http.StatusOK {
				t.Fatalf("status %d, want 200; body:\n%s", code, body)
			}
			for _, want := range append(page.want, notice) {
				if !strings.Contains(body, want) {
					t.Errorf("page does not contain %q", want)
				}
			}
			if strings.Contains(body, "€") {
				t.Error("page shows a euro amount while the currency service is down")
			}
		})
	}
}

func TestBrowsePagesConvertWhenCurrencyIsHealthy(t *testing.T) {
	s := newShop(t, healthyCurrency())

	code, body := serve(t, s.fe.homeHandler, httptest.NewRequest(http.MethodGet, "/", nil), "EUR")
	if code != http.StatusOK || !strings.Contains(body, "€39.98") || strings.Contains(body, notice) {
		t.Errorf("home: status %d, euro price shown %v, notice shown %v; want 200, true, false",
			code, strings.Contains(body, "€39.98"), strings.Contains(body, notice))
	}

	// Cart in EUR: items 2 × 39.98 and 37.98, shipping 17.98, total 135.92 (the fake doubles).
	code, body = serve(t, s.fe.viewCartHandler, httptest.NewRequest(http.MethodGet, "/cart", nil), "EUR")
	if code != http.StatusOK {
		t.Fatalf("cart: status %d, want 200", code)
	}
	for _, want := range []string{"€79.96", "€37.98", "€17.98", "€135.92"} {
		if !strings.Contains(body, want) {
			t.Errorf("cart does not contain %q", want)
		}
	}
	if strings.Contains(body, notice) {
		t.Error("cart shows the fallback notice while the currency service is healthy")
	}
}

func TestCartStaysInOneCurrencyWhenALaterConversionFails(t *testing.T) {
	// The first item converts, the second fails: the whole cart, total included, must be in USD.
	fake := healthyCurrency()
	fake.failFrom = 2
	s := newShop(t, fake)

	code, body := serve(t, s.fe.viewCartHandler, httptest.NewRequest(http.MethodGet, "/cart", nil), "EUR")
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200; body:\n%s", code, body)
	}
	if strings.Contains(body, "€") || !strings.Contains(body, "$67.96") || !strings.Contains(body, notice) {
		t.Errorf("want a USD-only cart with total $67.96 and the notice; euro shown %v, total shown %v, notice shown %v",
			strings.Contains(body, "€"), strings.Contains(body, "$67.96"), strings.Contains(body, notice))
	}
}

func TestCriticalDependenciesStillFail(t *testing.T) {
	// Fail-open covers the currency service only. Without the catalog there is nothing to show.
	s := newShop(t, healthyCurrency())
	s.catalog.err = status.Error(codes.Unavailable, "catalog down")
	code, _ := serve(t, s.fe.homeHandler, httptest.NewRequest(http.MethodGet, "/", nil), "EUR")
	if code != http.StatusInternalServerError {
		t.Errorf("home without the product catalog: status %d, want 500", code)
	}
}

func checkoutRequest() *http.Request {
	form := url.Values{
		"email":                        {"someone@example.com"},
		"street_address":               {"1600 Amphitheatre Parkway"},
		"zip_code":                     {"94043"},
		"city":                         {"Mountain View"},
		"state":                        {"CA"},
		"country":                      {"United States"},
		"credit_card_number":           {"4432801561520454"},
		"credit_card_expiration_month": {"1"},
		"credit_card_expiration_year":  {"2030"},
		"credit_card_cvv":              {"672"},
	}
	r := httptest.NewRequest(http.MethodPost, "/cart/checkout", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func TestCheckoutFailsWhenCheckoutCannotConvert(t *testing.T) {
	// checkoutservice converts every price before it charges the card, so with the currency
	// service down it refuses the order. The frontend must report that, not pretend success.
	s := newShop(t, currencyDown())
	s.checkout.err = status.Error(codes.Internal, "failed to convert currency")

	code, body := serve(t, s.fe.placeOrderHandler, checkoutRequest(), "EUR")
	if code != http.StatusInternalServerError {
		t.Errorf("status %d, want 500", code)
	}
	if strings.Contains(body, "Your order is complete") {
		t.Error("order confirmation shown although checkout failed")
	}
}

func TestPlacedOrderIsConfirmedWhenCurrencyListFails(t *testing.T) {
	// The order went through before the currency service failed. Upstream answered 500 here,
	// which invites the user to pay a second time.
	s := newShop(t, currencyDown())

	code, body := serve(t, s.fe.placeOrderHandler, checkoutRequest(), "EUR")
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200; body:\n%s", code, body)
	}
	if !strings.Contains(body, "Your order is complete") || !strings.Contains(body, "order-1") {
		t.Error("order confirmation missing")
	}
	if strings.Contains(body, notice) {
		t.Error("confirmation shows the USD notice, but its amounts are what checkout charged")
	}
	if n := s.checkout.orders.Load(); n != 1 {
		t.Errorf("placed %d orders, want 1", n)
	}
}

func TestAssistantPageSurvivesCurrencyFailure(t *testing.T) {
	s := newShop(t, currencyDown())
	code, _ := serve(t, s.fe.assistantHandler, httptest.NewRequest(http.MethodGet, "/assistant", nil), "EUR")
	if code != http.StatusOK {
		t.Errorf("status %d, want 200", code)
	}
}

func productRequest() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/product/OLJCESPC7Z", nil)
	return mux.SetURLVars(r, map[string]string{"id": "OLJCESPC7Z"})
}

func cartRequest() *http.Request {
	return httptest.NewRequest(http.MethodGet, "/cart", nil)
}

func productPage(fe *frontendServer) http.HandlerFunc { return fe.productHandler }

func cartPage(fe *frontendServer) http.HandlerFunc { return fe.viewCartHandler }

const recommendationsSection = "You May Also Like"

func TestProductPageShowsRecommendations(t *testing.T) {
	s := newShop(t, healthyCurrency())
	s.recommendations.ids = []string{"66VCHSJNUP"}

	code, body := serve(t, s.fe.productHandler, productRequest(), "USD")
	if code != http.StatusOK || !strings.Contains(body, recommendationsSection) || !strings.Contains(body, "Tank Top") {
		t.Errorf("status %d, recommendations shown %v; want 200 and the Tank Top recommendation",
			code, strings.Contains(body, recommendationsSection))
	}
}

func TestSlowRecommendationsDoNotHoldUpPages(t *testing.T) {
	// Faults that slow recommendationservice must cost a page at most recommendationsTimeout,
	// and the page renders without recommendations.
	pages := map[string]struct {
		handler func(*frontendServer) http.HandlerFunc
		req     func() *http.Request
	}{
		"product": {productPage, productRequest},
		"cart":    {cartPage, cartRequest},
	}
	for name, page := range pages {
		t.Run(name, func(t *testing.T) {
			s := newShop(t, healthyCurrency())
			s.recommendations.ids = []string{"66VCHSJNUP"}
			s.recommendations.block.Store(true)

			start := time.Now()
			code, body := serve(t, page.handler(s.fe), page.req(), "USD")
			elapsed := time.Since(start)

			if code != http.StatusOK {
				t.Fatalf("status %d, want 200", code)
			}
			if strings.Contains(body, recommendationsSection) {
				t.Error("recommendations shown although the call timed out")
			}
			// Without the deadline the fake would block until the test binary times out, so finishing
			// at all is the main check; the bound below leaves room for a slow CI machine.
			if elapsed > recommendationsTimeout+2*time.Second {
				t.Errorf("page took %v; want about %v at most", elapsed, recommendationsTimeout)
			}
			if s.recommendations.calls.Load() == 1 && !s.recommendations.sawDeadline.Load() {
				t.Error("the recommendation call reached the service without a deadline")
			}
		})
	}
}
