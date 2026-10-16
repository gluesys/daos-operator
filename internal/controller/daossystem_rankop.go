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
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	daosv1alpha1 "github.com/gluesys/daos-operator/api/v1alpha1"
	"github.com/gluesys/daos-operator/internal/dmg"
)

// Rank membership operations (#20).
//
// Draining, excluding and reintegrating a rank are decisions about where data
// lives, so they are requests a human makes -- `kubectl daos rank ...` writes
// daos.gluesys.com/rank-op: "<op>:<ranks>" after showing what it will do. The
// operator runs the matching `dmg system ...` once (re-sent only while the
// management service cannot be reached), copies the per-rank result
// into status.lastRankOp, and drops the annotation. It never decides by itself
// that a rank should leave or rejoin: a rank that dies is reported, not evicted.

const jobRankOpSuffix = "-dmg-rankop"

// DER_UNREACH: dmg could not reach the management service, so DAOS never saw
// the request. Sent while the server pods cycle (CI, 2026-10-03), a one-shot
// clear-exclude failed this way and the rank never came back (#9). Such a
// request is safe to send again; anything DAOS answered is not retried.
const dmgStatusUnreachable = -1009

var (
	rankOpRetryWindow = 3 * time.Minute
	rankOpRetryDelay  = 20 * time.Second
)

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

// rankOpPrecondition refuses an operation DAOS would only fail at after a long
// wait, and says what to do first. Learned on da1~4 (#30): excluding a rank
// stops its engine (it is refused at join), so a later reintegrate finds nothing
// to drain and returns only after the 5 minute SystemDrainReq timeout.
//
// The test is whether the ENGINE is back, not what the rank state says. A rank
// stays "excluded" after its engine returns -- DAOS keeps it out until something
// reintegrates it -- so refusing on the state alone blocks the one operation
// that can clear it, and the documented recovery (restart the server pod, then
// reintegrate) can never finish. Measured on cxl2, 2026-10-02: engine up and
// serving on rank 1, rank still "excluded", and this guard refused forever.
func rankOpPrecondition(op, ranks string, status *daosv1alpha1.DaosSystemStatus) string {
	if op != "reintegrate" {
		return ""
	}
	ready := map[string]bool{}
	for _, nc := range status.NodeConfigs {
		ready[nc.Node] = nc.ServerReady
	}
	want := parseRankSet(ranks)
	var admin, down []string
	for _, rk := range status.Ranks {
		if !want[rk.Rank] {
			continue
		}
		switch rk.State {
		case "adminexcluded":
			admin = append(admin, fmt.Sprintf("%d", rk.Rank))
		case "excluded", "errored", "unknown", "stopped", "awaitformat":
			if ready[rk.Node] {
				continue // engine is back; reintegrate is exactly what is wanted
			}
			down = append(down, fmt.Sprintf("%d (%s, on %s)", rk.Rank, rk.State, rk.Node))
		}
	}
	if len(admin) > 0 {
		return fmt.Sprintf("refused: rank(s) %s are administratively excluded; DAOS will not reintegrate them. "+
			"Clear the exclusion first (%s=clear-exclude:%s), restart that node's server pod so the engine rejoins, "+
			"then reintegrate", strings.Join(admin, ","), daosv1alpha1.AnnotationRankOp, strings.Join(admin, ","))
	}
	if len(down) > 0 {
		return fmt.Sprintf("refused: rank(s) %s have no running engine (their server pod is not ready); reintegrate "+
			"would wait out the 5 minute drain timeout and fail. Restart that node's server pod first, then reintegrate",
			strings.Join(down, ", "))
	}
	return ""
}

// unprotectedPools names the pools a reintegrate of ranks would empty: rd_fac 0 with
// data_sync reintegration (or a mode the operator does not know, DAOS's default being
// data_sync) on any of those ranks. DAOS discards the returning ranks' data and rebuilds it
// from other copies, and these pools have none; its maintainers say such a reintegration
// should not be attempted (daos@daos.groups.io, 2026-10-12). A pool whose spec chose
// properties.reintegration: data_sync accepted that loss when it was created.
func unprotectedPools(ranks string, pools []daosv1alpha1.DaosPool) []string {
	want := parseRankSet(ranks)
	var out []string
	for _, p := range pools {
		if redundancyFactor(&p) != 0 { // profile kvcache makes it 0 without spec.redundancyFactor
			continue
		}
		if m := p.Status.ReintegrationMode; m != "" && m != "data_sync" {
			continue
		}
		if p.Spec.Properties["reintegration"] == "data_sync" && p.Status.ReintegrationMode == "data_sync" {
			continue
		}
		on := len(p.Spec.Ranks) == 0 // empty = every rank
		for _, rk := range p.Spec.Ranks {
			on = on || want[rk]
		}
		if on {
			mode := p.Status.ReintegrationMode
			if mode == "" {
				mode = "unknown, DAOS default data_sync"
			}
			out = append(out, fmt.Sprintf("%s (reintegration %s)", p.Name, mode))
		}
	}
	return out
}

