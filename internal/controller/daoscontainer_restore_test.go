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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	daosv1alpha1 "github.com/gluesys/daos-operator/api/v1alpha1"
	"github.com/gluesys/daos-operator/internal/dmg"
)

var _ = Describe("DaosContainer restore from a snapshot", func() {
	ctx := context.Background()
	const sysName, poolName, ns = "rs1", "rp1", "default"
	const srcUUID, srcPoolUUID = "11111111-2222-3333-4444-555555555555", "8a9ca36d-495a-4d50-a0d2-f111b80d5d9d"
	dstNN := types.NamespacedName{Namespace: ns, Name: "r1"}
	snapNN := types.NamespacedName{Namespace: ns, Name: "rsnap"}
	restoredOK := &dmg.Result{Done: true, Output: `{"response": {"restored": true}, "error": null, "status": 0}`}

	BeforeEach(func() {
		sys := &daosv1alpha1.DaosSystem{ObjectMeta: metav1.ObjectMeta{Name: sysName},
			Spec: daosv1alpha1.DaosSystemSpec{Version: "2.8.0", Namespace: "daos-test",
				Images:  daosv1alpha1.ImagesSpec{Server: "s", Agent: "a", Admin: "d", Client: "c"},
				Engines: []daosv1alpha1.EngineSpec{{Targets: 8}}}}
		Expect(k8sClient.Create(ctx, sys)).To(Succeed())
		pool := &daosv1alpha1.DaosPool{ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec: daosv1alpha1.DaosPoolSpec{SystemRef: sysName, Size: resource.MustParse("10Gi")}}
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())
		pool.Status.UUID = srcPoolUUID
		Expect(k8sClient.Status().Update(ctx, pool)).To(Succeed())
		src := &daosv1alpha1.DaosContainer{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "rsrc"}, Spec: daosv1alpha1.DaosContainerSpec{PoolRef: poolName}}
		Expect(k8sClient.Create(ctx, src)).To(Succeed())
		src.Status.UUID, src.Status.PoolUUID, src.Status.Ready = srcUUID, srcPoolUUID, true
		Expect(k8sClient.Status().Update(ctx, src)).To(Succeed())
	})
	AfterEach(func() {
		for _, nn := range []types.NamespacedName{snapNN} {
			s := &daosv1alpha1.DaosContainerSnapshot{}
			if k8sClient.Get(ctx, nn, s) == nil {
				s.Finalizers = nil
				_ = k8sClient.Update(ctx, s)
				_ = k8sClient.Delete(ctx, s)
			}
		}
		for _, nn := range []types.NamespacedName{dstNN, {Namespace: ns, Name: "rsrc"}} {
			c := &daosv1alpha1.DaosContainer{}
			if k8sClient.Get(ctx, nn, c) == nil {
				c.Finalizers = nil
				_ = k8sClient.Update(ctx, c)
				_ = k8sClient.Delete(ctx, c)
			}
		}
		_ = k8sClient.Delete(ctx, &daosv1alpha1.DaosPool{ObjectMeta: metav1.ObjectMeta{Name: poolName}})
		_ = k8sClient.Delete(ctx, &daosv1alpha1.DaosSystem{ObjectMeta: metav1.ObjectMeta{Name: sysName}})
	})

	newDst := func(spec daosv1alpha1.DaosContainerSpec) {
		spec.PoolRef = poolName
		Expect(k8sClient.Create(ctx, &daosv1alpha1.DaosContainer{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: dstNN.Name}, Spec: spec})).To(Succeed())
	}
	get := func() *daosv1alpha1.DaosContainer {
		c := &daosv1alpha1.DaosContainer{}
		Expect(k8sClient.Get(ctx, dstNN, c)).To(Succeed())
		return c
	}
	reason := func() string {
		cond := meta.FindStatusCondition(get().Status.Conditions, daosv1alpha1.ConditionReady)
		Expect(cond).NotTo(BeNil())
		return cond.Reason
	}
	// newSnap plays the snapshot controller: a taken snapshot of rsrc, finalizer on
	newSnap := func() {
		s := &daosv1alpha1.DaosContainerSnapshot{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: snapNN.Name, Finalizers: []string{daosv1alpha1.FinalizerSnapshot}},
			Spec: daosv1alpha1.DaosContainerSnapshotSpec{ContainerRef: "rsrc"}}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		now := metav1.Now()
		s.Status = daosv1alpha1.DaosContainerSnapshotStatus{Ready: true, Epoch: "123456", ContainerUUID: srcUUID, PoolUUID: srcPoolUUID, CreationTime: &now}
		Expect(k8sClient.Status().Update(ctx, s)).To(Succeed())
	}

	It("copies the snapshot in before the container is Ready, retries a failed copy, and holds the snapshot's destroy meanwhile", func() {
		newDst(daosv1alpha1.DaosContainerSpec{RestoreFrom: &daosv1alpha1.ContainerRestoreSpec{SnapshotRef: snapNN.Name}})
		events := record.NewFakeRecorder(64)
		f := &fakeDmg{script: map[string]*dmg.Result{"-daos-query": {Done: true, Output: daosContQuery}}}
		rec := func() {
			r := &DaosContainerReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Dmg: f, DisableProbeHold: true, Recorder: events}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: dstNN})
			Expect(err).NotTo(HaveOccurred())
		}
		rec() // finalizer
		Expect(get().Spec.RestoreFrom.DeadlineSeconds).To(Equal(int64(86400)), "CRD default")

		By("the snapshot does not exist yet: the container exists but stays not Ready")
		rec()
		Expect(reason()).To(Equal("RestoreWaiting"))
		Expect(get().Status.Ready).To(BeFalse())
		Expect(f.count("-daos-restore")).To(BeZero())

		By("once it is taken, a restore Job mounts it by UUID and epoch and copies it in")
		newSnap()
		rec()
		Expect(get().Status.Operation).To(Equal(opRestore))
		Expect(reason()).To(Equal("RestoreInProgress"))
		j := f.last("-daos-restore")
		Expect(j).NotTo(BeNil())
		Expect(j.DeadlineSeconds).To(Equal(int64(86400)))
		Expect(j.Privileged).To(BeTrue())
		Expect(j.Sidecar.Image).To(Equal("a"))
		Expect(j.Command[2]).To(SatisfyAll(
			HavePrefix("for i in $(seq 1 60); do [ -S /var/run/daos_agent/daos_agent.sock ]"),
			ContainSubstring(`dfuse --sys-name 'daos_server' --pool '`+srcPoolUUID+`' --container '`+srcUUID+`' --mountpoint '/tmp/daos-restore-src' --snap-epoch '123456' --foreground`),
			ContainSubstring(`dfuse --sys-name 'daos_server' --pool 'rp1' --container 'r1' --mountpoint '/tmp/daos-restore-dst' --foreground`),
			ContainSubstring(`cp -a /tmp/daos-restore-src/. /tmp/daos-restore-dst/`),
			// fs copy resolves a dfuse path to the live container and would skip the snapshot
			Not(ContainSubstring("fs copy")),
		))

		By("an approved destroy of the snapshot waits for the copy")
		sr := &DaosContainerSnapshotReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Dmg: f, DisableProbeHold: true}
		s := &daosv1alpha1.DaosContainerSnapshot{}
		Expect(k8sClient.Get(ctx, snapNN, s)).To(Succeed())
		s.Annotations = map[string]string{daosv1alpha1.AnnotationDestroyApproved: "true"}
		Expect(k8sClient.Update(ctx, s)).To(Succeed())
		Expect(k8sClient.Delete(ctx, s)).To(Succeed())
		_, err := sr.Reconcile(ctx, reconcile.Request{NamespacedName: snapNN})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, snapNN, s)).To(Succeed())
		Expect(meta.FindStatusCondition(s.Status.Conditions, daosv1alpha1.ConditionReady).Reason).To(Equal("RestoreInProgress"))
		Expect(f.count("snap:destroysnap")).To(BeZero())

		By("a failed copy is reported and retried only after a while")
		f.set("-daos-restore", &dmg.Result{Done: true, Output: `{"response": null, "error": "copy failed: cp: error writing: No space left on device", "status": 1}`})
		rec()
		Expect(reason()).To(Equal("RestoreFailed"))
		Expect(get().Status.Operation).To(BeEmpty())
		Expect(drain(events)).To(ContainElement(ContainSubstring("No space left")))
		n := f.count("-daos-restore")
		rec()
		Expect(f.count("-daos-restore")).To(Equal(n), "no immediate retry")
		Expect(reason()).To(Equal("RestoreFailed"))
		c := get()
		past := metav1.NewTime(time.Now().Add(-poolRequeueFailed - time.Second))
		c.Status.RestoreStartedAt = &past
		Expect(k8sClient.Status().Update(ctx, c)).To(Succeed())
		f.set("-daos-restore", &dmg.Result{}) // the new Job is running
		rec()
		Expect(get().Status.Operation).To(Equal(opRestore))

		By("the copy finishes: recorded, then Ready")
		f.set("-daos-restore", restoredOK)
		rec()
		Expect(get().Status.RestoredFrom).To(Equal("rsnap@123456"))
		Expect(drain(events)).To(ContainElement(ContainSubstring("ContainerRestored")))
		rec()
		Expect(get().Status.Ready).To(BeTrue())
		n = f.count("-daos-restore")
		rec()
		Expect(f.count("-daos-restore")).To(Equal(n), "copied once; never again")

		By("the snapshot's destroy goes on")
		_, err = sr.Reconcile(ctx, reconcile.Request{NamespacedName: snapNN})
		Expect(err).NotTo(HaveOccurred())
		Expect(f.count("snap:destroysnap")).To(Equal(1))
	})

	It("refuses to restore into a container that is not POSIX", func() {
		newSnap()
		newDst(daosv1alpha1.DaosContainerSpec{Type: "PYTHON", RestoreFrom: &daosv1alpha1.ContainerRestoreSpec{SnapshotRef: snapNN.Name}})
		f := &fakeDmg{script: map[string]*dmg.Result{"-daos-query": {Done: true, Output: daosContQuery}}}
		r := &DaosContainerReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Dmg: f, DisableProbeHold: true}
		for range 2 {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: dstNN})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(reason()).To(Equal("RestoreUnsupported"))
		Expect(f.count("-daos-restore")).To(BeZero())
	})

	It("does not let restoreFrom be added to an existing container", func() {
		newDst(daosv1alpha1.DaosContainerSpec{})
		c := get()
		c.Spec.RestoreFrom = &daosv1alpha1.ContainerRestoreSpec{SnapshotRef: snapNN.Name}
		err := k8sClient.Update(ctx, c)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("restoreFrom cannot be added or removed"))
	})
})
