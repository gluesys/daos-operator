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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	daosv1alpha1 "gitlab.gluesys.com/exastor/daos-operator/api/v1alpha1"
)

func mkNode(ctx context.Context, name, ip string, ann map[string]string) {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name,
		Labels: map[string]string{daosv1alpha1.LabelRole: "storage"}, Annotations: ann}}
	Expect(k8sClient.Create(ctx, n)).To(Succeed())
	n.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}}
	Expect(k8sClient.Status().Update(ctx, n)).To(Succeed())
}

var _ = Describe("DaosSystem Controller", func() {
	ctx := context.Background()
	const sysName = "t1"
	nn := types.NamespacedName{Name: sysName}

	BeforeEach(func() {
		mkNode(ctx, "n1", "10.0.0.1", map[string]string{
			daosv1alpha1.AnnotationFabricIface: "ens2",
			daosv1alpha1.AnnotationBdevList:    "0000:03:00.0,0000:04:00.0",
			daosv1alpha1.AnnotationBdevDSN:     "0000:03:00.0=DSN1,0000:04:00.0=DSN2"})
		mkNode(ctx, "n2", "10.0.0.2", map[string]string{
			daosv1alpha1.AnnotationFabricIface: "ens2np0",
			daosv1alpha1.AnnotationBdevList:    "0000:03:00.0",
			daosv1alpha1.AnnotationBdevDSN:     "0000:03:00.0=DSN3"})
		mkNode(ctx, "n3", "10.0.0.3", nil) // no facts yet: must be reported, not rendered
		sys := &daosv1alpha1.DaosSystem{ObjectMeta: metav1.ObjectMeta{Name: sysName},
			Spec: daosv1alpha1.DaosSystemSpec{Version: "2.8.0", Namespace: "daos-test",
				Images:       daosv1alpha1.ImagesSpec{Server: "s", Agent: "a", Admin: "d"},
				NodeSelector: map[string]string{daosv1alpha1.LabelRole: "storage"},
				MsReplicas:   1, Provider: "ofi+verbs;ofi_rxm", NrHugepages: 8192, AllowInsecure: true,
				Engines: []daosv1alpha1.EngineSpec{{Targets: 8, Helpers: 2, ScmSizeGiB: 32}}}}
		Expect(k8sClient.Create(ctx, sys)).To(Succeed())
	})
	AfterEach(func() {
		sys := &daosv1alpha1.DaosSystem{}
		if err := k8sClient.Get(ctx, nn, sys); err == nil {
			Expect(k8sClient.Delete(ctx, sys)).To(Succeed())
		}
		for _, n := range []string{"n1", "n2", "n3"} {
			_ = k8sClient.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n}})
		}
		// envtest has no GC: remove ConfigMaps ourselves
		_ = k8sClient.Delete(ctx, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "daos-test", Name: "t1-hostprep"}})
		stss := &appsv1.StatefulSetList{}
		_ = k8sClient.List(ctx, stss)
		for i := range stss.Items {
			if stss.Items[i].Namespace == "daos-test" {
				_ = k8sClient.Delete(ctx, &stss.Items[i])
			}
		}
		cms := &corev1.ConfigMapList{}
		_ = k8sClient.List(ctx, cms)
		for i := range cms.Items {
			if cms.Items[i].Namespace == "daos-test" {
				_ = k8sClient.Delete(ctx, &cms.Items[i])
			}
		}
	})

	reconcileOnce := func() {
		r := &DaosSystemReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
	}

	It("renders one server ConfigMap per node with facts and reports the node without facts", func() {
		reconcileOnce()
		sys := &daosv1alpha1.DaosSystem{}
		Expect(k8sClient.Get(ctx, nn, sys)).To(Succeed())
		Expect(sys.Status.SelectedNodes).To(Equal([]string{"n1", "n2", "n3"}))
		Expect(sys.Status.MsReplicaNodes).To(Equal([]string{"n1"}))
		Expect(meta.IsStatusConditionTrue(sys.Status.Conditions, daosv1alpha1.ConditionNodesSelected)).To(BeTrue())
		Expect(meta.IsStatusConditionFalse(sys.Status.Conditions, daosv1alpha1.ConditionDriveConflict)).To(BeTrue())
		Expect(meta.IsStatusConditionFalse(sys.Status.Conditions, daosv1alpha1.ConditionConfigRendered)).To(BeTrue(), "n3 has no facts -> Partial")

		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "daos-test", Name: "t1-server-n2"}, cm)).To(Succeed())
		yml := cm.Data["daos_server.yml"]
		Expect(yml).To(ContainSubstring("fabric_iface: ens2np0"))
		Expect(yml).To(ContainSubstring(`- "0000:03:00.0"`))
		Expect(yml).To(ContainSubstring("mgmt_svc_replicas:\n  - 10.0.0.1"))
		Expect(yml).NotTo(ContainSubstring("access_points"))
		Expect(cm.OwnerReferences).To(HaveLen(1))

		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "daos-test", Name: "t1-agent"}, cm)).To(Succeed())
		Expect(cm.Data["daos_agent.yml"]).To(ContainSubstring("access_points:\n  - 10.0.0.1"))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "daos-test", Name: "t1-control"}, cm)).To(Succeed())
		Expect(strings.Count(cm.Data["daos_control.yml"], "\n  - 10.0.0.")).To(Equal(2), "hostlist = rendered nodes only")

		var n3 daosv1alpha1.NodeConfigStatus
		for _, nc := range sys.Status.NodeConfigs {
			if nc.Node == "n3" {
				n3 = nc
			}
		}
		Expect(n3.Ready).To(BeFalse())
		Expect(n3.Message).To(ContainSubstring(daosv1alpha1.AnnotationFabricIface))
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "daos-test", Name: "t1-server-n3"}, &corev1.ConfigMap{})
		Expect(err).To(HaveOccurred(), "no ConfigMap for a node without facts")

		By("deploying the host-preparation DaemonSet with the system's node selector")
		ds := &appsv1.DaemonSet{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "daos-test", Name: "t1-hostprep"}, ds)).To(Succeed())
		Expect(ds.Spec.Template.Spec.NodeSelector).To(HaveKeyWithValue(daosv1alpha1.LabelRole, "storage"))
		Expect(ds.Spec.Template.Spec.HostNetwork).To(BeTrue())
		Expect(*ds.Spec.Template.Spec.Containers[0].SecurityContext.Privileged).To(BeTrue())
		Expect(ds.Spec.Template.Spec.Containers[0].Env).To(ContainElement(corev1.EnvVar{Name: "DAOS_HOSTPREP_BIND_NVME", Value: "false"}), "discover-only by default")
		Expect(ds.OwnerReferences).To(HaveLen(1))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "daos-test", Name: "daos-hostprep"}, &corev1.ServiceAccount{})).To(Succeed())
	})

	It("creates one pinned server StatefulSet per rendered node (#9)", func() {
		reconcileOnce()
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "daos-test", Name: "t1-server-n2"}, sts)).To(Succeed())
		Expect(*sts.Spec.Replicas).To(Equal(int32(1)))
		Expect(sts.Spec.UpdateStrategy.Type).To(Equal(appsv1.OnDeleteStatefulSetStrategyType), "no automatic engine restarts")
		pod := sts.Spec.Template.Spec
		Expect(pod.HostNetwork).To(BeTrue())
		Expect(pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchFields[0].Values).To(Equal([]string{"n2"}))
		c := pod.Containers[0]
		Expect(c.Image).To(Equal("s"))
		Expect(*c.SecurityContext.Privileged).To(BeTrue())
		Expect(c.Resources.Requests.Memory().String()).To(Equal("34Gi"), "tmpfs 32Gi + 2Gi overhead")
		Expect(c.Resources.Requests.Cpu().String()).To(Equal("11"), "8 targets + 2 helpers + 1")
		hp := c.Resources.Limits[corev1.ResourceName("hugepages-2Mi")]
		Expect(hp.String()).To(Equal("16Gi"), "8192 x 2Mi")
		Expect(c.LivenessProbe).To(BeNil())
		Expect(c.ReadinessProbe.TCPSocket.Port.IntValue()).To(Equal(10001))
		var mounts []string
		for _, m := range c.VolumeMounts {
			mounts = append(mounts, m.MountPath)
		}
		Expect(mounts).To(ContainElements("/etc/daos/daos_server.yml", "/var/daos", "/var/log/daos", "/dev/hugepages", "/dev", "/sys"))
		for _, v := range pod.Volumes {
			switch v.Name {
			case "config":
				Expect(v.ConfigMap.Name).To(Equal("t1-server-n2"))
			case "data":
				Expect(v.HostPath.Path).To(Equal("/var/daos/t1"))
			}
		}
		Expect(sts.OwnerReferences).To(HaveLen(1))
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "daos-test", Name: "t1-server-n3"}, &appsv1.StatefulSet{})
		Expect(err).To(HaveOccurred(), "no workload for a node without facts")

		sys := &daosv1alpha1.DaosSystem{}
		Expect(k8sClient.Get(ctx, nn, sys)).To(Succeed())
		sr := meta.FindStatusCondition(sys.Status.Conditions, daosv1alpha1.ConditionServersReady)
		Expect(sr).NotTo(BeNil())
		Expect(sr.Status).To(Equal(metav1.ConditionFalse), "envtest runs no StatefulSet controller, so no pods")
		Expect(sr.Reason).To(Equal("PodsNotReady"))
		for _, nc := range sys.Status.NodeConfigs {
			if nc.Node == "n1" {
				Expect(nc.Workload).To(Equal("t1-server-n1"))
				Expect(nc.ServerReady).To(BeFalse())
			}
		}
		r := meta.FindStatusCondition(sys.Status.Conditions, daosv1alpha1.ConditionReady)
		Expect(r.Reason).To(Equal("FormatNotManaged"))

		By("honouring spec.server.resources and host paths")
		sys.Spec.Server.DataHostPath = "/data/daos"
		sys.Spec.Server.Resources = &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")}}
		Expect(k8sClient.Update(ctx, sys)).To(Succeed())
		reconcileOnce()
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "daos-test", Name: "t1-server-n2"}, sts)).To(Succeed())
		Expect(sts.Spec.Template.Spec.Containers[0].Resources.Requests.Memory().String()).To(Equal("4Gi"))
		for _, v := range sts.Spec.Template.Spec.Volumes {
			if v.Name == "data" {
				Expect(v.HostPath.Path).To(Equal("/data/daos"))
			}
		}
	})

	It("removes server workloads only when spec.server.enabled is set to false", func() {
		reconcileOnce()
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "daos-test", Name: "t1-server-n1"}, &appsv1.StatefulSet{})).To(Succeed())
		sys := &daosv1alpha1.DaosSystem{}
		Expect(k8sClient.Get(ctx, nn, sys)).To(Succeed())
		f := false
		sys.Spec.Server.Enabled = &f
		Expect(k8sClient.Update(ctx, sys)).To(Succeed())
		reconcileOnce()
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "daos-test", Name: "t1-server-n1"}, &appsv1.StatefulSet{})
		Expect(err).To(HaveOccurred())
		Expect(k8sClient.Get(ctx, nn, sys)).To(Succeed())
		Expect(meta.FindStatusCondition(sys.Status.Conditions, daosv1alpha1.ConditionServersReady).Reason).To(Equal("Disabled"))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "daos-test", Name: "t1-server-n1"}, &corev1.ConfigMap{})).To(Succeed(), "config stays")
	})

	It("removes the DaemonSet when hostPrep is disabled", func() {
		reconcileOnce()
		sys := &daosv1alpha1.DaosSystem{}
		Expect(k8sClient.Get(ctx, nn, sys)).To(Succeed())
		f := false
		sys.Spec.HostPrep.Enabled = &f
		Expect(k8sClient.Update(ctx, sys)).To(Succeed())
		reconcileOnce()
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "daos-test", Name: "t1-hostprep"}, &appsv1.DaemonSet{})
		Expect(err).To(HaveOccurred())
	})

	It("excludes nodes that share a physical drive and says so", func() {
		n2 := &corev1.Node{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "n2"}, n2)).To(Succeed())
		n2.Annotations[daosv1alpha1.AnnotationBdevDSN] = "0000:03:00.0=DSN1" // same serial as n1's first drive
		Expect(k8sClient.Update(ctx, n2)).To(Succeed())
		reconcileOnce()
		sys := &daosv1alpha1.DaosSystem{}
		Expect(k8sClient.Get(ctx, nn, sys)).To(Succeed())
		c := meta.FindStatusCondition(sys.Status.Conditions, daosv1alpha1.ConditionDriveConflict)
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Message).To(ContainSubstring("DSN1 on n1+n2"))
		for _, n := range []string{"n1", "n2"} {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "daos-test", Name: "t1-server-" + n}, &corev1.ConfigMap{})
			Expect(err).To(HaveOccurred(), "conflicting node %s must not be rendered", n)
		}
		// n3 has no facts, n1/n2 conflict -> nobody can host the MS replica
		Expect(meta.IsStatusConditionFalse(sys.Status.Conditions, daosv1alpha1.ConditionConfigRendered)).To(BeTrue())
	})
})
