package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testKubeconfig = `apiVersion: v1
kind: Config
current-context: lab
clusters:
- name: lab
  cluster: {server: "https://10.0.0.1"}
- name: other
  cluster: {server: "https://10.0.0.2"}
contexts:
- name: lab
  context: {cluster: lab, user: me}
- name: other
  context: {cluster: other, user: me}
users:
- name: me
  user: {token: not-a-real-token}
`

func TestNewKubeClients(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	kc, err := newKubeClients(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if kc.context != "lab" || kc.server != "https://10.0.0.1" || kc.kube == nil || kc.dyn == nil {
		t.Errorf("kc = %+v", kc)
	}
	if !strings.HasPrefix(kc.config.UserAgent, "cuj-discovery/") || kc.config.QPS != 20 {
		t.Errorf("config = %+v", kc.config)
	}
	kc, err = newKubeClients(path, "other")
	if err != nil || kc.context != "other" || kc.server != "https://10.0.0.2" {
		t.Errorf("context override: %+v, %v", kc, err)
	}
	if _, err := newKubeClients(path, "missing"); err == nil {
		t.Error("unknown context accepted")
	}
	broken := filepath.Join(t.TempDir(), "broken")
	if err := os.WriteFile(broken, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newKubeClients(broken, ""); err == nil || !strings.Contains(err.Error(), "read kubeconfig") {
		t.Errorf("broken kubeconfig: %v", err)
	}
}
