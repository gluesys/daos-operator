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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
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
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	daosv1alpha1 "gitlab.gluesys.com/exastor/daos-operator/api/v1alpha1"
	"gitlab.gluesys.com/exastor/daos-operator/internal/dmg"
)

// DaosPoolReconciler drives `dmg pool ...` through Jobs (#11).
//
// Rules: status mirrors `dmg pool query`; the operator creates a pool that does
// not exist, extends it to newly listed ranks, and rewrites the ACL when
// spec.acl changes. It never shrinks, never re-creates a pool that vanished
// (that is data loss to be looked at by a human), and destroys a pool on
// DaosPool deletion only when daos.gluesys.com/destroy-approved=true is set --
// otherwise the DAOS pool is kept and an Event says so.
type DaosPoolReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	Dmg              dmg.Runner
	Recorder         record.EventRecorder
	DisableProbeHold bool
}

const (
	poolRequeueIdle   = 60 * time.Second
	poolRequeueOp     = 10 * time.Second
	poolRequeueWait   = 30 * time.Second
	poolRequeueFailed = 5 * time.Minute
	aclFilePath       = "/tmp/daos-acl"
	controlConfigPath = "/etc/daos/daos_control.yml"

	opCreate  = "create"
	opExtend  = "extend"
	opACL     = "acl"
	opDestroy = "destroy"
)

// +kubebuilder:rbac:groups=daos.gluesys.com,resources=daospools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=daos.gluesys.com,resources=daospools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=daos.gluesys.com,resources=daospools/finalizers,verbs=update

func poolLabel(pool *daosv1alpha1.DaosPool) string { return pool.Name }

func poolKey(pool *daosv1alpha1.DaosPool) string { return "pool/" + pool.Name }

func aclHash(entries []string) string {
	if len(entries) == 0 {
		return ""
	}
	h := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	return hex.EncodeToString(h[:8])
}

func ranksCSV(ranks []int32) string {
	s := make([]string, 0, len(ranks))
	for _, r := range ranks {
		s = append(s, strconv.Itoa(int(r)))
	}
	return strings.Join(s, ",")
}

