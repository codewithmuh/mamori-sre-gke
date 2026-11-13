package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

func deployment(name string, replicas int32, mutate ...func(*appsv1.Deployment)) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: targetNamespace, ResourceVersion: "7"},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(replicas)},
	}
	for _, m := range mutate {
		m(d)
	}
	return d
}

func faultObject(apiVersion, kind, name string, labels map[string]any, annotations map[string]any) *unstructured.Unstructured {
	meta := map[string]any{"name": name, "namespace": targetNamespace, "uid": "uid-" + name, "resourceVersion": "42"}
	if labels != nil {
		meta["labels"] = labels
	}
	if annotations != nil {
		meta["annotations"] = annotations
	}
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": meta}}
}

var faultLabels = map[string]any{faultLabel: "true", faultIDLabel: "F01-currency-abort"}

func newKube(objs []runtime.Object, dynObjs ...runtime.Object) (*kubeCluster, *fake.Clientset, *dynamicfake.FakeDynamicClient) {
	client := fake.NewClientset(objs...)
	listKinds := map[schema.GroupVersionResource]string{}
	for _, gvr := range faultKinds {
		listKinds[gvr] = "List"
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds)
	for _, o := range dynObjs {
		u := o.(*unstructured.Unstructured)
		// The tracker would guess "podchaoses" from the kind; give it the real resource.
		gvr := faultKinds[strings.ToLower(u.GetKind())]
		if err := dyn.Tracker().Create(gvr, u, targetNamespace); err != nil {
			panic(err)
		}
	}
	k := &kubeCluster{client: client, dyn: dyn, now: func() time.Time { return time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC) }}
	return k, client, dyn
}

