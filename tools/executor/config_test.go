package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestParseConfig(t *testing.T) {
	cfg := testConfig(t, true)
	if !cfg.Enabled || cfg.ttl != 15*time.Minute || !cfg.isApprover("U0APPROVER") || cfg.isApprover("U0OTHER") {
		t.Fatalf("cfg = %+v", cfg)
	}
	for _, typ := range []string{actionRestart, actionScale, actionRemoveFault} {
		if !cfg.allows(typ) {
			t.Errorf("%s should be allowed", typ)
		}
	}
	if cfg.allows(actionRollback) || cfg.allows("exec") {
		t.Error("rollback and unknown types are never allowed")
	}
	if err := cfg.approvalPath(); err != nil {
		t.Error(err)
	}
}

func TestEnabledIsFailClosed(t *testing.T) {
	for value, want := range map[string]bool{"true": true, " true\n": true, "false": false, "True": false, "yes": false, "1": false, "": false} {
		cfg, err := parseConfig(map[string]string{"enabled": value, "config.yaml": testConfigYAML})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Enabled != want {
			t.Errorf("enabled=%q gives %v, want %v", value, cfg.Enabled, want)
		}
	}
}

func TestParseConfigErrors(t *testing.T) {
	cases := map[string]string{
		"empty":             ``,
		"not yaml":          `slack: [`,
		"unknown key":       "slack: {channel: C0EXEC0001}\nrestrat: {deployments: [frontend]}",
		"bad channel":       `slack: {channel: "#incidents"}`,
		"bad approver":      `slack: {approvers: ["rashid"]}`,
		"ttl unparsable":    `proposalTTL: soon`,
		"ttl too short":     `proposalTTL: 10s`,
		"ttl too long":      `proposalTTL: 3h`,
		"bad restart name":  `restart: {deployments: ["Front_End"]}`,
		"bad scale name":    `scale: {"Front_End": {min: 1, max: 2}}`,
		"scale min zero":    `scale: {frontend: {min: 0, max: 2}}`,
		"scale min > max":   `scale: {frontend: {min: 3, max: 2}}`,
		"scale above hard":  `scale: {frontend: {min: 1, max: 6}}`,
		"unsupported kind":  `removeFault: {kinds: [secret]}`,
		"rollback enabled":  `rollback: {deployments: [checkoutservice]}`,
		"rollback as empty": `rollback: {}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseConfig(map[string]string{"enabled": "true", "config.yaml": raw}); err == nil {
				t.Fatal("want an error")
			}
		})
	}
	_, err := parseConfig(map[string]string{"config.yaml": `rollback: {}`})
	if err == nil || !strings.Contains(err.Error(), "Git") {
		t.Errorf("rollback error should explain Git: %v", err)
	}
}

func TestApprovalPath(t *testing.T) {
	cfg, err := parseConfig(map[string]string{"config.yaml": "slack: {channel: C0EXEC0001}\nrestart: {deployments: [frontend]}"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.approvalPath() == nil {
		t.Error("no approvers must mean no approval path")
	}
	if cfg.ttl != defaultTTL {
		t.Errorf("default ttl = %s", cfg.ttl)
	}
}

func TestConfigMapSource(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configMapName, Namespace: "mamori-agent", ResourceVersion: "1"},
		Data:       map[string]string{"enabled": "false", "config.yaml": testConfigYAML},
	}
	client := fake.NewClientset(cm)
	src := &configMapSource{client: client, namespace: "mamori-agent"}
	ctx := context.Background()

	cfg, err := src.Get(ctx)
	if err != nil || cfg.Enabled {
		t.Fatalf("Get = %+v, %v", cfg, err)
	}
	// Flipping the switch counts from the next read.
	cm.Data["enabled"] = "true"
	cm.ResourceVersion = "2"
	if _, err := client.CoreV1().ConfigMaps("mamori-agent").Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if cfg, err = src.Get(ctx); err != nil || !cfg.Enabled {
		t.Fatalf("after enable: %+v, %v", cfg, err)
	}

	// An invalid config is an error, so callers treat the executor as off.
	cm.Data["config.yaml"] = "rollback: {}"
	cm.ResourceVersion = "3"
	if _, err := client.CoreV1().ConfigMaps("mamori-agent").Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Get(ctx); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("want invalid error, got %v", err)
	}

	missing := &configMapSource{client: fake.NewClientset(), namespace: "mamori-agent"}
	if _, err := missing.Get(ctx); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want not found, got %v", err)
	}

	broken := fake.NewClientset()
	broken.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	if _, err := (&configMapSource{client: broken, namespace: "mamori-agent"}).Get(ctx); err == nil {
		t.Fatal("want an error")
	}
}
