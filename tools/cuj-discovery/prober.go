package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// runResult is the prober's answer to POST /v1/run (contract, issue #16).
type runResult struct {
	CUJ       string      `json:"cuj"`
	Runs      int         `json:"runs"`
	Successes int         `json:"successes"`
	Failures  int         `json:"failures"`
	Results   []runDetail `json:"results"`
}

type runDetail struct {
	OK         bool    `json:"ok"`
	DurationMs float64 `json:"durationMs"`
	FailedStep string  `json:"failedStep"`
	Error      string  `json:"error"`
}

// prober runs a journey a number of times and reports how many runs failed.
type prober interface {
	Run(ctx context.Context, cuj string, runs int) (runResult, error)
}

// maxProberRuns is the prober's limit per call; it answers 400 above it.
const maxProberRuns = 100

// httpProber calls the prober's on-demand API. Those runs are recorded with
// source="on-demand", which the SLOs ignore.
type httpProber struct {
	baseURL string
	client  *http.Client

	// callTimeout bounds one call. 20 checkout runs under a fault take at most
	// about 255 s (5 rounds of 4 runs; 4 steps of up to 12 s each plus a 3 s
	// cart clean-up per run), so 10 minutes leaves room.
	callTimeout time.Duration

	// The prober runs one on-demand call per journey at a time and answers 409
	// "busy" to a second one. Someone else's call ends soon, so wait and retry.
	busyRetries int
	busyWait    time.Duration
	sleep       func(context.Context, time.Duration) error
}

func newHTTPProber(baseURL string) *httpProber {
	return &httpProber{
		baseURL:     strings.TrimRight(baseURL, "/"),
		client:      &http.Client{},
		callTimeout: 10 * time.Minute,
		busyRetries: 6,
		busyWait:    10 * time.Second,
		sleep:       sleepCtx,
	}
}

type proberError struct {
	Error  string `json:"error"`
	Reason string `json:"reason"`
}

func (p *httpProber) Run(ctx context.Context, cuj string, runs int) (runResult, error) {
	if runs < 1 || runs > maxProberRuns {
		return runResult{}, fmt.Errorf("runs=%d: the prober accepts 1 to %d", runs, maxProberRuns)
	}
	body, err := json.Marshal(map[string]any{"cuj": cuj, "runs": runs})
	if err != nil {
		return runResult{}, err
	}
	for attempt := 0; ; attempt++ {
		status, data, err := p.post(ctx, body)
		if err != nil {
			return runResult{}, fmt.Errorf("prober run %s: %w", cuj, err)
		}
		if status == http.StatusOK {
			return decodeRunResult(data, cuj, runs)
		}
		var perr proberError
		_ = json.Unmarshal(data, &perr)
		if status == http.StatusConflict && perr.Reason == "busy" && attempt < p.busyRetries {
			if err := p.sleep(ctx, p.busyWait); err != nil {
				return runResult{}, fmt.Errorf("prober run %s: waiting while busy: %w", cuj, err)
			}
			continue
		}
		msg := strings.TrimSpace(perr.Error)
		if msg == "" {
			msg = strings.TrimSpace(string(data))
		}
		return runResult{}, fmt.Errorf("prober run %s: HTTP %d reason=%q: %s", cuj, status, perr.Reason, truncate(msg, 300))
	}
}

func (p *httpProber) post(ctx context.Context, body []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, p.callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/run", bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	// 100 results with long error strings stay far below this.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("read response: %w", err)
	}
	return resp.StatusCode, data, nil
}

// decodeRunResult checks that the answer is about what was asked and adds up,
// so a prober bug can't turn into a wrong dependency map.
func decodeRunResult(data []byte, cuj string, runs int) (runResult, error) {
	var res runResult
	if err := json.Unmarshal(data, &res); err != nil {
		return runResult{}, fmt.Errorf("prober run %s: invalid JSON: %w", cuj, err)
	}
	var problems []error
	if res.CUJ != cuj {
		problems = append(problems, fmt.Errorf("cuj is %q, asked for %q", res.CUJ, cuj))
	}
	if res.Runs != runs {
		problems = append(problems, fmt.Errorf("runs is %d, asked for %d", res.Runs, runs))
	}
	if res.Successes < 0 || res.Failures < 0 || res.Successes+res.Failures != res.Runs {
		problems = append(problems, fmt.Errorf("successes %d + failures %d != runs %d", res.Successes, res.Failures, res.Runs))
	}
	if len(res.Results) > 0 {
		failed := 0
		for _, r := range res.Results {
			if !r.OK {
				failed++
			}
		}
		if len(res.Results) != res.Runs || failed != res.Failures {
			problems = append(problems, fmt.Errorf("results list (%d entries, %d failed) does not match runs %d, failures %d",
				len(res.Results), failed, res.Runs, res.Failures))
		}
	}
	if len(problems) > 0 {
		return runResult{}, fmt.Errorf("prober run %s: inconsistent answer: %w", cuj, errors.Join(problems...))
	}
	return res, nil
}

// failedSteps counts failed runs by the step that failed, and returns the
// first error message as an example.
func failedSteps(res runResult) (map[string]int, string) {
	steps := map[string]int{}
	example := ""
	for _, r := range res.Results {
		if r.OK {
			continue
		}
		step := r.FailedStep
		if step == "" {
			step = "unknown"
		}
		steps[step]++
		if example == "" {
			example = truncate(r.Error, 200)
		}
	}
	return steps, example
}

// rateLimited counts failed runs that the prober reports as HTTP 429.
func rateLimited(res runResult) int {
	n := 0
	for _, r := range res.Results {
		if !r.OK && strings.Contains(r.Error, "status 429") {
			n++
		}
	}
	return n
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// sleepCtx waits for d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
