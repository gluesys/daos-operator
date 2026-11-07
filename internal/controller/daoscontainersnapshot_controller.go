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
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	daosv1alpha1 "github.com/gluesys/daos-operator/api/v1alpha1"
	"github.com/gluesys/daos-operator/internal/dmg"
)

// DaosContainerSnapshotReconciler takes one DAOS snapshot per
// DaosContainerSnapshot and keeps its status in line with `daos cont list-snaps`.
//
// Same rules as containers: a snapshot that is missing is taken once (an existing
// one of the same name is adopted, so a lost Job or an operator restart never
// takes a second), the operator never re-takes one that vanished, and deleting
// the object destroys the DAOS snapshot only with
// daos.gluesys.com/destroy-approved=true. A snapshot pins its container UUID: if
// the DaosContainer is re-created, the snapshot belongs to the old one and is
// reported, not re-taken on the new one.
type DaosContainerSnapshotReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	Dmg              dmg.Runner
	Recorder         record.EventRecorder
	DisableProbeHold bool
}

const (
	opSnapList   = "listsnaps"
	opSnapCreate = "createsnap"
	opSnapDelete = "destroysnap"
	// snapRequeueIdle is the re-check interval of a ready snapshot. A snapshot does
	// not change once taken; the check only notices one destroyed by hand.
	snapRequeueIdle = 10 * time.Minute
)

// +kubebuilder:rbac:groups=daos.gluesys.com,resources=daoscontainersnapshots,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=daos.gluesys.com,resources=daoscontainersnapshots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=daos.gluesys.com,resources=daoscontainersnapshots/finalizers,verbs=update

func snapName(s *daosv1alpha1.DaosContainerSnapshot) string {
	if s.Spec.SnapshotName != "" {
		return s.Spec.SnapshotName
	}
	return s.Name
}

func snapKey(s *daosv1alpha1.DaosContainerSnapshot) string {
	return "snap/" + s.Namespace + "/" + s.Name
}

func snapJobLabel(s *daosv1alpha1.DaosContainerSnapshot) string { return s.Namespace + "." + s.Name }

// opSpec runs `daos cont <op>` the way the container controller does (same image,
// agent sidecar, certs), under this snapshot's Job name and label.
func (r *DaosContainerSnapshotReconciler) opSpec(s *daosv1alpha1.DaosContainerSnapshot, c *daosv1alpha1.DaosContainer, pool *daosv1alpha1.DaosPool, sys *daosv1alpha1.DaosSystem, ns, op string) dmg.RunSpec {
	spec := (&DaosContainerReconciler{}).opSpec(c, pool, sys, ns, "query")
	spec.Name = jobName("snap", s.Namespace+"/"+s.Name, op)
	spec.Labels = map[string]string{
		daosv1alpha1.LabelSystem: sys.Name, daosv1alpha1.LabelPool: pool.Name, daosv1alpha1.LabelSnapshot: snapJobLabel(s),
	}
	pl, cl := poolLabel(pool), contLabel(c)
	switch op {
	case opSnapList:
		spec.Command = clientCommand(systemName(sys), nil, "cont", "list-snaps", pl, cl)
	case opSnapCreate:
		spec.Command = clientCommand(systemName(sys), nil, "cont", "create-snap", pl, cl, "--snap", snapName(s))
	case opSnapDelete:
		spec.Command = clientCommand(systemName(sys), nil, "cont", "destroy-snap", pl, cl, "--epc", s.Status.Epoch)
	}
	return spec
}

func (r *DaosContainerSnapshotReconciler) event(obj runtime.Object, typ, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(obj, typ, reason, msg)
	}
}

// release drops the finalizer without touching DAOS and says why.
func (r *DaosContainerSnapshotReconciler) release(ctx context.Context, s *daosv1alpha1.DaosContainerSnapshot, typ, reason, msg string) (ctrl.Result, error) {
	r.event(s, typ, reason, msg)
	if controllerutil.ContainsFinalizer(s, daosv1alpha1.FinalizerSnapshot) {
		controllerutil.RemoveFinalizer(s, daosv1alpha1.FinalizerSnapshot)
		return ctrl.Result{}, r.Update(ctx, s)
	}
	return ctrl.Result{}, nil
}

