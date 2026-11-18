// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Gluesys Co., Ltd.

package interception

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

func verbsSystem() System {
	return System{
		Provider:         "ofi+verbs;ofi_rxm",
		AgentEnabled:     true,
		AgentHostNetwork: true,
		AgentSocketDir:   "/var/run/daos-operator/agent/daos",
		ClientImage:      "registry/daos-client:2.8.0",
		ImagePullSecrets: []corev1.LocalObjectReference{{Name: "regcred"}, {Name: "absent"}},
	}
}

// hostPod is the S0 bench pod: host network, privileged, a DAOS PVC.
func hostPod() *corev1.Pod {
	return &corev1.Pod{
		Spec: corev1.PodSpec{
			HostNetwork: true,
			Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "s0"}}}},
			Containers: []corev1.Container{
				{
					Name: "app", Image: "ubuntu:22.04",
					SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)},
					VolumeMounts:    []corev1.VolumeMount{{Name: "data", MountPath: "/mnt/csi"}},
				},
				{Name: "sidecar", Image: "busybox"},
			},
		},
	}
}

func req(pod *corev1.Pod, mode string) Request {
	return Request{Pod: pod, Mode: mode, DaosVolumes: map[string]bool{"data": true}}
}

func TestDecide(t *testing.T) {
	cfg := Config{RDMAResources: []string{"rdma/", "nvidia.com/hostdev"}}
	cases := []struct {
		name        string
		mode        string
		sys         func(*System)
		pod         func(*corev1.Pod)
		daos        map[string]bool
		podSecurity string
		reason      string
	}{
		{name: "host network, privileged", reason: ""},
		{name: "pil4dfs", mode: ModePil4dfs, reason: ""},
		{name: "unknown mode", mode: "fast", reason: "unknown-mode"},
		{name: "no DAOS volume mounted", daos: map[string]bool{"other": true}, reason: "no-daos-volume"},
		{name: "already injected", pod: func(p *corev1.Pod) {
			p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: BundleVolume})
		}, reason: "already-injected"},
		{name: "pod network over verbs", pod: func(p *corev1.Pod) { p.Spec.HostNetwork = false },
			sys: func(s *System) { s.AgentHostNetwork = false }, reason: "pod-network-rdma"},
		{name: "pod network, host-network agent", pod: func(p *corev1.Pod) { p.Spec.HostNetwork = false },
			reason: "agent-network-mismatch"},
		{name: "pod network over tcp", pod: func(p *corev1.Pod) {
			p.Spec.HostNetwork = false
			p.Spec.Containers[0].SecurityContext = nil
		}, sys: func(s *System) { s.Provider = "ofi+tcp"; s.AgentHostNetwork = false }, reason: ""},
		{name: "host network without an RDMA device", pod: func(p *corev1.Pod) {
			p.Spec.Containers[0].SecurityContext = nil
		}, reason: "no-rdma-device"},
		{name: "RDMA device by hostPath", pod: func(p *corev1.Pod) {
			p.Spec.Containers[0].SecurityContext = nil
			p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "ib", VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/dev/infiniband"}}})
			p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "ib", MountPath: "/dev/infiniband"})
		}, reason: ""},
		{name: "RDMA device by resource prefix", pod: func(p *corev1.Pod) {
			p.Spec.Containers[0].SecurityContext = nil
			p.Spec.Containers[0].Resources.Limits = corev1.ResourceList{"rdma/hca_shared_devices_a": resource.MustParse("1")}
		}, reason: ""},
		{name: "RDMA device by exact resource", pod: func(p *corev1.Pod) {
			p.Spec.Containers[0].SecurityContext = nil
			p.Spec.Containers[0].Resources.Requests = corev1.ResourceList{"nvidia.com/hostdev": resource.MustParse("1")}
		}, reason: ""},
		{name: "resource name that only shares a prefix", pod: func(p *corev1.Pod) {
			p.Spec.Containers[0].SecurityContext = nil
			p.Spec.Containers[0].Resources.Requests = corev1.ResourceList{"nvidia.com/hostdevice": resource.MustParse("1")}
		}, reason: "no-rdma-device"},
		{name: "two target images", pod: func(p *corev1.Pod) {
			p.Spec.Containers[1].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/d"}}
		}, reason: "mixed-images"},
		{name: "LD_PRELOAD from a ConfigMap", pod: func(p *corev1.Pod) {
			p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "LD_PRELOAD", ValueFrom: &corev1.EnvVarSource{}}}
		}, reason: "ld-preload-from-source"},
		{name: "baseline namespace, hostPath socket needed", podSecurity: "baseline", reason: "pod-security"},
		{name: "privileged namespace", podSecurity: "privileged", reason: ""},
		{name: "no client image", sys: func(s *System) { s.ClientImage = "" }, reason: "no-client-image"},
		{name: "IBV_DRIVERS from a Secret", pod: func(p *corev1.Pod) {
			p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "IBV_DRIVERS", ValueFrom: &corev1.EnvVarSource{}}}
		}, reason: "env-from-source"},
		{name: "client agent off, no socket of its own", sys: func(s *System) { s.AgentEnabled = false }, reason: "no-client-agent"},
		{name: "client agent off, pod brings its socket", sys: func(s *System) { s.AgentEnabled = false }, pod: func(p *corev1.Pod) {
			p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "DAOS_AGENT_DRPC_DIR", Value: "/run/agent"}}
		}, reason: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod, sys := hostPod(), verbsSystem()
			if tc.pod != nil {
				tc.pod(pod)
			}
			if tc.sys != nil {
				tc.sys(&sys)
			}
			mode := tc.mode
			if mode == "" {
				mode = ModeIoil
			}
			r := req(pod, mode)
			r.PodSecurity = tc.podSecurity
			if tc.daos != nil {
				r.DaosVolumes = tc.daos
			}
			if got := Decide(cfg, sys, r); got != tc.reason {
				t.Fatalf("Decide = %q, want %q", got, tc.reason)
			}
		})
	}
}

