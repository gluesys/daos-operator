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
	return r.runDmgThen(ctx, sys, ns, suffix, nil, args...)
}

// runDmgThen is runDmg with a second command in the same pod (dmg.RunSpec.Then).
func (r *DaosSystemReconciler) runDmgThen(ctx context.Context, sys *daosv1alpha1.DaosSystem, ns, suffix string, then []string, args ...string) (*dmg.Result, error) {
	return r.Dmg.Run(ctx, dmg.RunSpec{
		Owner: sys, Namespace: ns, Name: sys.Name + suffix,
		Image:            sys.Spec.Images.Admin,
		ControlConfigMap: sys.Name + "-control",
		Args:             args,
		Then:             then,
		NodeSelector:     sys.Spec.NodeSelector,
		Tolerations:      sys.Spec.Tolerations,
		Labels:           map[string]string{daosv1alpha1.LabelSystem: sys.Name},
		CertsSecret:      adminCertsSecret(sys),
		CertsFiles:       adminCertFiles(),
	})
}

// adminCertsSecret is the Secret dmg Jobs mount, "" when insecure.
func adminCertsSecret(sys *daosv1alpha1.DaosSystem) string {
	if !certsNeeded(sys) {
		return ""
	}
	return certsSecretName(sys)
}

// reconcileFormat updates the format/membership part of status and returns how
// soon to look again.
func (r *DaosSystemReconciler) reconcileFormat(ctx context.Context, sys *daosv1alpha1.DaosSystem, in formatInput, status *daosv1alpha1.DaosSystemStatus) (time.Duration, error) {
	if r.Dmg == nil {
		setCond(status, daosv1alpha1.ConditionFormatted, metav1.ConditionUnknown, "NoRunner", "operator started without a dmg runner")
		return 0, nil
	}
	// an external system has no server pods here by definition; for one we run,
	// there is nothing to ask until a server exists
	if !external(sys) && (!serverEnabled(sys) || in.rendered == 0) {
		setCond(status, daosv1alpha1.ConditionFormatted, metav1.ConditionUnknown, "NoServers", "no server workloads to query")
		return 0, nil
	}
	approved := formatApproved(sys) && !external(sys)
	if external(sys) && formatApproved(sys) {
		// refuse politely and drop the annotation: formatting a system we do not
		// run is not ours to decide
		r.event(sys, corev1.EventTypeWarning, "FormatRefused",
			"spec.externalMsReplicas is set: the operator will not format a DAOS system it does not run")
		if err := r.clearApproval(ctx, sys); err != nil {
			return 0, err
		}
	}

	// --- format path: only after pendingFormat was observed and a human approved.
	// A system that is already formatted reaches here when a node was added to
	// it: the drives of the ranks already carrying data must not be touched, so
	// the format is restricted to the addresses that still need one.
	needFormat := approved && status.PendingFormat
	var only []string
	if needFormat && status.Formatted {
		if only = formatHosts(status); len(only) == 0 {
			needFormat = false
		}
	}
	if needFormat {
		if !in.serversAllReady {
			setCond(status, daosv1alpha1.ConditionFormatted, metav1.ConditionFalse, "AwaitingServers",
				"format approved but not every server pod is ready; refusing to format a partial system")
			return requeueSlow, nil
		}
		args := []string{"storage", "format"}
		if len(only) > 0 {
			args = append([]string{"-l", strings.Join(only, ",")}, args...)
		}
		res, err := r.runDmg(ctx, sys, in.ns, jobFormatSuffix, args...)
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
			msg += formatHint(msg)
			now := metav1.Now()
			status.LastFormat = &daosv1alpha1.FormatAttempt{Time: now, Hosts: only, Succeeded: false, Message: msg}
			setCond(status, daosv1alpha1.ConditionFormatted, metav1.ConditionFalse, "FormatFailed", msg)
			r.event(sys, corev1.EventTypeWarning, "FormatFailed", msg)
			return requeueSlow, nil
		}
		now := metav1.Now()
		status.Formatted, status.PendingFormat, status.FormatTime = true, false, &now
		status.LastFormat = &daosv1alpha1.FormatAttempt{Time: now, Hosts: only, Succeeded: true}
		done := "dmg storage format succeeded"
		if len(only) > 0 {
			done += " on " + strings.Join(only, ",")
		}
		setCond(status, daosv1alpha1.ConditionFormatted, metav1.ConditionTrue, "Formatted", done+"; querying membership")
		r.event(sys, corev1.EventTypeNormal, "Formatted", done)
		clearProbeHold(sys.Name) // learn the membership right away
		return requeueProbe, nil
	}

	// --- query path. A finished Job is deleted, and that delete event would
	// reconcile us straight into the next Job; hold the cadence instead.
	if wait := r.probeHold(sys.Name); wait > 0 {
		return wait, nil
	}
	// the same pod also asks who leads: a membership answer alone cannot tell a
	// healthy system from one whose management service lost quorum (#34)
	res, err := r.runDmgThen(ctx, sys, in.ns, jobQuerySuffix,
		[]string{"system", "leader-query"}, "system", "query", "-v")
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
	envs, err := dmg.ParseAll(res.Output)
	if err != nil {
		setCond(status, daosv1alpha1.ConditionFormatted, metav1.ConditionUnknown, "ProbeFailed", err.Error())
		return requeueSlow, nil
	}
	env := envs[0]
	if len(envs) > 1 {
		r.setMsCondition(sys, status, envs[1])
	}
	if env.Error != nil {
		switch dmg.Classify(*env.Error) {
		case dmg.ErrUnformatted:
			status.PendingFormat, status.Formatted = true, false
			status.Ranks, status.RanksJoined, status.RanksTotal = nil, 0, 0
			msg := "storage is not formatted (" + *env.Error + ")"
			if external(sys) {
				msg = "the external system reports it is not formatted (" + *env.Error + "); format it where it runs"
			}
			// carry why the last attempt failed: approving again would fail the same way, and the
			// Job that held the answer is gone (same as the partly formatted branch below)
			if lf := status.LastFormat; lf != nil && !lf.Succeeded {
				msg += fmt.Sprintf("; last attempt %s failed: %s", lf.Time.Format(time.RFC3339), lf.Message)
			}
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
	// A node added to a running system needs the same human gate. It sits in
	// awaitformat when it once had a rank; a node that never joined is absent
	// from the member list altogether, so the member list alone cannot see it.
	unformatted := formatHosts(status)
	status.PendingFormat = awaiting > 0 || len(unformatted) > 0
	msg := fmt.Sprintf("%d/%d ranks joined%s", joined, len(members), awaitSuffix(awaiting))
	if len(unformatted) > 0 {
		msg += "; not formatted yet: " + strings.Join(unformatted, ",")
		// the condition is rewritten on every query, so carry the reason the last
		// attempt failed -- otherwise approving again just fails the same way with
		// nothing to read, and the Job holding the answer is already collected
		if lf := status.LastFormat; lf != nil && !lf.Succeeded {
			msg += fmt.Sprintf("; last attempt %s failed: %s", lf.Time.Format(time.RFC3339), lf.Message)
		} else if !approved {
			msg += fmt.Sprintf(" (a human must approve: kubectl annotate daossystem %s %s=true)", sys.Name, daosv1alpha1.AnnotationFormatApproved)
		}
	}
	setCond(status, daosv1alpha1.ConditionFormatted, metav1.ConditionTrue, "Formatted", msg)
	if approved && !status.PendingFormat {
		// stale approval on a formatted system: drop it so it cannot fire later
		if err := r.clearApproval(ctx, sys); err != nil {
			return 0, err
		}
		r.event(sys, corev1.EventTypeNormal, "ApprovalIgnored", "format approval removed: system is already formatted")
	}
	return requeueMembership, nil
}

// probeHold returns how long to wait before the next query Job may start.
func (r *DaosSystemReconciler) probeHold(name string) time.Duration {
	if r.DisableProbeHold {
		return 0
	}
	return holdFor(name)
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
	setHold(name, interval)
}

func clearProbeHold(name string) { clearHold(name) }

// formatHosts returns the control addresses of nodes this operator renders a
// server for that the management service is not counting as a healthy rank:
// members in awaitformat, and nodes that never joined (no rank, so no member
// entry). Only these may be handed to `dmg storage format` on a system that is
// already carrying data. Nodes whose server pod is not ready are left out --
// formatting one that cannot answer only produces a host error.
func formatHosts(status *daosv1alpha1.DaosSystemStatus) []string {
	member, await := map[string]bool{}, map[string]bool{}
	for _, r := range status.Ranks {
		member[r.Node] = true
		if r.State == memberAwaitFormat {
			await[r.Node] = true
		}
	}
	var out []string
	for _, nc := range status.NodeConfigs {
		if !nc.Ready || !nc.ServerReady || nc.ControlAddr == "" {
			continue
		}
		if !member[nc.Node] || await[nc.Node] {
			out = append(out, nc.ControlAddr)
		}
	}
	sort.Strings(out)
	return out
}

// formatHint turns dmg's terser refusals into the thing to go and check. The
// system-name mismatch is the one that costs the most time: it means something
// else answers on the control port -- on a node recycled from a native install
// that is the host's own daos_server, which `systemctl disable` does not stop
// (#32).
func formatHint(msg string) string {
	if strings.Contains(msg, "does not match running system") {
		return " -- another DAOS system answers on the control port of that host;" +
			" on a node recycled from a native install, stop and mask it" +
			" (systemctl mask --now daos_server daos_agent)"
	}
	return ""
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
	// a failed format comes back as a top-level "N host(s) had errors" AND the per-host errors;
	// the cause (e.g. fallocate: no space left on device) is only in the latter
	he, herr := dmg.StorageFormat(env)
	var hostMsgs []string
	for m, hosts := range he {
		hostMsgs = append(hostMsgs, hosts+": "+m)
	}
	sort.Strings(hostMsgs)
	if env.Error != nil {
		msg := "dmg storage format: " + *env.Error
		if len(hostMsgs) > 0 {
			msg += ": " + strings.Join(hostMsgs, "; ")
		}
		return msg
	}
	if herr != nil {
		return herr.Error()
	}
	if len(hostMsgs) > 0 {
		return "dmg storage format host errors: " + strings.Join(hostMsgs, "; ")
	}
	if res.ExitCode != 0 {
		return fmt.Sprintf("dmg exited %d", res.ExitCode)
	}
	return ""
}

// setMsCondition reports whether the management service has a leader. `dmg system
// query` is served from a replica's local copy and answers without quorum, so
// membership alone would show a healthy system while no write can be accepted (#34).
func (r *DaosSystemReconciler) setMsCondition(sys *daosv1alpha1.DaosSystem, st *daosv1alpha1.DaosSystemStatus, env *dmg.Envelope) {
	was := metav1.ConditionUnknown
	if c := findCond(st, daosv1alpha1.ConditionManagementService); c != nil {
		was = c.Status
	}
	status, reason, msg := metav1.ConditionUnknown, "QueryError", ""
	switch info, err := dmg.LeaderQuery(env); {
	case env.Error != nil:
		msg = *env.Error
	case err != nil:
		msg = err.Error()
	case info == nil:
		reason, msg = "NoAnswer", "dmg system leader-query returned no response"
	case info.CurrentLeader == "":
		status, reason = metav1.ConditionFalse, "NoQuorum"
		msg = fmt.Sprintf("no replica holds the leadership; %d/%d replicas answer",
			len(info.Replicas)-len(info.DownReplicas), len(info.Replicas))
		if len(info.DownReplicas) > 0 {
			msg += " (down: " + strings.Join(info.DownReplicas, ",") + ")"
		}
		msg += "; membership queries still answer from a replica's local copy"
	default:
		status, reason = metav1.ConditionTrue, "Leader"
		msg = fmt.Sprintf("leader %s, %d replicas", info.CurrentLeader, len(info.Replicas))
		if len(info.DownReplicas) > 0 {
			msg += "; down: " + strings.Join(info.DownReplicas, ",")
		}
	}
	setCond(st, daosv1alpha1.ConditionManagementService, status, reason, msg)
	if status != was && status != metav1.ConditionTrue {
		r.event(sys, corev1.EventTypeWarning, "ManagementServiceDegraded", msg)
	}
}

func findCond(st *daosv1alpha1.DaosSystemStatus, t string) *metav1.Condition {
	for i := range st.Conditions {
		if st.Conditions[i].Type == t {
			return &st.Conditions[i]
		}
	}
	return nil
}
