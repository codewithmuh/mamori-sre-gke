package main

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPromAPIQueryAgainstFake(t *testing.T) {
	fake := newFakeProm(t)
	srv := fake.serve(t)
	p, err := newPrometheus(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	at := fixtureStart.Add(2 * fixtureStepDuration)
	got, err := p.Query(context.Background(), "sum by (cuj) (increase(k6_mamori_cuj_requests_total[240s]))", at)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[1].Labels["cuj"] != "browse" || got[1].Value != 1334 {
		t.Errorf("Query() = %+v", got)
	}
	// The time travels as Unix seconds with milliseconds.
	if q := fake.queryAts[0]; !q.Equal(at) {
		t.Errorf("server saw time %s, want %s", q, at)
	}
	fake.failOn["boom"] = errors.New("bad query")
	if _, err := p.Query(context.Background(), "boom", at); err == nil || !strings.Contains(err.Error(), "HTTP 422: execution: bad query") {
		t.Errorf("error = %v", err)
	}
}

func TestParseQueryResponse(t *testing.T) {
	got, err := parseQueryResponse(200, []byte(`{"status":"success","data":{"resultType":"scalar","result":[1700000000.5,"NaN"]}}`))
	if err != nil || len(got) != 1 || !math.IsNaN(got[0].Value) {
		t.Errorf("scalar = %+v, %v", got, err)
	}
	got, err = parseQueryResponse(200, []byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	if err != nil || len(got) != 0 {
		t.Errorf("empty vector = %+v, %v", got, err)
	}

	errorsWanted := []struct{ body, want string }{
		// Google's front end answers authorization errors in its own format.
		{`{"error":{"code":403,"message":"Permission monitoring.timeSeries.list denied","status":"PERMISSION_DENIED"}}`, `HTTP 403: {"error"`},
		{`<html>bad gateway</html>`, "HTTP 403: <html>"},
		{`{"status":"error","errorType":"bad_data","error":"parse error"}`, "bad_data: parse error"},
		{`{"status":"success","data":{"resultType":"matrix","result":[]}}`, `result type "matrix"`},
		{`{"status":"success","data":{"resultType":"vector","result":{}}}`, "vector result"},
		{`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1]}]}}`, "not a [time, value] pair"},
		{`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1, 2]}]}}`, "sample value"},
		{`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1, "x"]}]}}`, `sample value "x"`},
		{`{"status":"success","data":{"resultType":"scalar","result":{}}}`, "scalar result"},
		{`{"status":"success","data":{"resultType":"scalar","result":[1]}}`, "not a [time, value] pair"},
	}
	for _, tt := range errorsWanted {
		if _, err := parseQueryResponse(403, []byte(tt.body)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("body %s: error = %v, want %q", tt.body, err, tt.want)
		}
	}
}

func TestSnippetIsShortAndOneLine(t *testing.T) {
	long := strings.Repeat("word\n", 200)
	got := snippet([]byte(long))
	if len(got) != 303 || strings.Contains(got, "\n") || !strings.HasSuffix(got, "...") {
		t.Errorf("snippet = %q", got)
	}
}

func TestNewPrometheusRejectsBadURLs(t *testing.T) {
	for _, u := range []string{"localhost:9090", "ftp://x", "http://", "::"} {
		if _, err := newPrometheus(u); err == nil {
			t.Errorf("newPrometheus(%q) accepted", u)
		}
	}
}

func TestNewManagedPrometheus(t *testing.T) {
	orig := googleClient
	t.Cleanup(func() { googleClient = orig })

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	t.Cleanup(srv.Close)
	googleClient = func(context.Context) (*http.Client, error) {
		return &http.Client{Transport: headerTransport{"Authorization", "Bearer test"}}, nil
	}

	p, err := newManagedPrometheus(context.Background(), "mamori-lab-123456")
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://monitoring.googleapis.com/v1/projects/mamori-lab-123456/location/global/prometheus"; p.base != want {
		t.Errorf("base = %s, want %s", p.base, want)
	}
	if p.client.Timeout != clientTimeout {
		t.Errorf("timeout = %s", p.client.Timeout)
	}
	p.base = srv.URL // send the request to the test server instead of Google
	if _, err := p.Query(context.Background(), "up", time.Unix(0, 0)); err != nil || gotAuth != "Bearer test" {
		t.Errorf("Query() err = %v, auth = %q", err, gotAuth)
	}

	if _, err := newManagedPrometheus(context.Background(), "Bad_Project"); err == nil {
		t.Error("bad project id accepted")
	}
	googleClient = func(context.Context) (*http.Client, error) { return nil, errors.New("no ADC") }
	if _, err := newManagedPrometheus(context.Background(), "mamori-lab-123456"); err == nil || !strings.Contains(err.Error(), "application-default login") {
		t.Errorf("error = %v", err)
	}
}

func TestPromAPIQueryTransportErrors(t *testing.T) {
	p, err := newPrometheus("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Query(context.Background(), "up", time.Now()); err == nil {
		t.Error("want a connection error")
	}
	p.base = "http://bad host"
	if _, err := p.Query(context.Background(), "up", time.Now()); err == nil {
		t.Error("want a request error")
	}
}

type headerTransport struct{ key, value string }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(h.key, h.value)
	return http.DefaultTransport.RoundTrip(r)
}