// missingRanks returns want minus have, sorted.
func missingRanks(want, have []int32) []int32 {
	seen := map[int32]bool{}
	for _, r := range have {
		seen[r] = true
	}
	var out []int32
	for _, r := range want {
		if !seen[r] {
			out = append(out, r)
			seen[r] = true
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// systemFor loads the DaosSystem a pool/container belongs to and its namespace.
func systemFor(ctx context.Context, c client.Client, name string) (*daosv1alpha1.DaosSystem, string, error) {
	sys := &daosv1alpha1.DaosSystem{}
	if err := c.Get(ctx, types.NamespacedName{Name: name}, sys); err != nil {
		return nil, "", err
	}
	ns := sys.Spec.Namespace
	if ns == "" {
		ns = "daos-system"
	}
	return sys, ns, nil
}

func (r *DaosPoolReconciler) event(obj runtime.Object, typ, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(obj, typ, reason, msg)
	}
}

// opSpec builds the Job for one operation deterministically from the spec, so a
// poll after an operator restart finds the same Job name.
func (r *DaosPoolReconciler) opSpec(pool *daosv1alpha1.DaosPool, sys *daosv1alpha1.DaosSystem, ns, op string) dmg.RunSpec {
	label := poolLabel(pool)
	spec := dmg.RunSpec{
		Owner: pool, Namespace: ns, Name: jobName("pool", pool.Name, "dmg-"+op),
		Image: sys.Spec.Images.Admin, ControlConfigMap: sys.Name + "-control",
		NodeSelector: sys.Spec.NodeSelector, Tolerations: sys.Spec.Tolerations,
		Labels:      map[string]string{daosv1alpha1.LabelSystem: sys.Name, daosv1alpha1.LabelPool: pool.Name},
		CertsSecret: adminCertsSecret(sys), CertsFiles: adminCertFiles(),
	}
	base := []string{"dmg", "-o", controlConfigPath, "-j"}
	switch op {
	case "query":
		spec.Args = []string{"pool", "query", "--show-enabled", label}
	case opCreate:
		rdFac := int32(2)
		if pool.Spec.RedundancyFactor != nil {
			rdFac = *pool.Spec.RedundancyFactor
		}
		props := []string{fmt.Sprintf("rd_fac:%d", rdFac)}
		keys := make([]string, 0, len(pool.Spec.Properties))
		for k := range pool.Spec.Properties {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			props = append(props, k+":"+pool.Spec.Properties[k])
		}
		// dmg parses sizes with humanize; plain bytes avoid Gi/GiB ambiguity
		args := []string{"pool", "create", "-z", fmt.Sprintf("%dB", pool.Spec.Size.Value()), "-P", strings.Join(props, ",")}
		if len(pool.Spec.Ranks) > 0 {
			args = append(args, "-r", ranksCSV(pool.Spec.Ranks))
		}
		if len(pool.Spec.ACL) > 0 {
			args = append(args, "-a", aclFilePath)
		}
		args = append(args, label)
		spec.Command = dmg.WithACLFile(pool.Spec.ACL, aclFilePath, append(base, args...))
	case opExtend:
		spec.Args = []string{"pool", "extend", "--ranks=" + ranksCSV(missingRanks(pool.Spec.Ranks, pool.Status.EnabledRanks)), label}
	case opACL:
		spec.Command = dmg.WithACLFile(pool.Spec.ACL, aclFilePath, append(base, "pool", "overwrite-acl", "-a", aclFilePath, label))
	case opDestroy:
		spec.Args = []string{"pool", "destroy", "--recursive", label}
	}
	return spec
}

// Reconcile drives one DaosPool.
func (r *DaosPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	pool := &daosv1alpha1.DaosPool{}
	if err := r.Get(ctx, req.NamespacedName, pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	status := *pool.Status.DeepCopy()
	setC := func(v metav1.ConditionStatus, reason, msg string) {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: daosv1alpha1.ConditionReady, Status: v, Reason: reason, Message: msg, ObservedGeneration: pool.Generation})
	}
	if r.Dmg == nil {
		setC(metav1.ConditionUnknown, "NoRunner", "operator started without a dmg runner")
		return r.updateStatus(ctx, pool, status, 0)
	}
	sys, ns, err := systemFor(ctx, r.Client, pool.Spec.SystemRef)
	if err != nil {
		if apierrors.IsNotFound(err) {
			setC(metav1.ConditionFalse, "SystemNotFound", "DaosSystem "+pool.Spec.SystemRef+" does not exist")
			return r.updateStatus(ctx, pool, status, poolRequeueWait)
		}
		return ctrl.Result{}, err
	}

	if pool.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(pool, daosv1alpha1.FinalizerPool) {
		controllerutil.AddFinalizer(pool, daosv1alpha1.FinalizerPool)
		return ctrl.Result{Requeue: true}, r.Update(ctx, pool)
	}
	if !pool.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, pool, sys, ns, status)
	}
	if !sys.Status.Formatted {
		setC(metav1.ConditionFalse, "SystemNotFormatted", "DaosSystem "+sys.Name+" is not formatted yet")
		return r.updateStatus(ctx, pool, status, poolRequeueWait)
	}

	// an operation in flight: poll only that Job
	if status.Operation != "" {
		return r.pollOp(ctx, pool, sys, ns, status)
	}

	// probe
	if !r.DisableProbeHold {
		if wait := holdFor(poolKey(pool)); wait > 0 {
			return ctrl.Result{RequeueAfter: wait}, nil
		}
	}
	res, err := r.Dmg.Run(ctx, r.opSpec(pool, sys, ns, "query"))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("pool query job: %w", err)
	}
	if !res.Done {
		if meta.FindStatusCondition(status.Conditions, daosv1alpha1.ConditionReady) == nil {
			setC(metav1.ConditionUnknown, "Probing", "dmg pool query is running")
		}
		return r.updateStatus(ctx, pool, status, poolRequeueOp)
	}
	if !r.DisableProbeHold {
		setHold(poolKey(pool), poolRequeueWait)
	}
	env, perr := parseResult(res)
	if perr != nil {
		setC(metav1.ConditionUnknown, "ProbeFailed", perr.Error())
		return r.updateStatus(ctx, pool, status, poolRequeueWait)
	}
	if env.Error != nil {
		switch dmg.Classify(*env.Error) {
		case dmg.ErrNotFound:
			if status.UUID != "" {
				// never silently re-create: an existing pool that disappeared is data loss
				setC(metav1.ConditionFalse, "PoolMissing", fmt.Sprintf("pool %s (uuid %s) no longer exists in DAOS; delete and re-create the DaosPool to make a new, empty pool", poolLabel(pool), status.UUID))
				return r.updateStatus(ctx, pool, status, poolRequeueIdle)
			}
			if c := meta.FindStatusCondition(status.Conditions, daosv1alpha1.ConditionReady); c != nil && c.Reason == "CreateFailed" && status.ObservedGeneration == pool.Generation {
				return r.updateStatus(ctx, pool, status, poolRequeueFailed) // wait for a spec change
			}
			return r.startOp(ctx, pool, sys, ns, status, opCreate)
		case dmg.ErrUnreachable:
			setC(metav1.ConditionUnknown, "ManagementUnreachable", *env.Error)
		default:
			setC(metav1.ConditionUnknown, "QueryError", *env.Error)
		}
		return r.updateStatus(ctx, pool, status, poolRequeueWait)
	}
	info, err := dmg.PoolQuery(env)
	if err != nil {
		setC(metav1.ConditionUnknown, "ProbeFailed", err.Error())
		return r.updateStatus(ctx, pool, status, poolRequeueWait)
	}
	now := metav1.Now()
	status.UUID, status.State, status.RebuildState, status.DisabledTargets = info.UUID, info.State, info.Rebuild.State, info.DisabledTargets
	status.Label = poolLabel(pool)
	status.TotalBytes, status.FreeBytes = info.Totals()
	status.EnabledRanks = dmg.Ranks(info.EnabledRanks)
	status.LastQueryTime = &now
	// space: one number that decides for every container and PV in this pool.
	// Reported on every successful query, even when an operation follows.
	r.checkSpace(pool, &status)

	// drift -> at most one operation per pass; a failed operation is not retried
	// until the spec changes
	failedForThisSpec := func(reason string) bool {
		c := meta.FindStatusCondition(status.Conditions, daosv1alpha1.ConditionReady)
		return c != nil && c.Reason == reason && status.ObservedGeneration == pool.Generation
	}
	if len(pool.Spec.Ranks) > 0 && len(status.EnabledRanks) > 0 && len(missingRanks(pool.Spec.Ranks, status.EnabledRanks)) > 0 && !failedForThisSpec("ExtendFailed") {
		pool.Status.EnabledRanks = status.EnabledRanks // opSpec reads it
		return r.startOp(ctx, pool, sys, ns, status, opExtend)
	}
	if h := aclHash(pool.Spec.ACL); h != "" && h != status.AppliedACLHash && !failedForThisSpec("ACLFailed") {
		return r.startOp(ctx, pool, sys, ns, status, opACL)
	}
	if info.State == "Ready" {
		setC(metav1.ConditionTrue, "Ready", fmt.Sprintf("state %s, rebuild %s, %d ranks", info.State, info.Rebuild.State, len(status.EnabledRanks)))
	} else {
		setC(metav1.ConditionFalse, "State"+info.State, fmt.Sprintf("dmg reports state %s (rebuild %s, %d disabled targets)", info.State, info.Rebuild.State, info.DisabledTargets))
	}
	log.Info("pool", "name", pool.Name, "state", info.State, "ranks", len(status.EnabledRanks))
	return r.updateStatus(ctx, pool, status, poolRequeueIdle)
}

