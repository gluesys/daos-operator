// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Gluesys Co., Ltd.

// Package interception decides whether a pod that mounts a DAOS CSI volume
// gets the DAOS interception library (libioil or libpil4dfs) preloaded, and
// rewrites the pod spec when it does (doc/plans/2026-11-19-client-interception.md).
//
// The library bypasses dfuse only if the pod is itself a working DAOS client:
// it must reach the engines over the system's provider (for RDMA: the host
// network and an RDMA device), and share the network side of the agent that
// hands it an interface name. Everything the webhook cannot know at admission
// time is left to the pod: the bundle copy and il-check init containers, and
// the library's own fallback to dfuse when daos_init fails.
package interception

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

const (
	// AnnotationMode on a pod (or label on its namespace, as a default) asks
	// for injection: "ioil", "pil4dfs" or "off".
	AnnotationMode = "daos.gluesys.com/interception"
	// AnnotationReport "true" sets D_IL_REPORT=1 in the application containers.
	AnnotationReport = "daos.gluesys.com/interception-report"
	// AnnotationResult records the admission decision: "injected/<lib>"
	// (still subject to il-check, whose verdict is the daos-il-check init
	// container's termination message) or "skipped/<reason>".
	AnnotationResult = "daos.gluesys.com/interception-result"

	ModeIoil    = "ioil"
	ModePil4dfs = "pil4dfs"
	ModeOff     = "off"

	// BundleVolume is the emptyDir holding the copied /opt/daos-il bundle.
	BundleVolume = "daos-il"
	// AgentVolume is the hostPath volume added when the pod has no agent socket mount.
	AgentVolume = "daos-il-agent"
	// MountPath is fixed: the bundle's RUNPATH names it (daos-images il-bundle.sh).
	MountPath = "/opt/daos-il"
	// AgentMountPath is where libdaos looks for the agent socket by default.
	AgentMountPath = "/var/run/daos_agent"

	bundleContainer = "daos-il-bundle"
	checkContainer  = "daos-il-check"
	copyMountPath   = "/il"
	infinibandDir   = "/dev/infiniband"
)

// Config is the operator-wide part of the decision.
type Config struct {
	// IBVDrivers is IBV_DRIVERS for the application: bundled rdma-core
	// providers by path, without the "-rdmav<N>.so" suffix libibverbs adds.
	IBVDrivers string
	// RDMAResources are extended resource names (exact, or prefix when they end
	// in "/") that give a container an RDMA device, e.g. "rdma/" or
	// "nvidia.com/hostdev".
	RDMAResources []string
	// DefaultPodSecurity is the cluster's default enforce level for namespaces
	// without the label, when the administrator knows it ("" = unknown).
	DefaultPodSecurity string
}

// DefaultIBVDrivers covers Mellanox/NVIDIA NICs, the only ones tested.
const DefaultIBVDrivers = MountPath + "/lib/libibverbs/libmlx5"

// System is what the decision needs from the DaosSystem that serves the CSI driver.
type System struct {
	Provider         string
	AgentEnabled     bool
	AgentHostNetwork bool
	AgentSocketDir   string
	ClientImage      string
	ImagePullSecrets []corev1.LocalObjectReference
}

// Request is one admission: the pod, the requested mode and which of its
// volumes are DAOS CSI volumes.
type Request struct {
	Pod         *corev1.Pod
	Mode        string
	Report      bool
	DaosVolumes map[string]bool
	// PodSecurity is the namespace's pod-security.kubernetes.io/enforce level ("" = none).
	PodSecurity string
}

// Library returns the shared object for a mode, or "" for an unknown one.
func Library(mode string) string {
	switch mode {
	case ModeIoil:
		return "libioil"
	case ModePil4dfs:
		return "libpil4dfs"
	}
	return ""
}

// ProviderIsTCP mirrors the operator's own test: anything naming tcp.
func ProviderIsTCP(p string) bool { return strings.Contains(p, "tcp") }

// targets are the application containers that mount a DAOS volume.
func targets(pod *corev1.Pod, daos map[string]bool) []int {
	var idx []int
	for i, c := range pod.Spec.Containers {
		for _, m := range c.VolumeMounts {
			if daos[m.Name] {
				idx = append(idx, i)
				break
			}
		}
	}
	return idx
}

