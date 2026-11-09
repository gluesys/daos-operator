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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	daosv1alpha1 "github.com/gluesys/daos-operator/api/v1alpha1"
	"github.com/gluesys/daos-operator/internal/dmg"
)

// Writable restore. DAOS 2.8 has no rollback and `daos fs copy` / `daos cont
// clone` cannot read an existing snapshot (clone takes a new one of the live
// container), so the copy goes through POSIX: dfuse mounts the snapshot
// read-only (--snap-epoch, by pool and container UUID, so a re-created source
// of the same name is never read) and `daos fs copy` writes it into the new
// container. fs copy truncates files it writes and accepts existing
// directories, so a retry after a failed or lost attempt is safe.

const (
	restoreMount = "/tmp/daos-restore-src"
	restoreLog   = "/tmp/daos-restore.log"
	// restoreDeadlineDefault bounds one copy attempt when spec.restoreFrom.deadlineSeconds is unset.
	restoreDeadlineDefault = int64(24 * 3600)
)

// restoreCommand is the restore Job's script. It prints exactly one JSON
// envelope at the end (the tools' own output goes to a log whose tail is put in
// the error), so dfuse or fs copy messages never parse as the answer.
func restoreCommand(sysName, srcPoolUUID, srcContUUID, epoch, dstPool, dstCont string) []string {
	q := dmg.ShellQuote
	mnt, log := q(restoreMount), q(restoreLog)
	var b strings.Builder
	b.WriteString(waitForAgent)
	b.WriteString(`envelope() { python3 -c 'import json,sys; e=sys.argv[1]; print(json.dumps({"response": None if e else {"restored": True}, "error": e or None, "status": 1 if e else 0}))' "$1"; }; `)
	b.WriteString(`fail() { fusermount3 -uz ` + mnt + ` >/dev/null 2>&1; envelope "$1: $(tail -c 1500 ` + log + ` 2>/dev/null)"; exit 0; }; `)
	b.WriteString(`mounted() { grep -q " ` + restoreMount + ` fuse" /proc/mounts; }; `)
	b.WriteString(`mkdir -p ` + mnt + ` && : > ` + log + `; `)
	b.WriteString(`dfuse --sys-name ` + q(sysName) + ` --pool ` + q(srcPoolUUID) + ` --container ` + q(srcContUUID) +
		` --mountpoint ` + mnt + ` --snap-epoch ` + q(epoch) + ` --foreground >>` + log + ` 2>&1 & `)
	b.WriteString(`for i in $(seq 1 60); do mounted && break; sleep 1; done; `)
	b.WriteString(`mounted || fail "dfuse did not mount the snapshot"; `)
	b.WriteString(`daos -G ` + q(sysName) + ` fs copy --src ` + mnt + ` --dst ` + q("daos://"+dstPool+"/"+dstCont) + ` >>` + log + ` 2>&1 || fail "daos fs copy failed"; `)
	b.WriteString(`fusermount3 -u ` + mnt + ` >>` + log + ` 2>&1; envelope ""`)
	return []string{"bash", "-c", b.String()}
}

// restoreSnapshot returns the snapshot spec.restoreFrom names once it was
// taken; otherwise why not (the copy waits). Its Ready is not required: it
// turns false while the snapshot waits for its own destroy (which waits for this
// copy) or after one failed probe, and neither says anything about the DAOS
// snapshot the copy reads. One destroyed by hand fails the copy, which says so.
func (r *DaosContainerReconciler) restoreSnapshot(ctx context.Context, c *daosv1alpha1.DaosContainer) (*daosv1alpha1.DaosContainerSnapshot, string, error) {
	s := &daosv1alpha1.DaosContainerSnapshot{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: c.Namespace, Name: c.Spec.RestoreFrom.SnapshotRef}, s); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "DaosContainerSnapshot " + c.Spec.RestoreFrom.SnapshotRef + " does not exist in namespace " + c.Namespace, nil
		}
		return nil, "", err
	}
	if s.Status.Epoch == "" || s.Status.PoolUUID == "" || s.Status.ContainerUUID == "" {
		return nil, "DaosContainerSnapshot " + s.Name + " is not taken yet", nil
	}
	return s, "", nil
}

func (r *DaosContainerReconciler) restoreSpec(c *daosv1alpha1.DaosContainer, pool *daosv1alpha1.DaosPool, sys *daosv1alpha1.DaosSystem, ns string, s *daosv1alpha1.DaosContainerSnapshot) dmg.RunSpec {
	spec := r.opSpec(c, pool, sys, ns, "query")
	spec.Name = jobName("cont", c.Namespace+"/"+c.Name, "daos-"+opRestore)
	spec.DeadlineSeconds = c.Spec.RestoreFrom.DeadlineSeconds
	if spec.DeadlineSeconds <= 0 {
		spec.DeadlineSeconds = restoreDeadlineDefault
	}
	spec.Command = restoreCommand(systemName(sys), s.Status.PoolUUID, s.Status.ContainerUUID, s.Status.Epoch, poolLabel(pool), contLabel(c))
	return spec
}

