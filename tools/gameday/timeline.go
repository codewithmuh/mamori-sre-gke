package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// runsDir holds one timeline per run, relative to the repo root. It is gitignored: timelines
// feed postmortems and the agent evaluation, they are not source.
const runsDir = "gameday-runs"

// Run statuses and end reasons, as written to the timeline.
const (
	statusRunning  = "running"
	statusFinished = "finished"
	statusFailed   = "failed"

	reasonDuration = "duration"
	reasonSignal   = "signal"
	reasonRemoved  = "removed-externally"
	reasonApply    = "apply-failed"
	reasonCleanup  = "cleanup-failed"
)

// timeline is gameday-runs/<run-id>.json. Only the `gameday start` process that owns the run
// writes it, after every event, so a crash leaves everything up to the last event on disk.
type timeline struct {
	RunID       string     `json:"runId"`
	FaultID     string     `json:"faultId"`
	Title       string     `json:"title"`
	Category    string     `json:"category"`
	Mode        string     `json:"mode"`
	KubeContext string     `json:"kubeContext,omitempty"`
	Duration    string     `json:"duration"`
	Objects     []string   `json:"objects,omitempty"`
	StartedAt   time.Time  `json:"startedAt"`
	EndsAt      time.Time  `json:"endsAt"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
	Status      string     `json:"status"`
	EndReason   string     `json:"endReason,omitempty"`
	Events      []event    `json:"events"`

	path string
	now  func() time.Time
}

type event struct {
	Time   time.Time `json:"time"`
	Event  string    `json:"event"`
	Object string    `json:"object,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

func newTimeline(repo, runID string, f *fault, dur time.Duration, kubeContext string, now func() time.Time) *timeline {
	start := now().UTC()
	t := &timeline{
		RunID:       runID,
		FaultID:     f.ID,
		Title:       f.Title,
		Category:    f.Category,
		Mode:        f.mode(),
		KubeContext: kubeContext,
		Duration:    dur.String(),
		StartedAt:   start,
		EndsAt:      start.Add(dur),
		Status:      statusRunning,
		Events:      []event{},
		path:        filepath.Join(repo, runsDir, runID+".json"),
		now:         now,
	}
	for _, o := range f.objects {
		t.Objects = append(t.Objects, objectRef(o))
	}
	return t
}

// add records one event and saves the file.
func (t *timeline) add(name, object, detail string) error {
	t.Events = append(t.Events, event{Time: t.now().UTC(), Event: name, Object: object, Detail: detail})
	return t.save()
}

// finish closes the run with a status and a reason, and saves the file.
func (t *timeline) finish(status, reason string) error {
	at := t.now().UTC()
	t.FinishedAt = &at
	t.Status = status
	t.EndReason = reason
	return t.add("run_finished", "", reason)
}

// save writes the file atomically: a reader never sees half a timeline.
func (t *timeline) save() error {
	if err := os.MkdirAll(filepath.Dir(t.path), 0o750); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(t.path), err)
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write timeline: %w", err)
	}
	if err := os.Rename(tmp, t.path); err != nil {
		return fmt.Errorf("write timeline: %w", err)
	}
	return nil
}
