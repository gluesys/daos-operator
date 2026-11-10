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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// a DAOS epoch (HLC); the exact value only has to round-trip through the status.
const testSnapEpoch uint64 = 0x2a9f6d2a1c840000

func snapListOut(entries ...string) string {
	if len(entries) == 0 {
		return `{"response": null, "error": null, "status": 0}`
	}
	out := `{"response": [`
	for i, e := range entries {
		if i > 0 {
			out += ","
		}
		out += e
	}
	return out + `], "error": null, "status": 0}`
}

func snapCreateOut(epoch uint64) *dmg.Result {
	return &dmg.Result{Done: true, Output: fmt.Sprintf(`{"response": %s, "error": null, "status": 0}`, snapEntry(epoch, ""))}
}

// hlcAt is the DAOS epoch of wall time t (inverse of dmg.HLCTime).
func hlcAt(t time.Time) uint64 {
	return uint64(t.UnixNano()-time.Unix(1609459200, 0).UnixNano()) * 16
}

func snapEntry(epoch uint64, name string) string {
	return fmt.Sprintf(`{"epoch": %d, "timestamp": "2026-11-07T21:00:00.000+09:00", "name": %q}`, epoch, name)
}

var _ = Describe("DaosContainerSnapshot Controller", func() {
	ctx := context.Background()
	const sysName, poolName, contName, ns = "ss1", "sp1", "sc1", "default"
	snapNN := types.NamespacedName{Namespace: ns, Name: "snap1"}
	const contUUID = "e4d6c5b3-efd6-4891-9526-3a263925212d"

	BeforeEach(func() {
		sys := &daosv1alpha1.DaosSystem{ObjectMeta: metav1.ObjectMeta{Name: sysName},
			Spec: daosv1alpha1.DaosSystemSpec{Version: "2.8.0", Namespace: "daos-test",
				Images:  daosv1alpha1.ImagesSpec{Server: "s", Agent: "a", Admin: "d", Client: "c"},
				Engines: []daosv1alpha1.EngineSpec{{Targets: 8}}}}
		Expect(k8sClient.Create(ctx, sys)).To(Succeed())
		pool := &daosv1alpha1.DaosPool{ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec: daosv1alpha1.DaosPoolSpec{SystemRef: sysName, Size: resource.MustParse("10Gi")}}
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())
		c := &daosv1alpha1.DaosContainer{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: contName},
			Spec: daosv1alpha1.DaosContainerSpec{PoolRef: poolName}}
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		c.Status.UUID, c.Status.PoolUUID, c.Status.Ready = contUUID, "8a9ca36d-495a-4d50-a0d2-f111b80d5d9d", true
		Expect(k8sClient.Status().Update(ctx, c)).To(Succeed())
	})
	AfterEach(func() {
		s := &daosv1alpha1.DaosContainerSnapshot{}
		if err := k8sClient.Get(ctx, snapNN, s); err == nil {
			s.Finalizers = nil
			_ = k8sClient.Update(ctx, s)
			_ = k8sClient.Delete(ctx, s)
		}
		_ = k8sClient.Delete(ctx, &daosv1alpha1.DaosContainer{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: contName}})
		_ = k8sClient.Delete(ctx, &daosv1alpha1.DaosPool{ObjectMeta: metav1.ObjectMeta{Name: poolName}})
		_ = k8sClient.Delete(ctx, &daosv1alpha1.DaosSystem{ObjectMeta: metav1.ObjectMeta{Name: sysName}})
	})

	newSnap := func(spec daosv1alpha1.DaosContainerSnapshotSpec) {
		Expect(k8sClient.Create(ctx, &daosv1alpha1.DaosContainerSnapshot{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: snapNN.Name}, Spec: spec})).To(Succeed())
	}
	get := func() *daosv1alpha1.DaosContainerSnapshot {
		s := &daosv1alpha1.DaosContainerSnapshot{}
		Expect(k8sClient.Get(ctx, snapNN, s)).To(Succeed())
		return s
	}
	reason := func() string {
		c := meta.FindStatusCondition(get().Status.Conditions, daosv1alpha1.ConditionReady)
		Expect(c).NotTo(BeNil())
		return c.Reason
	}

	It("takes the snapshot once, records its epoch and time, and keeps it when deleted without approval", func() {
		newSnap(daosv1alpha1.DaosContainerSnapshotSpec{ContainerRef: contName})
		events := record.NewFakeRecorder(32)
		f := &fakeDmg{script: map[string]*dmg.Result{"snap:listsnaps": {Done: true, Output: snapListOut()}}}
		rec := func() {
			r := &DaosContainerSnapshotReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Dmg: f, DisableProbeHold: true, Recorder: events}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: snapNN})
			Expect(err).NotTo(HaveOccurred())
		}
		rec() // finalizer
		Expect(get().Finalizers).To(ContainElement(daosv1alpha1.FinalizerSnapshot))

		By("no snapshot yet -> daos cont create-snap, unnamed (DAOS 2.8: named snapshots are DER_NOSYS)")
		rec()
		Expect(get().Status.Operation).To(Equal(opSnapCreate))
		l := f.last("-listsnaps")
		Expect(l.Command[2]).To(HaveSuffix("exec daos -j -G 'daos_server' 'cont' 'list-snaps' 'sp1' 'sc1'"))
		Expect(l.Labels[daosv1alpha1.LabelSnapshot]).To(Equal("default.snap1"))
		Expect(l.Labels).NotTo(HaveKey(daosv1alpha1.LabelContainer), "container Jobs and snapshot Jobs map back separately")
		Expect(l.Sidecar.Image).To(Equal("a"))
		cr := f.last("-createsnap")
		Expect(cr.Command[2]).To(HaveSuffix("'cont' 'create-snap' 'sp1' 'sc1'"))
		Expect(get().Status.CreateRequestedAt).NotTo(BeNil())

		By("create-snap answers the epoch -> status is filled, list-snaps confirms it")
		f.set("snap:createsnap", &dmg.Result{Done: true, Output: fmt.Sprintf(`{"response": %s, "error": null, "status": 0}`, snapEntry(testSnapEpoch, "snap1"))})
		rec()
		s := get()
		Expect(s.Status.Operation).To(BeEmpty())
		Expect(s.Status.Epoch).To(Equal(fmt.Sprint(testSnapEpoch)))
		Expect(s.Status.ContainerUUID).To(Equal(contUUID))
		Expect(s.Status.CreationTime.Time).To(BeTemporally("~", dmg.HLCTime(testSnapEpoch), time.Second), "metav1.Time keeps seconds")
		f.set("snap:listsnaps", &dmg.Result{Done: true, Output: snapListOut(snapEntry(testSnapEpoch, "snap1"))})
		rec()
		Expect(get().Status.Ready).To(BeTrue())
		Expect(f.count("snap:createsnap")).To(Equal(2), "start + one poll; never a second snapshot")

		By("deleting without approval keeps the DAOS snapshot and says how to remove it")
		Expect(k8sClient.Delete(ctx, get())).To(Succeed())
		rec()
		Expect(f.count("snap:destroysnap")).To(BeZero())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, snapNN, &daosv1alpha1.DaosContainerSnapshot{}))).To(BeTrue())
		Expect(drain(events)).To(ContainElement(SatisfyAll(ContainSubstring("SnapshotOrphaned"), ContainSubstring("--epc "+fmt.Sprint(testSnapEpoch)))))
	})

	It("adopts the snapshot a lost create-snap took instead of taking a second one", func() {
		newSnap(daosv1alpha1.DaosContainerSnapshotSpec{ContainerRef: contName})
		requested := time.Now().Add(-10 * time.Minute)
		before, after := hlcAt(requested.Add(-time.Hour)), hlcAt(requested.Add(time.Second))
		held := hlcAt(requested.Add(500 * time.Millisecond)) // taken after too, but another object's

		By("another DaosContainerSnapshot already holds one of the candidates")
		other := &daosv1alpha1.DaosContainerSnapshot{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "other"},
			Spec: daosv1alpha1.DaosContainerSnapshotSpec{ContainerRef: contName}}
		Expect(k8sClient.Create(ctx, other)).To(Succeed())
		other.Status.Epoch, other.Status.ContainerUUID = fmt.Sprint(held), contUUID
		Expect(k8sClient.Status().Update(ctx, other)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, other) })

		By("this one started create-snap and lost the answer (operator restart)")
		f := &fakeDmg{script: map[string]*dmg.Result{"snap:listsnaps": {Done: true, Output: snapListOut(snapEntry(before, ""), snapEntry(held, ""), snapEntry(after, ""))}}}
		r := &DaosContainerSnapshotReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Dmg: f, DisableProbeHold: true}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: snapNN}) // finalizer
		Expect(err).NotTo(HaveOccurred())
		s := get()
		t := metav1.NewTime(requested)
		s.Status.CreateRequestedAt = &t
		Expect(k8sClient.Status().Update(ctx, s)).To(Succeed())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: snapNN})
		Expect(err).NotTo(HaveOccurred())
		Expect(f.count("snap:createsnap")).To(BeZero())
		s = get()
		Expect(s.Status.Epoch).To(Equal(fmt.Sprint(after)), "not the older one, not the one another object holds")
		Expect(s.Status.Ready).To(BeTrue())
	})

	It("does not guess when a lost create-snap could have taken more than one snapshot", func() {
		newSnap(daosv1alpha1.DaosContainerSnapshotSpec{ContainerRef: contName})
		requested := time.Now().Add(-10 * time.Minute)
		first, second := hlcAt(requested.Add(time.Second)), hlcAt(requested.Add(2*time.Second))
		f := &fakeDmg{script: map[string]*dmg.Result{"snap:listsnaps": {Done: true, Output: snapListOut(snapEntry(first, ""), snapEntry(second, ""))}}}
		r := &DaosContainerSnapshotReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Dmg: f, DisableProbeHold: true}
		rec := func() {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: snapNN})
			Expect(err).NotTo(HaveOccurred())
		}
		rec() // finalizer
		s := get()
		t := metav1.NewTime(requested)
		s.Status.CreateRequestedAt = &t
		Expect(k8sClient.Status().Update(ctx, s)).To(Succeed())

		By("two snapshots fit (one taken by hand) -> AdoptAmbiguous, neither adopted, none taken")
		rec()
		Expect(reason()).To(Equal("AdoptAmbiguous"))
		Expect(get().Status.Epoch).To(BeEmpty())
		Expect(f.count("snap:createsnap")).To(BeZero())

		By("the adopt-epoch annotation settles it")
		s = get()
		s.Annotations = map[string]string{daosv1alpha1.AnnotationAdoptEpoch: fmt.Sprint(second)}
		Expect(k8sClient.Update(ctx, s)).To(Succeed())
		rec()
		Expect(get().Status.Epoch).To(Equal(fmt.Sprint(second)))
		Expect(get().Status.Ready).To(BeTrue())
	})

	It("does not adopt while another object of the same container is still taking its snapshot", func() {
		newSnap(daosv1alpha1.DaosContainerSnapshotSpec{ContainerRef: contName})
		requested := time.Now().Add(-10 * time.Minute)
		only := hlcAt(requested.Add(time.Second))
		other := &daosv1alpha1.DaosContainerSnapshot{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "other"},
			Spec: daosv1alpha1.DaosContainerSnapshotSpec{ContainerRef: contName}}
		Expect(k8sClient.Create(ctx, other)).To(Succeed())
		ot := metav1.NewTime(requested)
		other.Status.CreateRequestedAt, other.Status.Operation = &ot, opSnapCreate
		Expect(k8sClient.Status().Update(ctx, other)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, other) })

		f := &fakeDmg{script: map[string]*dmg.Result{"snap:listsnaps": {Done: true, Output: snapListOut(snapEntry(only, ""))}}}
		r := &DaosContainerSnapshotReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Dmg: f, DisableProbeHold: true}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: snapNN}) // finalizer
		Expect(err).NotTo(HaveOccurred())
		s := get()
		t := metav1.NewTime(requested)
		s.Status.CreateRequestedAt = &t
		Expect(k8sClient.Status().Update(ctx, s)).To(Succeed())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: snapNN})
		Expect(err).NotTo(HaveOccurred())
		Expect(reason()).To(Equal("AdoptAmbiguous"))
		Expect(get().Status.Epoch).To(BeEmpty(), "the one snapshot may be the other object's")

		By("once the other records its epoch, nothing is left to adopt and the snapshot is taken")
		other.Status.Epoch, other.Status.ContainerUUID, other.Status.Operation = fmt.Sprint(only), contUUID, ""
		Expect(k8sClient.Status().Update(ctx, other)).To(Succeed())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: snapNN})
		Expect(err).NotTo(HaveOccurred())
		Expect(get().Status.Epoch).To(BeEmpty())
		Expect(f.count("snap:createsnap")).To(Equal(1))
	})

	It("an approved delete during create-snap waits for the epoch and destroys it", func() {
		newSnap(daosv1alpha1.DaosContainerSnapshotSpec{ContainerRef: contName})
		events := record.NewFakeRecorder(32)
		f := &fakeDmg{script: map[string]*dmg.Result{"snap:listsnaps": {Done: true, Output: snapListOut()}}}
		r := &DaosContainerSnapshotReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Dmg: f, DisableProbeHold: true, Recorder: events}
		rec := func() {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: snapNN})
			Expect(err).NotTo(HaveOccurred())
		}
		rec() // finalizer
		rec() // create-snap starts
		Expect(get().Status.Operation).To(Equal(opSnapCreate))
		s := get()
		s.Annotations = map[string]string{daosv1alpha1.AnnotationDestroyApproved: "true"}
		Expect(k8sClient.Update(ctx, s)).To(Succeed())
		Expect(k8sClient.Delete(ctx, get())).To(Succeed())

		rec()
		Expect(reason()).To(Equal("DeletingAfterCreate"))
		Expect(get().Finalizers).To(ContainElement(daosv1alpha1.FinalizerSnapshot), "kept until the create-snap answers")

		f.set("snap:createsnap", snapCreateOut(testSnapEpoch))
		f.set("snap:destroysnap", &dmg.Result{Done: true, Output: `{"response": null, "error": null, "status": 0}`})
		rec()
		Expect(get().Status.Epoch).To(Equal(fmt.Sprint(testSnapEpoch)))
		rec()
		Expect(f.last("-destroysnap").Command[2]).To(HaveSuffix("'--epc' '" + fmt.Sprint(testSnapEpoch) + "'"))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, snapNN, &daosv1alpha1.DaosContainerSnapshot{}))).To(BeTrue())
		Expect(drain(events)).To(ContainElement(ContainSubstring("SnapshotDestroyed")))
	})

	It("destroys the DAOS snapshot by epoch only with destroy-approved", func() {
		newSnap(daosv1alpha1.DaosContainerSnapshotSpec{ContainerRef: contName})
		f := &fakeDmg{script: map[string]*dmg.Result{"snap:listsnaps": {Done: true, Output: snapListOut(snapEntry(testSnapEpoch, ""))}, "snap:createsnap": snapCreateOut(testSnapEpoch)}}
		r := &DaosContainerSnapshotReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Dmg: f, DisableProbeHold: true}
		rec := func() {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: snapNN})
			Expect(err).NotTo(HaveOccurred())
		}
		for i := 0; i < 6 && !get().Status.Ready; i++ { // finalizer, create-snap, poll, list-snaps
			rec()
		}
		s := get()
		Expect(s.Status.Ready).To(BeTrue())
		s.Annotations = map[string]string{daosv1alpha1.AnnotationDestroyApproved: "true"}
		Expect(k8sClient.Update(ctx, s)).To(Succeed())
		Expect(k8sClient.Delete(ctx, get())).To(Succeed())
		rec()
		Expect(get().Status.Operation).To(Equal(opSnapDelete))
		Expect(f.last("-destroysnap").Command[2]).To(HaveSuffix("'cont' 'destroy-snap' 'sp1' 'sc1' '--epc' '" + fmt.Sprint(testSnapEpoch) + "'"))
		f.set("snap:destroysnap", &dmg.Result{Done: true, Output: `{"response": null, "error": null, "status": 0}`})
		rec()
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, snapNN, &daosv1alpha1.DaosContainerSnapshot{}))).To(BeTrue())
	})

	It("does not re-take a snapshot destroyed by hand, nor act on a re-created container", func() {
		newSnap(daosv1alpha1.DaosContainerSnapshotSpec{ContainerRef: contName})
		f := &fakeDmg{script: map[string]*dmg.Result{"snap:listsnaps": {Done: true, Output: snapListOut(snapEntry(testSnapEpoch, ""))}, "snap:createsnap": snapCreateOut(testSnapEpoch)}}
		r := &DaosContainerSnapshotReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Dmg: f, DisableProbeHold: true}
		rec := func() {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: snapNN})
			Expect(err).NotTo(HaveOccurred())
		}
		for i := 0; i < 6 && !get().Status.Ready; i++ { // finalizer, create-snap, poll, list-snaps
			rec()
		}
		Expect(get().Status.Ready).To(BeTrue())

		By("the snapshot disappears from list-snaps -> SnapshotMissing, no create-snap")
		f.set("snap:listsnaps", &dmg.Result{Done: true, Output: snapListOut(snapEntry(testSnapEpoch+16, "snap1"))})
		creates := f.count("snap:createsnap")
		rec()
		Expect(reason()).To(Equal("SnapshotMissing"))
		Expect(f.count("snap:createsnap")).To(Equal(creates), "a snapshot with another epoch is not ours, and a vanished one is not re-taken")

		By("the DaosContainer now points at a different DAOS container -> ContainerReplaced, no Job")
		c := &daosv1alpha1.DaosContainer{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: contName}, c)).To(Succeed())
		c.Status.UUID = "00000000-1111-2222-3333-444444444444"
		Expect(k8sClient.Status().Update(ctx, c)).To(Succeed())
		lists := f.count("snap:listsnaps")
		rec()
		Expect(reason()).To(Equal("ContainerReplaced"))
		Expect(f.count("snap:listsnaps")).To(Equal(lists))
	})

	It("exposes the snapshot as a read-only claim and waits for it to go before destroying", func() {
		newSnap(daosv1alpha1.DaosContainerSnapshotSpec{ContainerRef: contName, Volume: &daosv1alpha1.SnapshotVolumeSpec{ClaimName: "ckpt"}})
		f := &fakeDmg{script: map[string]*dmg.Result{"snap:listsnaps": {Done: true, Output: snapListOut(snapEntry(testSnapEpoch, ""))}, "snap:createsnap": snapCreateOut(testSnapEpoch)}}
		r := &DaosContainerSnapshotReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Dmg: f, DisableProbeHold: true}
		rec := func() {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: snapNN})
			Expect(err).NotTo(HaveOccurred())
		}
		for i := 0; i < 6 && !get().Status.Ready; i++ { // finalizer, create-snap, poll, list-snaps
			rec()
		}
		s := get()
		Expect(s.Status.Ready).To(BeTrue())
		pvName := "daossnap-" + string(s.UID)
		Expect(s.Status.VolumeName).To(Equal(pvName))

		pvc := &corev1.PersistentVolumeClaim{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "ckpt"}, pvc)).To(Succeed())
		Expect(pvc.Spec.AccessModes).To(Equal([]corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany}))
		Expect(pvc.Spec.StorageClassName).To(Equal(ptr.To("")), "no dynamic provisioning")
		Expect(pvc.Spec.VolumeName).To(Equal(pvName))
		Expect(metav1.IsControlledBy(pvc, s)).To(BeTrue())

		pv := &corev1.PersistentVolume{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pvName}, pv)).To(Succeed())
		Expect(pv.Spec.PersistentVolumeReclaimPolicy).To(Equal(corev1.PersistentVolumeReclaimRetain), "CSI DeleteVolume would destroy the container")
		Expect(pv.Spec.ClaimRef.Name).To(Equal("ckpt"))
		Expect(pv.Spec.CSI.ReadOnly).To(BeTrue())
		Expect(pv.Spec.CSI.Driver).To(Equal("daos.csi.gluesys.com"))
		Expect(pv.Spec.CSI.VolumeAttributes).To(SatisfyAll(
			HaveKeyWithValue("snapshotEpoch", fmt.Sprint(testSnapEpoch)),
			HaveKeyWithValue("containerUUID", contUUID),
			HaveKeyWithValue("pool", poolName), HaveKeyWithValue("container", contName),
			HaveKeyWithValue("destroyOnDelete", "false")))

		By("the claim name cannot be changed in place (the PV is reserved for the first claim)")
		s = get()
		s.Spec.Volume.ClaimName = "ckpt2"
		Expect(k8sClient.Update(ctx, s)).To(MatchError(ContainSubstring("claimName is immutable")))

		By("approved delete: PVC and PV are removed first, destroy-snap waits while the PV stays")
		s = get()
		s.Annotations = map[string]string{daosv1alpha1.AnnotationDestroyApproved: "true"}
		Expect(k8sClient.Update(ctx, s)).To(Succeed())
		Expect(k8sClient.Delete(ctx, get())).To(Succeed())
		rec()
		Expect(f.count("snap:destroysnap")).To(BeZero())
		Expect(reason()).To(Equal("VolumeInUse"))

		By("once the PV is gone (pv-protection released), the snapshot is destroyed")
		Eventually(func() bool {
			pv := &corev1.PersistentVolume{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: pvName}, pv); apierrors.IsNotFound(err) {
				return true
			}
			pv.Finalizers = nil // envtest runs no pv-protection controller
			_ = k8sClient.Update(ctx, pv)
			return false
		}).Should(BeTrue())
		f.set("snap:destroysnap", &dmg.Result{Done: true, Output: `{"response": null, "error": null, "status": 0}`})
		rec()
		rec()
		Expect(f.count("snap:destroysnap")).To(BeNumerically(">=", 1))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, snapNN, &daosv1alpha1.DaosContainerSnapshot{}))).To(BeTrue())
	})

	It("does not take over a claim it did not create", func() {
		taken := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "busy"},
			Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}
		Expect(k8sClient.Create(ctx, taken)).To(Succeed())
		DeferCleanup(func() {
			p := &corev1.PersistentVolumeClaim{}
			if k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "busy"}, p) == nil {
				p.Finalizers = nil
				_ = k8sClient.Update(ctx, p)
				_ = k8sClient.Delete(ctx, p)
			}
		})
		newSnap(daosv1alpha1.DaosContainerSnapshotSpec{ContainerRef: contName, Volume: &daosv1alpha1.SnapshotVolumeSpec{ClaimName: "busy"}})
		f := &fakeDmg{script: map[string]*dmg.Result{"snap:listsnaps": {Done: true, Output: snapListOut(snapEntry(testSnapEpoch, ""))}, "snap:createsnap": snapCreateOut(testSnapEpoch)}}
		r := &DaosContainerSnapshotReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Dmg: f, DisableProbeHold: true}
		for i := 0; i < 4; i++ { // finalizer, create-snap, poll, list-snaps
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: snapNN})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(reason()).To(Equal("VolumeConflict"))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: "daossnap-" + string(get().UID)}, &corev1.PersistentVolume{}))).To(BeTrue())
		p := &corev1.PersistentVolumeClaim{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "busy"}, p)).To(Succeed())
		Expect(p.Spec.AccessModes).To(Equal([]corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}), "left as it was")
	})

	It("converts DAOS epochs to wall time like d_hlc2unixnsec", func() {
		// d_hlc2unixnsec(hlc) = hlc / 16 + 1609459200 s
		Expect(dmg.HLCTime(0)).To(Equal(time.Unix(1609459200, 0).UTC()))
		Expect(dmg.HLCTime(16 * uint64(time.Hour))).To(Equal(time.Unix(1609459200+3600, 0).UTC()))
	})
})