// checkSpace sets usedPercent and the SpaceLow condition, and emits an Event
// when the answer changes (not on every poll).
func (r *DaosPoolReconciler) checkSpace(pool *daosv1alpha1.DaosPool, status *daosv1alpha1.DaosPoolStatus) {
	threshold := int32(85)
	if pool.Spec.SpaceWarningPercent != nil {
		threshold = *pool.Spec.SpaceWarningPercent
	}
	if status.TotalBytes <= 0 {
		status.UsedPercent = 0
		return
	}
	used := status.TotalBytes - status.FreeBytes
	status.UsedPercent = int32(used * 100 / status.TotalBytes)
	if threshold <= 0 {
		meta.RemoveStatusCondition(&status.Conditions, daosv1alpha1.ConditionSpaceLow)
		return
	}
	was := meta.IsStatusConditionTrue(status.Conditions, daosv1alpha1.ConditionSpaceLow)
	low := status.UsedPercent >= threshold
	msg := fmt.Sprintf("%d%% used (%s of %s), %s free; DAOS enforces capacity per pool, so this applies to every container in it",
		status.UsedPercent, humanBytes(used), humanBytes(status.TotalBytes), humanBytes(status.FreeBytes))
	if low {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: daosv1alpha1.ConditionSpaceLow, Status: metav1.ConditionTrue,
			Reason: "AboveThreshold", Message: msg, ObservedGeneration: pool.Generation})
		if !was {
			r.event(pool, corev1.EventTypeWarning, "PoolSpaceLow", msg)
		}
		return
	}
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: daosv1alpha1.ConditionSpaceLow, Status: metav1.ConditionFalse,
		Reason: "BelowThreshold", Message: msg, ObservedGeneration: pool.Generation})
	if was {
		r.event(pool, corev1.EventTypeNormal, "PoolSpaceRecovered", msg)
	}
}

