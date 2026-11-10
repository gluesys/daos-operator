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
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	daosv1alpha1 "github.com/gluesys/daos-operator/api/v1alpha1"
	"github.com/gluesys/daos-operator/internal/dmg"
)

// the extended edition adds space_used_bytes to `daos -j cont query`
var daosContQueryExt = strings.Replace(daosContQuery, `"health": "HEALTHY",`, `"health": "HEALTHY", "space_used_bytes": 1048576,`, 1)

var _ = Describe("DaosContainer spaceLimit (container quota)", func() {
	ctx := context.Background()
	const sysName, poolName, ns = "qs1", "qp1", "default"
	nn := types.NamespacedName{Namespace: ns, Name: "q1"}

	BeforeEach(func() {
		Expect(k8sClient.Create(ctx, &daosv1alpha1.DaosSystem{ObjectMeta: metav1.ObjectMeta{Name: sysName},
			Spec: daosv1alpha1.DaosSystemSpec{Version: "2.8.0", Namespace: "daos-test",
				Images:  daosv1alpha1.ImagesSpec{Server: "s", Agent: "a", Admin: "d", Client: "c"},
				Engines: []daosv1alpha1.EngineSpec{{Targets: 8}}}})).To(Succeed())
		pool := &daosv1alpha1.DaosPool{ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec: daosv1alpha1.DaosPoolSpec{SystemRef: sysName, Size: resource.MustParse("10Gi")}}
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())
		pool.Status.UUID = "8a9ca36d-495a-4d50-a0d2-f111b80d5d9d"
		Expect(k8sClient.Status().Update(ctx, pool)).To(Succeed())
	})
	AfterEach(func() {
		c := &daosv1alpha1.DaosContainer{}
		if k8sClient.Get(ctx, nn, c) == nil {
			c.Finalizers = nil
			_ = k8sClient.Update(ctx, c)
			_ = k8sClient.Delete(ctx, c)
		}
		_ = k8sClient.Delete(ctx, &daosv1alpha1.DaosPool{ObjectMeta: metav1.ObjectMeta{Name: poolName}})
		_ = k8sClient.Delete(ctx, &daosv1alpha1.DaosSystem{ObjectMeta: metav1.ObjectMeta{Name: sysName}})
	})

	get := func() *daosv1alpha1.DaosContainer {
		c := &daosv1alpha1.DaosContainer{}
		Expect(k8sClient.Get(ctx, nn, c)).To(Succeed())
		return c
	}
	cond := func(t string) *metav1.Condition { return meta.FindStatusCondition(get().Status.Conditions, t) }
	setLimit := func(q *resource.Quantity) {
		c := get()
		c.Spec.SpaceLimit = q
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
	}
	newRec := func(f *fakeDmg, events *record.FakeRecorder) func() {
		return func() {
			r := &DaosContainerReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Dmg: f, DisableProbeHold: true, Recorder: events}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
		}
	}

	It("creates without the limit, sets it once the query shows the extended edition, and changes it in place", func() {
		Expect(k8sClient.Create(ctx, &daosv1alpha1.DaosContainer{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: nn.Name},
			Spec: daosv1alpha1.DaosContainerSpec{PoolRef: poolName, FileOclass: "EC_2P1GX", SpaceLimit: ptr.To(resource.MustParse("10Gi"))}})).To(Succeed())
		events := record.NewFakeRecorder(64)
		f := &fakeDmg{script: map[string]*dmg.Result{"-daos-query": {Done: true, ExitCode: 1, Output: daosContNotFound}}}
		rec := newRec(f, events)
		rec() // finalizer
		rec() // not found -> create
		cr := f.last("-daos-create")
		Expect(cr.Command[2]).NotTo(ContainSubstring("space_limit"), "an upstream create would refuse the property")
		f.set("-daos-create", &dmg.Result{Done: true, Output: daosContQueryExt})
		rec()

		By("the extended edition answers: space_limit (and the EC amplification) before Ready")
		f.set("-daos-query", &dmg.Result{Done: true, Output: daosContQueryExt})
		rec()
		c := get()
		Expect(c.Status.Edition).To(Equal("extended"))
		Expect(*c.Status.SpaceUsedBytes).To(Equal(int64(1048576)))
		Expect(c.Status.Operation).To(Equal(opQuota))
		Expect(c.Status.Ready).To(BeFalse(), "a new container is handed out only with its limit")
		Expect(f.last("-daos-quota").Command[2]).To(HaveSuffix(`'cont' 'set-prop' 'qp1' 'q1' '--properties' 'space_limit:10737418240,space_amp:1500'`))
		f.set("-daos-quota", &dmg.Result{Done: true, Output: dmgOK})
		rec()
		Expect(*get().Status.AppliedSpaceLimit).To(Equal(int64(10 << 30)))
		rec()
		c = get()
		Expect(c.Status.Ready).To(BeTrue())
		Expect(cond(daosv1alpha1.ConditionSpaceLimit).Status).To(Equal(metav1.ConditionTrue))
		Expect(cond(daosv1alpha1.ConditionSpaceLimit).Message).To(ContainSubstring("1048576 used"))
		n := f.count("-daos-quota")
		rec()
		Expect(f.count("-daos-quota")).To(Equal(n), "applied once")

		By("raising it on a container in use keeps it Ready")
		setLimit(ptr.To(resource.MustParse("20Gi")))
		rec()
		Expect(get().Status.Operation).To(Equal(opQuota))
		Expect(get().Status.Ready).To(BeTrue())
		Expect(f.last("-daos-quota").Command[2]).To(ContainSubstring("space_limit:21474836480"))
		rec()
		Expect(*get().Status.AppliedSpaceLimit).To(Equal(int64(20 << 30)))
		Expect(get().Status.Ready).To(BeTrue())
		Expect(cond(daosv1alpha1.ConditionReady).Status).To(Equal(metav1.ConditionTrue))

		By("changed again while set-prop runs: what the Job set is recorded, then the new value is set")
		setLimit(ptr.To(resource.MustParse("30Gi")))
		rec() // starts with 30Gi
		Expect(*get().Status.RequestedSpaceLimit).To(Equal(int64(30 << 30)))
		setLimit(ptr.To(resource.MustParse("40Gi")))
		rec() // the 30Gi Job finishes
		Expect(*get().Status.AppliedSpaceLimit).To(Equal(int64(30<<30)), "not the 40Gi it never set")
		rec()
		Expect(get().Status.Operation).To(Equal(opQuota))
		Expect(f.last("-daos-quota").Command[2]).To(ContainSubstring("space_limit:42949672960"))
		rec()
		Expect(*get().Status.AppliedSpaceLimit).To(Equal(int64(40 << 30)))

		By("removing it sets the container back to unlimited")
		setLimit(nil)
		rec()
		Expect(f.last("-daos-quota").Command[2]).To(HaveSuffix(`'--properties' 'space_limit:0'`))
		rec()
		Expect(*get().Status.AppliedSpaceLimit).To(BeZero())
		rec()
		Expect(cond(daosv1alpha1.ConditionSpaceLimit)).To(BeNil())
		Expect(drain(events)).To(ContainElement(ContainSubstring("SpaceLimitApplied")))
	})

	It("rejects a zero or negative spaceLimit", func() {
		for _, q := range []string{"0", "-1Gi"} {
			err := k8sClient.Create(ctx, &daosv1alpha1.DaosContainer{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: nn.Name},
				Spec: daosv1alpha1.DaosContainerSpec{PoolRef: poolName, SpaceLimit: ptr.To(resource.MustParse(q))}})
			Expect(err).To(MatchError(ContainSubstring("spaceLimit must be positive")), q)
		}
	})

	It("reports the limit as unsupported on upstream DAOS and serves the container anyway", func() {
		Expect(k8sClient.Create(ctx, &daosv1alpha1.DaosContainer{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: nn.Name},
			Spec: daosv1alpha1.DaosContainerSpec{PoolRef: poolName, SpaceLimit: ptr.To(resource.MustParse("10Gi"))}})).To(Succeed())
		events := record.NewFakeRecorder(64)
		f := &fakeDmg{script: map[string]*dmg.Result{"-daos-query": {Done: true, Output: daosContQuery}}}
		rec := newRec(f, events)
		rec()
		rec()
		rec()
		c := get()
		Expect(c.Status.Ready).To(BeTrue())
		Expect(c.Status.Edition).To(Equal("upstream"))
		Expect(c.Status.SpaceUsedBytes).To(BeNil())
		Expect(f.count("-daos-quota")).To(BeZero())
		Expect(cond(daosv1alpha1.ConditionSpaceLimit).Reason).To(Equal("Unsupported"))
		warned := 0
		for _, e := range drain(events) {
			if strings.Contains(e, "SpaceLimitUnsupported") {
				warned++
			}
		}
		Expect(warned).To(Equal(1), "said once, not on every query")
	})

	It("does not retry a failed set-prop until the spec changes, and keeps the container Ready", func() {
		Expect(k8sClient.Create(ctx, &daosv1alpha1.DaosContainer{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: nn.Name},
			Spec: daosv1alpha1.DaosContainerSpec{PoolRef: poolName}})).To(Succeed())
		f := &fakeDmg{script: map[string]*dmg.Result{"-daos-query": {Done: true, Output: daosContQueryExt}}}
		rec := newRec(f, record.NewFakeRecorder(64))
		rec()
		rec()
		Expect(get().Status.Ready).To(BeTrue())
		Expect(cond(daosv1alpha1.ConditionSpaceLimit)).To(BeNil(), "no spaceLimit, nothing to say")

		setLimit(ptr.To(resource.MustParse("1Gi")))
		f.set("-daos-quota", &dmg.Result{Done: true, Output: `{"response": null, "error": "failed to set properties: DER_INVAL(-1003)", "status": -1003}`})
		rec() // start
		rec() // failed
		Expect(cond(daosv1alpha1.ConditionSpaceLimit).Reason).To(Equal("ApplyFailed"))
		Expect(get().Status.Ready).To(BeTrue())
		n := f.count("-daos-quota")
		rec()
		rec()
		Expect(f.count("-daos-quota")).To(Equal(n))

		setLimit(ptr.To(resource.MustParse("2Gi")))
		f.set("-daos-quota", &dmg.Result{Done: true, Output: dmgOK})
		rec()
		rec()
		Expect(*get().Status.AppliedSpaceLimit).To(Equal(int64(2 << 30)))
	})
})
