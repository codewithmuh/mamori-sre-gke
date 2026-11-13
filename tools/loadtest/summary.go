package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"time"
)

// runPlan is what a run's k6 summary says about its schedule. deploy/loadtest/scripts/capacity.js
// puts it in setup_data; `make loadtest-run` saves the summary from the Job's log.
type runPlan struct {
	RunID               string
	Start               time.Time
	End                 time.Time
	Steps               []float64
	StepDuration        time.Duration
	LatencyThresholdsMs []float64
}

func readK6Summary(path string) (runPlan, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the operator names the file
	if err != nil {
		return runPlan{}, err
	}
	var s struct {
		SetupData *struct {
			RunID               string    `json:"runId"`
			Start               time.Time `json:"start"`
			Steps               []float64 `json:"steps"`
			StepSeconds         float64   `json:"stepSeconds"`
			LatencyThresholdsMs []float64 `json:"latencyThresholdsMs"`
		} `json:"setup_data"`
		State struct {
			TestRunDurationMs float64 `json:"testRunDurationMs"`
		} `json:"state"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return runPlan{}, fmt.Errorf("%s: not a k6 summary: %w", path, err)
	}
	d := s.SetupData
	if d == nil || d.Start.IsZero() || len(d.Steps) == 0 || !(d.StepSeconds > 0) || d.StepSeconds > math.MaxInt32 {
		return runPlan{}, fmt.Errorf("%s: no run plan in setup_data; was it made by deploy/loadtest/scripts/capacity.js?", path)
	}
	if !(s.State.TestRunDurationMs > 0) || s.State.TestRunDurationMs > math.MaxInt32 {
		return runPlan{}, errors.New(path + ": no state.testRunDurationMs")
	}
	return runPlan{
		RunID:               d.RunID,
		Start:               d.Start,
		End:                 d.Start.Add(time.Duration(s.State.TestRunDurationMs * float64(time.Millisecond))),
		Steps:               d.Steps,
		StepDuration:        time.Duration(d.StepSeconds * float64(time.Second)),
		LatencyThresholdsMs: d.LatencyThresholdsMs,
	}, nil
}
