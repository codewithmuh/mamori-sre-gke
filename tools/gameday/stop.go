package main

import (
	"context"
	"fmt"
	"text/tabwriter"
	"time"
)

type stopResult struct {
	Removed []string `json:"removed"`
}

// stop removes the objects of one fault (id) or of every fault (id ""), found by label in
// boutique, and waits until they are gone. Safe to repeat: nothing to remove is success.
// A `gameday start` still holding the fault notices within its poll interval and closes its
// timeline as removed-externally.
func (a *app) stop(ctx context.Context, opts options, id string) error {
	cl, kubeContext, err := a.cluster(opts)
	if err != nil {
		return err
	}
	selector := labelFault + "=true"
	if id != "" {
		selector += "," + labelFaultID + "=" + id
	}
	objs, err := cl.activeObjects(ctx, selector)
	if err != nil {
		return err
	}
	res := stopResult{Removed: []string{}}
	for _, o := range objs {
		res.Removed = append(res.Removed, objectRef(o))
	}
	log := a.log.With("kube_context", kubeContext, "selector", selector)
	if len(objs) > 0 {
		if err := cl.remove(ctx, objs, a.cleanupTimeout, a.pollInterval); err != nil {
			return err
		}
		log.Info("fault objects removed", "event", "fault_removed", "objects", res.Removed)
	} else {
		log.Info("nothing to remove", "event", "nothing_to_remove")
	}
	if opts.output == "json" {
		return a.printJSON(res)
	}
	if len(objs) == 0 {
		a.printf(opts, "nothing to stop")
		return nil
	}
	for _, o := range objs {
		a.printf(opts, "removed %s (%s)", objectRef(o), o.GetLabels()[labelFaultID])
	}
	return nil
}

type activeFault struct {
	FaultID   string    `json:"faultId"`
	Object    string    `json:"object"`
	RunID     string    `json:"runId,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt string    `json:"expiresAt,omitempty"`
	Expired   bool      `json:"expired"`
	Deleting  bool      `json:"deleting"`
}

// status lists the fault objects in the cluster now. An expired one means a `gameday start`
// died before cleaning up: remove it with `gameday stop`.
func (a *app) status(ctx context.Context, opts options) error {
	cl, _, err := a.cluster(opts)
	if err != nil {
		return err
	}
	objs, err := cl.activeObjects(ctx, labelFault+"=true")
	if err != nil {
		return err
	}
	now := a.now()
	out := []activeFault{}
	for _, o := range objs {
		af := activeFault{
			FaultID:   o.GetLabels()[labelFaultID],
			Object:    objectRef(o),
			RunID:     o.GetAnnotations()[annotationRunID],
			CreatedAt: o.GetCreationTimestamp().UTC(),
			ExpiresAt: o.GetAnnotations()[annotationExpiresAt],
			Deleting:  o.GetDeletionTimestamp() != nil,
		}
		if t, err := time.Parse(time.RFC3339, af.ExpiresAt); err == nil && now.After(t) {
			af.Expired = true
		}
		out = append(out, af)
	}
	if opts.output == "json" {
		return a.printJSON(map[string]any{"active": out})
	}
	if len(out) == 0 {
		a.printf(opts, "no fault objects in %s", faultNamespace)
		return nil
	}
	w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "FAULT\tOBJECT\tRUN\tEXPIRES\tSTATE")
	for _, af := range out {
		state := "active"
		switch {
		case af.Deleting:
			state = "deleting"
		case af.Expired:
			state = "EXPIRED: run `gameday stop " + af.FaultID + "`"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", af.FaultID, af.Object, af.RunID, af.ExpiresAt, state)
	}
	return w.Flush()
}