// Reconcile drives one DaosContainerSnapshot.
func (r *DaosContainerSnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	s := &daosv1alpha1.DaosContainerSnapshot{}
	if err := r.Get(ctx, req.NamespacedName, s); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	status := *s.Status.DeepCopy()
	setC := func(v metav1.ConditionStatus, reason, msg string) {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: daosv1alpha1.ConditionReady, Status: v, Reason: reason, Message: msg, ObservedGeneration: s.Generation})
		status.Ready = v == metav1.ConditionTrue
	}
	if r.Dmg == nil {
		setC(metav1.ConditionUnknown, "NoRunner", "operator started without a dmg runner")
		return r.updateStatus(ctx, s, status, 0)
	}
	deleting := !s.DeletionTimestamp.IsZero()
	kept := func(why string) string {
		if status.Epoch == "" {
			return why
		}
		return fmt.Sprintf("%s; DAOS snapshot %s (epoch %s) of container uuid %s is kept, remove it by hand with `daos cont destroy-snap <pool> <container> --epc %s` if that is intended",
			why, snapName(s), status.Epoch, status.ContainerUUID, status.Epoch)
	}

	c := &daosv1alpha1.DaosContainer{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: s.Spec.ContainerRef}, c); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if deleting {
			return r.release(ctx, s, corev1.EventTypeWarning, "DestroySkipped", kept("DaosContainer "+s.Spec.ContainerRef+" does not exist"))
		}
		setC(metav1.ConditionFalse, "ContainerNotFound", "DaosContainer "+s.Spec.ContainerRef+" does not exist in namespace "+s.Namespace)
		return r.updateStatus(ctx, s, status, poolRequeueWait)
	}
	pool := &daosv1alpha1.DaosPool{}
	if err := r.Get(ctx, types.NamespacedName{Name: c.Spec.PoolRef}, pool); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if deleting {
			return r.release(ctx, s, corev1.EventTypeWarning, "DestroySkipped", kept("DaosPool "+c.Spec.PoolRef+" does not exist"))
		}
		setC(metav1.ConditionFalse, "PoolNotFound", "DaosPool "+c.Spec.PoolRef+" does not exist")
		return r.updateStatus(ctx, s, status, poolRequeueWait)
	}
	sys, ns, err := systemFor(ctx, r.Client, pool.Spec.SystemRef)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if deleting {
			return r.release(ctx, s, corev1.EventTypeWarning, "DestroySkipped", kept("DaosSystem "+pool.Spec.SystemRef+" does not exist"))
		}
		setC(metav1.ConditionFalse, "SystemNotFound", "DaosSystem "+pool.Spec.SystemRef+" does not exist")
		return r.updateStatus(ctx, s, status, poolRequeueWait)
	}

	if !deleting && !controllerutil.ContainsFinalizer(s, daosv1alpha1.FinalizerSnapshot) {
		controllerutil.AddFinalizer(s, daosv1alpha1.FinalizerSnapshot)
		return ctrl.Result{Requeue: true}, r.Update(ctx, s)
	}
	// The snapshot was taken of a container that is no longer the one this name
	// points at: never act on the new container in its name.
	replaced := status.ContainerUUID != "" && c.Status.UUID != "" && status.ContainerUUID != c.Status.UUID
	if deleting {
		if replaced {
			return r.release(ctx, s, corev1.EventTypeWarning, "DestroySkipped", kept("DaosContainer "+c.Name+" was re-created"))
		}
		return r.finalize(ctx, s, c, pool, sys, ns, status, kept)
	}
	if replaced {
		setC(metav1.ConditionFalse, "ContainerReplaced", fmt.Sprintf("snapshot %s was taken of container uuid %s; DaosContainer %s is now uuid %s. Delete this DaosContainerSnapshot (and destroy the old snapshot by hand if the old container still exists)",
			snapName(s), status.ContainerUUID, c.Name, c.Status.UUID))
		return r.updateStatus(ctx, s, status, snapRequeueIdle)
	}
	if c.Status.UUID == "" || !c.Status.Ready {
		setC(metav1.ConditionFalse, "ContainerNotReady", "DaosContainer "+c.Name+" is not ready")
		return r.updateStatus(ctx, s, status, poolRequeueWait)
	}
	if status.Operation != "" {
		return r.pollOp(ctx, s, c, pool, sys, ns, status)
	}
	if !r.DisableProbeHold {
		if wait := holdFor(snapKey(s)); wait > 0 {
			return ctrl.Result{RequeueAfter: wait}, nil
		}
	}

	res, err := r.Dmg.Run(ctx, r.opSpec(s, c, pool, sys, ns, opSnapList))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("list-snaps job: %w", err)
	}
	if !res.Done {
		if meta.FindStatusCondition(status.Conditions, daosv1alpha1.ConditionReady) == nil {
			setC(metav1.ConditionUnknown, "Probing", "daos cont list-snaps is running")
		}
		return r.updateStatus(ctx, s, status, poolRequeueOp)
	}
	if !r.DisableProbeHold {
		setHold(snapKey(s), poolRequeueWait)
	}
	env, perr := parseResult(res)
	if perr == nil && env.Error != nil {
		perr = fmt.Errorf("%s", *env.Error)
	}
	if perr != nil {
		setC(metav1.ConditionUnknown, "ProbeFailed", "daos cont list-snaps: "+perr.Error())
		return r.updateStatus(ctx, s, status, poolRequeueWait)
	}
	snaps, err := dmg.Snapshots(env)
	if err != nil {
		setC(metav1.ConditionUnknown, "ProbeFailed", err.Error())
		return r.updateStatus(ctx, s, status, poolRequeueWait)
	}
	now := metav1.NewTime(res.QueriedAt(time.Now()))
	status.LastQueryTime = &now
	found := findSnapshot(snaps, snapName(s), status.Epoch)
	if found == nil {
		if status.Epoch != "" {
			setC(metav1.ConditionFalse, "SnapshotMissing", fmt.Sprintf("snapshot %s (epoch %s) no longer exists in container %s; it is not re-taken -- delete this object, or re-create it to take a new one",
				snapName(s), status.Epoch, contLabel(c)))
			return r.updateStatus(ctx, s, status, snapRequeueIdle)
		}
		if cond := meta.FindStatusCondition(status.Conditions, daosv1alpha1.ConditionReady); cond != nil && cond.Reason == "CreatesnapFailed" && status.ObservedGeneration == s.Generation {
			return r.updateStatus(ctx, s, status, poolRequeueFailed)
		}
		return r.startOp(ctx, s, c, pool, sys, ns, status, opSnapCreate)
	}
	adopted := status.Epoch == ""
	r.record(&status, found, c)
	if adopted {
		r.event(s, corev1.EventTypeNormal, "SnapshotAdopted", fmt.Sprintf("existing snapshot %s (epoch %s) of container %s adopted", snapName(s), status.Epoch, contLabel(c)))
	}
	setC(metav1.ConditionTrue, "Ready", fmt.Sprintf("snapshot %s of container %s, epoch %s, taken %s", snapName(s), contLabel(c), status.Epoch, status.CreationTime.UTC().Format(time.RFC3339)))
	logf.FromContext(ctx).Info("snapshot", "name", s.Name, "namespace", s.Namespace, "epoch", status.Epoch)
	return r.updateStatus(ctx, s, status, snapRequeueIdle)
}

