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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	daosv1alpha1 "gitlab.gluesys.com/exastor/daos-operator/api/v1alpha1"
	"gitlab.gluesys.com/exastor/daos-operator/internal/dmg"
)

// Full-stop upgrade (ADR-003, #13).
//
// DAOS 2.x has no server rolling upgrade, so the operator does the honest
// thing: with an explicit approval it stops the whole system, replaces every
// server pod with the new image, starts the system and checks that all ranks
// joined. The StatefulSets use updateStrategy OnDelete, therefore a changed
// spec.images.server never restarts anything by itself: the trigger is "a
// server pod runs an image other than spec.images.server" plus
// spec.upgrade.approved=true. No step formats, wipes or forces.

const (
	upgradePending        = "Pending"
	upgradeStopping       = "Stopping"
	upgradeUpdating       = "Updating"
	upgradeStarting       = "Starting"
	upgradeStartingSystem = "StartingSystem"
	upgradeVerifying      = "Verifying"
	upgradeCompleted      = "Completed"
	upgradeFailed         = "Failed"

	// triggers
	triggerImage = "ImageChange"
	triggerCerts = "CertificateRotation"

	jobStopSuffix   = "-dmg-stop"
	jobStartSuffix  = "-dmg-start"
	jobVerifySuffix = "-dmg-verify"
	requeueUpgrade  = 15 * time.Second
)

// +kubebuilder:rbac:groups="",resources=pods,verbs=delete

func upgradeInProgress(st *daosv1alpha1.DaosSystemStatus) bool {
	if st.Upgrade == nil {
		return false
	}
	switch st.Upgrade.Phase {
	case upgradeStopping, upgradeUpdating, upgradeStarting, upgradeStartingSystem, upgradeVerifying:
		return true
	}
	return false
}

func upgradeTimeout(sys *daosv1alpha1.DaosSystem) time.Duration {
	if sys.Spec.Upgrade.TimeoutMinutes > 0 {
		return time.Duration(sys.Spec.Upgrade.TimeoutMinutes) * time.Minute
	}
	return 30 * time.Minute
}

// serverPods lists this system's server pods.
func (r *DaosSystemReconciler) serverPods(ctx context.Context, sys *daosv1alpha1.DaosSystem, ns string) ([]corev1.Pod, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(ns), client.MatchingLabels{daosv1alpha1.LabelSystem: sys.Name, daosv1alpha1.LabelRole: "server"}); err != nil {
		return nil, err
	}
	sort.Slice(pods.Items, func(i, j int) bool { return pods.Items[i].Name < pods.Items[j].Name })
	return pods.Items, nil
}

func podImage(p *corev1.Pod) string {
	for _, c := range p.Spec.Containers {
		if c.Name == serverContainer {
			return c.Image
		}
	}
	return ""
}

