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
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	daosv1alpha1 "gitlab.gluesys.com/exastor/daos-operator/api/v1alpha1"
)

// Host-preparation DaemonSet (#8): one privileged pod per selected node runs
// cmd/hostprep, which publishes Node annotations the reconcile consumes.
// The ClusterRole it binds to is shipped with the operator (config/rbac/hostprep_role.yaml);
// the operator only creates the ServiceAccount and the binding.

// DefaultHostPrepImage is used when spec.images.hostPrep is empty. The Helm
// chart overrides it through the DAOS_HOSTPREP_DEFAULT_IMAGE environment variable.
var DefaultHostPrepImage = "registry.gitlab.gluesys.com/exastor/daos-operator/daos-hostprep:latest"

const (
	hostPrepClusterRole = "daos-hostprep"
	hostPrepSAName      = "daos-hostprep"
)

// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=patch;update

func hostPrepEnabled(sys *daosv1alpha1.DaosSystem) bool {
	return sys.Spec.HostPrep.Enabled == nil || *sys.Spec.HostPrep.Enabled
}

func (r *DaosSystemReconciler) ensureHostPrep(ctx context.Context, sys *daosv1alpha1.DaosSystem, ns string) error {
	name := sys.Name + "-hostprep"
	if !hostPrepEnabled(sys) {
		ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		return client.IgnoreNotFound(r.Delete(ctx, ds))
	}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: hostPrepSAName, Namespace: ns}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		labelManaged(&sa.ObjectMeta, sys, "")
		return nil
	}); err != nil {
		return fmt.Errorf("serviceaccount: %w", err)
	}
	crb := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: hostPrepClusterRole + "-" + ns}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, crb, func() error {
		labelManaged(&crb.ObjectMeta, sys, "")
		crb.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: hostPrepClusterRole}
		crb.Subjects = []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: hostPrepSAName, Namespace: ns}}
		return nil
	}); err != nil {
		return fmt.Errorf("clusterrolebinding: %w", err)
	}

	image := sys.Spec.Images.HostPrep
	if image == "" {
		image = DefaultHostPrepImage
	}
	interval := sys.Spec.HostPrep.IntervalSeconds
	if interval <= 0 {
		interval = 300
	}
	labels := map[string]string{"app.kubernetes.io/name": "daos-hostprep", daosv1alpha1.LabelSystem: sys.Name}
	hostPathDir := corev1.HostPathDirectory
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ds, func() error {
		labelManaged(&ds.ObjectMeta, sys, "")
		ds.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		ds.Spec.Template.ObjectMeta.Labels = labels
		pod := &ds.Spec.Template.Spec
		pod.ServiceAccountName = hostPrepSAName
		pod.NodeSelector = sys.Spec.NodeSelector
		pod.Tolerations = sys.Spec.Tolerations
		pod.HostNetwork = true
		pod.HostPID = true
		pod.PriorityClassName = "system-node-critical"
		pod.Volumes = []corev1.Volume{
			{Name: "sys", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/sys", Type: &hostPathDir}}},
			{Name: "dev", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev", Type: &hostPathDir}}},
			{Name: "procsys", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/proc/sys", Type: &hostPathDir}}},
		}
		pod.Containers = []corev1.Container{{
			Name:            "hostprep",
			Image:           image,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         []string{"/usr/local/bin/hostprep"},
			Env: []corev1.EnvVar{
				{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}},
				{Name: "DAOS_HOSTPREP_BIND_NVME", Value: strconv.FormatBool(sys.Spec.HostPrep.BindNvme)},
				{Name: "DAOS_HOSTPREP_FABRIC_CIDR", Value: sys.Spec.HostPrep.FabricCIDR},
				{Name: "DAOS_HOSTPREP_HUGEPAGES", Value: strconv.Itoa(int(hugepagesOf(sys)))},
				{Name: "DAOS_HOSTPREP_INTERVAL", Value: fmt.Sprintf("%ds", interval)},
				// /sys and /dev are the host's (hostPath at the same paths); /proc/sys/vm is reached
				// through the procsys mount, so hugepages writes hit the host kernel.
				{Name: "DAOS_HOSTPREP_SYS_ROOT", Value: "/host"},
			},
			SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "sys", MountPath: "/host/sys"},
				{Name: "dev", MountPath: "/host/dev"},
				{Name: "procsys", MountPath: "/host/proc/sys"},
				// daos_server nvme prepare needs the real /sys and /dev too
				{Name: "sys", MountPath: "/sys"},
				{Name: "dev", MountPath: "/dev"},
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
			},
		}}
		return controllerutil.SetControllerReference(sys, ds, r.Scheme)
	})
	return err
}

func labelManaged(m *metav1.ObjectMeta, sys *daosv1alpha1.DaosSystem, node string) {
	if m.Labels == nil {
		m.Labels = map[string]string{}
	}
	m.Labels["app.kubernetes.io/managed-by"] = "daos-operator"
	m.Labels[daosv1alpha1.LabelSystem] = sys.Name
	if node != "" {
		m.Labels[daosv1alpha1.LabelNode] = node
	}
}
