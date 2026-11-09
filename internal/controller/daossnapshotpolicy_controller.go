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
	"sort"
	"time"

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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	daosv1alpha1 "github.com/gluesys/daos-operator/api/v1alpha1"
)

// DaosSnapshotPolicyReconciler creates a DaosContainerSnapshot every interval
// and deletes, with destroy approval, the policy's oldest snapshots beyond
// spec.keep or older than spec.maxAge. It never runs daos itself: taking and
// destroying is the DaosContainerSnapshot controller's job.
//
// It only acts on snapshots labelled with its name, and only counts and prunes
// those already taken: a snapshot still being taken (or failing to be) holds
// back the next one instead of piling up behind it.
type DaosSnapshotPolicyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// Now is the clock (tests set it); nil means time.Now.
	Now func() time.Time
}

// snapshotNameTime is the timestamp suffix of the snapshots a policy takes
// (UTC, minute resolution: the interval is at least 5m).
const snapshotNameTime = "20060102-1504"

// +kubebuilder:rbac:groups=daos.gluesys.com,resources=daossnapshotpolicies,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=daos.gluesys.com,resources=daossnapshotpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=daos.gluesys.com,resources=daoscontainersnapshots,verbs=get;list;watch;create;update;patch;delete

func (r *DaosSnapshotPolicyReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *DaosSnapshotPolicyReconciler) event(obj runtime.Object, typ, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(obj, typ, reason, msg)
	}
}

// takenAt is when a snapshot was taken (DAOS time), or created if not known.
func takenAt(s *daosv1alpha1.DaosContainerSnapshot) time.Time {
	if s.Status.CreationTime != nil {
		return s.Status.CreationTime.Time
	}
	return s.CreationTimestamp.Time
}

// Reconcile drives one DaosSnapshotPolicy.
func (r *DaosSnapshotPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	p := &daosv1alpha1.DaosSnapshotPolicy{}
	if err := r.Get(ctx, req.NamespacedName, p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !p.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // its snapshots stay as they are
	}
	now := r.now()
	status := *p.Status.DeepCopy()
	status.ObservedGeneration = p.Generation
	setC := func(v metav1.ConditionStatus, reason, msg string) {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: daosv1alpha1.ConditionReady, Status: v, Reason: reason, Message: msg, ObservedGeneration: p.Generation})
	}

	var list daosv1alpha1.DaosContainerSnapshotList
	if err := r.List(ctx, &list, client.InNamespace(p.Namespace), client.MatchingLabels{daosv1alpha1.LabelSnapshotPolicy: p.Name}); err != nil {
		return ctrl.Result{}, err
	}
	var taken []*daosv1alpha1.DaosContainerSnapshot
	var pending *daosv1alpha1.DaosContainerSnapshot
	// the status write of a run was lost: its snapshot still says when it ran
	recoverLast := status.LastScheduleTime == nil
	for i := range list.Items {
		s := &list.Items[i]
		// (only its own runs: a labelled snapshot older than the policy is not one)
		if recoverLast && !s.CreationTimestamp.Before(&p.CreationTimestamp) &&
			(status.LastScheduleTime == nil || status.LastScheduleTime.Before(&s.CreationTimestamp)) {
			t := s.CreationTimestamp
			status.LastScheduleTime = &t
		}
		switch {
		case !s.DeletionTimestamp.IsZero(), s.Spec.ContainerRef != p.Spec.ContainerRef:
			// going away, or not of this policy's container (label copied by hand)
		case s.Status.Epoch == "":
			if pending == nil || s.CreationTimestamp.Before(&pending.CreationTimestamp) {
				pending = s
			}
		case s.Spec.Volume != nil:
			// exposed as a volume: someone reads it, never pruned nor counted
		default:
			taken = append(taken, s)
		}
	}
	sort.Slice(taken, func(i, j int) bool {
		ti, tj := takenAt(taken[i]), takenAt(taken[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return taken[i].Name > taken[j].Name
	})

	// prune: beyond keep, or (all but the newest) older than maxAge
	kept := taken[:0:0]
	for i, s := range taken {
		why := ""
		switch {
		case int32(i) >= p.Spec.Keep:
			why = fmt.Sprintf("more than keep=%d snapshots", p.Spec.Keep)
		case i > 0 && p.Spec.MaxAge != nil && now.Sub(takenAt(s)) > p.Spec.MaxAge.Duration:
			why = fmt.Sprintf("older than maxAge=%s", p.Spec.MaxAge.Duration)
		}
		if why == "" {
			kept = append(kept, s)
			continue
		}
		if err := r.prune(ctx, p, s, why); err != nil {
			return ctrl.Result{}, err
		}
	}
	status.Snapshots = int32(len(kept))
	status.LastSnapshot = ""
	if len(kept) > 0 {
		status.LastSnapshot = kept[0].Name
	}

	// schedule
	next := now
	if status.LastScheduleTime != nil {
		next = status.LastScheduleTime.Add(p.Spec.Interval.Duration)
	}
	requeue := next.Sub(now)
	c := &daosv1alpha1.DaosContainer{}
	cerr := r.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: p.Spec.ContainerRef}, c)
	switch {
	case cerr != nil && !apierrors.IsNotFound(cerr):
		return ctrl.Result{}, cerr
	case p.Spec.Suspend:
		status.NextScheduleTime = nil
		setC(metav1.ConditionTrue, "Suspended", fmt.Sprintf("not taking snapshots (spec.suspend); %d kept", len(kept)))
		return r.updateStatus(ctx, p, status, 0)
	case cerr != nil:
		status.NextScheduleTime = nil
		setC(metav1.ConditionFalse, "ContainerNotFound", "DaosContainer "+p.Spec.ContainerRef+" does not exist in namespace "+p.Namespace)
		return r.updateStatus(ctx, p, status, poolRequeueWait)
	case pending != nil:
		// one at a time: a snapshot that cannot be taken holds back the next
		msg := "snapshot " + pending.Name + " is not taken yet"
		if cond := meta.FindStatusCondition(pending.Status.Conditions, daosv1alpha1.ConditionReady); cond != nil {
			msg += " (" + cond.Reason + ": " + cond.Message + ")"
		}
		t := metav1.NewTime(next)
		status.NextScheduleTime = &t
		setC(metav1.ConditionFalse, "SnapshotPending", msg)
		return r.updateStatus(ctx, p, status, poolRequeueWait)
	case requeue > 0:
		t := metav1.NewTime(next)
		status.NextScheduleTime = &t
		setC(metav1.ConditionTrue, "Scheduled", fmt.Sprintf("%d snapshots kept, next at %s", len(kept), next.UTC().Format(time.RFC3339)))
		return r.updateStatus(ctx, p, status, requeue)
	}

	name := p.Name + "-" + now.UTC().Format(snapshotNameTime)
	s := &daosv1alpha1.DaosContainerSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: name, Labels: map[string]string{daosv1alpha1.LabelSnapshotPolicy: p.Name}},
		Spec:       daosv1alpha1.DaosContainerSnapshotSpec{ContainerRef: p.Spec.ContainerRef},
	}
	if err := r.Create(ctx, s); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
		// a retry of the same run: take it if it is ours, never take a second one
		existing := &daosv1alpha1.DaosContainerSnapshot{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: name}, existing); err != nil {
			return ctrl.Result{}, err
		}
		if existing.Labels[daosv1alpha1.LabelSnapshotPolicy] != p.Name {
			setC(metav1.ConditionFalse, "NameConflict", "DaosContainerSnapshot "+name+" already exists and does not belong to this policy")
			return r.updateStatus(ctx, p, status, poolRequeueWait)
		}
	} else {
		r.event(p, corev1.EventTypeNormal, "SnapshotScheduled", fmt.Sprintf("DaosContainerSnapshot %s of container %s created", name, p.Spec.ContainerRef))
	}
	t := metav1.NewTime(now)
	status.LastScheduleTime = &t
	nt := metav1.NewTime(now.Add(p.Spec.Interval.Duration))
	status.NextScheduleTime = &nt
	setC(metav1.ConditionFalse, "SnapshotPending", "snapshot "+name+" is being taken")
	logf.FromContext(ctx).Info("snapshot scheduled", "policy", p.Name, "namespace", p.Namespace, "snapshot", name)
	return r.updateStatus(ctx, p, status, p.Spec.Interval.Duration)
}

