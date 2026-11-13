package main

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// constraintList is `kubectl get constraints -o json`: every Gatekeeper constraint, of every kind.
type constraintList struct {
	Items []struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name        string            `json:"name"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec struct {
			EnforcementAction string `json:"enforcementAction"`
		} `json:"spec"`
		Status struct {
			AuditTimestamp  string `json:"auditTimestamp"`
			TotalViolations *int   `json:"totalViolations"`
			Violations      []struct {
				EnforcementAction string `json:"enforcementAction"`
				Kind              string `json:"kind"`
				Namespace         string `json:"namespace"`
				Name              string `json:"name"`
				Message           string `json:"message"`
			} `json:"violations"`
		} `json:"status"`
	} `json:"items"`
}

type gatekeeperData struct {
	Constraints     int `json:"constraints"`
	TotalViolations int `json:"totalViolations"`
	// Violations per the constraint's enforcement action (deny, warn, dryrun, scoped).
	ViolationsByAction map[string]int `json:"violationsByAction"`
	// Constraints that the audit has not reported on yet (no auditTimestamp).
	NotAudited int                `json:"notAudited"`
	LastAudit  string             `json:"lastAudit"`
	Items      []constraintStatus `json:"items"`
}

type constraintStatus struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// The readiness control the constraint implements (annotation mamori.dev/control-id), which
	// links the evidence to docs/compliance/controls.md.
	ControlID         string `json:"controlId"`
	EnforcementAction string `json:"enforcementAction"`
	AuditTimestamp    string `json:"auditTimestamp"`
	TotalViolations   int    `json:"totalViolations"`
	// As stored by the audit, which caps the list per constraint (--constraint-violations-limit).
	Violations []violation `json:"violations"`
}

type violation struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Message   string `json:"message"`
}

func parseConstraints(data []byte) (*gatekeeperData, bool, string, error) {
	var l constraintList
	if err := decode(data, &l); err != nil {
		return nil, false, "", err
	}
	d := &gatekeeperData{ViolationsByAction: map[string]int{}, Items: []constraintStatus{}}
	for _, c := range l.Items {
		cs := constraintStatus{
			Kind:              c.Kind,
			Name:              c.Metadata.Name,
			ControlID:         c.Metadata.Annotations["mamori.dev/control-id"],
			EnforcementAction: cmp.Or(c.Spec.EnforcementAction, "deny"), // Gatekeeper's default
			AuditTimestamp:    c.Status.AuditTimestamp,
			Violations:        []violation{},
		}
		for _, v := range c.Status.Violations {
			cs.Violations = append(cs.Violations, violation{Kind: v.Kind, Namespace: v.Namespace, Name: v.Name, Message: v.Message})
		}
		// totalViolations counts beyond the stored list; older audits may only have the list.
		cs.TotalViolations = len(c.Status.Violations)
		if c.Status.TotalViolations != nil {
			cs.TotalViolations = *c.Status.TotalViolations
		}
		if cs.AuditTimestamp == "" {
			d.NotAudited++
		}
		d.LastAudit = max(d.LastAudit, cs.AuditTimestamp)
		d.TotalViolations += cs.TotalViolations
		if cs.TotalViolations > 0 {
			d.ViolationsByAction[cs.EnforcementAction] += cs.TotalViolations
		}
		d.Items = append(d.Items, cs)
	}
	d.Constraints = len(d.Items)
	slices.SortFunc(d.Items, func(a, b constraintStatus) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Name, b.Name))
	})

	if d.Constraints == 0 {
		return d, false, "no constraints installed", nil
	}
	headline := fmt.Sprintf("%s, %s", plural(d.Constraints, "constraint", "constraints"), plural(d.TotalViolations, "violation", "violations"))
	if d.TotalViolations > 0 {
		headline += " (" + countsText(d.ViolationsByAction) + ")"
	}
	if d.LastAudit != "" {
		headline += ", last audit " + d.LastAudit
	}
	if d.NotAudited > 0 {
		headline += fmt.Sprintf(", %d not audited yet", d.NotAudited)
	}
	return d, d.TotalViolations > 0, headline, nil
}

// applicationList is `kubectl get applications.argoproj.io -A -o json`.
type applicationList struct {
	Items []struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec struct {
			Project string `json:"project"`
			Source  *struct {
				TargetRevision string `json:"targetRevision"`
			} `json:"source"`
			Sources []struct {
				TargetRevision string `json:"targetRevision"`
			} `json:"sources"`
		} `json:"spec"`
		Status struct {
			Sync struct {
				Status   string `json:"status"`
				Revision string `json:"revision"`
			} `json:"sync"`
			Health struct {
				Status string `json:"status"`
			} `json:"health"`
			OperationState struct {
				Phase string `json:"phase"`
			} `json:"operationState"`
			ReconciledAt string `json:"reconciledAt"`
		} `json:"status"`
	} `json:"items"`
}

type argoCDData struct {
	Applications  int           `json:"applications"`
	SyncedHealthy int           `json:"syncedHealthy"`
	Items         []application `json:"items"`
}

type application struct {
	Name           string `json:"name"`
	Namespace      string `json:"namespace"`
	Project        string `json:"project"`
	SyncStatus     string `json:"syncStatus"`
	HealthStatus   string `json:"healthStatus"`
	TargetRevision string `json:"targetRevision"`
	SyncedRevision string `json:"syncedRevision"`
	LastOperation  string `json:"lastOperation"`
	ReconciledAt   string `json:"reconciledAt"`
}

func parseArgoApps(data []byte) (*argoCDData, bool, string, error) {
	var l applicationList
	if err := decode(data, &l); err != nil {
		return nil, false, "", err
	}
	d := &argoCDData{Items: []application{}}
	var notOK []string
	for _, it := range l.Items {
		a := application{
			Name:           it.Metadata.Name,
			Namespace:      it.Metadata.Namespace,
			Project:        it.Spec.Project,
			SyncStatus:     cmp.Or(it.Status.Sync.Status, "Unknown"),
			HealthStatus:   cmp.Or(it.Status.Health.Status, "Unknown"),
			SyncedRevision: shortSHA(it.Status.Sync.Revision),
			LastOperation:  it.Status.OperationState.Phase,
			ReconciledAt:   it.Status.ReconciledAt,
		}
		switch {
		case it.Spec.Source != nil:
			a.TargetRevision = it.Spec.Source.TargetRevision
		case len(it.Spec.Sources) > 0:
			a.TargetRevision = it.Spec.Sources[0].TargetRevision
		}
		d.Items = append(d.Items, a)
	}
	slices.SortFunc(d.Items, func(a, b application) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	for _, a := range d.Items {
		if a.SyncStatus == "Synced" && a.HealthStatus == "Healthy" {
			d.SyncedHealthy++
		} else {
			notOK = append(notOK, fmt.Sprintf("%s %s/%s", a.Name, a.SyncStatus, a.HealthStatus))
		}
	}
	d.Applications = len(d.Items)

	if d.Applications == 0 {
		return d, true, "no Argo CD applications found", nil
	}
	headline := fmt.Sprintf("%d/%d applications Synced and Healthy", d.SyncedHealthy, d.Applications)
	if len(notOK) > 0 {
		headline += " (not: " + strings.Join(notOK, "; ") + ")"
	}
	return d, len(notOK) > 0, headline, nil
}

// countsText renders a count map in key order, e.g. "deny 2, warn 1".
func countsText(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}