// Decide returns "" when the pod should get the library, or the reason it
// should not ("skipped/<reason>" without the prefix).
func Decide(cfg Config, sys System, r Request) string {
	pod := r.Pod
	if Library(r.Mode) == "" {
		return "unknown-mode"
	}
	for _, v := range pod.Spec.Volumes {
		if v.Name == BundleVolume {
			return "already-injected"
		}
	}
	tg := targets(pod, r.DaosVolumes)
	if len(tg) == 0 {
		return "no-daos-volume"
	}
	if sys.ClientImage == "" {
		return "no-client-image"
	}
	image := pod.Spec.Containers[tg[0]].Image
	for _, i := range tg[1:] {
		if pod.Spec.Containers[i].Image != image {
			// one il-check per image would need one active/ per image; not yet
			return "mixed-images"
		}
	}
	needAgent := false
	for _, i := range tg {
		c := &pod.Spec.Containers[i]
		for _, e := range c.Env {
			if e.ValueFrom == nil {
				continue
			}
			switch e.Name {
			case "LD_PRELOAD":
				return "ld-preload-from-source"
			case "NA_PLUGIN_PATH", "IBV_DRIVERS":
				// the patch would set value next to valueFrom: an invalid pod
				return "env-from-source"
			case "D_IL_REPORT":
				if r.Report {
					return "env-from-source"
				}
			}
		}
		needAgent = needAgent || !hasAgentSocket(c)
	}
	if needAgent && !sys.AgentEnabled {
		// the socket directory only exists where the agent DaemonSet runs
		return "no-client-agent"
	}
	level := r.PodSecurity
	if level == "" {
		level = cfg.DefaultPodSecurity
	}
	if needAgent && hostPathMayBeRefused(level, pod) {
		// the agent socket is a hostPath, which baseline and restricted forbid
		return "pod-security"
	}
	if pod.Spec.HostNetwork != sys.AgentHostNetwork {
		// the agent hands out an interface name from its own namespace
		return "agent-network-mismatch"
	}
	if ProviderIsTCP(sys.Provider) {
		return ""
	}
	if !pod.Spec.HostNetwork {
		return "pod-network-rdma"
	}
	for _, i := range tg {
		if hasRDMADevice(cfg, pod, &pod.Spec.Containers[i]) {
			return ""
		}
	}
	return "no-rdma-device"
}

// hostPathMayBeRefused reports whether adding a hostPath volume could turn an
// admitted pod into one Pod Security rejects. With no enforce label the level
// is the cluster's default, which the webhook cannot see; a pod that already
// uses the host network, a hostPath or privileged mode has passed that default
// as privileged, so one more hostPath changes nothing for it.
func hostPathMayBeRefused(level string, pod *corev1.Pod) bool {
	switch level {
	case "privileged":
		return false
	case "baseline", "restricted":
		return true
	}
	if pod.Spec.HostNetwork {
		return false
	}
	for _, v := range pod.Spec.Volumes {
		if v.HostPath != nil {
			return false
		}
	}
	for _, c := range pod.Spec.Containers {
		if c.SecurityContext != nil && ptr.Deref(c.SecurityContext.Privileged, false) {
			return false
		}
	}
	return true
}

func hasRDMADevice(cfg Config, pod *corev1.Pod, c *corev1.Container) bool {
	if c.SecurityContext != nil && ptr.Deref(c.SecurityContext.Privileged, false) {
		return true
	}
	hostPaths := map[string]bool{}
	for _, v := range pod.Spec.Volumes {
		if v.HostPath != nil && strings.HasPrefix(v.HostPath.Path, infinibandDir) {
			hostPaths[v.Name] = true
		}
	}
	for _, m := range c.VolumeMounts {
		if hostPaths[m.Name] {
			return true
		}
	}
	for _, list := range []corev1.ResourceList{c.Resources.Limits, c.Resources.Requests} {
		for name := range list {
			for _, want := range cfg.RDMAResources {
				n := string(name)
				if n == want || (strings.HasSuffix(want, "/") && strings.HasPrefix(n, want)) {
					return true
				}
			}
		}
	}
	return false
}