// humanBytes formats a byte count for humans reading conditions.
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func parseResult(res *dmg.Result) (*dmg.Envelope, error) {
	if res.Failure != "" && res.Output == "" {
		return nil, fmt.Errorf("job failed: %s", res.Failure)
	}
	return dmg.Parse(res.Output)
}

// startOp records the operation and creates its Job.
func (r *DaosPoolReconciler) startOp(ctx context.Context, pool *daosv1alpha1.DaosPool, sys *daosv1alpha1.DaosSystem, ns string, status daosv1alpha1.DaosPoolStatus, op string) (ctrl.Result, error) {
	if _, err := r.Dmg.Run(ctx, r.opSpec(pool, sys, ns, op)); err != nil {
		return ctrl.Result{}, fmt.Errorf("start %s: %w", op, err)
	}
	status.Operation = op
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: daosv1alpha1.ConditionReady, Status: metav1.ConditionFalse,
		Reason: strings.ToUpper(op[:1]) + op[1:] + "InProgress", Message: "dmg pool " + op + " is running", ObservedGeneration: pool.Generation})
	r.event(pool, corev1.EventTypeNormal, "Pool"+strings.ToUpper(op[:1])+op[1:], "dmg pool "+op+" started")
	return r.updateStatus(ctx, pool, status, poolRequeueOp)
}

// pollOp waits for the in-flight Job and applies its result.
func (r *DaosPoolReconciler) pollOp(ctx context.Context, pool *daosv1alpha1.DaosPool, sys *daosv1alpha1.DaosSystem, ns string, status daosv1alpha1.DaosPoolStatus) (ctrl.Result, error) {
	op := status.Operation
	res, err := r.Dmg.Run(ctx, r.opSpec(pool, sys, ns, op))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("poll %s: %w", op, err)
	}
	if !res.Done {
		return ctrl.Result{RequeueAfter: poolRequeueOp}, nil
	}
	status.Operation = ""
	status.ObservedGeneration = pool.Generation
	env, perr := parseResult(res)
	failed := func(msg string) (ctrl.Result, error) {
		reason := strings.ToUpper(op[:1]) + op[1:] + "Failed"
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: daosv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: reason, Message: msg, ObservedGeneration: pool.Generation})
		r.event(pool, corev1.EventTypeWarning, reason, msg)
		return r.updateStatus(ctx, pool, status, poolRequeueFailed)
	}
	if perr != nil {
		return failed("dmg pool " + op + ": " + perr.Error())
	}
	if env.Error != nil {
		return failed("dmg pool " + op + ": " + *env.Error)
	}
	switch op {
	case opCreate:
		uuid, ranks, err := dmg.PoolCreate(env)
		if err != nil {
			return failed(err.Error())
		}
		status.UUID, status.Label, status.EnabledRanks = uuid, poolLabel(pool), ranks
		status.AppliedACLHash = aclHash(pool.Spec.ACL) // create wrote the ACL file
		r.event(pool, corev1.EventTypeNormal, "PoolCreated", "pool "+poolLabel(pool)+" created, uuid "+uuid)
	case opACL:
		status.AppliedACLHash = aclHash(pool.Spec.ACL)
		r.event(pool, corev1.EventTypeNormal, "PoolACLApplied", "ACL overwritten from spec.acl")
	case opExtend:
		r.event(pool, corev1.EventTypeNormal, "PoolExtended", "pool extended to ranks "+ranksCSV(pool.Spec.Ranks))
	}
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: daosv1alpha1.ConditionReady, Status: metav1.ConditionUnknown, Reason: "Querying", Message: "dmg pool " + op + " succeeded; refreshing", ObservedGeneration: pool.Generation})
	clearHold(poolKey(pool))
	return r.updateStatus(ctx, pool, status, time.Second)
}

