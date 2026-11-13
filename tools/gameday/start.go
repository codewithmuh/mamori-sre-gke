package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// startedLine and finishedLine are what `start -o json` prints on stdout.
type startedLine struct {
	Event     string    `json:"event"`
	RunID     string    `json:"runId"`
	FaultID   string    `json:"faultId"`
	StartedAt time.Time `json:"startedAt"`
	EndsAt    time.Time `json:"endsAt"`
	Objects   []string  `json:"objects"`
	Timeline  string    `json:"timeline"`
}

type finishedLine struct {
	Event      string    `json:"event"`
	RunID      string    `json:"runId"`
	FinishedAt time.Time `json:"finishedAt"`
	Reason     string    `json:"reason"`
}

func (a *app) runDuration(f *fault, requested time.Duration) (time.Duration, error) {
	if requested == 0 {
		return f.duration, nil
	}
	if requested < a.minDuration || requested > maxDuration {
		return 0, refused("--duration %s must be between %s and %s", requested, a.minDuration, maxDuration)
	}
	return requested, nil
}

// start runs one fault. For a manifest fault it applies the objects, holds them until the
// duration ends, a signal arrives, or someone else removes them, then removes them and closes
// the timeline. For a manual fault it prints the procedure (and, with --manual, records a
// timeline while a person follows it).
func (a *app) start(ctx context.Context, opts options, f *fault, requested time.Duration, manual bool) error {
	dur, err := a.runDuration(f, requested)
	if err != nil {
		return err
	}
	switch {
	case f.manual() && !manual:
		if opts.output == "json" {
			_ = a.printJSON(map[string]any{"event": "manual", "faultId": f.ID, "inject": f.Procedure.Inject, "revert": f.Procedure.Revert})
		} else {
			a.printProcedure(f)
		}
		return &cliError{code: exitManual, err: fmt.Errorf("%s needs a person to follow its procedure; run `gameday start %s --manual` to record the timeline", f.ID, f.ID)}
	case f.manual():
		repo, err := a.repoRoot(opts.repo)
		if err != nil {
			return err
		}
		return a.startManual(ctx, opts, repo, f, dur)
	case manual:
		return refused("%s is applied by gameday itself; --manual is only for faults with a procedure", f.ID)
	}

	repo, err := a.repoRoot(opts.repo)
	if err != nil {
		return err
	}
	cl, kubeContext, err := a.cluster(opts)
	if err != nil {
		return err
	}
	log := a.log.With("fault_id", f.ID, "kube_context", kubeContext)

	// Preflight: one fault at a time, and no rule that would make this one undefined.
	active, err := cl.activeObjects(ctx, labelFault+"=true")
	if err != nil {
		return err
	}
	if len(active) > 0 {
		var refs []string
		for _, o := range active {
			refs = append(refs, objectRef(o)+" ("+o.GetLabels()[labelFaultID]+")")
		}
		return refused("another fault is active in %s: %s. One fault at a time: `gameday stop --all` first", faultNamespace, joinRefs(refs))
	}
	markers, err := cl.discoveryMarkers(ctx)
	if err != nil {
		return err
	}
	if len(markers) > 0 {
		return refused("a stopped discovery run left %s in %s (redis-cart may still be scaled to zero): %s first",
			joinRefs(markers), faultNamespace, discoveryHint)
	}
	clashes, err := cl.conflicts(ctx, f.objects)
	if err != nil {
		return err
	}
	if len(clashes) > 0 {
		return refused("%s would clash with %s (one VirtualService per host, one Sidecar per workload)", f.ID, joinRefs(clashes))
	}

	runID := a.now().UTC().Format("20060102T150405Z") + "-" + f.ID
	tl := newTimeline(repo, runID, f, dur, kubeContext, a.now)
	log = log.With("run_id", runID)
	if err := tl.add("run_started", "", "duration "+durationText(dur)); err != nil {
		return err
	}
	objs := prepareObjects(f, runID, tl.EndsAt, dur)

	// Apply. If any object fails, take back the ones already created.
	var created []*unstructured.Unstructured
	for _, o := range objs {
		if err := cl.create(ctx, o); err != nil {
			log.Error("apply failed; removing what was applied", "event", "apply_failed", "object", objectRef(o), "error", err.Error())
			_ = tl.add("apply_failed", objectRef(o), err.Error())
			cleanupErr := cl.remove(cleanupContext(ctx), created)
			_ = tl.finish(statusFailed, reasonApply)
			return errors.Join(err, cleanupErr)
		}
		created = append(created, o)
		_ = tl.add("object_applied", objectRef(o), "")
	}
	if err := tl.add("fault_active", "", "until "+tl.EndsAt.Format(time.RFC3339)); err != nil {
		log.Warn("timeline not written", "error", err.Error())
	}
	log.Info("fault active", "event", "fault_active", "objects", tl.Objects, "ends_at", tl.EndsAt)
	if opts.output == "json" {
		if err := a.printJSON(startedLine{Event: "started", RunID: runID, FaultID: f.ID, StartedAt: tl.StartedAt, EndsAt: tl.EndsAt, Objects: tl.Objects, Timeline: tl.path}); err != nil {
			log.Warn("stdout", "error", err.Error())
		}
	}
	a.printf(opts, "run %s: %s is active (%s) until %s. Ctrl-C ends it early.\ntimeline: %s",
		runID, f.ID, joinRefs(tl.Objects), tl.EndsAt.Local().Format("15:04:05"), tl.path)

	reason := a.hold(ctx, cl, tl, objs, dur)
	if reason != reasonRemoved {
		_ = tl.add("removing", "", reason)
		if err := cl.remove(cleanupContext(ctx), objs); err != nil {
			log.Error("cleanup failed", "event", "cleanup_failed", "error", err.Error())
			_ = tl.add("cleanup_failed", "", err.Error())
			_ = tl.finish(statusFailed, reasonCleanup)
			return fmt.Errorf("%w; run `gameday stop %s` until it succeeds", err, f.ID)
		}
		_ = tl.add("fault_removed", "", "")
	}
	if err := tl.finish(statusFinished, reason); err != nil {
		log.Warn("timeline not written", "error", err.Error())
	}
	log.Info("run finished", "event", "run_finished", "reason", reason)
	if opts.output == "json" {
		return a.printJSON(finishedLine{Event: "finished", RunID: runID, FinishedAt: *tl.FinishedAt, Reason: reason})
	}
	a.printf(opts, "run %s finished (%s)", runID, reason)
	return nil
}