// restore runs once the container exists and spec.restoreFrom was not copied
// in yet. The container is not Ready until it was.
func (r *DaosContainerReconciler) restore(ctx context.Context, c *daosv1alpha1.DaosContainer, pool *daosv1alpha1.DaosPool, sys *daosv1alpha1.DaosSystem, ns string, status daosv1alpha1.DaosContainerStatus) (ctrl.Result, error) {
	setC := func(reason, msg string) {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: daosv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: reason, Message: msg, ObservedGeneration: c.Generation})
		status.Ready = false
	}
	if c.Spec.Type != "" && c.Spec.Type != "POSIX" {
		setC("RestoreUnsupported", "spec.restoreFrom copies files (daos fs copy): only a POSIX container can be restored into, this one is "+string(c.Spec.Type))
		return r.updateStatus(ctx, c, status, poolRequeueIdle)
	}
	if cond := meta.FindStatusCondition(status.Conditions, daosv1alpha1.ConditionReady); cond != nil && cond.Reason == "RestoreFailed" && status.RestoreStartedAt != nil {
		if wait := time.Until(status.RestoreStartedAt.Add(poolRequeueFailed)); wait > 0 {
			return r.updateStatus(ctx, c, status, wait)
		}
	}
	s, why, err := r.restoreSnapshot(ctx, c)
	if err != nil {
		return ctrl.Result{}, err
	}
	if s == nil {
		setC("RestoreWaiting", why+"; the container stays empty and not Ready until it is copied in")
		return r.updateStatus(ctx, c, status, poolRequeueWait)
	}
	if _, err := r.Dmg.Run(ctx, r.restoreSpec(c, pool, sys, ns, s)); err != nil {
		return ctrl.Result{}, fmt.Errorf("start restore: %w", err)
	}
	now := metav1.Now()
	status.Operation, status.RestoreStartedAt = opRestore, &now
	setC("RestoreInProgress", fmt.Sprintf("copying snapshot %s (epoch %s of container uuid %s) into the container", s.Name, s.Status.Epoch, s.Status.ContainerUUID))
	r.event(c, corev1.EventTypeNormal, "ContainerRestoring", fmt.Sprintf("copying DaosContainerSnapshot %s (epoch %s) into container %s", s.Name, s.Status.Epoch, contLabel(c)))
	return r.updateStatus(ctx, c, status, poolRequeueOp)
}

// pollRestore waits for the restore Job and records the result.
func (r *DaosContainerReconciler) pollRestore(ctx context.Context, c *daosv1alpha1.DaosContainer, pool *daosv1alpha1.DaosPool, sys *daosv1alpha1.DaosSystem, ns string, status daosv1alpha1.DaosContainerStatus) (ctrl.Result, error) {
	failed := func(msg string) (ctrl.Result, error) {
		status.Operation = ""
		msg += fmt.Sprintf("; retried in %s (the copy overwrites what it wrote)", poolRequeueFailed)
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: daosv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "RestoreFailed", Message: msg, ObservedGeneration: c.Generation})
		status.Ready = false
		r.event(c, corev1.EventTypeWarning, "RestoreFailed", msg)
		return r.updateStatus(ctx, c, status, poolRequeueFailed)
	}
	s, why, err := r.restoreSnapshot(ctx, c)
	if err != nil {
		return ctrl.Result{}, err
	}
	if s == nil {
		// deleted under a running copy (without destroy approval: an approved
		// destroy waits for the copy); the next attempt waits for it to come back
		return failed("restore source went away: " + why)
	}
	res, err := r.Dmg.Run(ctx, r.restoreSpec(c, pool, sys, ns, s))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("poll restore: %w", err)
	}
	if !res.Done {
		return ctrl.Result{RequeueAfter: poolRequeueOp}, nil
	}
	env, perr := parseResult(res)
	if perr == nil && env.Error != nil {
		perr = fmt.Errorf("%s", *env.Error)
	}
	if perr != nil {
		return failed("restore from " + s.Name + ": " + perr.Error())
	}
	status.Operation = ""
	status.RestoredFrom = s.Name + "@" + s.Status.Epoch
	r.event(c, corev1.EventTypeNormal, "ContainerRestored", fmt.Sprintf("DaosContainerSnapshot %s (epoch %s) copied into container %s", s.Name, s.Status.Epoch, contLabel(c)))
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: daosv1alpha1.ConditionReady, Status: metav1.ConditionUnknown, Reason: "Querying", Message: "restore finished; refreshing", ObservedGeneration: c.Generation})
	clearHold(contKey(c))
	return r.updateStatus(ctx, c, status, time.Second)
}

// restoresPending names the containers of the namespace still being filled
// from snapshot s; its destroy waits for them (the copy reads it through dfuse).
func restoresPending(ctx context.Context, cl client.Client, s *daosv1alpha1.DaosContainerSnapshot) ([]string, error) {
	var list daosv1alpha1.DaosContainerList
	if err := cl.List(ctx, &list, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}
	var out []string
	for _, c := range list.Items {
		if c.Spec.RestoreFrom != nil && c.Spec.RestoreFrom.SnapshotRef == s.Name && c.Status.RestoredFrom == "" && c.DeletionTimestamp.IsZero() {
			out = append(out, c.Name)
		}
	}
	return out, nil
}
