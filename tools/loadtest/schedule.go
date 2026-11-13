package main

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// step is one load level of the run and the part of it that is measured.
type step struct {
	Index        int       // 1-based, as in the report
	PlannedRPS   float64   // requests per second k6 was asked for
	Start        time.Time // the step's ramp starts here
	MeasuredFrom time.Time // Start + warmup
	End          time.Time
}

// window is the measured part of the step, which PromQL range selectors cover.
func (s step) window() time.Duration { return s.End.Sub(s.MeasuredFrom) }

// minWindow keeps a few scrapes (k6 relay every 10 s, GKE system metrics every 60 s) in every window.
const minWindow = time.Minute

// parseSteps reads "5,10,20": positive and rising, because the report looks for the first step that
// broke an SLO.
func parseSteps(s string) ([]float64, error) {
	var out []float64
	for _, part := range strings.Split(s, ",") {
		v, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil || !(v > 0) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("steps %q: %q is not a positive number", s, strings.TrimSpace(part))
		}
		if len(out) > 0 && v <= out[len(out)-1] {
			return nil, fmt.Errorf("steps %q: each step must be higher than the one before", s)
		}
		out = append(out, v)
	}
	return out, nil
}

// buildSchedule lays the steps end to end from the run's start, as the k6 script does.
func buildSchedule(start time.Time, rps []float64, stepDuration, warmup time.Duration) ([]step, error) {
	if len(rps) == 0 {
		return nil, errors.New("no steps")
	}
	if stepDuration%time.Second != 0 {
		return nil, fmt.Errorf("step duration %s must be whole seconds", stepDuration)
	}
	if stepDuration-warmup < minWindow {
		return nil, fmt.Errorf("step duration %s leaves less than %s to measure after the %s warm-up", stepDuration, minWindow, warmup)
	}
	steps := make([]step, len(rps))
	for i, r := range rps {
		begin := start.Add(time.Duration(i) * stepDuration)
		steps[i] = step{
			Index:        i + 1,
			PlannedRPS:   r,
			Start:        begin,
			MeasuredFrom: begin.Add(warmup),
			End:          begin.Add(stepDuration),
		}
	}
	return steps, nil
}

// promDuration writes a window as a PromQL duration in whole seconds, e.g. 240s.
func promDuration(d time.Duration) string {
	return strconv.FormatInt(int64(d/time.Second), 10) + "s"
}