// kvcachePoolsOn names the kvcache pools without redundancy that span any of
// ranks. Excluding such a rank is the one thing their profile exists to avoid:
// the system's self_heal (pool_exclude) then takes the rank's targets out of
// the pool, and with rd_fac 0 the data on them is gone -- whereas a rank left
// alone through a reboot simply rejoins and its blocks hit again.
func kvcachePoolsOn(ranks string, pools []daosv1alpha1.DaosPool) []string {
	want := parseRankSet(ranks)
	var out []string
	for _, p := range pools {
		if p.Spec.Profile != daosv1alpha1.PoolProfileKVCache || kvcacheRedundancy(&p) != "none" {
			continue
		}
		on := len(p.Spec.Ranks) == 0 // empty = every rank
		for _, rk := range p.Spec.Ranks {
			on = on || want[rk]
		}
		if on {
			out = append(out, p.Name)
		}
	}
	return out
}

// parseRankSet expands "0,2-3" into a set.
func parseRankSet(ranks string) map[int32]bool {
	out := map[int32]bool{}
	for _, part := range strings.Split(ranks, ",") {
		lo, hi, found := strings.Cut(part, "-")
		a, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			continue
		}
		b := a
		if found {
			if v, err := strconv.Atoi(strings.TrimSpace(hi)); err == nil {
				b = v
			}
		}
		for i := a; i <= b; i++ {
			out[int32(i)] = true
		}
	}
	return out
}

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
	delete(sys.Annotations, daosv1alpha1.AnnotationRankOpDiscardData)
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
	// when the request was first seen: kept across the job's run and its retries
	requested := func(op, ranks string) *metav1.Time {
		if lr := status.LastRankOp; lr != nil && lr.FinishedAt == nil && lr.Op == op && lr.Ranks == ranks && lr.RequestedAt != nil {
			return lr.RequestedAt
		}
		return &now
	}
	record := func(op, ranks string, ok bool, msg string) (time.Duration, error) {
		status.LastRankOp = &daosv1alpha1.RankOpStatus{Op: op, Ranks: ranks, RequestedAt: requested(op, ranks), FinishedAt: &now, Succeeded: ok, Message: msg}
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
	if why := rankOpPrecondition(op, ranks, status); why != "" {
		return record(op, ranks, false, why)
	}
	if (op == "reintegrate" || op == "exclude") && sys.Annotations[daosv1alpha1.AnnotationRankOpDiscardData] != ranks {
		pools := &daosv1alpha1.DaosPoolList{}
		if err := r.List(ctx, pools); err != nil {
			return 0, fmt.Errorf("list pools: %w", err)
		}
		var mine []daosv1alpha1.DaosPool
		for _, p := range pools.Items {
			if p.Spec.SystemRef == sys.Name {
				mine = append(mine, p)
			}
		}
		if names := kvcachePoolsOn(ranks, mine); op == "exclude" && len(names) > 0 {
			return record(op, ranks, false, fmt.Sprintf("refused: excluding rank(s) %s takes them out of kvcache pool(s) %s, "+
				"which have no redundancy: the system's self_heal then excludes their targets from the pool and the data on "+
				"them is lost. A rank that reboots does not need this -- it rejoins by itself and its blocks hit again. To "+
				"accept the loss, set %s=%s with the request (kubectl daos rank exclude %s --ranks=%s --discard-data)",
				ranks, strings.Join(names, ", "), daosv1alpha1.AnnotationRankOpDiscardData, ranks, sys.Name, ranks))
		}
		if names := unprotectedPools(ranks, mine); op == "reintegrate" && len(names) > 0 {
			return record(op, ranks, false, fmt.Sprintf("refused: reintegrating rank(s) %s would discard their data in pool(s) %s: "+
				"DAOS empties the returning ranks before it rebuilds, and rd_fac 0 has no other copy. To accept that loss, "+
				"set %s=%s with the request (kubectl daos rank reintegrate %s --ranks=%s --discard-data)",
				ranks, strings.Join(names, ", "), daosv1alpha1.AnnotationRankOpDiscardData, ranks, sys.Name, ranks))
		}
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
		msg := fmt.Sprintf("dmg system %s %s: %s", op, ranks, *env.Error)
		// drain and reintegrate act on the pools that hold the ranks; with none
		// (no pool at all, or the ranks already enabled/disabled) DAOS has
		// nothing to do and says so as an error (#14)
		if (op == "drain" || op == "reintegrate") && strings.Contains(*env.Error, "no pool-ranks found to operate on") {
			return record(op, ranks, true, fmt.Sprintf("dmg system %s %s: nothing to do, no pool has these ranks to %s", op, ranks, op))
		}
		if since := requested(op, ranks); env.Status == dmgStatusUnreachable && time.Since(since.Time) < rankOpRetryWindow {
			status.LastRankOp = &daosv1alpha1.RankOpStatus{Op: op, Ranks: ranks, RequestedAt: since,
				Message: fmt.Sprintf("%s; retrying (the request never reached DAOS; gives up %s after it was made)", msg, rankOpRetryWindow)}
			r.event(sys, corev1.EventTypeWarning, "RankOpRetrying", status.LastRankOp.Message)
			return rankOpRetryDelay, nil
		}
		return record(op, ranks, false, msg)
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