// prepareObjects copies the fault's objects for one run: annotations that tie them to the run
// and say when they should be gone, and a Chaos Mesh duration equal to the run's, so Chaos
// Mesh stops injecting by itself even if this process dies.
func prepareObjects(f *fault, runID string, endsAt time.Time, dur time.Duration) []*unstructured.Unstructured {
	out := make([]*unstructured.Unstructured, 0, len(f.objects))
	for _, src := range f.objects {
		o := src.DeepCopy()
		ann := o.GetAnnotations()
		if ann == nil {
			ann = map[string]string{}
		}
		ann[annotationRunID] = runID
		ann[annotationExpiresAt] = endsAt.UTC().Format(time.RFC3339)
		o.SetAnnotations(ann)
		if o.GroupVersionKind().Group == "chaos-mesh.org" {
			if _, found, _ := unstructured.NestedString(o.Object, "spec", "duration"); found {
				_ = unstructured.SetNestedField(o.Object, durationText(dur), "spec", "duration")
			}
		}
		out = append(out, o)
	}
	return out
}

// hold waits until the run should end and says why.
func (a *app) hold(ctx context.Context, cl cluster, tl *timeline, objs []*unstructured.Unstructured, dur time.Duration) string {
	timer := time.NewTimer(dur)
	defer timer.Stop()
	tick := time.NewTicker(a.pollInterval)
	defer tick.Stop()
	gone := map[string]bool{}
	deleting := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return reasonSignal
		case <-timer.C:
			return reasonDuration
		case <-tick.C:
			for _, o := range objs {
				ref := objectRef(o)
				if gone[ref] {
					continue
				}
				there, del, err := cl.state(ctx, o)
				if err != nil {
					a.log.Warn("could not check fault object", "object", ref, "error", err.Error())
					continue
				}
				if there && del && !deleting[ref] {
					// Someone deleted it; Chaos Mesh is still recovering the pods. This is the
					// time of the mitigating action; "removed" below is when recovery is done.
					deleting[ref] = true
					_ = tl.add("object_deletion_started", ref, "")
				}
				if !there {
					gone[ref] = true
					_ = tl.add("object_removed_externally", ref, "")
					a.log.Info("fault object removed by someone else", "event", "object_removed_externally", "object", ref)
				}
			}
			if len(gone) == len(objs) {
				return reasonRemoved
			}
		}
	}
}

