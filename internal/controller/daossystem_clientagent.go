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
	"path/filepath"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	daosv1alpha1 "gitlab.gluesys.com/exastor/daos-operator/api/v1alpha1"
)

// clientAgentName is the DaemonSet (and its pods' label) for spec.clientAgent.
func clientAgentName(sys *daosv1alpha1.DaosSystem) string { return sys.Name + "-agent" }

// clientAgentHostNetwork is spec.clientAgent.hostNetwork with its default of true.
func clientAgentHostNetwork(sys *daosv1alpha1.DaosSystem) bool {
	return sys.Spec.ClientAgent.HostNetwork == nil || *sys.Spec.ClientAgent.HostNetwork
}

// clientAgentSocketDir is where the node-level agent publishes its socket on the host.
func clientAgentSocketDir(sys *daosv1alpha1.DaosSystem) string {
	if d := sys.Spec.ClientAgent.HostSocketDir; d != "" {
		return filepath.Clean(d)
	}
	return filepath.Join("/var/run/daos_agent", sys.Name)
}

// ensureClientAgent keeps the per-node daos_agent DaemonSet in step with
// spec.clientAgent: one agent per selected node, socket on the host at
// clientAgentSocketDir. It reads the same <sys>-agent ConfigMap (and the agent
// certificate set when the system uses TLS) as every other agent the operator
// runs. Returns the DaemonSet's ready/desired counts for the status condition.
func (r *DaosSystemReconciler) ensureClientAgent(ctx context.Context, sys *daosv1alpha1.DaosSystem, ns string) (ready, desired int32, err error) {
	name := clientAgentName(sys)
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if !sys.Spec.ClientAgent.Enabled {
		return 0, 0, client.IgnoreNotFound(r.Delete(ctx, ds))
	}
	labels := map[string]string{"app.kubernetes.io/name": "daos-agent", daosv1alpha1.LabelSystem: sys.Name, daosv1alpha1.LabelRole: "client-agent"}
	hostDir, hostDirCreate := corev1.HostPathDirectory, corev1.HostPathDirectoryOrCreate
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, ds, func() error {
		labelManaged(&ds.ObjectMeta, sys, "")
		ds.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		ds.Spec.Template.ObjectMeta.Labels = labels
		pod := &ds.Spec.Template.Spec
		pod.NodeSelector = sys.Spec.ClientAgent.NodeSelector
		pod.Tolerations = sys.Spec.ClientAgent.Tolerations
		// the agent enumerates the fabric of the namespace it runs in, and its
		// clients must see the same interfaces: host namespace for hostNetwork
		// clients, pod namespace for ordinary pods (see ClientAgentSpec)
		pod.HostNetwork = clientAgentHostNetwork(sys)
		if pod.HostNetwork {
			pod.DNSPolicy = corev1.DNSClusterFirstWithHostNet
		} else {
			pod.DNSPolicy = corev1.DNSClusterFirst
		}
		pod.PriorityClassName = "system-node-critical"
		pod.Volumes = []corev1.Volume{
			{Name: "agentcfg", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: sys.Name + "-agent"}}}},
			{Name: "agentsock", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: clientAgentSocketDir(sys), Type: &hostDirCreate}}},
			{Name: "dev", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev", Type: &hostDir}}},
		}
		c := corev1.Container{
			Name:            "agent",
			Image:           sys.Spec.Images.Agent,
			ImagePullPolicy: corev1.PullIfNotPresent,
			// RDMA devices for the fabric scan, same reason as the sidecars
			SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "agentcfg", MountPath: "/etc/daos/daos_agent.yml", SubPath: "daos_agent.yml", ReadOnly: true},
				{Name: "agentsock", MountPath: agentSocketDir},
				{Name: "dev", MountPath: "/dev"},
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
			},
		}
		if s := adminCertsSecret(sys); s != "" {
			pod.Volumes = append(pod.Volumes, corev1.Volume{Name: "certs", VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: s, DefaultMode: ptr.To(int32(0o400))}}})
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "certs", MountPath: "/etc/daos/certs", ReadOnly: true})
		}
		// The socket lives on a hostPath, so a socket file from a previous agent
		// survives the pod and daos_agent then refuses to start with "Configured
		// dRPC socket file is already in use" -- permanently, after any node or
		// runtime restart (cxl2, 2026-09-27). Remove a stale one first; a live
		// agent on the same directory would already be a misconfiguration.
		pod.InitContainers = []corev1.Container{{
			Name:            "clean-socket",
			Image:           sys.Spec.Images.Agent,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         []string{"sh", "-c", "rm -f " + agentSocketDir + "/daos_agent.sock"},
			VolumeMounts:    []corev1.VolumeMount{{Name: "agentsock", MountPath: agentSocketDir}},
		}}
		pod.Containers = []corev1.Container{c}
		return controllerutil.SetControllerReference(sys, ds, r.Scheme)
	})
	if err != nil {
		return 0, 0, fmt.Errorf("client agent daemonset: %w", err)
	}
	return ds.Status.NumberReady, ds.Status.DesiredNumberScheduled, nil
}

// setClientAgentCondition writes ConditionClientAgent from the DaemonSet state.
func (r *DaosSystemReconciler) setClientAgentCondition(ctx context.Context, sys *daosv1alpha1.DaosSystem, ns string, status *daosv1alpha1.DaosSystemStatus) error {
	ready, desired, err := r.ensureClientAgent(ctx, sys, ns)
	if err != nil {
		return err
	}
	switch {
	case !sys.Spec.ClientAgent.Enabled:
		setCond(status, daosv1alpha1.ConditionClientAgent, metav1.ConditionFalse, "Disabled", "spec.clientAgent.enabled is false")
	case desired == 0:
		setCond(status, daosv1alpha1.ConditionClientAgent, metav1.ConditionFalse, "NoNodes", "no node matches spec.clientAgent.nodeSelector")
	case ready < desired:
		setCond(status, daosv1alpha1.ConditionClientAgent, metav1.ConditionFalse, "Starting", fmt.Sprintf("%d/%d node agent(s) ready", ready, desired))
	default:
		setCond(status, daosv1alpha1.ConditionClientAgent, metav1.ConditionTrue, "Ready",
			fmt.Sprintf("%d node agent(s), %s; clients mount hostPath %s as %s", ready,
				map[bool]string{true: "host network", false: "pod network"}[clientAgentHostNetwork(sys)], clientAgentSocketDir(sys), agentSocketDir))
	}
	return nil
}
