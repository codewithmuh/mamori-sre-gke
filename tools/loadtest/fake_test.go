package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fixture run: testdata/k6-summary.json and testdata/run-fixture.json describe the same schedule.
var (
	fixtureStart = time.Date(2027, 1, 12, 10, 0, 0, 250_000_000, time.UTC)
	fixtureSteps = []float64{10, 20, 40, 60, 80}
)

const (
	fixtureStepDuration = 300 * time.Second
	fixtureRunID        = "20270112-100000"
)

type fixtureStep struct {
	Requests     map[string]float64            `json:"requests"`
	Good         map[string]float64            `json:"good"`
	Fast         map[string]map[string]float64 `json:"fast"`
	Dropped      *float64                      `json:"dropped"`
	RateLimited  *float64                      `json:"rateLimited"`
	P95          map[string]float64            `json:"p95"`
	GeneratorCPU *float64                      `json:"generatorCPU"`
	NodeCPU      *float64                      `json:"nodeCPU"`
	Nodes        *float64                      `json:"nodes"`
	FrontendRPS  *float64                      `json:"frontendRPS"`
}

// fakeProm answers the queries `loadtest report` sends from the fixture, the way Managed Service for
// Prometheus would. It finds the step from the evaluation time and the series from the metric name.
type fakeProm struct {
	t        *testing.T
	start    time.Time
	stepDur  time.Duration
	steps    []fixtureStep
	failOn   map[string]error // substring of a query -> error to return
	mu       sync.Mutex
	queries  []string
	queryAts []time.Time
}

func newFakeProm(t *testing.T) *fakeProm {
	t.Helper()
	data, err := os.ReadFile("testdata/run-fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Steps []fixtureStep `json:"steps"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	return &fakeProm{t: t, start: fixtureStart, stepDur: fixtureStepDuration, steps: f.Steps, failOn: map[string]error{}}
}

func (f *fakeProm) Query(_ context.Context, q string, at time.Time) ([]sample, error) {
	f.mu.Lock()
	f.queries = append(f.queries, q)
	f.queryAts = append(f.queryAts, at)
	f.mu.Unlock()
	for sub, err := range f.failOn {
		if strings.Contains(q, sub) {
			return nil, err
		}
	}
	idx := int(math.Round(float64(at.Sub(f.start))/float64(f.stepDur))) - 1
	if idx < 0 || idx >= len(f.steps) {
		return nil, nil // no data outside the run
	}
	s := f.steps[idx]
	byCUJ := func(m map[string]float64) []sample {
		var out []sample
		for _, k := range sortedMapKeys(m) {
			out = append(out, sample{Labels: map[string]string{"cuj": k}, Value: m[k]})
		}
		return out
	}
	one := func(v *float64) []sample {
		if v == nil {
			return nil
		}
		return []sample{{Labels: map[string]string{}, Value: *v}}
	}
	switch {
	case strings.Contains(q, "k6_mamori_cuj_requests_total"):
		return byCUJ(s.Requests), nil
	case strings.Contains(q, "k6_mamori_cuj_good_total"):
		return byCUJ(s.Good), nil
	case strings.Contains(q, "k6_mamori_cuj_fast_total"):
		var out []sample
		for _, cuj := range sortedMapKeys(s.Fast) {
			for _, th := range sortedMapKeys(s.Fast[cuj]) {
				out = append(out, sample{Labels: map[string]string{"cuj": cuj, "threshold_ms": th}, Value: s.Fast[cuj][th]})
			}
		}
		return out, nil
	case strings.Contains(q, "k6_dropped_iterations_total"):
		return one(s.Dropped), nil
	case strings.Contains(q, `k6_http_reqs_total{run="20270112-100000",status="429"}`),
		strings.Contains(q, `k6_http_reqs_total{status="429"}`):
		return one(s.RateLimited), nil
	case strings.Contains(q, "histogram_quantile"):
		return byCUJ(s.P95), nil
	case strings.Contains(q, "container_cpu_core_usage_time"):
		return one(s.GeneratorCPU), nil
	case strings.HasPrefix(q, "avg(") && strings.Contains(q, "node_cpu_allocatable_utilization"):
		return one(s.NodeCPU), nil
	case strings.HasPrefix(q, "count(") && strings.Contains(q, "node_cpu_allocatable_utilization"):
		return one(s.Nodes), nil
	case strings.Contains(q, "istio_requests_total"):
		return one(s.FrontendRPS), nil
	}
	f.t.Errorf("fake Prometheus: unexpected query %q", q)
	return nil, nil
}

func (f *fakeProm) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries...)
}

// serve exposes the fake as a Prometheus HTTP API, for tests of the real client and of run().
func (f *fakeProm) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" || r.Method != http.MethodGet {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		sec, err := strconv.ParseFloat(r.URL.Query().Get("time"), 64)
		if err != nil {
			http.Error(w, `{"status":"error","errorType":"bad_data","error":"bad time"}`, http.StatusBadRequest)
			return
		}
		at := time.UnixMilli(int64(math.Round(sec * 1000))).UTC()
		samples, qerr := f.Query(r.Context(), r.URL.Query().Get("query"), at)
		w.Header().Set("Content-Type", "application/json")
		if qerr != nil {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = fmt.Fprintf(w, `{"status":"error","errorType":"execution","error":%q}`, qerr.Error())
			return
		}
		type point struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		}
		result := []point{}
		for _, s := range samples {
			result = append(result, point{Metric: s.Labels, Value: [2]any{sec, strconv.FormatFloat(s.Value, 'f', -1, 64)}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func mustConfig(t *testing.T) config {
	t.Helper()
	cfg, err := loadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func mustSLOs(t *testing.T) sloSpec {
	t.Helper()
	s, err := loadSLOs("testdata/slos.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func fixtureSchedule(t *testing.T, warmup time.Duration) []step {
	t.Helper()
	s, err := buildSchedule(fixtureStart, fixtureSteps, fixtureStepDuration, warmup)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
