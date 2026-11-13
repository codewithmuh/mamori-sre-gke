package main

import (
	"context"
	"fmt"
	"text/tabwriter"
	"time"
)

type stopResult struct {
	Removed          []string `json:"removed"`
	DiscoveryMarkers []string `json:"discoveryMarkers,omitempty"`
}

// stop removes the objects of one fault (id) or of every fault (id ""), found by label in
// boutique, and waits until they are gone. Safe to repeat: nothing to remove is success.
// A `gameday start` still holding the fault notices within its poll interval and closes its
// timeline as removed-externally. `stop --all` also reports discovery markers, which it leaves
// alone (see configMapsGVR).
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
		if err := cl.remove(ctx, objs); err != nil {
			return err
		}
		log.Info("fault objects removed", "event", "fault_removed", "objects", res.Removed)
	} else {
		log.Info("nothing to remove", "event", "nothing_to_remove")
	}
	if id == "" {
		if res.DiscoveryMarkers, err = cl.discoveryMarkers(ctx); err != nil {
			return err
		}
		if len(res.DiscoveryMarkers) > 0 {
			log.Warn("discovery markers left in place", "event", "discovery_marker", "objects", res.DiscoveryMarkers)
		}
	}
	if opts.output == "json" {
		return a.printJSON(res)
	}
	for _, o := range objs {
		a.printf(opts, "removed %s (%s)", objectRef(o), o.GetLabels()[labelFaultID])
	}
	for _, m := range res.DiscoveryMarkers {
		a.printf(opts, "%s is a marker of a stopped discovery run, which gameday leaves alone: %s", m, discoveryHint)
	}
	if len(objs) == 0 && len(res.DiscoveryMarkers) == 0 {
		a.printf(opts, "nothing to stop")
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

type statusResult struct {
	Active           []activeFault `json:"active"`
	DiscoveryMarkers []string      `json:"discoveryMarkers"`
}

// status lists the fault objects in the cluster now. An expired one means a `gameday start`
// died before cleaning up: the fault-reaper CronJob removes it within a few minutes, or remove
// it with `gameday stop`.
func (a *app) status(ctx context.Context, opts options) error {
	cl, _, err := a.cluster(opts)
	if err != nil {
		return err
	}
	objs, err := cl.activeObjects(ctx, labelFault+"=true")
	if err != nil {
		return err
	}
	markers, err := cl.discoveryMarkers(ctx)
	if err != nil {
		return err
	}
	now := a.now()
	res := statusResult{Active: []activeFault{}, DiscoveryMarkers: markers}
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
		res.Active = append(res.Active, af)
	}
	if opts.output == "json" {
		return a.printJSON(res)
	}
	if len(res.Active) == 0 && len(markers) == 0 {
		a.printf(opts, "no fault objects in %s", faultNamespace)
		return nil
	}
	w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "FAULT\tOBJECT\tRUN\tEXPIRES\tSTATE")
	for _, af := range res.Active {
		state := "active"
		switch {
		case af.Deleting:
			state = "deleting"
		case af.Expired:
			state = "EXPIRED: run `gameday stop " + af.FaultID + "`"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", af.FaultID, af.Object, af.RunID, af.ExpiresAt, state)
	}
	for _, m := range markers {
		_, _ = fmt.Fprintf(w, "(discovery)\t%s\t\t\tdiscovery marker: %s\n", m, discoveryHint)
	}
	return w.Flush()
}
