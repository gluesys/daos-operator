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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	daosv1alpha1 "gitlab.gluesys.com/exastor/daos-operator/api/v1alpha1"
	"gitlab.gluesys.com/exastor/daos-operator/internal/dmg"
)

var _ = Describe("DaosPool / DaosContainer Controllers", func() {
	ctx := context.Background()
	const sysName, poolName = "ps1", "p1"
	poolNN := types.NamespacedName{Name: poolName}
	contNN := types.NamespacedName{Namespace: "default", Name: "c1"}

	BeforeEach(func() {
		sys := &daosv1alpha1.DaosSystem{ObjectMeta: metav1.ObjectMeta{Name: sysName},
			Spec: daosv1alpha1.DaosSystemSpec{Version: "2.8.0", Namespace: "daos-test",
				Images:  daosv1alpha1.ImagesSpec{Server: "s", Agent: "a", Admin: "d", Client: "c"},
				Engines: []daosv1alpha1.EngineSpec{{Targets: 8}}}}
		Expect(k8sClient.Create(ctx, sys)).To(Succeed())
		sys.Status.Formatted = true
		Expect(k8sClient.Status().Update(ctx, sys)).To(Succeed())
		pool := &daosv1alpha1.DaosPool{ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec: daosv1alpha1.DaosPoolSpec{SystemRef: sysName, Size: resource.MustParse("10Gi"), Ranks: []int32{0, 1}, RedundancyFactor: 2,
				Properties: map[string]string{"ec_cell_sz": "131072"}, ACL: []string{"A::OWNER@:rw", "A:G:GROUP@:rw"}}}
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())
	})
	AfterEach(func() {
		f := &fakeDmg{script: map[string]*dmg.Result{}}
		for _, nn := range []types.NamespacedName{contNN} {
			c := &daosv1alpha1.DaosContainer{}
			if err := k8sClient.Get(ctx, nn, c); err == nil {
				_ = k8sClient.Delete(ctx, c)
				_, _ = (&DaosContainerReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Dmg: f, DisableProbeHold: true}).Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			}
		}
		p := &daosv1alpha1.DaosPool{}
		if err := k8sClient.Get(ctx, poolNN, p); err == nil {
			_ = k8sClient.Delete(ctx, p)
			_, _ = (&DaosPoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Dmg: f, DisableProbeHold: true}).Reconcile(ctx, reconcile.Request{NamespacedName: poolNN})
		}
		_ = k8sClient.Delete(ctx, &daosv1alpha1.DaosSystem{ObjectMeta: metav1.ObjectMeta{Name: sysName}})
	})

	poolRec := func(f *fakeDmg) reconcile.Result {
		r := &DaosPoolReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Dmg: f, DisableProbeHold: true}
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: poolNN})
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	getPool := func() *daosv1alpha1.DaosPool {
		p := &daosv1alpha1.DaosPool{}
		Expect(k8sClient.Get(ctx, poolNN, p)).To(Succeed())
		return p
	}
	ready := func(conds []metav1.Condition) *metav1.Condition {
		c := meta.FindStatusCondition(conds, daosv1alpha1.ConditionReady)
		Expect(c).NotTo(BeNil())
		return c
	}
	setPoolUUID := func() {
		p := getPool()
		p.Status.UUID = "8a9ca36d-495a-4d50-a0d2-f111b80d5d9d"
		Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
	}

	It("creates a missing pool from spec, mirrors dmg pool query, extends and rewrites the ACL on drift", func() {
		f := &fakeDmg{script: map[string]*dmg.Result{}}
		poolRec(f) // adds finalizer
		Expect(getPool().Finalizers).To(ContainElement(daosv1alpha1.FinalizerPool))
		poolRec(f)
		Expect(ready(getPool().Status.Conditions).Reason).To(Equal("Probing"))
		q := f.last("-dmg-query")
		Expect(q.Args).To(Equal([]string{"pool", "query", "--show-enabled", "p1"}))
		Expect(q.Image).To(Equal("d"))
		Expect(q.ControlConfigMap).To(Equal("ps1-control"))

		By("not found -> dmg pool create with size in bytes, rd_fac, properties, ranks and ACL file")
		f.set("-dmg-query", &dmg.Result{Done: true, ExitCode: 1, Output: dmgPoolNotFound})
		poolRec(f)
		p := getPool()
		Expect(p.Status.Operation).To(Equal(opCreate))
		Expect(ready(p.Status.Conditions).Reason).To(Equal("CreateInProgress"))
		cr := f.last("-dmg-create")
		Expect(cr.Command[0]).To(Equal("bash"))
		Expect(cr.Command[2]).To(ContainSubstring(`printf '%s\n' 'A::OWNER@:rw' 'A:G:GROUP@:rw' > '/tmp/daos-acl' && exec 'dmg' '-o' '/etc/daos/daos_control.yml' '-j' 'pool' 'create' '-z' '10737418240B' '-P' 'rd_fac:2,ec_cell_sz:131072' '-r' '0,1' '-a' '/tmp/daos-acl' 'p1'`))

		By("create still running -> only the create Job is polled")
		poolRec(f)
		Expect(f.count("-dmg-query")).To(Equal(2), "no query while an operation is in flight")

		By("create done -> uuid recorded, ACL considered applied")
		f.set("-dmg-create", &dmg.Result{Done: true, Output: dmgPoolCreate})
		poolRec(f)
		p = getPool()
		Expect(p.Status.Operation).To(BeEmpty())
		Expect(p.Status.UUID).To(Equal("8a9ca36d-495a-4d50-a0d2-f111b80d5d9d"))
		Expect(p.Status.EnabledRanks).To(Equal([]int32{0, 1}))
		Expect(p.Status.AppliedACLHash).To(Equal(aclHash(p.Spec.ACL)))

		By("query mirrors state, sizes, rebuild and ranks")
		f.set("-dmg-query", &dmg.Result{Done: true, Output: dmgPoolQuery})
		res := poolRec(f)
		p = getPool()
		Expect(ready(p.Status.Conditions).Status).To(Equal(metav1.ConditionTrue))
		Expect(p.Status.State).To(Equal("Ready"))
		Expect(p.Status.TotalBytes).To(Equal(int64(486539264 + 7520000000)))
		Expect(p.Status.FreeBytes).To(Equal(int64(443035176 + 7456817152)))
		Expect(p.Status.RebuildState).To(Equal("idle"))
		Expect(p.Status.LastQueryTime).NotTo(BeNil())
		Expect(res.RequeueAfter).To(Equal(poolRequeueIdle))
		Expect(f.count("-dmg-create")).To(Equal(3), "create ran once (start + running poll + done poll)")

		By("adding a rank -> dmg pool extend with only the missing rank")
		p.Spec.Ranks = []int32{0, 1, 2}
		Expect(k8sClient.Update(ctx, p)).To(Succeed())
		poolRec(f)
		Expect(getPool().Status.Operation).To(Equal(opExtend))
		Expect(f.last("-dmg-extend").Args).To(Equal([]string{"pool", "extend", "--ranks=2", "p1"}))
		f.set("-dmg-extend", &dmg.Result{Done: true, Output: dmgOK})
		poolRec(f)
		Expect(getPool().Status.Operation).To(BeEmpty())

		By("changing spec.acl -> dmg pool overwrite-acl once")
		p = getPool()
		p.Spec.ACL = []string{"A::OWNER@:rw", "A::alice@:rw"}
		Expect(k8sClient.Update(ctx, p)).To(Succeed())
		f.set("-dmg-query", &dmg.Result{Done: true, Output: strings.Replace(dmgPoolQuery, `"[0-1]"`, `"[0-2]"`, 1)})
		poolRec(f)
		Expect(getPool().Status.Operation).To(Equal(opACL))
		Expect(f.last("-dmg-acl").Command[2]).To(ContainSubstring(`'A::alice@:rw' > '/tmp/daos-acl' && exec 'dmg' '-o' '/etc/daos/daos_control.yml' '-j' 'pool' 'overwrite-acl' '-a' '/tmp/daos-acl' 'p1'`))
		f.set("-dmg-acl", &dmg.Result{Done: true, Output: dmgOK})
		poolRec(f)
		poolRec(f)
		p = getPool()
		Expect(p.Status.AppliedACLHash).To(Equal(aclHash(p.Spec.ACL)))
		Expect(f.count("-dmg-acl")).To(Equal(2))
		Expect(ready(p.Status.Conditions).Status).To(Equal(metav1.ConditionTrue))

		By("deleting the DaosPool without approval keeps the DAOS pool")
		Expect(k8sClient.Delete(ctx, p)).To(Succeed())
		poolRec(f)
		Expect(f.count("-dmg-destroy")).To(BeZero())
		err := k8sClient.Get(ctx, poolNN, &daosv1alpha1.DaosPool{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "finalizer removed, object gone")
	})

	It("reports pool usage and warns once when it crosses the threshold (#21)", func() {
		f := &fakeDmg{script: map[string]*dmg.Result{"-dmg-query": {Done: true, Output: dmgPoolQuery}}}
		poolRec(f) // finalizer
		p := getPool()
		p.Spec.ACL = nil // no ACL drift, so the query result is not followed by an operation
		Expect(k8sClient.Update(ctx, p)).To(Succeed())
		poolRec(f)
		p = getPool()
		// dmgPoolQuery: 486539264+7520000000 total, 443035176+7456817152 free -> ~1% used
		Expect(p.Status.UsedPercent).To(Equal(int32(1)))
		Expect(meta.IsStatusConditionFalse(p.Status.Conditions, daosv1alpha1.ConditionSpaceLow)).To(BeTrue())

		By("crossing spec.spaceWarningPercent flips SpaceLow with the numbers in the message")
		p.Spec.SpaceWarningPercent = ptr.To(int32(1))
		Expect(k8sClient.Update(ctx, p)).To(Succeed())
		poolRec(f)
		p = getPool()
		c := meta.FindStatusCondition(p.Status.Conditions, daosv1alpha1.ConditionSpaceLow)
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Message).To(ContainSubstring("1% used"))
		Expect(c.Message).To(ContainSubstring("free"))

		By("0 disables the check but keeps the number")
		p.Spec.SpaceWarningPercent = ptr.To(int32(0))
		Expect(k8sClient.Update(ctx, p)).To(Succeed())
		poolRec(f)
		p = getPool()
		Expect(meta.FindStatusCondition(p.Status.Conditions, daosv1alpha1.ConditionSpaceLow)).To(BeNil())
		Expect(p.Status.UsedPercent).To(Equal(int32(1)))
	})

	It("does not retry a failed create until the spec changes", func() {
		f := &fakeDmg{script: map[string]*dmg.Result{"-dmg-query": {Done: true, ExitCode: 1, Output: dmgPoolNotFound}}}
		poolRec(f)
		poolRec(f)
		Expect(getPool().Status.Operation).To(Equal(opCreate))
		f.set("-dmg-create", &dmg.Result{Done: true, ExitCode: 1, Output: dmgPoolTooSmall})
		res := poolRec(f)
		p := getPool()
		Expect(ready(p.Status.Conditions).Reason).To(Equal("CreateFailed"))
		Expect(ready(p.Status.Conditions).Message).To(ContainSubstring("requested NVMe capacity too small"))
		Expect(res.RequeueAfter).To(Equal(poolRequeueFailed))
		poolRec(f)
		poolRec(f)
		Expect(f.count("-dmg-create")).To(Equal(2), "start + poll, no retry")
		p.Spec.Size = resource.MustParse("20Gi")
		Expect(k8sClient.Update(ctx, p)).To(Succeed())
		poolRec(f)
		Expect(f.count("-dmg-create")).To(Equal(3), "spec change re-arms create")
	})

	It("never re-creates a pool that vanished and destroys only with approval", func() {
		f := &fakeDmg{script: map[string]*dmg.Result{"-dmg-query": {Done: true, ExitCode: 1, Output: dmgPoolNotFound}}}
		poolRec(f)
		setPoolUUID()
		poolRec(f)
		p := getPool()
		Expect(ready(p.Status.Conditions).Reason).To(Equal("PoolMissing"))
		Expect(f.count("-dmg-create")).To(BeZero())

		By("approved deletion runs dmg pool destroy --recursive and waits for it")
		p.Annotations = map[string]string{daosv1alpha1.AnnotationDestroyApproved: "true"}
		Expect(k8sClient.Update(ctx, p)).To(Succeed())
		Expect(k8sClient.Delete(ctx, p)).To(Succeed())
		poolRec(f)
		Expect(f.last("-dmg-destroy").Args).To(Equal([]string{"pool", "destroy", "--recursive", "p1"}))
		Expect(getPool().Status.Operation).To(Equal(opDestroy), "still exists while destroy runs")
		f.set("-dmg-destroy", &dmg.Result{Done: true, Output: dmgOK})
		poolRec(f)
		err := k8sClient.Get(ctx, poolNN, &daosv1alpha1.DaosPool{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("creates a container with the agent sidecar Job and mirrors daos cont query", func() {
		setPoolUUID()
		c := &daosv1alpha1.DaosContainer{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "c1"},
			Spec: daosv1alpha1.DaosContainerSpec{PoolRef: poolName, RedundancyFactor: ptr.To(int32(1)), ACL: []string{"A::OWNER@:rw"}}}
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		f := &fakeDmg{script: map[string]*dmg.Result{}}
		rec := func() {
			r := &DaosContainerReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Dmg: f, DisableProbeHold: true}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: contNN})
			Expect(err).NotTo(HaveOccurred())
		}
		get := func() *daosv1alpha1.DaosContainer {
			Expect(k8sClient.Get(ctx, contNN, c)).To(Succeed())
			return c
		}
		rec() // finalizer
		rec() // query
		q := f.last("-daos-query")
		Expect(q).NotTo(BeNil())
		Expect(q.Image).To(Equal("c"))
		Expect(q.Sidecar.Image).To(Equal("a"))
		Expect(q.Privileged).To(BeTrue())
		Expect(q.ShareProcessNamespace).To(BeTrue())
		Expect(q.Owner.GetName()).To(Equal(poolName), "Jobs are owned by the cluster-scoped DaosPool")
		Expect(q.Labels[daosv1alpha1.LabelContainer]).To(Equal("default.c1"))
		Expect(q.Command[2]).To(HavePrefix("for i in $(seq 1 60); do [ -S /var/run/daos_agent/daos_agent.sock ]"))
		Expect(q.Command[2]).To(HaveSuffix("exec daos -j -G 'daos_server' 'cont' 'query' 'p1' 'c1'"))

		By("not found -> daos cont create with CRD defaults (POSIX, RP_2GX/RP_2G1, 4 MiB, crc32) and rd_fac")
		f.set("-daos-query", &dmg.Result{Done: true, ExitCode: 1, Output: daosContNotFound})
		rec()
		Expect(get().Status.Operation).To(Equal(opCreate))
		cr := f.last("-daos-create")
		Expect(cr.Command[2]).To(ContainSubstring(`exec daos -j -G 'daos_server' 'cont' 'create' 'p1' 'c1' '--type' 'POSIX' '--file-oclass' 'RP_2GX' '--dir-oclass' 'RP_2G1' '--chunk-size' '4194304' '--properties' 'rd_fac:1,cksum:crc32' '--acl-file' '/tmp/daos-acl'`))
		Expect(cr.Command[2]).To(ContainSubstring(`printf '%s\n' 'A::OWNER@:rw' > '/tmp/daos-acl'`))
		f.set("-daos-create", &dmg.Result{Done: true, Output: daosContQuery})
		rec()
		Expect(get().Status.UUID).To(Equal("e4d6c5b3-efd6-4891-9526-3a263925212d"))
		Expect(c.Status.Operation).To(BeEmpty())

		f.set("-daos-query", &dmg.Result{Done: true, Output: daosContQuery})
		rec()
		get()
		Expect(c.Status.Ready).To(BeTrue())
		Expect(c.Status.Health).To(Equal("HEALTHY"))
		Expect(c.Status.PoolUUID).To(Equal("8a9ca36d-495a-4d50-a0d2-f111b80d5d9d"))
		Expect(ready(c.Status.Conditions).Status).To(Equal(metav1.ConditionTrue))

		By("deleting without approval keeps the DAOS container")
		Expect(k8sClient.Delete(ctx, c)).To(Succeed())
		rec()
		Expect(f.count("-daos-destroy")).To(BeZero())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, contNN, &daosv1alpha1.DaosContainer{}))).To(BeTrue())
	})
})
