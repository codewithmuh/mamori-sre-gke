package main

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

func TestSelectCUJs(t *testing.T) {
	all, err := selectCUJs("")
	if err != nil || !slices.Equal(all, allCUJs) {
		t.Errorf("all = %v, %v", all, err)
	}
	got, err := selectCUJs(" view-cart,browse,,browse ")
	if err != nil || !slices.Equal(got, []string{"browse", "view-cart"}) {
		t.Errorf("got %v, %v", got, err)
	}
	if _, err := selectCUJs("browse,search"); err == nil {
		t.Error("unknown journey accepted")
	}
}

func TestSelectTargets(t *testing.T) {
	all, err := selectTargets("")
	if err != nil || len(all) != 10 || !slices.IsSortedFunc(all, func(a, b target) int { return strings.Compare(a.Service, b.Service) }) {
		t.Errorf("all = %v, %v", all, err)
	}
	got, err := selectTargets("redis-cart, adservice")
	if err != nil || len(got) != 2 || got[0].Service != "adservice" || got[1].Method != methodScaleToZero {
		t.Errorf("got %v, %v", got, err)
	}
	if _, err := selectTargets("loadgenerator"); err == nil || !strings.Contains(err.Error(), "known: adservice") {
		t.Errorf("err = %v", err)
	}
}

func TestNamesAndLabels(t *testing.T) {
	if objectName("redis-cart") != "cuj-discovery-redis-cart" || serviceHost("adservice") != "adservice.boutique.svc.cluster.local" {
		t.Error("names")
	}
	l := faultLabels("run1", "adservice")
	if l[labelFault] != "true" || l[labelManagedBy] != managedByValue || l[labelRun] != "run1" || l[labelTarget] != "adservice" {
		t.Errorf("labels = %v", l)
	}
	if !strings.Contains(ourSelector, anyFaultSelector) {
		t.Error("ourSelector must narrow anyFaultSelector")
	}
}

func TestLoggerSeverity(t *testing.T) {
	var buf bytes.Buffer
	log := newLogger(&buf)
	log.Info("i")
	log.Warn("w")
	log.Error("e")
	log.WithGroup("g").Info("grouped", "level", "kept")
	out := buf.String()
	for _, s := range []string{`"severity":"INFO","message":"i"`, `"severity":"WARNING"`, `"severity":"ERROR"`, `"g":{"level":"kept"}`} {
		if !strings.Contains(out, s) {
			t.Errorf("log lacks %s:\n%s", s, out)
		}
	}
	if severity(slog.LevelDebug) != "DEBUG" {
		t.Error("debug severity")
	}
}