// Mutate injects the bundle into the pod. Call it only after Decide returned "".
// existingSecrets filters sys.ImagePullSecrets to those present in the pod's
// namespace (nil means none are).
func Mutate(cfg Config, sys System, r Request, existingSecrets map[string]bool) {
	pod := r.Pod
	lib := Library(r.Mode)
	tg := targets(pod, r.DaosVolumes)
	first := &pod.Spec.Containers[tg[0]]

	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name:         BundleVolume,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})
	small := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("10m"),
			corev1.ResourceMemory: resource.MustParse("16Mi"),
		},
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
	}
	// The client image runs as root by default; run the copy as nobody so a
	// pod with runAsNonRoot still starts. The emptyDir is world-writable.
	pod.Spec.InitContainers = append(pod.Spec.InitContainers,
		corev1.Container{
			Name:         bundleContainer,
			Image:        sys.ClientImage,
			Command:      []string{"sh", "-c", fmt.Sprintf("cp -R %s/. %s/ && chmod 0777 %s/active", MountPath, copyMountPath, copyMountPath)},
			VolumeMounts: []corev1.VolumeMount{{Name: BundleVolume, MountPath: copyMountPath}},
			Resources:    small,
			SecurityContext: &corev1.SecurityContext{
				RunAsUser:                ptr.To[int64](65534),
				RunAsGroup:               ptr.To[int64](65534),
				RunAsNonRoot:             ptr.To(true),
				AllowPrivilegeEscalation: ptr.To(false),
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
		},
		corev1.Container{
			// the application's own image, user and loader decide whether the
			// bundle can be preloaded; il-check is static and needs no shell
			Name:            checkContainer,
			Image:           first.Image,
			ImagePullPolicy: first.ImagePullPolicy,
			Command:         []string{MountPath + "/bin/il-check", MountPath, lib},
			// the application's environment, so LD_LIBRARY_PATH and the like pick
			// the same libraries in the check as in the application
			Env:             checkEnv(first.Env),
			EnvFrom:         first.EnvFrom,
			VolumeMounts:    []corev1.VolumeMount{{Name: BundleVolume, MountPath: MountPath}},
			Resources:       small,
			SecurityContext: first.SecurityContext.DeepCopy(),
		},
	)
	for _, s := range sys.ImagePullSecrets {
		if existingSecrets[s.Name] && !hasPullSecret(pod, s.Name) {
			pod.Spec.ImagePullSecrets = append(pod.Spec.ImagePullSecrets, s)
		}
	}

	needAgent := false
	for _, i := range tg {
		c := &pod.Spec.Containers[i]
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: BundleVolume, MountPath: MountPath, ReadOnly: true})
		preload := MountPath + "/active/" + lib + ".so"
		setEnv(c, "LD_PRELOAD", func(old string) string {
			if old == "" {
				return preload
			}
			return preload + " " + old
		})
		setEnv(c, "NA_PLUGIN_PATH", func(string) string { return MountPath + "/lib/mercury" })
		ibv := cfg.IBVDrivers
		if ibv == "" {
			ibv = DefaultIBVDrivers
		}
		setEnv(c, "IBV_DRIVERS", func(string) string { return ibv })
		if r.Report {
			setEnv(c, "D_IL_REPORT", func(string) string { return "1" })
		}
		if !hasAgentSocket(c) {
			needAgent = true
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: AgentVolume, MountPath: AgentMountPath})
			setEnv(c, "DAOS_AGENT_DRPC_DIR", func(string) string { return AgentMountPath })
		}
	}
	if needAgent {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: AgentVolume,
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
				Path: sys.AgentSocketDir, Type: ptr.To(corev1.HostPathDirectory),
			}},
		})
	}
	setResult(pod, "injected/"+lib)
}

func checkEnv(env []corev1.EnvVar) []corev1.EnvVar {
	var out []corev1.EnvVar
	for _, e := range env {
		if e.Name != "LD_PRELOAD" {
			out = append(out, e)
		}
	}
	return out
}

// Skip records why a pod that asked for injection did not get it.
func Skip(pod *corev1.Pod, reason string) { setResult(pod, "skipped/"+reason) }

func setResult(pod *corev1.Pod, v string) {
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[AnnotationResult] = v
}

func hasPullSecret(pod *corev1.Pod, name string) bool {
	for _, s := range pod.Spec.ImagePullSecrets {
		if s.Name == name {
			return true
		}
	}
	return false
}

func hasAgentSocket(c *corev1.Container) bool {
	for _, e := range c.Env {
		if e.Name == "DAOS_AGENT_DRPC_DIR" {
			return true
		}
	}
	for _, m := range c.VolumeMounts {
		if m.MountPath == AgentMountPath {
			return true
		}
	}
	return false
}

func setEnv(c *corev1.Container, name string, f func(old string) string) {
	for i := range c.Env {
		if c.Env[i].Name == name {
			c.Env[i].Value = f(c.Env[i].Value)
			return
		}
	}
	c.Env = append(c.Env, corev1.EnvVar{Name: name, Value: f("")})
}