// findSnapshot picks this object's snapshot: by epoch once known (a name can be
// given to more than one snapshot by hand), by name before that.
func findSnapshot(snaps []dmg.SnapshotInfo, name, epoch string) *dmg.SnapshotInfo {
	for i := range snaps {
		if epoch != "" {
			if strconv.FormatUint(snaps[i].Epoch, 10) == epoch {
				return &snaps[i]
			}
			continue
		}
		if snaps[i].Name == name {
			return &snaps[i]
		}
	}
	return nil
}

func (r *DaosContainerSnapshotReconciler) record(status *daosv1alpha1.DaosContainerSnapshotStatus, snap *dmg.SnapshotInfo, c *daosv1alpha1.DaosContainer) {
	status.Epoch = strconv.FormatUint(snap.Epoch, 10)
	t := metav1.NewTime(dmg.HLCTime(snap.Epoch))
	status.CreationTime = &t
	status.ContainerUUID, status.PoolUUID = c.Status.UUID, c.Status.PoolUUID
}

func (r *DaosContainerSnapshotReconciler) startOp(ctx context.Context, s *daosv1alpha1.DaosContainerSnapshot, c *daosv1alpha1.DaosContainer, pool *daosv1alpha1.DaosPool, sys *daosv1alpha1.DaosSystem, ns string, status daosv1alpha1.DaosContainerSnapshotStatus, op string) (ctrl.Result, error) {
	if _, err := r.Dmg.Run(ctx, r.opSpec(s, c, pool, sys, ns, op)); err != nil {
		return ctrl.Result{}, fmt.Errorf("start %s: %w", op, err)
	}
	status.Operation = op
	status.Ready = false
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: daosv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "Creating", Message: "daos cont create-snap is running", ObservedGeneration: s.Generation})
	r.event(s, corev1.EventTypeNormal, "SnapshotCreating", fmt.Sprintf("taking snapshot %s of container %s", snapName(s), contLabel(c)))
	return r.updateStatus(ctx, s, status, poolRequeueOp)
}

