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
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	daosv1alpha1 "gitlab.gluesys.com/exastor/daos-operator/api/v1alpha1"
	"gitlab.gluesys.com/exastor/daos-operator/internal/dmg"
)

// Format gate and membership (#10).
//
// `dmg storage format` wipes the drives. The operator therefore never decides to
// format: it observes (via `dmg system query`) that the management service is
// uninitialized, reports status.pendingFormat=true, and waits for a human to
// put daos.gluesys.com/format-approved=true on the DaosSystem. Only then, and
// only when every rendered node's server pod is Ready (formatting a partial
// system would leave ranks out), it runs `dmg storage format` once through a
// Job and drops the annotation whatever the outcome. Everything in status
// (formatted, ranks, joined counts) is copied from dmg output -- the operator
// keeps no membership database of its own.

const (
	jobQuerySuffix    = "-dmg-query"
	jobFormatSuffix   = "-dmg-format"
	requeueProbe      = 10 * time.Second
	requeueMembership = 60 * time.Second
	memberJoined      = "joined"
	memberAwaitFormat = "awaitformat"
)

// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get

type formatInput struct {
	ns              string
	nodeByAddr      map[string]string // control address -> node name
	rendered        int
	serversAllReady bool
}

func formatApproved(sys *daosv1alpha1.DaosSystem) bool {
	return sys.Annotations[daosv1alpha1.AnnotationFormatApproved] == "true"
}

func (r *DaosSystemReconciler) event(sys *daosv1alpha1.DaosSystem, typ, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(sys, typ, reason, msg)
	}
}

// clearApproval removes the one-shot annotation.
func (r *DaosSystemReconciler) clearApproval(ctx context.Context, sys *daosv1alpha1.DaosSystem) error {
	if _, ok := sys.Annotations[daosv1alpha1.AnnotationFormatApproved]; !ok {
		return nil
	}
	patch := client.MergeFrom(sys.DeepCopy())
	delete(sys.Annotations, daosv1alpha1.AnnotationFormatApproved)
	return r.Patch(ctx, sys, patch)
}

func (r *DaosSystemReconciler) runDmg(ctx context.Context, sys *daosv1alpha1.DaosSystem, ns, suffix string, args ...string) (*dmg.Result, error) {
	return r.Dmg.Run(ctx, dmg.RunSpec{
		Owner: sys, Namespace: ns, Name: sys.Name + suffix,
		Image:            sys.Spec.Images.Admin,
		ControlConfigMap: sys.Name + "-control",
		Args:             args,
		NodeSelector:     sys.Spec.NodeSelector,
		Tolerations:      sys.Spec.Tolerations,
		Labels:           map[string]string{daosv1alpha1.LabelSystem: sys.Name},
	})
}

