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
	"sort"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	daosv1alpha1 "github.com/gluesys/daos-operator/api/v1alpha1"
)

var _ = Describe("DaosSnapshotPolicy Controller", func() {
	ctx := context.Background()
	const ns, contName, polName = "default", "pc1", "pol1"
	polNN := types.NamespacedName{Namespace: ns, Name: polName}
	// a fixed clock the tests move by hand
	t0 := time.Date(2026, 11, 9, 3, 0, 0, 0, time.UTC)
	var now time.Time
	var events *record.FakeRecorder

	BeforeEach(func() {
		now = t0
		events = record.NewFakeRecorder(64)
		Expect(k8sClient.Create(ctx, &daosv1alpha1.DaosContainer{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: contName},
			Spec: daosv1alpha1.DaosContainerSpec{PoolRef: "nopool"}})).To(Succeed())
	})
	AfterEach(func() {
		var l daosv1alpha1.DaosContainerSnapshotList
		Expect(k8sClient.List(ctx, &l, client.InNamespace(ns))).To(Succeed())
		for i := range l.Items {
			s := &l.Items[i]
			s.Finalizers = nil
			_ = k8sClient.Update(ctx, s)
			_ = k8sClient.Delete(ctx, s)
		}
		_ = k8sClient.Delete(ctx, &daosv1alpha1.DaosSnapshotPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: polName}})
		_ = k8sClient.Delete(ctx, &daosv1alpha1.DaosContainer{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: contName}})
	})

	newPolicy := func(spec daosv1alpha1.DaosSnapshotPolicySpec) {
		spec.ContainerRef = contName
		Expect(k8sClient.Create(ctx, &daosv1alpha1.DaosSnapshotPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: polName}, Spec: spec})).To(Succeed())
	}
	rec := func() reconcile.Result {
		r := &DaosSnapshotPolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: events, Now: func() time.Time { return now }}
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: polNN})
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	policy := func() *daosv1alpha1.DaosSnapshotPolicy {
		p := &daosv1alpha1.DaosSnapshotPolicy{}
		Expect(k8sClient.Get(ctx, polNN, p)).To(Succeed())
		return p
	}
	reason := func() string {
		c := meta.FindStatusCondition(policy().Status.Conditions, daosv1alpha1.ConditionReady)
		Expect(c).NotTo(BeNil())
		return c.Reason
	}
	// snapshots lists the live (not deleting) snapshots of the namespace, oldest name first
	snapshots := func() []daosv1alpha1.DaosContainerSnapshot {
		var l daosv1alpha1.DaosContainerSnapshotList
		Expect(k8sClient.List(ctx, &l, client.InNamespace(ns))).To(Succeed())
		var out []daosv1alpha1.DaosContainerSnapshot
		for _, s := range l.Items {
			if s.DeletionTimestamp.IsZero() {
				out = append(out, s)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out
	}
	names := func(l []daosv1alpha1.DaosContainerSnapshot) []string {
		var n []string
		for _, s := range l {
			n = append(n, s.Name)
		}
		return n
	}
	// take plays the snapshot controller: the snapshot gets its epoch, taken at t.
	// A finalizer keeps a pruned one visible, as the real controller's does.
	take := func(name string, t time.Time) {
		s := &daosv1alpha1.DaosContainerSnapshot{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, s)).To(Succeed())
		s.Finalizers = []string{daosv1alpha1.FinalizerSnapshot}
		Expect(k8sClient.Update(ctx, s)).To(Succeed())
		mt := metav1.NewTime(t)
		s.Status.Epoch, s.Status.CreationTime, s.Status.Ready = "1"+t.Format("150405"), &mt, true
		Expect(k8sClient.Status().Update(ctx, s)).To(Succeed())
	}
	pruned := func(name string) bool {
		s := &daosv1alpha1.DaosContainerSnapshot{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, s)).To(Succeed())
		return !s.DeletionTimestamp.IsZero() && s.Annotations[daosv1alpha1.AnnotationDestroyApproved] == "true"
	}

	It("takes one snapshot per interval, one at a time, and destroys the oldest beyond keep", func() {
		newPolicy(daosv1alpha1.DaosSnapshotPolicySpec{Interval: metav1.Duration{Duration: time.Hour}, Keep: 2})

		By("a snapshot is taken as soon as the policy exists")
		res := rec()
		Expect(names(snapshots())).To(Equal([]string{"pol1-20261109-0300"}))
		s := snapshots()[0]
		Expect(s.Labels[daosv1alpha1.LabelSnapshotPolicy]).To(Equal(polName))
		Expect(s.Spec.ContainerRef).To(Equal(contName))
		Expect(s.Spec.Volume).To(BeNil())
		Expect(res.RequeueAfter).To(Equal(time.Hour))
		Expect(policy().Status.LastScheduleTime.Time).To(BeTemporally("==", t0))
		Expect(reason()).To(Equal("SnapshotPending"))

		By("the next run waits for the interval")
		take("pol1-20261109-0300", t0)
		now = t0.Add(20 * time.Minute)
		res = rec()
		Expect(snapshots()).To(HaveLen(1))
		Expect(res.RequeueAfter).To(Equal(40 * time.Minute))
		Expect(reason()).To(Equal("Scheduled"))
		Expect(policy().Status.Snapshots).To(Equal(int32(1)))
		Expect(policy().Status.LastSnapshot).To(Equal("pol1-20261109-0300"))

		By("a snapshot not taken yet holds back the next one")
		now = t0.Add(time.Hour)
		rec()
		Expect(names(snapshots())).To(Equal([]string{"pol1-20261109-0300", "pol1-20261109-0400"}))
		now = t0.Add(2*time.Hour + time.Minute)
		rec()
		Expect(snapshots()).To(HaveLen(2), "0400 is still being taken: no third one behind it")
		Expect(reason()).To(Equal("SnapshotPending"))
		Expect(meta.FindStatusCondition(policy().Status.Conditions, daosv1alpha1.ConditionReady).Message).To(ContainSubstring("pol1-20261109-0400"))

		By("once taken, the next run goes and nothing is pruned while within keep")
		take("pol1-20261109-0400", t0.Add(time.Hour))
		rec()
		Expect(names(snapshots())).To(Equal([]string{"pol1-20261109-0300", "pol1-20261109-0400", "pol1-20261109-0501"}))
		Expect(pruned("pol1-20261109-0300")).To(BeFalse(), "the third is not taken yet: two taken = keep")

		By("the third is taken: the oldest goes, with destroy approval")
		take("pol1-20261109-0501", t0.Add(2*time.Hour+time.Minute))
		rec()
		Expect(pruned("pol1-20261109-0300")).To(BeTrue())
		Expect(names(snapshots())).To(Equal([]string{"pol1-20261109-0400", "pol1-20261109-0501"}))
		Expect(policy().Status.Snapshots).To(Equal(int32(2)))
		Expect(policy().Status.LastSnapshot).To(Equal("pol1-20261109-0501"))
		Expect(drain(events)).To(ContainElement(SatisfyAll(ContainSubstring("SnapshotPruned"), ContainSubstring("pol1-20261109-0300"), ContainSubstring("keep=2"))))
	})

	It("never counts nor prunes snapshots without its label or exposed as a volume", func() {
		newPolicy(daosv1alpha1.DaosSnapshotPolicySpec{Interval: metav1.Duration{Duration: time.Hour}, Keep: 1})
		mk := func(name string, labels map[string]string, vol *daosv1alpha1.SnapshotVolumeSpec, t time.Time) {
			Expect(k8sClient.Create(ctx, &daosv1alpha1.DaosContainerSnapshot{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
				Spec: daosv1alpha1.DaosContainerSnapshotSpec{ContainerRef: contName, Volume: vol}})).To(Succeed())
			take(name, t)
		}
		mine := map[string]string{daosv1alpha1.LabelSnapshotPolicy: polName}
		mk("by-hand", nil, nil, t0.Add(-5*time.Hour))
		mk("pinned", map[string]string{"x": "y"}, nil, t0.Add(-4*time.Hour)) // label removed by the user
		mk("mounted", mine, &daosv1alpha1.SnapshotVolumeSpec{ClaimName: "ro"}, t0.Add(-3*time.Hour))
		mk("old", mine, nil, t0.Add(-2*time.Hour))
		mk("new", mine, nil, t0.Add(-time.Hour))
		p := policy()
		lt := metav1.NewTime(t0.Add(-time.Hour))
		p.Status.LastScheduleTime = &lt
		Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())

		rec()
		Expect(pruned("old")).To(BeTrue())
		for _, n := range []string{"by-hand", "pinned", "mounted", "new"} {
			Expect(pruned(n)).To(BeFalse(), n)
		}
		Expect(policy().Status.Snapshots).To(Equal(int32(1)))
	})

	It("destroys snapshots older than maxAge but always keeps the newest", func() {
		newPolicy(daosv1alpha1.DaosSnapshotPolicySpec{Interval: metav1.Duration{Duration: time.Hour}, Keep: 10,
			MaxAge: &metav1.Duration{Duration: 3 * time.Hour}, Suspend: true})
		mine := map[string]string{daosv1alpha1.LabelSnapshotPolicy: polName}
		for name, age := range map[string]time.Duration{"a": 10 * time.Hour, "b": 5 * time.Hour, "c": 4 * time.Hour} {
			Expect(k8sClient.Create(ctx, &daosv1alpha1.DaosContainerSnapshot{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: mine},
				Spec: daosv1alpha1.DaosContainerSnapshotSpec{ContainerRef: contName}})).To(Succeed())
			take(name, t0.Add(-age))
		}
		By("suspended: no new snapshot, but pruning goes on")
		res := rec()
		Expect(reason()).To(Equal("Suspended"))
		Expect(res.RequeueAfter).To(BeZero())
		Expect(pruned("a")).To(BeTrue())
		Expect(pruned("b")).To(BeTrue())
		Expect(pruned("c")).To(BeFalse(), "the newest is kept even past maxAge: a stuck policy never ends with none")
		Expect(names(snapshots())).To(Equal([]string{"c"}))
		Expect(policy().Status.NextScheduleTime).To(BeNil())
	})

	It("takes nothing for a container that does not exist", func() {
		Expect(k8sClient.Create(ctx, &daosv1alpha1.DaosSnapshotPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: polName},
			Spec: daosv1alpha1.DaosSnapshotPolicySpec{ContainerRef: "missing", Interval: metav1.Duration{Duration: time.Hour}, Keep: 3}})).To(Succeed())
		rec()
		Expect(snapshots()).To(BeEmpty())
		Expect(reason()).To(Equal("ContainerNotFound"))
	})

	It("does not take a second snapshot when the same run is retried", func() {
		newPolicy(daosv1alpha1.DaosSnapshotPolicySpec{Interval: metav1.Duration{Duration: time.Hour}, Keep: 3})
		rec()
		// the status write was lost: the run is retried within the same minute
		p := policy()
		p.Status = daosv1alpha1.DaosSnapshotPolicyStatus{}
		Expect(k8sClient.Status().Update(ctx, p)).To(Succeed())
		now = t0.Add(30 * time.Second)
		rec()
		Expect(snapshots()).To(HaveLen(1))
		Expect(policy().Status.LastScheduleTime).NotTo(BeNil())
	})

	It("takes its first snapshot at once even when labelled snapshots older than it exist", func() {
		Expect(k8sClient.Create(ctx, &daosv1alpha1.DaosContainerSnapshot{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "seed",
			Labels: map[string]string{daosv1alpha1.LabelSnapshotPolicy: polName}},
			Spec: daosv1alpha1.DaosContainerSnapshotSpec{ContainerRef: contName}})).To(Succeed())
		take("seed", t0.Add(-time.Minute))
		time.Sleep(1100 * time.Millisecond) // creationTimestamp has second resolution
		newPolicy(daosv1alpha1.DaosSnapshotPolicySpec{Interval: metav1.Duration{Duration: time.Hour}, Keep: 1})
		now = time.Now() // the recovery compares real creation times
		rec()
		Expect(snapshots()).To(HaveLen(2), "the seed is not one of the policy's runs: no interval to wait")
	})

	It("rejects an interval under 5m, maxAge under interval and long names", func() {
		bad := func(name string, spec daosv1alpha1.DaosSnapshotPolicySpec, msg string) {
			spec.ContainerRef = contName
			err := k8sClient.Create(ctx, &daosv1alpha1.DaosSnapshotPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: spec})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(msg))
		}
		bad("short", daosv1alpha1.DaosSnapshotPolicySpec{Interval: metav1.Duration{Duration: time.Minute}, Keep: 1}, "at least 5m")
		bad("age", daosv1alpha1.DaosSnapshotPolicySpec{Interval: metav1.Duration{Duration: time.Hour}, Keep: 1, MaxAge: &metav1.Duration{Duration: time.Minute * 30}}, "maxAge must not be shorter")
		bad("a-policy-name-that-is-longer-than-forty-chars", daosv1alpha1.DaosSnapshotPolicySpec{Interval: metav1.Duration{Duration: time.Hour}, Keep: 1}, "at most 40")
		bad("zero", daosv1alpha1.DaosSnapshotPolicySpec{Interval: metav1.Duration{Duration: time.Hour}}, "keep")
	})
})
