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

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/GoogleCloudPlatform/microservices-demo/src/frontend/genproto"
)

// fakeCurrency is a pb.CurrencyServiceClient that converts by doubling the amount into the asked
// currency, and fails on demand.
type fakeCurrency struct {
	mu sync.Mutex

	currencies []string
	listErr    error
	convertErr error
	failFrom   int  // fail Convert call number failFrom (1-based) and every later one; 0: never
	block      bool // Convert waits until its context ends
	result     func(*pb.CurrencyConversionRequest) *pb.Money

	listCalls    int
	convertCalls int
	sawDeadline  bool
}

func (f *fakeCurrency) GetSupportedCurrencies(ctx context.Context, _ *pb.Empty, _ ...grpc.CallOption) (*pb.GetSupportedCurrenciesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	_, f.sawDeadline = ctx.Deadline()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return &pb.GetSupportedCurrenciesResponse{CurrencyCodes: f.currencies}, nil
}

func (f *fakeCurrency) Convert(ctx context.Context, in *pb.CurrencyConversionRequest, _ ...grpc.CallOption) (*pb.Money, error) {
	f.mu.Lock()
	f.convertCalls++
	n := f.convertCalls
	_, f.sawDeadline = ctx.Deadline()
	block, convertErr, failFrom, result := f.block, f.convertErr, f.failFrom, f.result
	f.mu.Unlock()

	switch {
	case block:
		<-ctx.Done()
		return nil, status.FromContextError(ctx.Err()).Err()
	case convertErr != nil:
		return nil, convertErr
	case failFrom > 0 && n >= failFrom:
		return nil, status.Error(codes.Unavailable, "currency service down")
	case result != nil:
		return result(in), nil
	}
	return double(in.GetFrom(), in.GetToCode()), nil
}

func (f *fakeCurrency) calls() (list, convert int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls, f.convertCalls
}

// double returns 2 × m in currency code.
func double(m *pb.Money, code string) *pb.Money {
	nanos := int64(m.GetNanos()) * 2
	return &pb.Money{
		CurrencyCode: code,
		Units:        m.GetUnits()*2 + nanos/1e9,
		Nanos:        int32(nanos % 1e9),
	}
}

func usd(units int64, nanos int32) *pb.Money {
	return &pb.Money{CurrencyCode: "USD", Units: units, Nanos: nanos}
}

var errCurrencyDown = status.Error(codes.Unavailable, "currency service down")

func newTestView(t *testing.T, fake *fakeCurrency, timeout time.Duration, want string) (*currencyView, *logtest.Hook) {
	t.Helper()
	logger, hook := logtest.NewNullLogger()
	fe := &frontendServer{currency: fake, currencyTimeout: timeout}
	v := fe.newCurrencyView(context.Background(), logger, want)
	t.Cleanup(v.close)
	return v, hook
}

func warnings(hook *logtest.Hook) int {
	n := 0
	for _, e := range hook.AllEntries() {
		if e.Level == logrus.WarnLevel {
			n++
		}
	}
	return n
}

func TestCurrencyViewConvertsWhenHealthy(t *testing.T) {
	fake := &fakeCurrency{currencies: []string{"EUR", "JPY", "USD", "XXX"}}
	v, hook := newTestView(t, fake, 0, "EUR")

	got := v.supported()
	// Only the currencies the frontend allows (whitelistedCurrencies), as upstream.
	if want := []string{"EUR", "JPY", "USD"}; !equalStrings(got, want) {
		t.Errorf("supported() = %v, want %v", got, want)
	}
	prices := v.convert(usd(19, 990000000), usd(5, 0))
	want := []*pb.Money{
		{CurrencyCode: "EUR", Units: 39, Nanos: 980000000},
		{CurrencyCode: "EUR", Units: 10, Nanos: 0},
	}
	for i := range want {
		if !moneyEqual(prices[i], want[i]) {
			t.Errorf("price %d = %v, want %v", i, prices[i], want[i])
		}
	}
	if v.fallback() || v.currency() != "EUR" {
		t.Errorf("fallback() = %v, currency() = %q; want false, EUR", v.fallback(), v.currency())
	}
	if n := warnings(hook); n != 0 {
		t.Errorf("logged %d warnings, want none", n)
	}
	if fake.sawDeadline {
		t.Error("currency calls had a deadline although CURRENCY_TIMEOUT is unset")
	}
}

func TestCurrencyViewFallsBackWhenListFails(t *testing.T) {
	fake := &fakeCurrency{listErr: errCurrencyDown}
	v, hook := newTestView(t, fake, 0, "JPY")

	if got := v.supported(); !equalStrings(got, []string{"USD"}) {
		t.Errorf("supported() = %v, want [USD]", got)
	}
	in := []*pb.Money{usd(19, 990000000), usd(5, 0)}
	got := v.convert(in...)
	for i := range in {
		if got[i] != in[i] {
			t.Errorf("price %d = %v, want the USD price %v unchanged", i, got[i], in[i])
		}
	}
	if _, convert := fake.calls(); convert != 0 {
		t.Errorf("made %d Convert calls after the service failed, want 0", convert)
	}
	if !v.fallback() || v.currency() != "USD" {
		t.Errorf("fallback() = %v, currency() = %q; want true, USD", v.fallback(), v.currency())
	}
	if n := warnings(hook); n != 1 {
		t.Errorf("logged %d warnings, want 1", n)
	}
}

