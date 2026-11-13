package main

import (
	"encoding/json"
	"fmt"
)

// dashboardJSON is a Cloud Monitoring dashboard with one row per journey:
// availability and latency of both signals against the target, and the error
// budget left on each of the journey's SLOs.
func (m *Model) dashboardJSON() (string, error) {
	const width, height = 16, 16
	var tiles []obj
	for i, j := range m.Journeys {
		y := i * height
		tiles = append(tiles,
			tile(0, y, width, height, m.sliChart(j, sliAvailability, j.Availability.Target)),
			tile(width, y, width, height, m.sliChart(j, sliLatency, j.Latency.Target)),
			tile(2*width, y, width, height, m.budgetChart(j)),
		)
	}
	d := obj{
		"displayName":  "Mamori CUJ SLOs",
		"mosaicLayout": obj{"columns": 3 * width, "tiles": tiles},
	}
	b, err := json.Marshal(d)
	return string(b), err
}

func tile(x, y, w, h int, widget obj) obj {
	return obj{"xPos": x, "yPos": y, "width": w, "height": h, "widget": widget}
}

func (m *Model) sliChart(j Journey, sli string, target float64) obj {
	pw := m.Spec.Prober.RecordWindows[len(m.Spec.Prober.RecordWindows)-1]
	mw := m.Spec.Mesh.RecordWindows[0]
	sets := []obj{promDataSet(selector(recCUJ(signalProber, sli, pw), matcher("cuj", "=", j.ID)), fmt.Sprintf("prober (%s)", pw))}
	if j.HasMesh() {
		sets = append(sets, promDataSet(selector(recCUJ(signalMesh, sli, mw), matcher("cuj", "=", j.ID)), fmt.Sprintf("mesh (%s)", mw)))
	}
	return obj{
		"title": fmt.Sprintf("CUJ %s: %s", j.ID, sli),
		"xyChart": obj{
			"dataSets":   sets,
			"thresholds": []obj{{"value": target, "label": "target " + percent(target) + " %"}},
			"yAxis":      obj{"label": "ratio", "scale": "LINEAR"},
		},
	}
}

func promDataSet(query, legend string) obj {
	return obj{
		"timeSeriesQuery": obj{"prometheusQuery": query},
		"plotType":        "LINE",
		"legendTemplate":  legend,
		"targetAxis":      "Y1",
	}
}

func (m *Model) budgetChart(j Journey) obj {
	var sets []obj
	for _, s := range j.SLOs {
		sets = append(sets, obj{
			"timeSeriesQuery": obj{"timeSeriesFilter": obj{
				"filter": fmt.Sprintf("select_slo_budget_fraction(\"%s\")", ref("google_monitoring_slo", tfName(j.ID, s.Signal, s.SLI), "name")),
			}},
			"plotType":       "LINE",
			"legendTemplate": s.ID(),
			"targetAxis":     "Y1",
		})
	}
	return obj{
		"title": fmt.Sprintf("CUJ %s: error budget left (%s)", j.ID, m.Spec.Window),
		"xyChart": obj{
			"dataSets":   sets,
			"thresholds": []obj{{"value": 0, "label": "budget spent"}},
			"yAxis":      obj{"label": "fraction of budget left", "scale": "LINEAR"},
		},
	}
}