// prune approves the destroy of one of the policy's snapshots and deletes it;
// the DaosContainerSnapshot controller then runs destroy-snap.
func (r *DaosSnapshotPolicyReconciler) prune(ctx context.Context, p *daosv1alpha1.DaosSnapshotPolicy, s *daosv1alpha1.DaosContainerSnapshot, why string) error {
	if s.Annotations[daosv1alpha1.AnnotationDestroyApproved] != "true" {
		orig := s.DeepCopy()
		if s.Annotations == nil {
			s.Annotations = map[string]string{}
		}
		s.Annotations[daosv1alpha1.AnnotationDestroyApproved] = "true"
		if err := r.Patch(ctx, s, client.MergeFrom(orig)); err != nil {
			return client.IgnoreNotFound(err)
		}
	}
	if err := r.Delete(ctx, s); client.IgnoreNotFound(err) != nil {
		return err
	}
	r.event(p, corev1.EventTypeNormal, "SnapshotPruned", fmt.Sprintf("DaosContainerSnapshot %s (epoch %s) deleted with destroy approval: %s", s.Name, s.Status.Epoch, why))
	return nil
}

func (r *DaosSnapshotPolicyReconciler) updateStatus(ctx context.Context, p *daosv1alpha1.DaosSnapshotPolicy, st daosv1alpha1.DaosSnapshotPolicyStatus, requeue time.Duration) (ctrl.Result, error) {
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &daosv1alpha1.DaosSnapshotPolicy{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: p.Name}, latest); err != nil {
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

// SetupWithManager watches the policy and its labelled snapshots, so a snapshot
// becoming Ready (or going away) re-runs the pruning.
func (r *DaosSnapshotPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	snapToPolicy := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
		name := o.GetLabels()[daosv1alpha1.LabelSnapshotPolicy]
		if name == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: name}}}
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&daosv1alpha1.DaosSnapshotPolicy{}).
		Watches(&daosv1alpha1.DaosContainerSnapshot{}, snapToPolicy).
		Named("daossnapshotpolicy").
		Complete(r)
}