// cleanupContext survives the Ctrl-C that cancelled ctx: the cleanup must still run.
func cleanupContext(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}

func (a *app) printProcedure(f *fault) {
	var b strings.Builder
	_, _ = fmt.Fprintf(&b, "%s: %s\n%s\n\nInject:\n", f.ID, f.Title, f.Procedure.Reason)
	for i, s := range f.Procedure.Inject {
		_, _ = fmt.Fprintf(&b, "  %d. %s\n", i+1, s)
	}
	b.WriteString("\nRevert:\n")
	for i, s := range f.Procedure.Revert {
		_, _ = fmt.Fprintf(&b, "  %d. %s\n", i+1, s)
	}
	_, _ = fmt.Fprint(a.stdout, b.String())
}

// startManual records a timeline around a procedure a person follows: Enter marks the
// injection, the run then lasts its duration (or until Ctrl-C), and Enter marks the revert.
func (a *app) startManual(ctx context.Context, opts options, repo string, f *fault, dur time.Duration) error {
	runID := a.now().UTC().Format("20060102T150405Z") + "-" + f.ID
	tl := newTimeline(repo, runID, f, dur, "", a.now)
	if err := tl.add("run_started", "", "manual procedure, duration "+durationText(dur)); err != nil {
		return err
	}
	a.printProcedure(f)
	lines := make(chan string)
	go func() {
		sc := bufio.NewScanner(a.stdin)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	waitEnter := func(ctx context.Context, prompt string) bool {
		_, _ = fmt.Fprintf(a.stdout, "\n%s ", prompt)
		select {
		case <-ctx.Done():
			return false
		case _, ok := <-lines:
			return ok
		}
	}

	if !waitEnter(ctx, "Press Enter once the fault is injected (Ctrl-C to abort):") {
		_ = tl.finish(statusFailed, reasonSignal)
		return refused("aborted before the fault was injected")
	}
	tl.EndsAt = a.now().UTC().Add(dur)
	_ = tl.add("fault_injected", "", "by hand; until "+tl.EndsAt.Format(time.RFC3339))
	a.log.Info("manual fault injected", "event", "fault_injected", "run_id", runID, "fault_id", f.ID)
	a.printf(opts, "run %s: %s marked injected; the run lasts until %s. Ctrl-C ends it early.", runID, f.ID, tl.EndsAt.Local().Format("15:04:05"))

	reason := reasonDuration
	select {
	case <-ctx.Done():
		reason = reasonSignal
	case <-time.After(dur):
	}
	_ = tl.add("revert_requested", "", reason)
	a.printf(opts, "Time to revert:")
	for i, s := range f.Procedure.Revert {
		a.printf(opts, "  %d. %s", i+1, s)
	}
	// The run's context may be cancelled by now (Ctrl-C), but the revert still needs a person, so
	// this prompt waits for stdin only. Ctrl-D (end of input) skips the confirmation.
	if waitEnter(context.WithoutCancel(ctx), "Press Enter once the revert is done (Ctrl-D to skip):") {
		_ = tl.add("fault_reverted", "", "by hand")
	} else {
		_ = tl.add("revert_not_confirmed", "", "")
	}
	if err := tl.finish(statusFinished, reason); err != nil {
		return err
	}
	a.printf(opts, "run %s finished (%s); timeline: %s", runID, reason, tl.path)
	return nil
}