func TestCurrencyViewDropsConvertedPricesWhenOneConversionFails(t *testing.T) {
	fake := &fakeCurrency{currencies: []string{"EUR", "USD"}, failFrom: 3}
	v, hook := newTestView(t, fake, 0, "EUR")

	in := []*pb.Money{usd(1, 0), usd(2, 0), usd(3, 0), usd(4, 0), usd(5, 0)}
	got := v.convert(in...)
	for i := range in {
		if got[i] != in[i] {
			t.Errorf("price %d = %v, want the USD price %v (a page never mixes currencies)", i, got[i], in[i])
		}
	}
	if _, convert := fake.calls(); convert != 3 {
		t.Errorf("made %d Convert calls, want 3 (stop at the first failure)", convert)
	}

	// The rest of the page stays in USD without calling the service again.
	more := usd(6, 0)
	if got := v.convert(more); got[0] != more {
		t.Errorf("later convert() = %v, want the USD price", got[0])
	}
	if got := v.supported(); !equalStrings(got, []string{"USD"}) {
		t.Errorf("later supported() = %v, want [USD]", got)
	}
	list, convert := fake.calls()
	if list != 0 || convert != 3 {
		t.Errorf("calls after the failure: list %d, convert %d; want 0, 3", list, convert)
	}
	if n := warnings(hook); n != 1 {
		t.Errorf("logged %d warnings, want exactly 1 per page", n)
	}
}

func TestCurrencyViewRejectsUnusableResults(t *testing.T) {
	cases := map[string]func(*pb.CurrencyConversionRequest) *pb.Money{
		"no amount":      func(*pb.CurrencyConversionRequest) *pb.Money { return nil },
		"other currency": func(*pb.CurrencyConversionRequest) *pb.Money { return &pb.Money{CurrencyCode: "GBP", Units: 1} },
		"invalid amount": func(in *pb.CurrencyConversionRequest) *pb.Money {
			return &pb.Money{CurrencyCode: in.GetToCode(), Units: 1, Nanos: -5}
		},
	}
	for name, result := range cases {
		t.Run(name, func(t *testing.T) {
			fake := &fakeCurrency{currencies: []string{"EUR"}, result: result}
			v, _ := newTestView(t, fake, 0, "EUR")
			in := usd(7, 500000000)
			if got := v.convert(in); got[0] != in || !v.fallback() {
				t.Errorf("convert() = %v, fallback() = %v; want the USD price and true", got[0], v.fallback())
			}
		})
	}
}

func TestCurrencyViewTimeoutFallsBack(t *testing.T) {
	fake := &fakeCurrency{currencies: []string{"EUR"}, block: true}
	v, _ := newTestView(t, fake, 50*time.Millisecond, "EUR")

	start := time.Now()
	in := usd(3, 0)
	got := v.convert(in, usd(4, 0))
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("convert() took %v with a 50ms budget", elapsed)
	}
	if got[0] != in || !v.fallback() {
		t.Errorf("convert() = %v, fallback() = %v; want the USD price and true", got[0], v.fallback())
	}
	if !fake.sawDeadline {
		t.Error("currency call had no deadline although CURRENCY_TIMEOUT is set")
	}
	if _, convert := fake.calls(); convert != 1 {
		t.Errorf("made %d Convert calls, want 1 (the budget is per page, not per call)", convert)
	}
}

func TestParseCurrencyTimeout(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"300ms", 300 * time.Millisecond, false},
		{"1s", time.Second, false},
		{"-1s", 0, true},
		{"300", 0, true},
		{"soon", 0, true},
	}
	for _, c := range cases {
		got, err := parseCurrencyTimeout(c.in)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("parseCurrencyTimeout(%q) = %v, %v; want %v, error %v", c.in, got, err, c.want, c.wantErr)
		}
	}
}

func TestCheckConverted(t *testing.T) {
	if err := checkConverted(&pb.Money{CurrencyCode: "EUR", Units: 1, Nanos: 5}, "EUR"); err != nil {
		t.Errorf("valid amount rejected: %v", err)
	}
	if err := checkConverted(nil, "EUR"); err == nil {
		t.Error("nil amount accepted")
	}
	if err := checkConverted(&pb.Money{CurrencyCode: "USD"}, "EUR"); err == nil {
		t.Error("other currency accepted")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func moneyEqual(a, b *pb.Money) bool {
	return a.GetCurrencyCode() == b.GetCurrencyCode() && a.GetUnits() == b.GetUnits() && a.GetNanos() == b.GetNanos()
}
