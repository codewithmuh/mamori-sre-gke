package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2/google"
)

// sample is one series of an instant-query result.
type sample struct {
	Labels map[string]string
	Value  float64
}

// querier runs a PromQL instant query. promAPI implements it; tests use a fake.
type querier interface {
	Query(ctx context.Context, query string, at time.Time) ([]sample, error)
}

// promAPI talks to the Prometheus HTTP API: Managed Service for Prometheus serves it per project at
// monitoring.googleapis.com, and any Prometheus serves it at /api/v1.
type promAPI struct {
	base   string // without the trailing /api/v1
	client *http.Client
}

const (
	gmpBaseURL    = "https://monitoring.googleapis.com/v1/projects/%s/location/global/prometheus"
	monitoringRO  = "https://www.googleapis.com/auth/monitoring.read"
	maxBodyBytes  = 16 << 20
	clientTimeout = 60 * time.Second
)

var projectIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)

// googleClient returns an HTTP client that sends Application Default Credentials (for example from
// `gcloud auth application-default login`). A variable so tests never look for real credentials.
var googleClient = func(ctx context.Context) (*http.Client, error) {
	return google.DefaultClient(ctx, monitoringRO)
}

// newManagedPrometheus queries the project's Managed Service for Prometheus. The caller needs
// roles/monitoring.viewer (or any role with monitoring.timeSeries.list) on the project.
func newManagedPrometheus(ctx context.Context, projectID string) (*promAPI, error) {
	if !projectIDPattern.MatchString(projectID) {
		return nil, fmt.Errorf("project %q is not a Google Cloud project id", projectID)
	}
	c, err := googleClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("no Google credentials (run `gcloud auth application-default login`): %w", err)
	}
	c.Timeout = clientTimeout
	return &promAPI{base: fmt.Sprintf(gmpBaseURL, projectID), client: c}, nil
}

// newPrometheus queries any Prometheus-compatible API without authentication, for example the
// Managed Service for Prometheus frontend UI through a port-forward, or a local Prometheus.
func newPrometheus(rawURL string) (*promAPI, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("prometheus URL %q must be http(s)://host[:port][/path]", rawURL)
	}
	return &promAPI{base: strings.TrimRight(rawURL, "/"), client: &http.Client{Timeout: clientTimeout}}, nil
}

func (p *promAPI) Query(ctx context.Context, query string, at time.Time) ([]sample, error) {
	params := url.Values{
		"query": {query},
		"time":  {strconv.FormatFloat(float64(at.UnixMilli())/1000, 'f', 3, 64)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+"/api/v1/query?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body; a failed close changes nothing here
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return parseQueryResponse(resp.StatusCode, body)
}

// parseQueryResponse reads the Prometheus API's JSON. Google's front end answers some errors (403,
// 404, 429) in its own error format instead; those come back with the body's first bytes.
func parseQueryResponse(status int, body []byte) ([]sample, error) {
	var r struct {
		Status    string `json:"status"`
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
		Data      struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.Status == "" {
		return nil, fmt.Errorf("HTTP %d: %s", status, snippet(body))
	}
	if r.Status != "success" {
		return nil, fmt.Errorf("HTTP %d: %s: %s", status, r.ErrorType, r.Error)
	}
	switch r.Data.ResultType {
	case "vector":
		var series []struct {
			Metric map[string]string `json:"metric"`
			Value  []json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(r.Data.Result, &series); err != nil {
			return nil, fmt.Errorf("vector result: %w", err)
		}
		out := make([]sample, 0, len(series))
		for _, s := range series {
			v, err := pointValue(s.Value)
			if err != nil {
				return nil, err
			}
			out = append(out, sample{Labels: s.Metric, Value: v})
		}
		return out, nil
	case "scalar":
		var point []json.RawMessage
		if err := json.Unmarshal(r.Data.Result, &point); err != nil {
			return nil, fmt.Errorf("scalar result: %w", err)
		}
		v, err := pointValue(point)
		if err != nil {
			return nil, err
		}
		return []sample{{Labels: map[string]string{}, Value: v}}, nil
	default:
		return nil, fmt.Errorf("result type %q: want an instant vector or scalar", r.Data.ResultType)
	}
}

// pointValue reads [<unix time>, "<value>"]; the value is a string so it can say NaN or +Inf.
func pointValue(point []json.RawMessage) (float64, error) {
	if len(point) != 2 {
		return 0, errors.New("a sample is not a [time, value] pair")
	}
	var s string
	if err := json.Unmarshal(point[1], &s); err != nil {
		return 0, fmt.Errorf("sample value: %w", err)
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("sample value %q: %w", s, err)
	}
	return v, nil
}

func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}
