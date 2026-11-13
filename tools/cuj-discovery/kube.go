package main

import (
	"fmt"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// kubeClients are built once from the kubeconfig, like kubectl does.
type kubeClients struct {
	config  *rest.Config
	kube    kubernetes.Interface
	dyn     dynamic.Interface
	context string // kubeconfig context in use, for the log
	server  string
}

// newKubeClients reads the kubeconfig (an explicit path, else $KUBECONFIG,
// else ~/.kube/config) and an optional context. For GKE the kubeconfig comes
// from `gcloud container clusters get-credentials` and needs
// gke-gcloud-auth-plugin on the PATH.
func newKubeClients(kubeconfig, kubeContext string) (*kubeClients, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: kubeContext})
	raw, err := cc.RawConfig()
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig: %w", err)
	}
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	cfg.UserAgent = "cuj-discovery/" + version
	// The preflight lists every namespaced kind once; the default 5 QPS would
	// make that take tens of seconds.
	cfg.QPS, cfg.Burst = 20, 40

	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	current := raw.CurrentContext
	if kubeContext != "" {
		current = kubeContext
	}
	return &kubeClients{config: cfg, kube: kube, dyn: dyn, context: current, server: cfg.Host}, nil
}
