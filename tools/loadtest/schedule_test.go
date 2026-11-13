package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseSteps(t *testing.T) {
	got, err := parseSteps(" 5, 10,20.5 ,40")
	if err != nil || !reflect.DeepEqual(got, []float64{5, 10, 20.5, 40}) {
		t.Fatalf("parseSteps() = %v, %v", got, err)
	}
	for in, want := range map[string]string{
		"":       `"" is not a positive number`,
		"5,,10":  `"" is not a positive number`,
		"5,0":    `"0" is not a positive number`,
		"5,-1":   `"-1" is not a positive number`,
		"5,NaN":  `"NaN" is not a positive number`,
		"5,+Inf": `"+Inf" is not a positive number`,
		"10,5":   "higher than the one before",
		"10,10":  "higher than the one before",
	} {
		if _, err := parseSteps(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseSteps(%q) error = %v, want %q", in, err, want)
		}
	}
}

func TestBuildSchedule(t *testing.T) {
	steps := fixtureSchedule(t, time.Minute)
	if len(steps) != 5 {
		t.Fatalf("len = %d", len(steps))
	}
	s3 := steps[2]
	if s3.Index != 3 || s3.PlannedRPS != 40 ||
		!s3.Start.Equal(fixtureStart.Add(600*time.Second)) ||
		!s3.MeasuredFrom.Equal(fixtureStart.Add(660*time.Second)) ||
		!s3.End.Equal(fixtureStart.Add(900*time.Second)) {
		t.Errorf("step 3 = %+v", s3)
	}
	if got := promDuration(s3.window()); got != "240s" {
		t.Errorf("window = %s, want 240s", got)
	}
}

func TestBuildScheduleErrors(t *testing.T) {
	start := fixtureStart
	tests := []struct {
		name         string
		rps          []float64
		step, warmup time.Duration
		want         string
	}{
		{"no steps", nil, 5 * time.Minute, time.Minute, "no steps"},
		{"fractional", []float64{5}, 90500 * time.Millisecond, 0, "whole seconds"},
		{"too short", []float64{5}, 90 * time.Second, time.Minute, "leaves less than 1m0s"},
	}
	for _, tt := range tests {
		if _, err := buildSchedule(start, tt.rps, tt.step, tt.warmup); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error = %v, want %q", tt.name, err, tt.want)
		}
	}
}