func podReady(p *corev1.Pod) bool {
	if !p.DeletionTimestamp.IsZero() {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// resetApproval sets spec.upgrade.approved back to false (one-shot approval).
func (r *DaosSystemReconciler) resetApproval(ctx context.Context, sys *daosv1alpha1.DaosSystem) error {
	if !sys.Spec.Upgrade.Approved {
		return nil
	}
	patch := client.MergeFrom(sys.DeepCopy())
	sys.Spec.Upgrade.Approved = false
	return r.Patch(ctx, sys, patch)
}

// reconcileUpgrade returns (inProgress, requeue, error). While inProgress the
// caller skips the format/membership probe and reports Ready=False (Upgrading).
func (r *DaosSystemReconciler) reconcileUpgrade(ctx context.Context, sys *daosv1alpha1.DaosSystem, ns string, rendered int, status *daosv1alpha1.DaosSystemStatus) (bool, time.Duration, error) {
	if r.Dmg == nil || !serverEnabled(sys) || rendered == 0 {
		return false, 0, nil
	}
	pods, err := r.serverPods(ctx, sys, ns)
	if err != nil {
		return false, 0, err
	}
	want := sys.Spec.Images.Server
	var stale []string
	for i := range pods {
		if img := podImage(&pods[i]); img != "" && img != want {
			stale = append(stale, img)
		}
	}
	rotateWanted := certsRenewApproved(sys)
	up := status.Upgrade
	// both approvals are one-shot: whatever ends the restart consumes them
	done := func() error {
		if err := r.resetApproval(ctx, sys); err != nil {
			return err
		}
		return r.clearCertsApproval(ctx, sys)
	}
	fail := func(msg string) (bool, time.Duration, error) {
		now := metav1.Now()
		up.Phase, up.Message, up.FinishedAt = upgradeFailed, msg, &now
		setCond(status, daosv1alpha1.ConditionUpgrading, metav1.ConditionFalse, upgradeFailed, msg)
		r.event(sys, corev1.EventTypeWarning, "UpgradeFailed", msg)
		return false, requeueSlow, done()
	}

	if !upgradeInProgress(status) {
		switch {
		case rotateWanted:
			// certificate rotation replaces the CA: the same full-stop restart,
			// with the Secret swapped while every engine is down
			now := metav1.Now()
			status.Upgrade = &daosv1alpha1.UpgradeStatus{Trigger: triggerCerts, Phase: upgradeStopping, FromImage: want, ToImage: want, StartedAt: &now,
				Message: "certificate rotation approved; stopping the system (dmg system stop)"}
			up = status.Upgrade
			setCond(status, daosv1alpha1.ConditionUpgrading, metav1.ConditionTrue, upgradeStopping, up.Message)
			r.event(sys, corev1.EventTypeNormal, "CertificateRotationStarted", "full-stop restart to replace the transport certificates")
		case len(stale) == 0:
			// nothing to do: image is what the pods run
			if up != nil && up.Phase == upgradePending {
				status.Upgrade = nil
			}
			if len(pods) > 0 {
				status.ObservedVersion = sys.Spec.Version
			}
			if c := findCond(status, daosv1alpha1.ConditionUpgrading); c == nil || c.Reason == upgradePending {
				setCond(status, daosv1alpha1.ConditionUpgrading, metav1.ConditionFalse, "UpToDate", "server pods run spec.images.server")
			}
			return false, 0, nil
		case !sys.Spec.Upgrade.Approved:
			if up == nil || up.Phase != upgradePending {
				status.Upgrade = &daosv1alpha1.UpgradeStatus{Trigger: triggerImage, Phase: upgradePending, FromImage: stale[0], ToImage: want}
			}
			status.Upgrade.Message = fmt.Sprintf("%d server pod(s) run %s, spec wants %s; full-stop upgrade waits for spec.upgrade.approved=true (drain clients first: the operator cannot see client handles)", len(stale), stale[0], want)
			setCond(status, daosv1alpha1.ConditionUpgrading, metav1.ConditionFalse, upgradePending, status.Upgrade.Message)
			return false, 0, nil
		default:
			now := metav1.Now()
			status.Upgrade = &daosv1alpha1.UpgradeStatus{Trigger: triggerImage, Phase: upgradeStopping, FromImage: stale[0], ToImage: want, StartedAt: &now,
				Message: "approved; stopping the system (dmg system stop)"}
			up = status.Upgrade
			setCond(status, daosv1alpha1.ConditionUpgrading, metav1.ConditionTrue, upgradeStopping, up.Message)
			r.event(sys, corev1.EventTypeNormal, "UpgradeStarted", fmt.Sprintf("full-stop upgrade %s -> %s", stale[0], want))
		}
	}

	if up.StartedAt != nil && time.Since(up.StartedAt.Time) > upgradeTimeout(sys) {
		return fail(fmt.Sprintf("upgrade timed out in phase %s after %s", up.Phase, upgradeTimeout(sys)))
	}
	switch up.Phase {
	case upgradeStopping:
		res, err := r.runDmg(ctx, sys, ns, jobStopSuffix, "system", "stop")
		if err != nil {
			return true, 0, err
		}
		if !res.Done {
			return true, requeueProbe, nil
		}
		env, perr := parseResult(res)
		if perr != nil {
			return fail("dmg system stop: " + perr.Error())
		}
		if env.Error != nil {
			return fail("dmg system stop: " + *env.Error)
		}
		up.Phase, up.Message = upgradeUpdating, "system stopped; replacing server pods"
		setCond(status, daosv1alpha1.ConditionUpgrading, metav1.ConditionTrue, up.Phase, up.Message)
		return true, time.Second, nil

	case upgradeUpdating:
		if up.Trigger == triggerCerts {
			// the engines are down: swap the certificates now, then restart every pod
			if err := r.rotateCerts(ctx, sys, ns, status); err != nil {
				return fail("certificate rotation: " + err.Error())
			}
		}
		// the StatefulSets already carry spec.images.server (OnDelete); deleting the
		// pods is what makes them come back with it -- and with the new certificates
		deleted := 0
		for i := range pods {
			if !pods[i].DeletionTimestamp.IsZero() {
				continue
			}
			if up.Trigger == triggerCerts || podImage(&pods[i]) != want {
				if err := r.Delete(ctx, &pods[i]); client.IgnoreNotFound(err) != nil {
					return true, 0, err
				}
				deleted++
			}
		}
		up.Phase, up.Message = upgradeStarting, fmt.Sprintf("%d pod(s) deleted; waiting for %d server pod(s) with %s to become Ready", deleted, rendered, want)
		setCond(status, daosv1alpha1.ConditionUpgrading, metav1.ConditionTrue, up.Phase, up.Message)
		r.event(sys, corev1.EventTypeNormal, "UpgradePodsReplaced", up.Message)
		return true, requeueUpgrade, nil

	case upgradeStarting:
		ready := 0
		for i := range pods {
			if podImage(&pods[i]) == want && podReady(&pods[i]) && pods[i].DeletionTimestamp.IsZero() {
				ready++
			}
		}
		if ready < rendered {
			up.Message = fmt.Sprintf("%d/%d server pod(s) Ready with %s", ready, rendered, want)
			setCond(status, daosv1alpha1.ConditionUpgrading, metav1.ConditionTrue, up.Phase, up.Message)
			return true, requeueUpgrade, nil
		}
		up.Phase, up.Message = upgradeStartingSystem, "all server pods Ready; dmg system start"
		setCond(status, daosv1alpha1.ConditionUpgrading, metav1.ConditionTrue, up.Phase, up.Message)
		return true, time.Second, nil

	case upgradeStartingSystem:
		res, err := r.runDmg(ctx, sys, ns, jobStartSuffix, "system", "start")
		if err != nil {
			return true, 0, err
		}
		if !res.Done {
			return true, requeueProbe, nil
		}
		env, perr := parseResult(res)
		if perr != nil {
			return fail("dmg system start: " + perr.Error())
		}
		if env.Error != nil {
			return fail("dmg system start: " + *env.Error)
		}
		up.Phase, up.Message = upgradeVerifying, "system started; waiting for all ranks to join"
		setCond(status, daosv1alpha1.ConditionUpgrading, metav1.ConditionTrue, up.Phase, up.Message)
		return true, time.Second, nil

	case upgradeVerifying:
		res, err := r.runDmg(ctx, sys, ns, jobVerifySuffix, "system", "query", "-v")
		if err != nil {
			return true, 0, err
		}
		if !res.Done {
			return true, requeueProbe, nil
		}
		env, perr := parseResult(res)
		if perr != nil {
			up.Message = "dmg system query: " + perr.Error() + "; retrying"
			return true, requeueUpgrade, nil
		}
		if env.Error != nil {
			up.Message = "dmg system query: " + *env.Error + "; retrying"
			setCond(status, daosv1alpha1.ConditionUpgrading, metav1.ConditionTrue, up.Phase, up.Message)
			return true, requeueUpgrade, nil
		}
		members, err := dmg.SystemQuery(env)
		if err != nil {
			return fail(err.Error())
		}
		var notJoined []string
		for _, m := range members {
			if m.State != memberJoined {
				notJoined = append(notJoined, fmt.Sprintf("rank %d %s", m.Rank, m.State))
			}
		}
		if len(members) == 0 || len(notJoined) > 0 {
			up.Message = fmt.Sprintf("%d/%d ranks joined; %s", len(members)-len(notJoined), len(members), strings.Join(notJoined, ", "))
			setCond(status, daosv1alpha1.ConditionUpgrading, metav1.ConditionTrue, up.Phase, up.Message)
			return true, requeueUpgrade, nil
		}
		now := metav1.Now()
		up.Phase, up.FinishedAt = upgradeCompleted, &now
		if up.Trigger == triggerCerts {
			up.Message = fmt.Sprintf("%d ranks joined with the new certificates; restart DAOS clients (CSI node DaemonSet, agent sidecars) to pick up the new CA", len(members))
			r.event(sys, corev1.EventTypeNormal, "CertificateRotationCompleted", up.Message)
		} else {
			up.Message = fmt.Sprintf("%d ranks joined on %s", len(members), want)
			status.ObservedVersion = sys.Spec.Version
			r.event(sys, corev1.EventTypeNormal, "UpgradeCompleted", up.Message)
		}
		setCond(status, daosv1alpha1.ConditionUpgrading, metav1.ConditionFalse, upgradeCompleted, up.Message)
		clearProbeHold(sys.Name)
		return false, time.Second, done()
	}
	return false, 0, nil
}