func TestRestart(t *testing.T) {
	k, client, _ := newKube([]runtime.Object{deployment("currencyservice", 1)})
	a := Action{Type: actionRestart, Namespace: targetNamespace, Target: "deployment/currencyservice"}
	if err := k.Check(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	res, err := k.Execute(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "2026-11-13T09:00:00Z") {
		t.Errorf("result = %q", res)
	}
	var patch k8stesting.PatchAction
	for _, act := range client.Actions() {
		if p, ok := act.(k8stesting.PatchAction); ok {
			patch = p
		}
	}
	if patch == nil {
		t.Fatal("no patch sent")
	}
	// The patch touches only the restart annotation of the pod template.
	var body map[string]any
	if err := json.Unmarshal(patch.GetPatch(), &body); err != nil {
		t.Fatal(err)
	}
	want := `{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":"2026-11-13T09:00:00Z"}}}}}`
	if got, _ := json.Marshal(body); string(got) != want {
		t.Errorf("patch = %s", got)
	}
	d, _ := client.AppsV1().Deployments(targetNamespace).Get(context.Background(), "currencyservice", metav1.GetOptions{})
	if d.Spec.Template.Annotations[restartedAtKey] == "" {
		t.Error("annotation not set")
	}
}

func TestRestartRefusals(t *testing.T) {
	paused := deployment("frontend", 1, func(d *appsv1.Deployment) { d.Spec.Paused = true })
	k, _, _ := newKube([]runtime.Object{paused})
	for target, want := range map[string]string{
		"deployment/frontend": "paused",
		"deployment/missing":  "does not exist",
	} {
		_, err := k.Execute(context.Background(), Action{Type: actionRestart, Namespace: targetNamespace, Target: target})
		if r := asRefusal(t, err); !strings.Contains(r.msg, want) {
			t.Errorf("%s: %s", target, r.msg)
		}
	}
}

func TestGitOwnsReplicas(t *testing.T) {
	entry := func(manager string, op metav1.ManagedFieldsOperationType, fields string) metav1.ManagedFieldsEntry {
		return metav1.ManagedFieldsEntry{Manager: manager, Operation: op, FieldsV1: metav1.NewFieldsV1(fields)}
	}
	cases := []struct {
		name string
		d    *appsv1.Deployment
		want string
	}{
		{"nothing", deployment("a", 1), ""},
		{"last-applied with replicas", deployment("a", 1, func(d *appsv1.Deployment) {
			d.Annotations = map[string]string{lastAppliedKey: `{"spec":{"replicas":2}}`}
		}), "last-applied-configuration"},
		{"last-applied without replicas", deployment("a", 1, func(d *appsv1.Deployment) {
			d.Annotations = map[string]string{lastAppliedKey: `{"spec":{}}`}
		}), ""},
		{"broken last-applied", deployment("a", 1, func(d *appsv1.Deployment) {
			d.Annotations = map[string]string{lastAppliedKey: `{`}
		}), ""},
		{"argo cd server-side apply owns replicas", deployment("a", 1, func(d *appsv1.Deployment) {
			d.ManagedFields = []metav1.ManagedFieldsEntry{entry("argocd-controller", metav1.ManagedFieldsOperationApply, `{"f:spec":{"f:replicas":{}}}`)}
		}), "server-side apply by argocd-controller"},
		{"argo cd applies other fields", deployment("a", 1, func(d *appsv1.Deployment) {
			d.ManagedFields = []metav1.ManagedFieldsEntry{entry("argocd-controller", metav1.ManagedFieldsOperationApply, `{"f:spec":{"f:template":{}}}`)}
		}), ""},
		{"controller updates replicas", deployment("a", 1, func(d *appsv1.Deployment) {
			d.ManagedFields = []metav1.ManagedFieldsEntry{entry("kube-controller-manager", metav1.ManagedFieldsOperationUpdate, `{"f:spec":{"f:replicas":{}}}`)}
		}), ""},
	}
	for _, c := range cases {
		if got := gitOwnsReplicas(c.d); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func scaleReactors(client *fake.Clientset, current int32, updated *int32) {
	client.PrependReactor("get", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "scale" {
			return false, nil, nil
		}
		return true, &autoscalingv1.Scale{
			ObjectMeta: metav1.ObjectMeta{Name: "cartservice", Namespace: targetNamespace, ResourceVersion: "7"},
			Spec:       autoscalingv1.ScaleSpec{Replicas: current},
		}, nil
	})
	client.PrependReactor("update", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "scale" {
			return false, nil, nil
		}
		sc := a.(k8stesting.UpdateAction).GetObject().(*autoscalingv1.Scale)
		*updated = sc.Spec.Replicas
		return true, sc, nil
	})
}

func TestScale(t *testing.T) {
	k, client, _ := newKube([]runtime.Object{deployment("cartservice", 1)})
	var updated int32
	scaleReactors(client, 1, &updated)
	a := Action{Type: actionScale, Namespace: targetNamespace, Target: "deployment/cartservice", Params: map[string]any{"replicas": 2}}
	if err := k.Check(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	res, err := k.Execute(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if updated != 2 || res != "deployment/cartservice scaled from 1 to 2 replicas" {
		t.Errorf("updated = %d, result = %q", updated, res)
	}
}

func TestScaleRefusals(t *testing.T) {
	lastApplied := deployment("frontend", 1, func(d *appsv1.Deployment) {
		d.Annotations = map[string]string{lastAppliedKey: `{"apiVersion":"apps/v1","kind":"Deployment","spec":{"replicas":1}}`}
	})
	// Git without replicas, Argo CD's last-applied present: scaling is fine (checked below).
	noReplicasInGit := deployment("paymentservice", 1, func(d *appsv1.Deployment) {
		d.Annotations = map[string]string{lastAppliedKey: `{"spec":{"template":{}}}`}
	})
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "currencyservice", Namespace: targetNamespace},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "currencyservice", APIVersion: "apps/v1"},
		},
	}
	k, _, _ := newKube([]runtime.Object{lastApplied, noReplicasInGit, hpa, deployment("currencyservice", 1), deployment("cartservice", 2)})
	cases := map[string]string{
		"deployment/frontend":        "set in Git (last-applied",
		"deployment/currencyservice": "HorizontalPodAutoscaler currencyservice",
		"deployment/cartservice":     "already has 2 replicas",
		"deployment/missing":         "does not exist",
	}
	for target, want := range cases {
		err := k.Check(context.Background(), Action{Type: actionScale, Namespace: targetNamespace, Target: target, Params: map[string]any{"replicas": 2}})
		if r := asRefusal(t, err); !strings.Contains(r.msg, want) {
			t.Errorf("%s: %q does not contain %q", target, r.msg, want)
		}
	}
	if err := k.Check(context.Background(), Action{Type: actionScale, Namespace: targetNamespace, Target: "deployment/paymentservice", Params: map[string]any{"replicas": 2}}); err != nil {
		t.Errorf("paymentservice: %v", err)
	}
	if err := k.Check(context.Background(), Action{Type: actionScale, Namespace: targetNamespace, Target: "deployment/paymentservice", Params: map[string]any{"replicas": "x"}}); err == nil {
		t.Error("bad replicas must fail")
	}
}