func (r *DaosContainerSnapshotReconciler) pollOp(ctx context.Context, s *daosv1alpha1.DaosContainerSnapshot, c *daosv1alpha1.DaosContainer, pool *daosv1alpha1.DaosPool, sys *daosv1alpha1.DaosSystem, ns string, status daosv1alpha1.DaosContainerSnapshotStatus) (ctrl.Result, error) {
	op := status.Operation
	res, err := r.Dmg.Run(ctx, r.opSpec(s, c, pool, sys, ns, op))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("poll %s: %w", op, err)
	}
	if !res.Done {
		return ctrl.Result{RequeueAfter: poolRequeueOp}, nil
	}
	status.Operation = ""
	status.ObservedGeneration = s.Generation
	env, perr := parseResult(res)
	if perr == nil && env.Error != nil {
		perr = fmt.Errorf("%s", *env.Error)
	}
	var snap *dmg.SnapshotInfo
	if perr == nil {
		snap, perr = dmg.SnapshotCreated(env)
	}
	if perr != nil {
		// The Job may have taken the snapshot and still failed to report it; the next
		// list-snaps adopts it by name, so a retry never takes a second one.
		msg := "daos cont create-snap: " + perr.Error()
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: daosv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "CreatesnapFailed", Message: msg, ObservedGeneration: s.Generation})
		status.Ready = false
		r.event(s, corev1.EventTypeWarning, "SnapshotFailed", msg)
		return r.updateStatus(ctx, s, status, poolRequeueFailed)
	}
	r.record(&status, snap, c)
	r.event(s, corev1.EventTypeNormal, "SnapshotCreated", fmt.Sprintf("snapshot %s of container %s taken, epoch %s", snapName(s), contLabel(c), status.Epoch))
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: daosv1alpha1.ConditionReady, Status: metav1.ConditionUnknown, Reason: "Querying", Message: "daos cont create-snap succeeded; refreshing", ObservedGeneration: s.Generation})
	clearHold(snapKey(s))
	return r.updateStatus(ctx, s, status, time.Second)
}

