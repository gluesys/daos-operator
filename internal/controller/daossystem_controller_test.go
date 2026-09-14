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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
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
