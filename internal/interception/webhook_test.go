// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Gluesys Co., Ltd.

package interception

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	daosv1alpha1 "github.com/gluesys/daos-operator/api/v1alpha1"
)

const driver = "daos.csi.gluesys.com"

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := daosv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func handler(t *testing.T, objs ...runtime.Object) *Handler {
	t.Helper()
	s := scheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(objs...).Build()
	return &Handler{Client: c, Reader: c, Decoder: admission.NewDecoder(s), DriverName: driver, SystemName: "daos"}
}

func system() *daosv1alpha1.DaosSystem {
	ds := &daosv1alpha1.DaosSystem{ObjectMeta: metav1.ObjectMeta{Name: "daos"}}
	ds.Spec.Provider = "ofi+verbs;ofi_rxm"
	ds.Spec.Images.Client = "registry/daos-client:2.8.0"
	ds.Spec.ClientAgent.Enabled = true
	return ds
}

func objects() []runtime.Object {
	return []runtime.Object{
		system(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "app"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ml", Labels: map[string]string{AnnotationMode: ModeIoil}}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "locked", Labels: map[string]string{"pod-security.kubernetes.io/enforce": "baseline"}}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "locked", Name: "bound"},
			Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pv-bound"}},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "daos"}, Provisioner: driver},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "local"}, Provisioner: "rancher.io/local-path"},
		&corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv-bound"}, Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: driver}}}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "bound"},
			Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pv-bound"}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "pending"},
			Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: ptr.To("daos")}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "other"},
			Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: ptr.To("local")}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "ml", Name: "bound"},
			Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pv-bound"}},
	}
}

func podWith(ns, claim string, ann map[string]string) *corev1.Pod {
	p := hostPod()
	p.Namespace, p.Name, p.Annotations = ns, "p", ann
	p.Spec.Volumes[0].PersistentVolumeClaim.ClaimName = claim
	return p
}

// admit runs the handler and returns the pod as patched.
func admit(t *testing.T, h *Handler, pod *corev1.Pod) *corev1.Pod {
	t.Helper()
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	resp := h.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Namespace: pod.Namespace, Operation: admissionv1.Create,
		Object: runtime.RawExtension{Raw: raw},
	}})
	if !resp.Allowed {
		t.Fatalf("denied: %+v", resp.Result)
	}
	if len(resp.Patches) == 0 {
		return pod
	}
	ops, _ := json.Marshal(resp.Patches)
	p, err := jsonpatch.DecodePatch(ops)
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.Apply(raw)
	if err != nil {
		t.Fatal(err)
	}
	var got corev1.Pod
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	return &got
}

func TestHandle(t *testing.T) {
	h := handler(t, objects()...)
	cases := []struct {
		name   string
		pod    *corev1.Pod
		result string
	}{
		{"not requested", podWith("app", "bound", nil), ""},
		{"off", podWith("app", "bound", map[string]string{AnnotationMode: ModeOff}), ""},
		{"bound PVC", podWith("app", "bound", map[string]string{AnnotationMode: ModeIoil}), "injected/libioil"},
		{"unbound PVC, DAOS StorageClass", podWith("app", "pending", map[string]string{AnnotationMode: ModePil4dfs}), "injected/libpil4dfs"},
		{"other driver", podWith("app", "other", map[string]string{AnnotationMode: ModeIoil}), "skipped/no-daos-volume"},
		{"missing PVC", podWith("app", "nope", map[string]string{AnnotationMode: ModeIoil}), "skipped/no-daos-volume"},
		{"pil4dfs-compat", podWith("app", "bound", map[string]string{AnnotationMode: ModePil4dfsCompat}), "injected/libpil4dfs"},
		{"namespace default", podWith("ml", "bound", nil), "injected/libioil"},
		{"pod annotation overrides namespace", podWith("ml", "bound", map[string]string{AnnotationMode: ModeOff}), ""},
		{"baseline Pod Security", podWith("locked", "bound", map[string]string{AnnotationMode: ModeIoil}), "skipped/pod-security"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := admit(t, h, tc.pod)
			if r := got.Annotations[AnnotationResult]; r != tc.result {
				t.Fatalf("result %q, want %q", r, tc.result)
			}
		})
	}
}

func TestHandleNamespaceDefaultWithoutDaosVolume(t *testing.T) {
	// a namespace label must not stamp pods that have nothing to do with DAOS
	got := admit(t, handler(t, objects()...), podWith("ml", "absent", nil))
	if _, ok := got.Annotations[AnnotationResult]; ok {
		t.Fatalf("annotated %v", got.Annotations)
	}
}

func TestHandleNoSystem(t *testing.T) {
	objs := objects()[1:] // without the DaosSystem
	got := admit(t, handler(t, objs...), podWith("app", "bound", map[string]string{AnnotationMode: ModeIoil}))
	if r := got.Annotations[AnnotationResult]; r != "skipped/no-daos-system" {
		t.Fatalf("result %q", r)
	}
}

func TestSystemFromDefaults(t *testing.T) {
	ds := system()
	ds.Spec.Provider = ""
	s := SystemFrom(ds)
	if s.Provider != "ofi+verbs;ofi_rxm" || !s.AgentHostNetwork || s.AgentSocketDir != "/var/run/daos-operator/agent/daos" {
		t.Fatalf("%+v", s)
	}
	ds.Spec.ClientAgent.HostNetwork = ptr.To(false)
	ds.Spec.ClientAgent.HostSocketDir = "/run/x/"
	ds.Spec.ClientAgent.NodeSelector = map[string]string{"daos/client": "true"}
	if s := SystemFrom(ds); s.AgentHostNetwork || s.AgentSocketDir != "/run/x" || s.AgentNodeSelector["daos/client"] != "true" {
		t.Fatalf("%+v", s)
	}
}

func TestHandlePodSecurityReadLive(t *testing.T) {
	// the cache has not seen the baseline label yet; the live read has
	h := handler(t, objects()...)
	cached := fake.NewClientBuilder().WithScheme(scheme(t)).
		WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "locked"}}).Build()
	h.Client = cached
	h.Config.DefaultPodSecurity = "privileged"
	got := admit(t, h, podWith("locked", "bound", map[string]string{AnnotationMode: ModeIoil}))
	if r := got.Annotations[AnnotationResult]; r != "skipped/pod-security" {
		t.Fatalf("result %q", r)
	}
}

func TestHandleNamespaceUnavailable(t *testing.T) {
	h := handler(t, objects()...)
	h.Config.DefaultPodSecurity = "privileged"
	h.Reader = fake.NewClientBuilder().WithScheme(scheme(t)).WithRuntimeObjects(objects()...).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Namespace); ok {
				return errors.New("apiserver unavailable")
			}
			return c.Get(ctx, key, obj, opts...)
		}}).Build()
	got := admit(t, h, podWith("app", "bound", map[string]string{AnnotationMode: ModeIoil}))
	if r := got.Annotations[AnnotationResult]; r != "skipped/namespace-unavailable" {
		t.Fatalf("result %q", r)
	}
	for _, v := range got.Spec.Volumes {
		if v.HostPath != nil {
			t.Fatalf("hostPath %s added without knowing the namespace's Pod Security level", v.Name)
		}
	}
}
