package main

import (
	"net/http"
	"net/url"
	"slices"
	"testing"
)

func TestProductIDs(t *testing.T) {
	page := []byte(`<html><body>
<a href="/">home</a><a href="/cart">cart</a>
<a href="/product/OLJCESPC7Z"><img src="/static/img/products/sunglasses.jpg"></a>
<a href="/product/66VCHSJNUP">Tank top</a>
<a href="/product/OLJCESPC7Z">again</a>
<A HREF="/shop/product/1YMWWN1N4O?ref=ad#top">ad</A>
<a href="/product/">empty</a>
<a href="/product/has%20space">bad id</a>
<a href="/product-meta/OLJCESPC7Z">meta</a>
<a>no href</a>
<a href="http://[::1">broken</a>
<link href="/product/L9ECAV7KIM">
</body></html>`)
	want := []string{"OLJCESPC7Z", "66VCHSJNUP", "1YMWWN1N4O"}
	if got := productIDs(page); !slices.Equal(got, want) {
		t.Errorf("productIDs() = %v, want %v", got, want)
	}
	if got := productIDs(nil); len(got) != 0 {
		t.Errorf("productIDs(nil) = %v", got)
	}
}

func TestFindForm(t *testing.T) {
	page := []byte(`<html><body>
<form method="POST" action="/cart/empty"><input type="hidden" name="wrong" value="form"></form>
<form class="cart-checkout-form" action="/cart/checkout" method="POST">
  <input type="email" name="email" value="someone@example.com" required>
  <input type="text" name="quoted" value="a &amp; b">
  <input type="password" name="credit_card_cvv" value="672">
  <input type="text" value="no name">
  <input type="text" name="off" value="x" disabled>
  <input type="checkbox" name="news" value="yes">
  <input type="checkbox" name="terms" checked>
  <input type="radio" name="speed" value="slow">
  <input type="radio" name="speed" value="fast" checked>
  <input type="submit" name="go" value="Place Order">
  <input type="image" name="img" src="x.png">
  <select name="credit_card_expiration_month">
    <option value="1">January</option>
    <option value="2">February</option>
  </select>
  <select name="credit_card_expiration_year">
    <option value="2026"
    >2026</option><option value="2027"
        selected="selected"
    >2027</option>
  </select>
  <select name="quantity"><option>  1 </option><option>2</option></select>
  <select name="none"></select>
  <select name="disabled" disabled><option value="x">x</option></select>
  <select><option value="nameless">x</option></select>
  <textarea name="note">leave at the door</textarea>
  <button type="submit">Place Order</button>
</form>
<form action="/cart/checkout"><input name="second" value="form"></form>
</body></html>`)

	got, ok := findForm(page, "/cart/checkout")
	if !ok {
		t.Fatal("findForm() found no form")
	}
	want := url.Values{
		"email":                        {"someone@example.com"},
		"quoted":                       {"a & b"},
		"credit_card_cvv":              {"672"},
		"terms":                        {"on"},
		"speed":                        {"fast"},
		"credit_card_expiration_month": {"1"},
		"credit_card_expiration_year":  {"2027"},
		"quantity":                     {"1"},
		"note":                         {"leave at the door"},
	}
	if got.Encode() != want.Encode() {
		t.Errorf("findForm() =\n %v\nwant\n %v", got, want)
	}

	if _, ok := findForm(page, "/cart"); ok {
		t.Error(`findForm("/cart") matched a form whose action is /cart/empty or /cart/checkout`)
	}
	if _, ok := findForm([]byte(`<form action="/cart/checkout"><input name="a" value="b">`), "/cart/checkout"); ok {
		t.Error("findForm() accepted a form that never ends")
	}
	product, ok := findForm([]byte(`<form method="POST" action="/shop/cart"><input type="hidden" name="product_id" value="X1"/></form>`), "/cart")
	if !ok || product.Get("product_id") != "X1" {
		t.Errorf("findForm() with a base path = %v, %v", product, ok)
	}
	if actionMatches("http://[::1", "/cart") {
		t.Error("actionMatches() accepted a broken URL")
	}
}

func TestPreText(t *testing.T) {
	page := []byte(`<html><h1>Uh, oh!</h1><p>HTTP Status: 500</p><pre class="border">
    could not retrieve currencies: rpc error
</pre><pre>second</pre></html>`)
	if got := preText(page); got != "\n    could not retrieve currencies: rpc error\n" {
		t.Errorf("preText() = %q", got)
	}
	if got := preText([]byte("<html><p>no pre</p></html>")); got != "" {
		t.Errorf("preText() without <pre> = %q", got)
	}
}

func TestResponseDetail(t *testing.T) {
	tests := []struct {
		contentType string
		body        string
		want        string
	}{
		{"text/html; charset=utf-8", "<pre>\n  a\n  b  </pre>", "a b"},
		{"text/html", "<p>no detail</p>", ""},
		{"text/plain", "upstream connect error or disconnect/reset before headers. reset reason: connection failure\n", "upstream connect error or disconnect/reset before headers. reset reason: connection failure"},
		{"", "fault filter abort", "fault filter abort"},
	}
	for _, tt := range tests {
		header := http.Header{"Content-Type": {tt.contentType}}
		if got := responseDetail(header, []byte(tt.body)); got != tt.want {
			t.Errorf("responseDetail(%q, %q) = %q, want %q", tt.contentType, tt.body, got, tt.want)
		}
	}
	if got := truncate("ééé", 2); got != "éé..." {
		t.Errorf("truncate() = %q", got)
	}
}
