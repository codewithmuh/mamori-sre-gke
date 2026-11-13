package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const (
	fieldManager        = "mamori-executor"
	restartedAtKey      = "kubectl.kubernetes.io/restartedAt"
	lastAppliedKey      = "kubectl.kubernetes.io/last-applied-configuration"
	argoTrackingIDKey   = "argocd.argoproj.io/tracking-id"
	argoManagerFragment = "argocd"
)

// cluster runs the live checks and the actions. Check is read-only; Execute repeats the
// checks immediately before it writes, because minutes pass between proposal and approval.
type cluster interface {
	Check(ctx context.Context, a Action) error
	Execute(ctx context.Context, a Action) (string, error)
}

// kubeCluster acts with the executor's own service account. Its Role in boutique allows
// only what these functions call (see deploy/executor/rbac.yaml).
type kubeCluster struct {
	client kubernetes.Interface
	dyn    dynamic.Interface
	now    func() time.Time
}

// Check runs the read-only checks for an action.
func (k *kubeCluster) Check(ctx context.Context, a Action) error {
	_, name, r := parseTarget(a.Target)
	if r != nil {
		return r
	}
	switch a.Type {
	case actionRestart:
		_, err := k.restartable(ctx, name)
		return err
	case actionScale:
		replicas, r := replicasParam(a.Params)
		if r != nil {
			return r
		}
		_, err := k.scalable(ctx, name, replicas)
		return err
	case actionRemoveFault:
		_, err := k.faultObject(ctx, a.Target)
		return err
	}
	return refuse(reasonNotAllowed, "action type %q is not known", a.Type)
}

// Execute re-checks and runs an action, and returns a one-line result.
func (k *kubeCluster) Execute(ctx context.Context, a Action) (string, error) {
	_, name, r := parseTarget(a.Target)
	if r != nil {
		return "", r
	}
	switch a.Type {
	case actionRestart:
		return k.restart(ctx, name)
	case actionScale:
		replicas, r := replicasParam(a.Params)
		if r != nil {
			return "", r
		}
		return k.scale(ctx, name, replicas)
	case actionRemoveFault:
		return k.removeFault(ctx, a.Target)
	}
	return "", refuse(reasonNotAllowed, "action type %q is not known", a.Type)
}

func (k *kubeCluster) deployment(ctx context.Context, name string) (*appsv1.Deployment, error) {
	d, err := k.client.AppsV1().Deployments(targetNamespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, refuse(reasonNotAllowed, "deployment %s/%s does not exist", targetNamespace, name)
	}
	if err != nil {
		return nil, fmt.Errorf("get deployment %s/%s: %w", targetNamespace, name, err)
	}
	return d, nil
}

func (k *kubeCluster) restartable(ctx context.Context, name string) (*appsv1.Deployment, error) {
	d, err := k.deployment(ctx, name)
	if err != nil {
		return nil, err
	}
	if d.Spec.Paused {
		return nil, refuse(reasonNotAllowed, "deployment %s is paused, so a restart would do nothing", name)
	}
	return d, nil
}

// restart does what `kubectl rollout restart` does: it stamps the pod template, and the
// Deployment controller replaces the pods one by one. Argo CD's default (legacy three-way)
// diff ignores the annotation because Git never sets it, so self-heal leaves it alone; Argo
// CD's own built-in "restart" resource action writes the same annotation.
func (k *kubeCluster) restart(ctx context.Context, name string) (string, error) {
	if _, err := k.restartable(ctx, name); err != nil {
		return "", err
	}
	stamp := k.now().UTC().Format(time.RFC3339)
	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
			"annotations": map[string]string{restartedAtKey: stamp},
		}}},
	})
	if err != nil {
		return "", fmt.Errorf("build patch: %w", err)
	}
	_, err = k.client.AppsV1().Deployments(targetNamespace).Patch(ctx, name, types.StrategicMergePatchType, patch,
		metav1.PatchOptions{FieldManager: fieldManager})
	if err != nil {
		return "", fmt.Errorf("patch deployment %s/%s: %w", targetNamespace, name, err)
	}
	return fmt.Sprintf("rolling restart of deployment/%s started (%s=%s)", name, restartedAtKey, stamp), nil
}

// scalable refuses a scale that GitOps or an autoscaler would immediately undo, or that
// changes nothing.
func (k *kubeCluster) scalable(ctx context.Context, name string, replicas int32) (*appsv1.Deployment, error) {
	d, err := k.deployment(ctx, name)
	if err != nil {
		return nil, err
	}
	if field := gitOwnsReplicas(d); field != "" {
		return nil, refuse(reasonNotAllowed,
			"spec.replicas of deployment %s is set in Git (%s), so Argo CD self-heal would undo a scale; change it in Git", name, field)
	}
	hpas, err := k.client.AutoscalingV2().HorizontalPodAutoscalers(targetNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list HorizontalPodAutoscalers in %s: %w", targetNamespace, err)
	}
	for _, h := range hpas.Items {
		ref := h.Spec.ScaleTargetRef
		if ref.Kind == "Deployment" && ref.Name == name {
			return nil, refuse(reasonNotAllowed,
				"HorizontalPodAutoscaler %s owns the replicas of deployment %s; change its minReplicas in Git instead", h.Name, name)
		}
	}
	current := int32(1)
	if d.Spec.Replicas != nil {
		current = *d.Spec.Replicas
	}
	if current == replicas {
		return nil, refuse(reasonNotAllowed, "deployment %s already has %d replicas", name, replicas)
	}
	return d, nil
}

