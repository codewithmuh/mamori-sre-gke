package main

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Below this share of error budget left, an SLO is "low". The error-budget policy acts at zero;
// this only makes the trend visible a week earlier.
const lowBudget = 0.25

// sloStatusInput is slo-status.json, which the workflow builds from the Cloud Monitoring API:
// every SLO of every service in the project, with the newest point of
// select_slo_compliance and select_slo_budget_fraction. Null means no data.
type sloStatusInput struct {
	SLOs *[]struct {
		Name               string   `json:"name"`
		DisplayName        string   `json:"displayName"`
		Service            string   `json:"service"`
		ServiceDisplayName string   `json:"serviceDisplayName"`
		Goal               float64  `json:"goal"`
		RollingPeriod      string   `json:"rollingPeriod"`
		CalendarPeriod     string   `json:"calendarPeriod"`
		Compliance         *float64 `json:"compliance"`
		BudgetFraction     *float64 `json:"budgetFraction"`
	} `json:"slos"`
}

type sloData struct {
	SLOs      []slo `json:"slos"`
	Exhausted int   `json:"exhausted"`
	Low       int   `json:"low"`
	NoData    int   `json:"noData"`
}

type slo struct {
	Name        string  `json:"name"`
	DisplayName string  `json:"displayName"`
	Service     string  `json:"service"`
	Goal        float64 `json:"goal"`
	Window      string  `json:"window"`
	// Share of good events since the start of the compliance window.
	Compliance *float64 `json:"compliance"`
	// Share of the error budget left: 1 untouched, 0 spent, below 0 overspent.
	BudgetRemaining *float64 `json:"budgetRemaining"`
	// "ok", "low", "exhausted" or "no data".
	State string `json:"state"`
}

func parseSLOStatus(data []byte) (*sloData, bool, string, error) {
	var in sloStatusInput
	if err := decode(data, &in); err != nil {
		return nil, false, "", err
	}
	if in.SLOs == nil {
		return nil, false, "", fmt.Errorf("slos is missing")
	}

	d := &sloData{SLOs: []slo{}}
	for _, s := range *in.SLOs {
		if s.Goal <= 0 || s.Goal >= 1 {
			return nil, false, "", fmt.Errorf("SLO %q has goal %v, want a fraction between 0 and 1", s.Name, s.Goal)
		}
		o := slo{
			Name:            s.Name,
			DisplayName:     cmp.Or(s.DisplayName, lastSegment(s.Name)),
			Service:         cmp.Or(s.ServiceDisplayName, lastSegment(s.Service)),
			Goal:            s.Goal,
			Window:          window(s.RollingPeriod, s.CalendarPeriod),
			Compliance:      s.Compliance,
			BudgetRemaining: s.BudgetFraction,
		}
		switch {
		case o.BudgetRemaining == nil:
			o.State = "no data"
			d.NoData++
		case *o.BudgetRemaining <= 0:
			o.State = "exhausted"
			d.Exhausted++
		case *o.BudgetRemaining < lowBudget:
			o.State = "low"
			d.Low++
		default:
			o.State = "ok"
		}
		d.SLOs = append(d.SLOs, o)
	}
	slices.SortFunc(d.SLOs, func(a, b slo) int {
		return cmp.Or(cmp.Compare(a.Service, b.Service), cmp.Compare(a.DisplayName, b.DisplayName), cmp.Compare(a.Name, b.Name))
	})

	if len(d.SLOs) == 0 {
		return d, false, "no SLOs defined in Cloud Monitoring yet", nil
	}
	headline := fmt.Sprintf("%s: %d exhausted, %d low (under %d %% left), %d without data",
		plural(len(d.SLOs), "SLO", "SLOs"), d.Exhausted, d.Low, int(lowBudget*100), d.NoData)
	if lowest := lowestBudget(d.SLOs); lowest != nil {
		headline += fmt.Sprintf("; least budget left: %s (%s)", lowest.DisplayName, percent(*lowest.BudgetRemaining))
	}
	return d, d.Exhausted > 0, headline, nil
}

func lowestBudget(slos []slo) *slo {
	var lowest *slo
	for i := range slos {
		if b := slos[i].BudgetRemaining; b != nil && (lowest == nil || *b < *lowest.BudgetRemaining) {
			lowest = &slos[i]
		}
	}
	return lowest
}

// window renders "2419200s" as "rolling 28d" and "MONTH" as "calendar month".
func window(rolling, calendar string) string {
	if rolling != "" {
		if secs, err := strconv.ParseFloat(strings.TrimSuffix(rolling, "s"), 64); err == nil && secs > 0 {
			d := time.Duration(secs * float64(time.Second))
			if d%(24*time.Hour) == 0 {
				return fmt.Sprintf("rolling %dd", int(d/(24*time.Hour)))
			}
			return "rolling " + d.String()
		}
		return "rolling " + rolling
	}
	if calendar != "" {
		return "calendar " + strings.ToLower(calendar)
	}
	return "unknown"
}

func lastSegment(name string) string {
	return name[strings.LastIndex(name, "/")+1:]
}

func percent(f float64) string {
	return strconv.FormatFloat(f*100, 'f', 1, 64) + " %"
}
