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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	daosv1alpha1 "gitlab.gluesys.com/exastor/daos-operator/api/v1alpha1"
	"gitlab.gluesys.com/exastor/daos-operator/internal/dmg"
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
		_ = k8sClient.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "daos-test", Name: "t1-metrics"}})
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
	reconcileWith := func(f *fakeDmg) reconcile.Result {
		r := &DaosSystemReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Dmg: f, DisableProbeHold: true}
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	getSys := func() *daosv1alpha1.DaosSystem {
		sys := &daosv1alpha1.DaosSystem{}
		Expect(k8sClient.Get(ctx, nn, sys)).To(Succeed())
		return sys
	}
	cond := func(sys *daosv1alpha1.DaosSystem, t string) *metav1.Condition {
		c := meta.FindStatusCondition(sys.Status.Conditions, t)
		Expect(c).NotTo(BeNil(), t)
		return c
	}
	// envtest runs no StatefulSet controller: fake the pods being Ready
	markServersReady := func(nodes ...string) {
		for _, n := range nodes {
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "daos-test", Name: "t1-server-" + n}, sts)).To(Succeed())
			sts.Status.Replicas, sts.Status.ReadyReplicas = 1, 1
			Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())
		}
	}
	approve := func() {
		sys := getSys()
		if sys.Annotations == nil {
			sys.Annotations = map[string]string{}
		}
		sys.Annotations[daosv1alpha1.AnnotationFormatApproved] = "true"
		Expect(k8sClient.Update(ctx, sys)).To(Succeed())
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
		Expect(r.Reason).To(Equal("ServersNotReady"))
		Expect(meta.FindStatusCondition(sys.Status.Conditions, daosv1alpha1.ConditionFormatted).Reason).To(Equal("NoRunner"), "no Dmg runner in this test")

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

	It("never formats by itself: reports pendingFormat, formats once after approval, then mirrors membership (#10)", func() {
		f := &fakeDmg{script: map[string]*dmg.Result{}}
		By("probe still running: nothing decided yet")
		res := reconcileWith(f)
		Expect(res.RequeueAfter).To(Equal(requeueProbe))
		Expect(f.count("system query -v")).To(Equal(1))
		Expect(cond(getSys(), daosv1alpha1.ConditionFormatted).Reason).To(Equal("Probing"))

		By("dmg says uninitialized -> pendingFormat, no format call")
		f.set("system query -v", &dmg.Result{Done: true, ExitCode: 1, Output: "some log line\n" + dmgUninitialized})
		reconcileWith(f)
		sys := getSys()
		Expect(sys.Status.PendingFormat).To(BeTrue())
		Expect(sys.Status.Formatted).To(BeFalse())
		c := cond(sys, daosv1alpha1.ConditionFormatted)
		Expect(c.Status).To(Equal(metav1.ConditionFalse))
		Expect(c.Reason).To(Equal("AwaitingApproval"))
		Expect(c.Message).To(ContainSubstring("kubectl annotate daossystem t1 " + daosv1alpha1.AnnotationFormatApproved + "=true"))
		Expect(f.count("storage format")).To(BeZero())
		// n3 has no facts -> ServersNotReady wins over AwaitingFormat? no: n3 is not rendered, so servers=all rendered
		Expect(cond(sys, daosv1alpha1.ConditionReady).Reason).To(Equal("ServersNotReady"), "pods not Ready in envtest")

		By("approval with servers not ready is refused")
		approve()
		reconcileWith(f)
		Expect(f.count("storage format")).To(BeZero())
		Expect(cond(getSys(), daosv1alpha1.ConditionFormatted).Reason).To(Equal("AwaitingServers"))

		By("approval with all servers ready runs dmg storage format exactly once")
		markServersReady("n1", "n2")
		f.set("storage format", &dmg.Result{Done: true, ExitCode: 0, Output: dmgFormatOK})
		reconcileWith(f)
		Expect(f.count("storage format")).To(Equal(1))
		sys = getSys()
		Expect(sys.Status.Formatted).To(BeTrue())
		Expect(sys.Status.PendingFormat).To(BeFalse())
		Expect(sys.Status.FormatTime).NotTo(BeNil())
		Expect(sys.Annotations).NotTo(HaveKey(daosv1alpha1.AnnotationFormatApproved), "one-shot approval is consumed")
		Expect(cond(sys, daosv1alpha1.ConditionFormatted).Status).To(Equal(metav1.ConditionTrue))
		spec := f.specs[len(f.specs)-1]
		Expect(spec.Name).To(Equal("t1-dmg-format"))
		Expect(spec.Image).To(Equal("d"))
		Expect(spec.ControlConfigMap).To(Equal("t1-control"))

		By("membership is copied from dmg system query")
		f.set("system query -v", &dmg.Result{Done: true, Output: dmgMembers})
		res = reconcileWith(f)
		Expect(f.count("storage format")).To(Equal(1), "no second format")
		sys = getSys()
		Expect(sys.Status.RanksTotal).To(Equal(int32(2)))
		Expect(sys.Status.RanksJoined).To(Equal(int32(2)))
		Expect(sys.Status.Ranks).To(Equal([]daosv1alpha1.RankStatus{{Rank: 0, Node: "n1", State: "joined"}, {Rank: 1, Node: "n2", State: "joined"}}))
		Expect(cond(sys, daosv1alpha1.ConditionReady).Status).To(Equal(metav1.ConditionTrue))
		Expect(res.RequeueAfter).To(Equal(requeueMembership))

		By("a rank in awaitformat (expansion) re-opens the gate; a stale approval is dropped when nothing is pending")
		f.set("system query -v", &dmg.Result{Done: true, Output: dmgMembersAwait})
		reconcileWith(f)
		sys = getSys()
		Expect(sys.Status.PendingFormat).To(BeTrue())
		Expect(sys.Status.RanksJoined).To(Equal(int32(1)))
		Expect(cond(sys, daosv1alpha1.ConditionReady).Reason).To(Equal("RanksNotJoined"))
		f.set("system query -v", &dmg.Result{Done: true, Output: dmgMembers})
		approve()
		reconcileWith(f)
		sys = getSys()
		Expect(sys.Annotations).NotTo(HaveKey(daosv1alpha1.AnnotationFormatApproved))
		Expect(f.count("storage format")).To(Equal(1))

		By("an unreachable management service leaves the last known state and says Unknown")
		f.set("system query -v", &dmg.Result{Done: true, ExitCode: 1, Output: dmgUnreachable})
		reconcileWith(f)
		sys = getSys()
		Expect(sys.Status.Formatted).To(BeTrue())
		Expect(cond(sys, daosv1alpha1.ConditionFormatted).Status).To(Equal(metav1.ConditionUnknown))
		Expect(cond(sys, daosv1alpha1.ConditionFormatted).Reason).To(Equal("ManagementUnreachable"))
	})

	It("holds the query cadence so a finished Job does not immediately spawn the next one", func() {
		f := &fakeDmg{script: map[string]*dmg.Result{"system query -v": {Done: true, ExitCode: 1, Output: dmgUninitialized}}}
		r := &DaosSystemReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Dmg: f}
		clearProbeHold(sysName)
		defer clearProbeHold(sysName)
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(f.count("system query -v")).To(Equal(1), "second reconcile within the hold must not run dmg")
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))
		Expect(res.RequeueAfter).To(BeNumerically("<=", requeueSlow))
		clearProbeHold(sysName)
	})

	It("reports a failed format, consumes the approval and stays pending", func() {
		f := &fakeDmg{script: map[string]*dmg.Result{"system query -v": {Done: true, ExitCode: 1, Output: dmgUninitialized}}}
		reconcileWith(f)
		markServersReady("n1", "n2")
		approve()
		f.set("storage format", &dmg.Result{Done: true, ExitCode: 1, Output: dmgFormatHostErr})
		reconcileWith(f)
		sys := getSys()
		Expect(sys.Status.Formatted).To(BeFalse())
		Expect(sys.Status.PendingFormat).To(BeTrue())
		Expect(sys.Annotations).NotTo(HaveKey(daosv1alpha1.AnnotationFormatApproved))
		c := cond(sys, daosv1alpha1.ConditionFormatted)
		Expect(c.Reason).To(Equal("FormatFailed"))
		Expect(c.Message).To(ContainSubstring("10.0.0.2: storage format failed"))
		By("a second reconcile does not format again without a new approval")
		reconcileWith(f)
		Expect(f.count("storage format")).To(Equal(1))
	})

	It("fronts the servers with a headless metrics Service and renders telemetry_port (#12)", func() {
		reconcileOnce()
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "daos-test", Name: "t1-metrics"}, svc)).To(Succeed())
		Expect(svc.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))
		Expect(svc.Spec.Selector).To(Equal(map[string]string{daosv1alpha1.LabelSystem: "t1", daosv1alpha1.LabelRole: "server"}))
		Expect(svc.Spec.Ports[0].Port).To(Equal(int32(9191)))
		Expect(svc.OwnerReferences).To(HaveLen(1))
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "daos-test", Name: "t1-server-n1"}, cm)).To(Succeed())
		Expect(cm.Data["daos_server.yml"]).To(ContainSubstring("telemetry_port: 9191"))
		sys := getSys()
		c := telemetryCond(&sys.Status)
		Expect(c).NotTo(BeNil())
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Reason).To(Equal("NoPrometheusOperator"), "envtest has no ServiceMonitor CRD: say so instead of failing")

		By("disabling telemetry removes the Service and the port")
		f := false
		sys.Spec.Telemetry.Enabled = &f
		Expect(k8sClient.Update(ctx, sys)).To(Succeed())
		reconcileOnce()
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "daos-test", Name: "t1-metrics"}, svc)).NotTo(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "daos-test", Name: "t1-server-n1"}, cm)).To(Succeed())
		Expect(cm.Data["daos_server.yml"]).NotTo(ContainSubstring("telemetry_port"))
		Expect(telemetryCond(&getSys().Status).Reason).To(Equal("Disabled"))
	})

	It("runs the full-stop upgrade only after approval: stop, replace pods, start, verify (#13)", func() {
		f := &fakeDmg{script: map[string]*dmg.Result{"system query -v": {Done: true, Output: dmgMembers}}}
		mkPod := func(name, image string) {
			p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "daos-test", Name: name,
				Labels: map[string]string{daosv1alpha1.LabelSystem: sysName, daosv1alpha1.LabelRole: "server"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: serverContainer, Image: image}}}}
			Expect(k8sClient.Create(ctx, p)).To(Succeed())
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
			Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
		}
		DeferCleanup(func() {
			pods := &corev1.PodList{}
			_ = k8sClient.List(ctx, pods, client.InNamespace("daos-test"))
			for i := range pods.Items {
				_ = k8sClient.Delete(ctx, &pods.Items[i], client.GracePeriodSeconds(0))
			}
		})
		reconcileWith(f) // StatefulSets exist, image "s"
		markServersReady("n1", "n2")
		mkPod("t1-server-n1-0", "s")
		mkPod("t1-server-n2-0", "s")
		reconcileWith(f)
		sys := getSys()
		Expect(cond(sys, daosv1alpha1.ConditionUpgrading).Reason).To(Equal("UpToDate"))
		Expect(sys.Status.ObservedVersion).To(Equal("2.8.0"))

		By("a new image without approval only reports Pending; nothing is stopped")
		sys.Spec.Images.Server, sys.Spec.Version = "s2", "2.8.1"
		Expect(k8sClient.Update(ctx, sys)).To(Succeed())
		reconcileWith(f)
		sys = getSys()
		Expect(sys.Status.Upgrade.Phase).To(Equal(upgradePending))
		Expect(sys.Status.Upgrade.FromImage).To(Equal("s"))
		Expect(sys.Status.Upgrade.ToImage).To(Equal("s2"))
		Expect(f.count("system stop")).To(BeZero())
		Expect(sys.Status.ObservedVersion).To(Equal("2.8.0"), "version follows the pods, not the spec")
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "daos-test", Name: "t1-server-n1"}, sts)).To(Succeed())
		Expect(sts.Spec.Template.Spec.Containers[0].Image).To(Equal("s2"), "template updated, OnDelete keeps pods")

		By("approval -> dmg system stop")
		sys.Spec.Upgrade.Approved = true
		Expect(k8sClient.Update(ctx, sys)).To(Succeed())
		reconcileWith(f)
		sys = getSys()
		Expect(sys.Status.Upgrade.Phase).To(Equal(upgradeStopping))
		Expect(f.count("system stop")).To(Equal(1))
		Expect(cond(sys, daosv1alpha1.ConditionReady).Reason).To(Equal("Upgrading"))
		Expect(f.count("system query -v")).To(Equal(3), "three probes before approval, none while upgrading")

		By("stopped -> pods deleted -> waiting for new pods")
		f.set("system stop", &dmg.Result{Done: true, Output: dmgOK})
		reconcileWith(f)
		Expect(getSys().Status.Upgrade.Phase).To(Equal(upgradeUpdating))
		reconcileWith(f)
		sys = getSys()
		Expect(sys.Status.Upgrade.Phase).To(Equal(upgradeStarting))
		// unscheduled pods are removed by the API server at once (no kubelet in envtest)
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "daos-test", Name: "t1-server-n1-0"}, &corev1.Pod{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "old pods deleted")
		reconcileWith(f)
		Expect(getSys().Status.Upgrade.Message).To(HavePrefix("0/2 server pod(s) Ready"))
		// no StatefulSet controller in envtest: bring the new pods up by hand
		mkPod("t1-server-n1-0", "s2")
		mkPod("t1-server-n2-0", "s2")

		By("all pods Ready -> dmg system start -> verify -> completed, approval consumed")
		reconcileWith(f)
		Expect(getSys().Status.Upgrade.Phase).To(Equal(upgradeStartingSystem))
		reconcileWith(f)
		Expect(f.count("system start")).To(Equal(1))
		f.set("system start", &dmg.Result{Done: true, Output: dmgOK})
		reconcileWith(f)
		Expect(getSys().Status.Upgrade.Phase).To(Equal(upgradeVerifying))
		reconcileWith(f)
		sys = getSys()
		Expect(sys.Status.Upgrade.Phase).To(Equal(upgradeCompleted))
		Expect(sys.Status.Upgrade.FinishedAt).NotTo(BeNil())
		Expect(sys.Status.ObservedVersion).To(Equal("2.8.1"))
		Expect(sys.Spec.Upgrade.Approved).To(BeFalse(), "one-shot approval")
		Expect(cond(sys, daosv1alpha1.ConditionUpgrading).Reason).To(Equal(upgradeCompleted))
		reconcileWith(f)
		Expect(f.count("system stop")).To(Equal(2), "start + poll only; completed upgrade does not run again")
	})

	It("fails the upgrade honestly when dmg system stop errors and drops the approval", func() {
		f := &fakeDmg{script: map[string]*dmg.Result{"system query -v": {Done: true, Output: dmgMembers},
			"system stop": {Done: true, ExitCode: 1, Output: dmgUnreachable}}}
		reconcileWith(f)
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "daos-test", Name: "t1-server-n1-0",
			Labels: map[string]string{daosv1alpha1.LabelSystem: sysName, daosv1alpha1.LabelRole: "server"}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: serverContainer, Image: "s"}}}}
		Expect(k8sClient.Create(ctx, p)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, p, client.GracePeriodSeconds(0)) })
		sys := getSys()
		sys.Spec.Images.Server, sys.Spec.Upgrade.Approved = "s2", true
		Expect(k8sClient.Update(ctx, sys)).To(Succeed())
		reconcileWith(f)
		sys = getSys()
		Expect(sys.Status.Upgrade.Phase).To(Equal(upgradeFailed))
		Expect(sys.Status.Upgrade.Message).To(ContainSubstring("dmg system stop"))
		Expect(sys.Spec.Upgrade.Approved).To(BeFalse())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "daos-test", Name: "t1-server-n1-0"}, p)).To(Succeed())
		Expect(p.DeletionTimestamp.IsZero()).To(BeTrue(), "no pod touched after a failed stop")
	})

	It("removes server workloads only when spec.server.enabled is set to false", func() {
		reconcileOnce()
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "daos-test", Name: "t1-server-n1"}, &appsv1.StatefulSet{})).To(Succeed())
		sys := &daosv1alpha1.DaosSystem{}
		Expect(k8sClient.Get(ctx, nn, sys)).To(Succeed())
		f := false
		sys.Spec.Server.Enabled = &f
		Expect(k8sClient.Update(ctx, sys)).To(Succeed())
		// the node lost its facts in the meantime (hostprep rewrote them): the workload must still go
		n1 := &corev1.Node{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "n1"}, n1)).To(Succeed())
		n1.Annotations = map[string]string{}
		Expect(k8sClient.Update(ctx, n1)).To(Succeed())
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
