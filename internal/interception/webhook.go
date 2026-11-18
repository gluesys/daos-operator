// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Gluesys Co., Ltd.

package interception

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	daosv1alpha1 "github.com/gluesys/daos-operator/api/v1alpha1"
)

// Path is where the mutating webhook is served.
const Path = "/mutate-daos-interception"

// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=mutatingwebhookconfigurations,verbs=get;update

// Handler is the pod mutating webhook. It never denies: every error path
// admits the pod unchanged (and the webhook configuration uses
// failurePolicy Ignore), because a pod without interception still works
// through dfuse.
type Handler struct {
	// Client may be cached; it is only used for the namespace's default mode.
	Client client.Client
	// Reader reads PVCs, PVs, StorageClasses, Secrets, the DaosSystem and the
	// namespace's Pod Security label directly, so the operator keeps no
	// cluster-wide cache of them and never decides on a stale label.
	Reader     client.Reader
	Decoder    admission.Decoder
	Config     Config
	DriverName string
	SystemName string
}

// Handle implements admission.Handler.
func (h *Handler) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := logf.FromContext(ctx).WithValues("namespace", req.Namespace, "pod", req.Name)
	pod := &corev1.Pod{}
	if err := h.Decoder.Decode(req, pod); err != nil {
		return admission.Allowed("undecodable pod")
	}
	ns := pod.Namespace
	if ns == "" {
		ns = req.Namespace
	}
	var nso corev1.Namespace
	nsErr := h.Client.Get(ctx, types.NamespacedName{Name: ns}, &nso)
	mode, fromPod := pod.Annotations[AnnotationMode]
	if !fromPod && nsErr == nil {
		mode = nso.Labels[AnnotationMode]
	}
	if mode == "" || mode == ModeOff {
		return admission.Allowed("interception not requested")
	}

	daosVols := h.daosVolumes(ctx, ns, pod)
	if len(daosVols) == 0 {
		// a namespace default should not stamp every pod in the namespace
		if !fromPod {
			return admission.Allowed("no DAOS volume")
		}
		Skip(pod, "no-daos-volume")
		return patch(req, pod)
	}
	// The Pod Security level decides whether the agent hostPath may be added,
	// so read it live: a stale or missing cache entry would fall back to the
	// configured default and could add a hostPath the namespace refuses, and
	// failurePolicy Ignore does not help once the webhook itself succeeded.
	var live corev1.Namespace
	if err := h.Reader.Get(ctx, types.NamespacedName{Name: ns}, &live); err != nil {
		log.Info("interception: namespace unavailable", "err", err.Error())
		Skip(pod, "namespace-unavailable")
		return patch(req, pod)
	}
	r := Request{Pod: pod, Mode: mode, Report: pod.Annotations[AnnotationReport] == "true", DaosVolumes: daosVols,
		PodSecurity: live.Labels["pod-security.kubernetes.io/enforce"]}

	var ds daosv1alpha1.DaosSystem
	if err := h.Reader.Get(ctx, types.NamespacedName{Name: h.SystemName}, &ds); err != nil {
		log.Info("interception: DaosSystem unavailable", "system", h.SystemName, "err", err.Error())
		Skip(pod, "no-daos-system")
		return patch(req, pod)
	}
	sys := SystemFrom(&ds)
	if reason := Decide(h.Config, sys, r); reason != "" {
		Skip(pod, reason)
		return patch(req, pod)
	}
	existing := map[string]bool{}
	for _, s := range sys.ImagePullSecrets {
		var sec corev1.Secret
		if err := h.Reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: s.Name}, &sec); err == nil {
			existing[s.Name] = true
		}
	}
	Mutate(h.Config, sys, r, existing)
	log.Info("interception: injected", "library", Library(mode))
	return patch(req, pod)
}

// SystemFrom extracts what the decision needs from a DaosSystem, with the
// operator's defaults (keep in step with the controller's clientagent code).
func SystemFrom(ds *daosv1alpha1.DaosSystem) System {
	provider := ds.Spec.Provider
	if provider == "" {
		provider = "ofi+verbs;ofi_rxm"
	}
	socket := ds.Spec.ClientAgent.HostSocketDir
	if socket == "" {
		socket = filepath.Join("/var/run/daos-operator/agent", ds.Name)
	}
	return System{
		Provider:         provider,
		AgentEnabled:     ds.Spec.ClientAgent.Enabled,
		AgentHostNetwork: ds.Spec.ClientAgent.HostNetwork == nil || *ds.Spec.ClientAgent.HostNetwork,
		AgentSocketDir:   filepath.Clean(socket),
		ClientImage:      ds.Spec.Images.Client,
		ImagePullSecrets: ds.Spec.ImagePullSecrets,
	}
}

// daosVolumes returns the pod volumes backed by the DAOS CSI driver: a bound
// PVC's PV, or an unbound PVC's StorageClass provisioner.
func (h *Handler) daosVolumes(ctx context.Context, ns string, pod *corev1.Pod) map[string]bool {
	out := map[string]bool{}
	for _, v := range pod.Spec.Volumes {
		switch {
		case v.CSI != nil:
			if v.CSI.Driver == h.DriverName {
				out[v.Name] = true
			}
		case v.PersistentVolumeClaim != nil:
			if h.claimIsDaos(ctx, ns, v.PersistentVolumeClaim.ClaimName) {
				out[v.Name] = true
			}
		case v.Ephemeral != nil && v.Ephemeral.VolumeClaimTemplate != nil:
			sc := v.Ephemeral.VolumeClaimTemplate.Spec.StorageClassName
			if sc != nil && h.classIsDaos(ctx, *sc) {
				out[v.Name] = true
			}
		}
	}
	return out
}

func (h *Handler) claimIsDaos(ctx context.Context, ns, name string) bool {
	var pvc corev1.PersistentVolumeClaim
	if err := h.Reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &pvc); err != nil {
		return false
	}
	if pvc.Spec.VolumeName != "" {
		var pv corev1.PersistentVolume
		if err := h.Reader.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, &pv); err == nil {
			return pv.Spec.CSI != nil && pv.Spec.CSI.Driver == h.DriverName
		} else if !apierrors.IsNotFound(err) {
			return false
		}
	}
	return pvc.Spec.StorageClassName != nil && h.classIsDaos(ctx, *pvc.Spec.StorageClassName)
}

func (h *Handler) classIsDaos(ctx context.Context, name string) bool {
	var sc storagev1.StorageClass
	if err := h.Reader.Get(ctx, types.NamespacedName{Name: name}, &sc); err != nil {
		return false
	}
	return sc.Provisioner == h.DriverName
}

func patch(req admission.Request, pod *corev1.Pod) admission.Response {
	raw, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, raw)
}