func TestScaleAPIErrors(t *testing.T) {
	k, client, _ := newKube([]runtime.Object{deployment("cartservice", 1)})
	client.PrependReactor("list", "horizontalpodautoscalers", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("timeout")
	})
	err := k.Check(context.Background(), Action{Type: actionScale, Namespace: targetNamespace, Target: "deployment/cartservice", Params: map[string]any{"replicas": 2}})
	var r *refusal
	if err == nil || errors.As(err, &r) {
		t.Fatalf("want a plain error, got %v", err)
	}

	k, client, _ = newKube([]runtime.Object{deployment("cartservice", 1)})
	client.PrependReactor("get", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() == "scale" {
			return true, nil, errors.New("boom")
		}
		return false, nil, nil
	})
	if _, err := k.Execute(context.Background(), Action{Type: actionScale, Namespace: targetNamespace, Target: "deployment/cartservice", Params: map[string]any{"replicas": 2}}); err == nil {
		t.Error("GetScale error must fail")
	}

	k, client, _ = newKube([]runtime.Object{deployment("cartservice", 1)})
	var updated int32
	scaleReactors(client, 1, &updated)
	client.PrependReactor("update", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, "cartservice", errors.New("changed"))
	})
	if _, err := k.Execute(context.Background(), Action{Type: actionScale, Namespace: targetNamespace, Target: "deployment/cartservice", Params: map[string]any{"replicas": 2}}); !apierrors.IsConflict(errors.Unwrap(err)) {
		t.Errorf("want conflict, got %v", err)
	}

	k, client, _ = newKube([]runtime.Object{deployment("frontend", 1)})
	client.PrependReactor("patch", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "frontend", errors.New("ValidatingAdmissionPolicy denied"))
	})
	if _, err := k.Execute(context.Background(), Action{Type: actionRestart, Namespace: targetNamespace, Target: "deployment/frontend"}); !apierrors.IsForbidden(errors.Unwrap(err)) {
		t.Errorf("want forbidden, got %v", err)
	}

	k, client, _ = newKube(nil)
	client.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("unavailable")
	})
	if err := k.Check(context.Background(), Action{Type: actionRestart, Namespace: targetNamespace, Target: "deployment/frontend"}); err == nil {
		t.Error("get error must fail")
	}
}