// reconcileFormat updates the format/membership part of status and returns how
// soon to look again.
func (r *DaosSystemReconciler) reconcileFormat(ctx context.Context, sys *daosv1alpha1.DaosSystem, in formatInput, status *daosv1alpha1.DaosSystemStatus) (time.Duration, error) {
	if r.Dmg == nil {
		setCond(status, daosv1alpha1.ConditionFormatted, metav1.ConditionUnknown, "NoRunner", "operator started without a dmg runner")
		return 0, nil
	}
	if !serverEnabled(sys) || in.rendered == 0 {
		setCond(status, daosv1alpha1.ConditionFormatted, metav1.ConditionUnknown, "NoServers", "no server workloads to query")
		return 0, nil
	}
	approved := formatApproved(sys)

	// --- format path: only after pendingFormat was observed and a human approved
	if approved && status.PendingFormat && !status.Formatted {
		if !in.serversAllReady {
			setCond(status, daosv1alpha1.ConditionFormatted, metav1.ConditionFalse, "AwaitingServers",
				"format approved but not every server pod is ready; refusing to format a partial system")
			return requeueSlow, nil
		}
		res, err := r.runDmg(ctx, sys, in.ns, jobFormatSuffix, "storage", "format")
		if err != nil {
			return 0, fmt.Errorf("format job: %w", err)
		}
		if !res.Done {
			setCond(status, daosv1alpha1.ConditionFormatted, metav1.ConditionFalse, "Formatting", "dmg storage format is running")
			return requeueProbe, nil
		}
		// one shot: the approval is consumed by this attempt, success or not
		if err := r.clearApproval(ctx, sys); err != nil {
			return 0, err
		}
		if msg := formatFailure(res); msg != "" {
			setCond(status, daosv1alpha1.ConditionFormatted, metav1.ConditionFalse, "FormatFailed", msg)
			r.event(sys, corev1.EventTypeWarning, "FormatFailed", msg)
			return requeueSlow, nil
		}
		now := metav1.Now()
		status.Formatted, status.PendingFormat, status.FormatTime = true, false, &now
		setCond(status, daosv1alpha1.ConditionFormatted, metav1.ConditionTrue, "Formatted", "dmg storage format succeeded; querying membership")
		r.event(sys, corev1.EventTypeNormal, "Formatted", "dmg storage format succeeded")
		clearProbeHold(sys.Name) // learn the membership right away
		return requeueProbe, nil
	}

	// --- query path. A finished Job is deleted, and that delete event would
	// reconcile us straight into the next Job; hold the cadence instead.
	if wait := r.probeHold(sys.Name); wait > 0 {
		return wait, nil
	}
	res, err := r.runDmg(ctx, sys, in.ns, jobQuerySuffix, "system", "query", "-v")
	if err != nil {
		return 0, fmt.Errorf("query job: %w", err)
	}
	if !res.Done {
		if c := findCond(status, daosv1alpha1.ConditionFormatted); c == nil {
			setCond(status, daosv1alpha1.ConditionFormatted, metav1.ConditionUnknown, "Probing", "dmg system query is running")
		}
		return requeueProbe, nil
	}
	defer r.setProbeHold(sys.Name, status)
	if res.Failure != "" && res.Output == "" {
		setCond(status, daosv1alpha1.ConditionFormatted, metav1.ConditionUnknown, "ProbeFailed", "dmg Job failed: "+res.Failure)
		return requeueSlow, nil
	}
	env, err := dmg.Parse(res.Output)
	if err != nil {
		setCond(status, daosv1alpha1.ConditionFormatted, metav1.ConditionUnknown, "ProbeFailed", err.Error())
		return requeueSlow, nil
	}
	if env.Error != nil {
		switch dmg.Classify(*env.Error) {
		case dmg.ErrUnformatted:
			status.PendingFormat, status.Formatted = true, false
			status.Ranks, status.RanksJoined, status.RanksTotal = nil, 0, 0
			msg := "storage is not formatted (" + *env.Error + ")"
			if approved {
				msg += "; approval present, formatting on the next pass"
			} else {
				msg += fmt.Sprintf("; a human must approve: kubectl annotate daossystem %s %s=true", sys.Name, daosv1alpha1.AnnotationFormatApproved)
			}
			setCond(status, daosv1alpha1.ConditionFormatted, metav1.ConditionFalse, "AwaitingApproval", msg)
			if approved {
				return requeueProbe, nil
			}
			return requeueSlow, nil
		case dmg.ErrUnreachable:
			setCond(status, daosv1alpha1.ConditionFormatted, metav1.ConditionUnknown, "ManagementUnreachable", *env.Error)
		default:
			setCond(status, daosv1alpha1.ConditionFormatted, metav1.ConditionUnknown, "QueryError", *env.Error)
		}
		return requeueSlow, nil
	}
	members, err := dmg.SystemQuery(env)
	if err != nil {
		setCond(status, daosv1alpha1.ConditionFormatted, metav1.ConditionUnknown, "ProbeFailed", err.Error())
		return requeueSlow, nil
	}
	now := metav1.Now()
	status.Formatted, status.LastQueryTime = true, &now
	status.Ranks = nil
	joined, awaiting := 0, 0
	for _, m := range members {
		node := in.nodeByAddr[dmg.Host(m.Addr)]
		if node == "" {
			node = dmg.Host(m.Addr)
		}
		status.Ranks = append(status.Ranks, daosv1alpha1.RankStatus{Rank: m.Rank, Node: node, State: m.State})
		switch m.State {
		case memberJoined:
			joined++
		case memberAwaitFormat:
			awaiting++
		}
	}
	status.RanksJoined, status.RanksTotal = int32(joined), int32(len(members))
	// ranks in awaitformat (a node added to a formatted system) need the same human gate
	status.PendingFormat = awaiting > 0
	setCond(status, daosv1alpha1.ConditionFormatted, metav1.ConditionTrue, "Formatted", fmt.Sprintf("%d/%d ranks joined%s", joined, len(members), awaitSuffix(awaiting)))
	if approved && !status.PendingFormat {
		// stale approval on a formatted system: drop it so it cannot fire later
		if err := r.clearApproval(ctx, sys); err != nil {
			return 0, err
		}
		r.event(sys, corev1.EventTypeNormal, "ApprovalIgnored", "format approval removed: system is already formatted")
	}
	return requeueMembership, nil
}

// probe cadence: in-memory per system, so a restart simply probes once more.
var (
	probeMu   sync.Mutex
	probeNext = map[string]time.Time{}
)

// probeHold returns how long to wait before the next query Job may start.
func (r *DaosSystemReconciler) probeHold(name string) time.Duration {
	if r.DisableProbeHold {
		return 0
	}
	probeMu.Lock()
	defer probeMu.Unlock()
	if t, ok := probeNext[name]; ok {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// setProbeHold schedules the next query: sooner while a format is pending or
// the service is unknown, slower once membership is healthy.
func (r *DaosSystemReconciler) setProbeHold(name string, st *daosv1alpha1.DaosSystemStatus) {
	if r.DisableProbeHold {
		return
	}
	interval := requeueSlow
	if c := findCond(st, daosv1alpha1.ConditionFormatted); c != nil && c.Status == metav1.ConditionTrue && !st.PendingFormat {
		interval = requeueMembership
	}
	probeMu.Lock()
	probeNext[name] = time.Now().Add(interval)
	probeMu.Unlock()
}

func clearProbeHold(name string) {
	probeMu.Lock()
	delete(probeNext, name)
	probeMu.Unlock()
}

func awaitSuffix(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(", %d awaiting format", n)
}

// formatFailure returns "" on success, else a message built from the Job/dmg output.
func formatFailure(res *dmg.Result) string {
	if res.Failure != "" && res.Output == "" {
		return "format Job failed: " + res.Failure
	}
	env, err := dmg.Parse(res.Output)
	if err != nil {
		return "format output: " + err.Error()
	}
	if env.Error != nil {
		return "dmg storage format: " + *env.Error
	}
	he, err := dmg.StorageFormat(env)
	if err != nil {
		return err.Error()
	}
	if len(he) > 0 {
		var msgs []string
		for m, hosts := range he {
			msgs = append(msgs, hosts+": "+m)
		}
		sort.Strings(msgs)
		return "dmg storage format host errors: " + strings.Join(msgs, "; ")
	}
	if res.ExitCode != 0 {
		return fmt.Sprintf("dmg exited %d", res.ExitCode)
	}
	return ""
}

func findCond(st *daosv1alpha1.DaosSystemStatus, t string) *metav1.Condition {
	for i := range st.Conditions {
		if st.Conditions[i].Type == t {
			return &st.Conditions[i]
		}
	}
	return nil
}
