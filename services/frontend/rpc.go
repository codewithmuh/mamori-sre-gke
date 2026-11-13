// Copyright 2018 Google LLC
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
// Modified by the mamori-sre-gke project: currency calls go through fe.currency,
// getShippingQuote returns the quote in USD (the caller converts it together with the other
// prices of the page), and getRecommendations has a deadline and looks up only the products it
// shows, in parallel, keeping those that arrive. See CHANGES.md.

package main

import (
	"context"
	"strings"
	"sync"
	"time"

	pb "github.com/GoogleCloudPlatform/microservices-demo/src/frontend/genproto"

	"github.com/pkg/errors"
)

const (
	avoidNoopCurrencyConversionRPC = false
)

func (fe *frontendServer) getCurrencies(ctx context.Context) ([]string, error) {
	currs, err := fe.currency.GetSupportedCurrencies(ctx, &pb.Empty{})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range currs.CurrencyCodes {
		if _, ok := whitelistedCurrencies[c]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}

func (fe *frontendServer) getProducts(ctx context.Context) ([]*pb.Product, error) {
	resp, err := pb.NewProductCatalogServiceClient(fe.productCatalogSvcConn).
		ListProducts(ctx, &pb.Empty{})
	return resp.GetProducts(), err
}

func (fe *frontendServer) getProduct(ctx context.Context, id string) (*pb.Product, error) {
	resp, err := pb.NewProductCatalogServiceClient(fe.productCatalogSvcConn).
		GetProduct(ctx, &pb.GetProductRequest{Id: id})
	return resp, err
}

func (fe *frontendServer) getCart(ctx context.Context, userID string) ([]*pb.CartItem, error) {
	resp, err := pb.NewCartServiceClient(fe.cartSvcConn).GetCart(ctx, &pb.GetCartRequest{UserId: userID})
	return resp.GetItems(), err
}

func (fe *frontendServer) emptyCart(ctx context.Context, userID string) error {
	_, err := pb.NewCartServiceClient(fe.cartSvcConn).EmptyCart(ctx, &pb.EmptyCartRequest{UserId: userID})
	return err
}

func (fe *frontendServer) insertCart(ctx context.Context, userID, productID string, quantity int32) error {
	_, err := pb.NewCartServiceClient(fe.cartSvcConn).AddItem(ctx, &pb.AddItemRequest{
		UserId: userID,
		Item: &pb.CartItem{
			ProductId: productID,
			Quantity:  quantity},
	})
	return err
}

func (fe *frontendServer) convertCurrency(ctx context.Context, money *pb.Money, currency string) (*pb.Money, error) {
	if avoidNoopCurrencyConversionRPC && money.GetCurrencyCode() == currency {
		return money, nil
	}
	return fe.currency.Convert(ctx, &pb.CurrencyConversionRequest{
		From:   money,
		ToCode: currency})
}

// getShippingQuote returns the shipping cost in USD. The cart page converts it together with the
// item prices, so that one failed conversion puts the whole page in USD.
func (fe *frontendServer) getShippingQuote(ctx context.Context, items []*pb.CartItem) (*pb.Money, error) {
	quote, err := pb.NewShippingServiceClient(fe.shippingSvcConn).GetQuote(ctx,
		&pb.GetQuoteRequest{
			Address: nil,
			Items:   items})
	if err != nil {
		return nil, err
	}
	if quote.GetCostUsd() == nil {
		return nil, errors.New("shipping quote has no cost")
	}
	return quote.GetCostUsd(), nil
}

// recommendationsTimeout bounds the whole recommendations lookup: the recommendation call and the
// catalog lookups of the products it returns. Recommendations are optional (every caller renders
// the page without them), so a slow recommendationservice must not hold up the product and cart
// pages; upstream waited without a deadline. 250 ms is half of the browse journey's 500 ms latency
// threshold (docs/prd.md), an estimate to tune with measured latency. getAd uses 100 ms the same way.
const recommendationsTimeout = 250 * time.Millisecond

// maxRecommendations is how many recommended products fit the pages (upstream's "first four").
const maxRecommendations = 4

func (fe *frontendServer) getRecommendations(ctx context.Context, userID string, productIDs []string) ([]*pb.Product, error) {
	ctx, cancel := context.WithTimeout(ctx, recommendationsTimeout)
	defer cancel()

	resp, err := pb.NewRecommendationServiceClient(fe.recommendationSvcConn).ListRecommendations(ctx,
		&pb.ListRecommendationsRequest{UserId: userID, ProductIds: productIDs})
	if err != nil {
		return nil, err
	}
	// Look up only the products the page shows, all at once, and keep the ones that arrive within
	// the deadline: one slow or missing product must not cost the page every recommendation.
	// Upstream fetched all of them one after another and kept the first four only afterwards.
	ids := resp.GetProductIds()
	if len(ids) > maxRecommendations {
		ids = ids[:maxRecommendations]
	}
	products := make([]*pb.Product, len(ids))
	errs := make([]error, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Go(func() { products[i], errs[i] = fe.getProduct(ctx, id) })
	}
	wg.Wait()

	out := make([]*pb.Product, 0, len(ids))
	var firstErr error
	var failed []string
	for i, p := range products {
		if errs[i] != nil {
			if firstErr == nil {
				firstErr = errs[i]
			}
			failed = append(failed, ids[i])
			continue
		}
		out = append(out, p)
	}
	if firstErr != nil {
		// The callers log this and still show the products in out.
		return out, errors.Wrapf(firstErr, "failed to get recommended product info (#%s)", strings.Join(failed, ", #"))
	}
	return out, nil
}

func (fe *frontendServer) getAd(ctx context.Context, ctxKeys []string) ([]*pb.Ad, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Millisecond*100)
	defer cancel()

	resp, err := pb.NewAdServiceClient(fe.adSvcConn).GetAds(ctx, &pb.AdRequest{
		ContextKeys: ctxKeys,
	})
	return resp.GetAds(), errors.Wrap(err, "failed to get ads")
}