func (r *DaosContainerSnapshotReconciler) finalize(ctx context.Context, s *daosv1alpha1.DaosContainerSnapshot, c *daosv1alpha1.DaosContainer, pool *daosv1alpha1.DaosPool, sys *daosv1alpha1.DaosSystem, ns string, status daosv1alpha1.DaosContainerSnapshotStatus, kept func(string) string) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(s, daosv1alpha1.FinalizerSnapshot) {
		return ctrl.Result{}, nil
	}
	if status.Epoch == "" {
		// nothing was ever taken (or it is still being taken: a create-snap Job
		// left running finishes on its own and the snapshot is then unmanaged)
		msg := "no snapshot was recorded for this object"
		if status.Operation == opSnapCreate {
			msg = "deleted while create-snap was running; if it completes, snapshot " + snapName(s) + " is left unmanaged in container " + contLabel(c)
		}
		return r.release(ctx, s, corev1.EventTypeNormal, "SnapshotForgotten", msg)
	}
	if s.Annotations[daosv1alpha1.AnnotationDestroyApproved] != "true" {
		return r.release(ctx, s, corev1.EventTypeWarning, "SnapshotOrphaned", kept("DaosContainerSnapshot deleted without "+daosv1alpha1.AnnotationDestroyApproved+"=true"))
	}
	if wait := holdFor(snapKey(s)); wait > 0 && !r.DisableProbeHold {
		return ctrl.Result{RequeueAfter: wait}, nil
	}
	res, err := r.Dmg.Run(ctx, r.opSpec(s, c, pool, sys, ns, opSnapDelete))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("destroy-snap job: %w", err)
	}
	if !res.Done {
		if status.Operation != opSnapDelete {
			status.Operation = opSnapDelete
			meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: daosv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "Destroying", Message: "daos cont destroy-snap is running", ObservedGeneration: s.Generation})
			return r.updateStatus(ctx, s, status, poolRequeueOp)
		}
		return ctrl.Result{RequeueAfter: poolRequeueOp}, nil
	}
	env, perr := parseResult(res)
	if perr == nil && env.Error != nil && dmg.Classify(*env.Error) != dmg.ErrNotFound {
		perr = fmt.Errorf("%s", *env.Error)
	}
	if perr != nil {
		status.Operation = ""
		msg := "daos cont destroy-snap: " + perr.Error() + "; the DaosContainerSnapshot stays until destroy succeeds or the annotation is removed (then the snapshot is kept)"
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: daosv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "DestroyFailed", Message: msg, ObservedGeneration: s.Generation})
		r.event(s, corev1.EventTypeWarning, "DestroyFailed", msg)
		setHold(snapKey(s), poolRequeueWait)
		return r.updateStatus(ctx, s, status, poolRequeueWait)
	}
	r.event(s, corev1.EventTypeNormal, "SnapshotDestroyed", fmt.Sprintf("DAOS snapshot %s (epoch %s) of container %s destroyed on request", snapName(s), status.Epoch, contLabel(c)))
	controllerutil.RemoveFinalizer(s, daosv1alpha1.FinalizerSnapshot)
	return ctrl.Result{}, r.Update(ctx, s)
}

func (r *DaosContainerSnapshotReconciler) updateStatus(ctx context.Context, s *daosv1alpha1.DaosContainerSnapshot, st daosv1alpha1.DaosContainerSnapshotStatus, requeue time.Duration) (ctrl.Result, error) {
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &daosv1alpha1.DaosContainerSnapshot{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: s.Name}, latest); err != nil {
			return err
		}
		latest.Status = st
		return r.Status().Update(ctx, latest)
	})
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if requeue > 0 {
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager. Jobs are owned by
// the DaosPool, so they are mapped back to the snapshot through the label.
func (r *DaosContainerSnapshotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	jobToSnapshot := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
		v := o.GetLabels()[daosv1alpha1.LabelSnapshot]
		i := strings.IndexByte(v, '.')
		if i <= 0 {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: v[:i], Name: v[i+1:]}}}
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&daosv1alpha1.DaosContainerSnapshot{}).
		Watches(&batchv1.Job{}, jobToSnapshot).
		Named("daoscontainersnapshot").
		Complete(r)
}