// gitOwnsReplicas says where Git sets spec.replicas, or "" if it does not. Argo CD applies
// client-side by default and records what it applied in the last-applied annotation; with
// server-side apply its field manager owns the field instead. Either way a change to the
// field shows as drift and self-heal reverts it.
func gitOwnsReplicas(d *appsv1.Deployment) string {
	if raw, ok := d.Annotations[lastAppliedKey]; ok {
		var applied struct {
			Spec struct {
				Replicas *int32 `json:"replicas"`
			} `json:"spec"`
		}
		if json.Unmarshal([]byte(raw), &applied) == nil && applied.Spec.Replicas != nil {
			return "last-applied-configuration"
		}
	}
	for _, mf := range d.ManagedFields {
		if mf.Operation != metav1.ManagedFieldsOperationApply || !strings.Contains(mf.Manager, argoManagerFragment) || mf.FieldsV1 == nil {
			continue
		}
		var fields map[string]map[string]any
		if json.Unmarshal(mf.FieldsV1.GetRawBytes(), &fields) == nil {
			if _, ok := fields["f:spec"]["f:replicas"]; ok {
				return "server-side apply by " + mf.Manager
			}
		}
	}
	return ""
}

func (k *kubeCluster) scale(ctx context.Context, name string, replicas int32) (string, error) {
	if _, err := k.scalable(ctx, name, replicas); err != nil {
		return "", err
	}
	deployments := k.client.AppsV1().Deployments(targetNamespace)
	sc, err := deployments.GetScale(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get scale of deployment %s/%s: %w", targetNamespace, name, err)
	}
	from := sc.Spec.Replicas
	sc.Spec.Replicas = replicas
	// The scale keeps its resourceVersion, so a change made in between makes this fail
	// with a conflict instead of overwriting it.
	if _, err := deployments.UpdateScale(ctx, name, sc, metav1.UpdateOptions{FieldManager: fieldManager}); err != nil {
		return "", fmt.Errorf("update scale of deployment %s/%s: %w", targetNamespace, name, err)
	}
	return fmt.Sprintf("deployment/%s scaled from %d to %d replicas", name, from, replicas), nil
}

// faultObject fetches a remove-fault target and checks it really is an injected fault.
// RBAC cannot filter by label, so this check (and the admission policy) is what stops the
// executor from deleting the shop's own VirtualServices or Deployments.
func (k *kubeCluster) faultObject(ctx context.Context, target string) (*unstructured.Unstructured, error) {
	kind, name, r := parseTarget(target)
	if r != nil {
		return nil, r
	}
	gvr, ok := faultKinds[kind]
	if !ok {
		return nil, refuse(reasonNotAllowed, "kind %q cannot be removed as a fault", kind)
	}
	obj, err := k.dyn.Resource(gvr).Namespace(targetNamespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, refuse(reasonNotAllowed, "%s %s/%s does not exist (already removed?)", kind, targetNamespace, name)
	}
	if err != nil {
		return nil, fmt.Errorf("get %s %s/%s: %w", kind, targetNamespace, name, err)
	}
	if v := obj.GetLabels()[faultLabel]; v != "true" {
		return nil, refuse(reasonNotAllowed, "%s %s/%s is not an injected fault: label %s is %q, not \"true\"", kind, targetNamespace, name, faultLabel, v)
	}
	if _, tracked := obj.GetAnnotations()[argoTrackingIDKey]; tracked {
		return nil, refuse(reasonNotAllowed, "%s %s/%s is managed by Argo CD; faults never live in Git, so remove it in Git", kind, targetNamespace, name)
	}
	if obj.GetDeletionTimestamp() != nil {
		return nil, refuse(reasonNotAllowed, "%s %s/%s is already being deleted", kind, targetNamespace, name)
	}
	return obj, nil
}

func (k *kubeCluster) removeFault(ctx context.Context, target string) (string, error) {
	obj, err := k.faultObject(ctx, target)
	if err != nil {
		return "", err
	}
	kind, name, _ := parseTarget(target)
	uid, rv := obj.GetUID(), obj.GetResourceVersion()
	// The preconditions make the API server delete exactly the object checked above: if it
	// was replaced or relabelled since, the delete fails instead.
	policy := metav1.DeletePropagationBackground
	err = k.dyn.Resource(faultKinds[kind]).Namespace(targetNamespace).Delete(ctx, name, metav1.DeleteOptions{
		Preconditions:     &metav1.Preconditions{UID: &uid, ResourceVersion: &rv},
		PropagationPolicy: &policy,
	})
	if err != nil {
		return "", fmt.Errorf("delete %s %s/%s: %w", kind, targetNamespace, name, err)
	}
	return fmt.Sprintf("deleted %s %s/%s (%s=%s)", kind, targetNamespace, name, faultIDLabel, obj.GetLabels()[faultIDLabel]), nil
}