func TestRemoveFault(t *testing.T) {
	vs := faultObject("networking.istio.io/v1", "VirtualService", "f01-currency-abort", faultLabels, nil)
	k, _, dyn := newKube(nil, vs)
	a := Action{Type: actionRemoveFault, Namespace: targetNamespace, Target: "virtualservice/f01-currency-abort"}
	if err := k.Check(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	res, err := k.Execute(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "F01-currency-abort") {
		t.Errorf("result = %q", res)
	}
	var del k8stesting.DeleteAction
	for _, act := range dyn.Actions() {
		if d, ok := act.(k8stesting.DeleteAction); ok {
			del = d
		}
	}
	if del == nil {
		t.Fatal("no delete")
	}
	pre := del.GetDeleteOptions().Preconditions
	if pre == nil || *pre.UID != "uid-f01-currency-abort" || *pre.ResourceVersion != "42" {
		t.Errorf("preconditions = %+v", pre)
	}
	_, err = dyn.Resource(faultKinds["virtualservice"]).Namespace(targetNamespace).Get(context.Background(), "f01-currency-abort", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("object still there: %v", err)
	}
}

func TestRemoveFaultLabelCheck(t *testing.T) {
	now := metav1.NewTime(time.Now())
	deleting := faultObject("chaos-mesh.org/v1alpha1", "PodChaos", "f03-redis-cart-pod-kill", faultLabels, nil)
	deleting.SetDeletionTimestamp(&now)
	objs := []runtime.Object{
		// The shop's own VirtualService: no fault label. RBAC would allow deleting it.
		faultObject("networking.istio.io/v1", "VirtualService", "frontend", map[string]any{"app": "frontend"}, nil),
		faultObject("networking.istio.io/v1", "VirtualService", "half", map[string]any{faultLabel: "false"}, nil),
		faultObject("apps/v1", "Deployment", "checkoutservice", faultLabels, map[string]any{argoTrackingIDKey: "online-boutique:apps/Deployment:boutique/checkoutservice"}),
		deleting,
	}
	k, _, dyn := newKube(nil, objs...)
	cases := map[string]string{
		"virtualservice/frontend":          "not an injected fault",
		"virtualservice/half":              `is "false"`,
		"deployment/checkoutservice":       "managed by Argo CD",
		"podchaos/f03-redis-cart-pod-kill": "already being deleted",
		"virtualservice/missing":           "does not exist",
		"secret/db-password":               "cannot be removed",
		"virtualservice":                   "must look like",
	}
	for target, want := range cases {
		_, err := k.Execute(context.Background(), Action{Type: actionRemoveFault, Namespace: targetNamespace, Target: target})
		if r := asRefusal(t, err); !strings.Contains(r.msg, want) {
			t.Errorf("%s: %q does not contain %q", target, r.msg, want)
		}
	}
	for _, act := range dyn.Actions() {
		if act.GetVerb() == "delete" {
			t.Fatalf("deleted something: %v", act)
		}
	}
}

func TestRemoveFaultAPIErrors(t *testing.T) {
	vs := faultObject("networking.istio.io/v1", "VirtualService", "f01-currency-abort", faultLabels, nil)
	k, _, dyn := newKube(nil, vs)
	dyn.PrependReactor("delete", "virtualservices", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "virtualservices"}, "f01-currency-abort", errors.New("precondition failed"))
	})
	if _, err := k.Execute(context.Background(), Action{Type: actionRemoveFault, Namespace: targetNamespace, Target: "virtualservice/f01-currency-abort"}); err == nil {
		t.Error("delete error must fail")
	}
	dyn.PrependReactor("get", "virtualservices", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("unavailable")
	})
	if err := k.Check(context.Background(), Action{Type: actionRemoveFault, Namespace: targetNamespace, Target: "virtualservice/f01-currency-abort"}); err == nil {
		t.Error("get error must fail")
	}
}

func TestKubeUnknownAndBadTargets(t *testing.T) {
	k, _, _ := newKube(nil)
	ctx := context.Background()
	if err := k.Check(ctx, Action{Type: "exec", Target: "deployment/x"}); err == nil {
		t.Error("unknown type must fail Check")
	}
	if _, err := k.Execute(ctx, Action{Type: "exec", Target: "deployment/x"}); err == nil {
		t.Error("unknown type must fail Execute")
	}
	if err := k.Check(ctx, Action{Type: actionRestart, Target: "nope"}); err == nil {
		t.Error("bad target must fail Check")
	}
	if _, err := k.Execute(ctx, Action{Type: actionRestart, Target: "nope"}); err == nil {
		t.Error("bad target must fail Execute")
	}
	if err := k.Check(ctx, Action{Type: actionScale, Target: "deployment/x", Params: map[string]any{}}); err == nil {
		t.Error("scale without replicas must fail Check")
	}
	if _, err := k.Execute(ctx, Action{Type: actionScale, Target: "deployment/x", Params: map[string]any{}}); err == nil {
		t.Error("scale without replicas must fail Execute")
	}
}
