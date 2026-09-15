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
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	daosv1alpha1 "gitlab.gluesys.com/exastor/daos-operator/api/v1alpha1"
	"gitlab.gluesys.com/exastor/daos-operator/internal/dmg"
)

// Rank membership operations (#20).
//
// Draining, excluding and reintegrating a rank are decisions about where data
// lives, so they are requests a human makes -- `kubectl daos rank ...` writes
// daos.gluesys.com/rank-op: "<op>:<ranks>" after showing what it will do. The
// operator runs the matching `dmg system ...` once, copies the per-rank result
// into status.lastRankOp, and drops the annotation. It never decides by itself
// that a rank should leave or rejoin: a rank that dies is reported, not evicted.

const jobRankOpSuffix = "-dmg-rankop"

var (
	rankSetRe = regexp.MustCompile(`^[0-9]+(-[0-9]+)?(,[0-9]+(-[0-9]+)?)*$`)
	// the operations the operator will run; everything else is refused with a message
	rankOps = map[string]string{
		"drain":         "migrate data off the ranks (graceful; pools stay redundant)",
		"exclude":       "mark the ranks down now; every pool on them rebuilds",
		"reintegrate":   "bring the ranks back into their pools; data rebuilds onto them",
		"clear-exclude": "clear an administrative exclusion so the ranks may rejoin",
	}
)

// parseRankOp splits "<op>:<ranks>" and validates both halves.
func parseRankOp(v string) (op, ranks string, err error) {
	i := strings.IndexByte(v, ':')
	if i <= 0 || i == len(v)-1 {
		return "", "", fmt.Errorf("want \"<op>:<ranks>\", e.g. drain:2 or exclude:1,3-4")
	}
	op, ranks = strings.TrimSpace(v[:i]), strings.TrimSpace(v[i+1:])
	if _, ok := rankOps[op]; !ok {
		var names []string
		for k := range rankOps {
			names = append(names, k)
		}
		return "", "", fmt.Errorf("unknown operation %q (want one of %s)", op, strings.Join(names, ", "))
	}
	if !rankSetRe.MatchString(ranks) {
		return "", "", fmt.Errorf("%q is not a rank set (ranks are numbers and ranges: 1 or 0,2 or 1-3)", ranks)
	}
	return op, ranks, nil
}

func (r *DaosSystemReconciler) clearRankOp(ctx context.Context, sys *daosv1alpha1.DaosSystem) error {
	if _, ok := sys.Annotations[daosv1alpha1.AnnotationRankOp]; !ok {
		return nil
	}
	patch := client.MergeFrom(sys.DeepCopy())
	delete(sys.Annotations, daosv1alpha1.AnnotationRankOp)
	return r.Patch(ctx, sys, patch)
}

// reconcileRankOp runs a requested rank operation. It returns how soon to look
// again; 0 means there is nothing in flight.
func (r *DaosSystemReconciler) reconcileRankOp(ctx context.Context, sys *daosv1alpha1.DaosSystem, ns string, status *daosv1alpha1.DaosSystemStatus) (time.Duration, error) {
	req, ok := sys.Annotations[daosv1alpha1.AnnotationRankOp]
	if !ok || r.Dmg == nil {
		return 0, nil
	}
	now := metav1.Now()
	record := func(op, ranks string, ok bool, msg string) (time.Duration, error) {
		status.LastRankOp = &daosv1alpha1.RankOpStatus{Op: op, Ranks: ranks, RequestedAt: &now, FinishedAt: &now, Succeeded: ok, Message: msg}
		typ, reason := corev1.EventTypeNormal, "RankOpCompleted"
		if !ok {
			typ, reason = corev1.EventTypeWarning, "RankOpFailed"
		}
		r.event(sys, typ, reason, msg)
		return 0, r.clearRankOp(ctx, sys)
	}
	op, ranks, err := parseRankOp(req)
	if err != nil {
		return record(req, "", false, fmt.Sprintf("rejected %s=%q: %v", daosv1alpha1.AnnotationRankOp, req, err))
	}
	if !status.Formatted {
		return record(op, ranks, false, "refused: the system is not formatted, so it has no ranks to operate on")
	}

	res, err := r.runDmg(ctx, sys, ns, jobRankOpSuffix, "system", op, "--ranks="+ranks)
	if err != nil {
		return 0, fmt.Errorf("rank op job: %w", err)
	}
	if !res.Done {
		if status.LastRankOp == nil || status.LastRankOp.FinishedAt != nil {
			status.LastRankOp = &daosv1alpha1.RankOpStatus{Op: op, Ranks: ranks, RequestedAt: &now, Message: "dmg system " + op + " is running"}
		}
		return requeueProbe, nil
	}
	env, perr := parseResult(res)
	if perr != nil {
		return record(op, ranks, false, fmt.Sprintf("dmg system %s %s: %v", op, ranks, perr))
	}
	if env.Error != nil {
		return record(op, ranks, false, fmt.Sprintf("dmg system %s %s: %s", op, ranks, *env.Error))
	}
	results, err := dmg.RankOp(env)
	if err != nil {
		return record(op, ranks, false, err.Error())
	}
	var failed []string
	for _, rr := range results {
		if rr.Errored {
			where := ""
			if rr.Pool != "" {
				where = " (pool " + rr.Pool + ")"
			}
			failed = append(failed, fmt.Sprintf("rank %d%s: %s", rr.Rank, where, rr.Msg))
		}
	}
	// the membership we report next comes from dmg system query, not from here
	clearProbeHold(sys.Name)
	if len(failed) > 0 {
		return record(op, ranks, false, fmt.Sprintf("dmg system %s %s: %s", op, ranks, strings.Join(failed, "; ")))
	}
	return record(op, ranks, true, fmt.Sprintf("dmg system %s %s: %d rank result(s), no errors", op, ranks, len(results)))
}
