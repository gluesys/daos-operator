/*
SPDX-License-Identifier: Apache-2.0
Copyright 2026 Gluesys Co., Ltd.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"path"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	daosv1alpha1 "gitlab.gluesys.com/exastor/daos-operator/api/v1alpha1"
	"gitlab.gluesys.com/exastor/daos-operator/internal/render"
)

// Server workloads (#9): one StatefulSet per rendered node.
//
// Why a StatefulSet with replicas=1 per node instead of one StatefulSet or a
// DaemonSet for the whole system:
//   - a rank belongs to the node that holds its NVMe and control_metadata; the
//     pod must never move, so the workload is pinned with a required nodeAffinity
//     and the data lives on that node (hostPath, ADR-002);
//   - the per-node daos_server.yml differs (fabric_iface, bdev_list), so each
//     workload mounts its own ConfigMap;
//   - StatefulSet gives at-most-one semantics for the pod (no two engines on the
//     same superblock during a rollout) and updateStrategy OnDelete keeps image
//     or config changes from restarting engines behind the operator's back --
//     the upgrade procedure (ADR-003) does that step by step.
//
// The operator only creates and updates these workloads. It removes one only
// when spec.server.enabled is set to false by a human or when the DaosSystem is
// deleted (owner GC). Nodes that later lose facts or leave the selector keep
// their workload until the rank-exclusion procedure (#10+) exists: stopping an
// engine automatically is exactly the kind of surprise these ADRs forbid.

const (
	serverContainer   = "daos-server"
	serverGracePeriod = int64(120)
	// serverMemoryOverheadGiB is added to the tmpfs total for the engine's own RSS
	// (SPDK, xstreams, RPC buffers). Users override via spec.server.resources.
	serverMemoryOverheadGiB = 2
)

// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

func serverEnabled(sys *daosv1alpha1.DaosSystem) bool {
	return sys.Spec.Server.Enabled == nil || *sys.Spec.Server.Enabled
}

func serverWorkloadName(sys *daosv1alpha1.DaosSystem, node string) string {
	return fmt.Sprintf("%s-server-%s", sys.Name, node)
}

// serverHostPaths returns the node directories for data and logs.
func serverHostPaths(sys *daosv1alpha1.DaosSystem) (data, logs string) {
	data, logs = sys.Spec.Server.DataHostPath, sys.Spec.Server.LogHostPath
	if data == "" {
		data = path.Join("/var/daos", sys.Name)
	}
	if logs == "" {
		logs = path.Join("/var/log/daos", sys.Name)
	}
	return data, logs
}

// serverResources computes what the scheduler must know about an engine pod:
// tmpfs (class: ram) pages are charged to the pod, hugepages back SPDK.
func serverResources(sys *daosv1alpha1.DaosSystem, engines []render.Engine) corev1.ResourceRequirements {
	if sys.Spec.Server.Resources != nil {
		return *sys.Spec.Server.Resources
	}
	var scm, cpu int64
	for _, e := range engines {
		scm += int64(e.ScmSizeGiB)
		cpu += int64(e.Targets) + int64(e.Helpers) + 1
	}
	mem := resource.MustParse(fmt.Sprintf("%dGi", scm+serverMemoryOverheadGiB))
	req := corev1.ResourceList{corev1.ResourceCPU: *resource.NewQuantity(cpu, resource.DecimalSI), corev1.ResourceMemory: mem}
	lim := corev1.ResourceList{}
	if sys.Spec.NrHugepages > 0 {
		hp := resource.MustParse(fmt.Sprintf("%dMi", int64(sys.Spec.NrHugepages)*2))
		req["hugepages-2Mi"] = hp
		lim["hugepages-2Mi"] = hp // hugepages request must equal limit
	}
	return corev1.ResourceRequirements{Requests: req, Limits: lim}
}

// ensureServer creates or updates the pinned StatefulSet for one node.
func (r *DaosSystemReconciler) ensureServer(ctx context.Context, sys *daosv1alpha1.DaosSystem, ns, node, configMap string, engines []render.Engine) error {
	name := serverWorkloadName(sys, node)
	if !serverEnabled(sys) {
		sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		return client.IgnoreNotFound(r.Delete(ctx, sts))
	}
	dataPath, logPath := serverHostPaths(sys)
	grace := ptr.To(serverGracePeriod)
	if sys.Spec.Server.TerminationGracePeriodSeconds != nil {
		grace = sys.Spec.Server.TerminationGracePeriodSeconds
	}
	labels := map[string]string{"app.kubernetes.io/name": "daos-server", "app.kubernetes.io/managed-by": "daos-operator",
		daosv1alpha1.LabelSystem: sys.Name, daosv1alpha1.LabelNode: node, daosv1alpha1.LabelRole: "server"}
	dirCreate := corev1.HostPathDirectoryOrCreate
	dir := corev1.HostPathDirectory
	hostVol := func(name, p string, t *corev1.HostPathType) corev1.Volume {
		return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: p, Type: t}}}
	}
	var ports []corev1.ContainerPort
	ports = append(ports, corev1.ContainerPort{Name: "control", ContainerPort: controlPort, Protocol: corev1.ProtocolTCP})
	for _, e := range engines {
		ports = append(ports, corev1.ContainerPort{Name: fmt.Sprintf("fabric%d", e.Index), ContainerPort: e.FabricPort, Protocol: corev1.ProtocolTCP})
	}

	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		labelManaged(&sts.ObjectMeta, sys, node)
		sts.Spec.Replicas = ptr.To(int32(1))
		sts.Spec.ServiceName = sys.Name + "-server"
		sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{daosv1alpha1.LabelSystem: sys.Name, daosv1alpha1.LabelNode: node, daosv1alpha1.LabelRole: "server"}}
		// never restart an engine because a manifest changed; ADR-003 sequences that
		sts.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType}
		sts.Spec.PodManagementPolicy = appsv1.OrderedReadyPodManagement
		sts.Spec.Template.ObjectMeta.Labels = labels
		pod := &sts.Spec.Template.Spec
		pod.HostNetwork = true
		pod.DNSPolicy = corev1.DNSClusterFirstWithHostNet
		pod.TerminationGracePeriodSeconds = grace
		pod.Tolerations = sys.Spec.Tolerations
		pod.PriorityClassName = "system-node-critical"
		pod.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{node}}}}}}}}
		pod.Volumes = []corev1.Volume{
			{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: configMap}}}},
			hostVol("data", dataPath, &dirCreate),
			hostVol("logs", logPath, &dirCreate),
			// SPDK: hugetlbfs, VFIO/uio device nodes and the PCI/NUMA topology. /dev and /sys are
			// mounted whole because uio_pci_generic (testbeds) needs /dev/uio* and daos_server
			// binds devices through /sys/bus/pci at start.
			hostVol("hugepages", "/dev/hugepages", &dirCreate),
			hostVol("dev", "/dev", &dir),
			hostVol("sys", "/sys", &dir),
		}
		pod.Containers = []corev1.Container{{
			Name:            serverContainer,
			Image:           sys.Spec.Images.Server,
			ImagePullPolicy: corev1.PullIfNotPresent,
			// entrypoint sees an uncommented engines: block and skips env rendering
			Ports:           ports,
			SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)},
			Resources:       serverResources(sys, engines),
			VolumeMounts: []corev1.VolumeMount{
				{Name: "config", MountPath: "/etc/daos/daos_server.yml", SubPath: keyServerYML, ReadOnly: true},
				{Name: "data", MountPath: "/var/daos"},
				{Name: "logs", MountPath: "/var/log/daos"},
				{Name: "hugepages", MountPath: "/dev/hugepages"},
				{Name: "dev", MountPath: "/dev"},
				{Name: "sys", MountPath: "/sys"},
			},
			// the control plane listens as soon as daos_server is up, before format;
			// no liveness probe: a slow engine must never be killed by a probe
			ReadinessProbe: &corev1.Probe{
				ProbeHandler:        corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(controlPort)}},
				InitialDelaySeconds: 10, PeriodSeconds: 10, FailureThreshold: 3,
			},
		}}
		return controllerutil.SetControllerReference(sys, sts, r.Scheme)
	})
	return err
}

// serverReady reports whether the node's server pod is Ready.
func (r *DaosSystemReconciler) serverReady(ctx context.Context, sys *daosv1alpha1.DaosSystem, ns, node string) (bool, string) {
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: serverWorkloadName(sys, node)}, sts); err != nil {
		return false, "server workload not found"
	}
	if sts.Status.ReadyReplicas >= 1 {
		return true, ""
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(ns), client.MatchingLabels(sts.Spec.Selector.MatchLabels)); err != nil || len(pods.Items) == 0 {
		return false, "server pod not scheduled"
	}
	p := pods.Items[0]
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Waiting != nil {
			return false, fmt.Sprintf("server pod %s: %s", p.Name, cs.State.Waiting.Reason)
		}
		if cs.State.Terminated != nil {
			return false, fmt.Sprintf("server pod %s: exited %d (%s)", p.Name, cs.State.Terminated.ExitCode, cs.State.Terminated.Reason)
		}
	}
	return false, fmt.Sprintf("server pod %s: %s, not ready", p.Name, p.Status.Phase)
}
