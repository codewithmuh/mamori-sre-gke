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
	"fmt"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	pb "github.com/GoogleCloudPlatform/microservices-demo/src/frontend/genproto"
	"github.com/GoogleCloudPlatform/microservices-demo/src/frontend/money"
)

// fallbackCurrency is what a page shows when the currency service fails: the product catalog and
// the shipping service already price everything in USD, so no conversion is needed.
const fallbackCurrency = "USD"

// currencyView does the currency calls of one page.
//
// The currency service only changes how prices are shown, so browsing should not depend on it.
// Upstream answers 500 on the home, product and cart pages when it fails; with this view those
// pages show USD prices and a notice instead.
//
// The first failure switches the whole page to USD. Later calls are skipped (each would most
// likely fail too and only add latency), and prices that were already converted are dropped, so a
// page never mixes currencies. That also keeps money.Sum, which panics on mixed currencies, safe.
//
// With fe.currencyTimeout set, the page's currency calls share that budget. Only time spent inside
// currency calls counts: a slow catalog or recommendation service between two currency calls must
// not end up blamed on the currency service.
type currencyView struct {
	fe     *frontendServer
	ctx    context.Context // the request's context
	log    logrus.FieldLogger
	want   string        // the user's currency (cookie)
	budget time.Duration // 0: no budget
	spent  time.Duration // time spent inside currency calls so far
	err    error         // the first failure; once set, the page is in USD
}

// errBudgetSpent ends a page's currency calls once their time budget is used up.
var errBudgetSpent = errors.New("currency time budget (CURRENCY_TIMEOUT) used up")

// newCurrencyView prepares the currency calls of one page. It makes no call itself.
func (fe *frontendServer) newCurrencyView(ctx context.Context, log logrus.FieldLogger, want string) *currencyView {
	return &currencyView{fe: fe, ctx: ctx, log: log, want: want, budget: fe.currencyTimeout}
}

// call runs one currency call with what is left of the page's budget as its deadline.
func (v *currencyView) call(do func(ctx context.Context) error) error {
	ctx := v.ctx
	if v.budget > 0 {
		left := v.budget - v.spent
		if left <= 0 {
			return errBudgetSpent
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, left)
		defer cancel()
	}
	start := time.Now()
	err := do(ctx)
	v.spent += time.Since(start)
	return err
}

// fallback reports whether the page shows USD because a currency call failed.
func (v *currencyView) fallback() bool { return v.err != nil }

// currency is the currency of the page's prices.
func (v *currencyView) currency() string {
	if v.fallback() {
		return fallbackCurrency
	}
	return v.want
}

// supported returns the currencies for the currency selector: the service's list, or only USD
// once a call has failed.
func (v *currencyView) supported() []string {
	if v.fallback() {
		return []string{fallbackCurrency}
	}
	var currencies []string
	err := v.call(func(ctx context.Context) (err error) {
		currencies, err = v.fe.getCurrencies(ctx)
		return err
	})
	if err != nil {
		v.fail(errors.Wrap(err, "could not retrieve currencies"))
		return []string{fallbackCurrency}
	}
	return currencies
}

// convert returns the given USD prices in the user's currency, in the same order. If any
// conversion fails, now or earlier on the page, it returns the USD prices unchanged.
func (v *currencyView) convert(usd ...*pb.Money) []*pb.Money {
	if v.fallback() {
		return usd
	}
	out := make([]*pb.Money, len(usd))
	for i, m := range usd {
		var converted *pb.Money
		err := v.call(func(ctx context.Context) (err error) {
			converted, err = v.fe.convertCurrency(ctx, m, v.want)
			return err
		})
		if err == nil {
			err = checkConverted(converted, v.want)
		}
		if err != nil {
			v.fail(errors.Wrapf(err, "could not convert %s to %s", m.GetCurrencyCode(), v.want))
			return usd
		}
		out[i] = converted
	}
	return out
}

// fail switches the page to USD and logs why, once per page. If the request itself is over
// (the client went away, or the request's own deadline passed), the currency service is not to
// blame: that is logged at debug level only, so it doesn't send responders the wrong way.
func (v *currencyView) fail(err error) {
	v.err = err
	log := v.log.WithField("error", err).WithField("currency", v.want)
	if v.ctx.Err() != nil {
		log.Debug("request ended during a currency call; not a currency failure")
		return
	}
	log.Warn("currency service failed; falling back to USD")
}

// checkConverted rejects a conversion result the page can't add up: no result, another currency
// than asked for, or an invalid amount.
func checkConverted(m *pb.Money, want string) error {
	switch {
	case m == nil:
		return errors.New("currency service returned no amount")
	case m.GetCurrencyCode() != want:
		return fmt.Errorf("currency service returned %q, want %q", m.GetCurrencyCode(), want)
	case !money.IsValid(pb.Money{Units: m.GetUnits(), Nanos: m.GetNanos(), CurrencyCode: m.GetCurrencyCode()}):
		return errors.New("currency service returned an invalid amount")
	}
	return nil
}

// parseCurrencyTimeout reads CURRENCY_TIMEOUT: a Go duration such as "300ms" that bounds the time
// one page spends on currency calls before it falls back to USD. Empty or "0" means no bound, as
// upstream.
func parseCurrencyTimeout(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("CURRENCY_TIMEOUT=%q: want a duration such as 300ms, or empty for none", s)
	}
	return d, nil
}