func env(c corev1.Container, name string) (string, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

func TestMutate(t *testing.T) {
	pod, sys := hostPod(), verbsSystem()
	pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "LD_PRELOAD", Value: "/usr/lib/libjemalloc.so"}, {Name: "LD_LIBRARY_PATH", Value: "/app/lib"}}
	r := req(pod, ModePil4dfs)
	r.Report = true
	Mutate(Config{}, sys, r, map[string]bool{"regcred": true})

	if got := pod.Annotations[AnnotationResult]; got != "injected/libpil4dfs" {
		t.Fatalf("result annotation %q", got)
	}
	if n := len(pod.Spec.InitContainers); n != 2 {
		t.Fatalf("%d init containers", n)
	}
	b, c := pod.Spec.InitContainers[0], pod.Spec.InitContainers[1]
	if b.Image != sys.ClientImage || !strings.Contains(b.Command[2], "chmod 0777 /il/active") {
		t.Fatalf("bundle init container: %+v", b)
	}
	if b.SecurityContext == nil || ptr.Deref(b.SecurityContext.RunAsUser, 0) != 65534 {
		t.Fatalf("bundle copy must not run as root")
	}
	if c.Image != "ubuntu:22.04" || strings.Join(c.Command, " ") != "/opt/daos-il/bin/il-check /opt/daos-il libpil4dfs" {
		t.Fatalf("check init container: %+v", c)
	}
	if c.SecurityContext == nil || !ptr.Deref(c.SecurityContext.Privileged, false) {
		t.Fatalf("check must run with the application's securityContext")
	}
	if v, _ := env(c, "LD_LIBRARY_PATH"); v != "/app/lib" {
		t.Fatalf("check container must see the application's LD_LIBRARY_PATH, got %q", v)
	}
	for _, e := range c.Env {
		if e.Name == "LD_PRELOAD" {
			t.Fatalf("check container inherits LD_PRELOAD; il-check sets its own")
		}
	}
	app := pod.Spec.Containers[0]
	if v, _ := env(app, "LD_PRELOAD"); v != "/opt/daos-il/active/libpil4dfs.so /usr/lib/libjemalloc.so" {
		t.Fatalf("LD_PRELOAD %q", v)
	}
	if v, _ := env(app, "NA_PLUGIN_PATH"); v != "/opt/daos-il/lib/mercury" {
		t.Fatalf("NA_PLUGIN_PATH %q", v)
	}
	if v, _ := env(app, "IBV_DRIVERS"); v != DefaultIBVDrivers {
		t.Fatalf("IBV_DRIVERS %q", v)
	}
	if v, _ := env(app, "D_IL_REPORT"); v != "1" {
		t.Fatalf("D_IL_REPORT %q", v)
	}
	if v, _ := env(app, "DAOS_AGENT_DRPC_DIR"); v != AgentMountPath {
		t.Fatalf("DAOS_AGENT_DRPC_DIR %q", v)
	}
	var bundleRO, agent bool
	for _, m := range app.VolumeMounts {
		bundleRO = bundleRO || (m.Name == BundleVolume && m.MountPath == MountPath && m.ReadOnly)
		agent = agent || (m.Name == AgentVolume && m.MountPath == AgentMountPath)
	}
	if !bundleRO || !agent {
		t.Fatalf("mounts %+v", app.VolumeMounts)
	}
	if len(pod.Spec.Containers[1].Env) != 0 {
		t.Fatalf("the sidecar mounts no DAOS volume and must be left alone")
	}
	if len(pod.Spec.ImagePullSecrets) != 1 || pod.Spec.ImagePullSecrets[0].Name != "regcred" {
		t.Fatalf("pull secrets %+v (only those in the namespace)", pod.Spec.ImagePullSecrets)
	}
	var agentVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == AgentVolume {
			agentVol = &pod.Spec.Volumes[i]
		}
	}
	if agentVol == nil || agentVol.HostPath.Path != sys.AgentSocketDir {
		t.Fatalf("agent volume %+v", agentVol)
	}
	if Decide(Config{}, sys, r) != "already-injected" {
		t.Fatalf("a second admission must not inject again")
	}
}

func TestMutateKeepsExistingAgentMount(t *testing.T) {
	pod, sys := hostPod(), verbsSystem()
	pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "DAOS_AGENT_DRPC_DIR", Value: "/run/agent"}}
	Mutate(Config{IBVDrivers: "/opt/daos-il/lib/libibverbs/libbnxt_re"}, sys, req(pod, ModeIoil), nil)
	for _, v := range pod.Spec.Volumes {
		if v.Name == AgentVolume {
			t.Fatalf("agent volume added although the container has one")
		}
	}
	if v, _ := env(pod.Spec.Containers[0], "DAOS_AGENT_DRPC_DIR"); v != "/run/agent" {
		t.Fatalf("DAOS_AGENT_DRPC_DIR rewritten to %q", v)
	}
	if v, _ := env(pod.Spec.Containers[0], "IBV_DRIVERS"); v != "/opt/daos-il/lib/libibverbs/libbnxt_re" {
		t.Fatalf("IBV_DRIVERS %q", v)
	}
	if _, ok := env(pod.Spec.Containers[0], "D_IL_REPORT"); ok {
		t.Fatalf("D_IL_REPORT set without the annotation")
	}
	if len(pod.Spec.ImagePullSecrets) != 0 {
		t.Fatalf("pull secrets added that are not in the namespace")
	}
}