// finalize decides what deletion of the DaosPool means for the DAOS pool.
func (r *DaosPoolReconciler) finalize(ctx context.Context, pool *daosv1alpha1.DaosPool, sys *daosv1alpha1.DaosSystem, ns string, status daosv1alpha1.DaosPoolStatus) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(pool, daosv1alpha1.FinalizerPool) {
		return ctrl.Result{}, nil
	}
	approved := pool.Annotations[daosv1alpha1.AnnotationDestroyApproved] == "true"
	if !approved || status.UUID == "" {
		if status.UUID != "" {
			r.event(pool, corev1.EventTypeWarning, "PoolOrphaned", fmt.Sprintf("DaosPool deleted without %s=true: DAOS pool %s (uuid %s) is kept; remove it by hand with `dmg pool destroy %s` if that is intended",
				daosv1alpha1.AnnotationDestroyApproved, poolLabel(pool), status.UUID, poolLabel(pool)))
		}
		controllerutil.RemoveFinalizer(pool, daosv1alpha1.FinalizerPool)
		return ctrl.Result{}, r.Update(ctx, pool)
	}
	res, err := r.Dmg.Run(ctx, r.opSpec(pool, sys, ns, opDestroy))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("destroy job: %w", err)
	}
	if !res.Done {
		if status.Operation != opDestroy {
			status.Operation = opDestroy
			meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: daosv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "Destroying", Message: "dmg pool destroy is running", ObservedGeneration: pool.Generation})
			return r.updateStatus(ctx, pool, status, poolRequeueOp)
		}
		return ctrl.Result{RequeueAfter: poolRequeueOp}, nil
	}
	env, perr := parseResult(res)
	if perr == nil && env.Error != nil && dmg.Classify(*env.Error) != dmg.ErrNotFound {
		perr = fmt.Errorf("%s", *env.Error)
	}
	if perr != nil {
		status.Operation = ""
		msg := "dmg pool destroy: " + perr.Error() + "; the DaosPool stays until destroy succeeds or the annotation is removed (then the pool is kept)"
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: daosv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "DestroyFailed", Message: msg, ObservedGeneration: pool.Generation})
		r.event(pool, corev1.EventTypeWarning, "DestroyFailed", msg)
		return r.updateStatus(ctx, pool, status, poolRequeueIdle)
	}
	r.event(pool, corev1.EventTypeNormal, "PoolDestroyed", "DAOS pool "+poolLabel(pool)+" destroyed on request")
	controllerutil.RemoveFinalizer(pool, daosv1alpha1.FinalizerPool)
	return ctrl.Result{}, r.Update(ctx, pool)
}

func (r *DaosPoolReconciler) updateStatus(ctx context.Context, pool *daosv1alpha1.DaosPool, st daosv1alpha1.DaosPoolStatus, requeue time.Duration) (ctrl.Result, error) {
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &daosv1alpha1.DaosPool{}
		if err := r.Get(ctx, types.NamespacedName{Name: pool.Name}, latest); err != nil {
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

// SetupWithManager sets up the controller with the Manager.
func (r *DaosPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&daosv1alpha1.DaosPool{}).
		Owns(&batchv1.Job{}).
		Named("daospool").
		Complete(r)
}
