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
	"sigs.k8s.io/controller-runtime/pkg/client"

	daosv1alpha1 "gitlab.gluesys.com/exastor/daos-operator/api/v1alpha1"
	"gitlab.gluesys.com/exastor/daos-operator/internal/dmg"
)

// Engine restart (design §13 #24).
//
// daos_server restarts an engine only after the engine terminated itself (DAOS 2.8
// engine_self_terminated). An engine that crashes -- signal 11 in libfabric verbs at start,
// seen on a recreated server pod (CI, 2026-10-05 exaci5-3b) -- stays down while daos_server
// and the pod stay up, so the rank never returns and a later reintegrate only times out.
// A liveness probe cannot tell that crash from an engine the upgrade stopped on purpose
// (dmg system stop): the difference is the rank state, which only the management service
// knows. So the operator asks DAOS to start it: `dmg system start --ranks=N` starts only
// engines that are not running (StartRanks skips started ones) and the management service
// leaves admin-excluded ranks alone. A stopped rank (dmg system stop) is never a candidate.
//
// That start cannot help when the engine died before it reported ready: daos_server 2.8 then
// waits in waitReady for a signal that never comes, never handles the exit, and every later
// start times out after 5 minutes (CI lane A, 2026-10-06: engine restarted by dmg crashed at
// start, #24). Only a new daos_server clears it, so a start that times out or fails per rank
// restarts that node's server pod -- unless another rank on the node is still joined: the pod
// would take a healthy engine down with it, and that is left to a human.
//
// It only starts engines. Pool membership stays a human's decision (#20): a rank that comes
// back excluded still needs `kubectl daos rank reintegrate`.

const jobEngineStartSuffix = "-dmg-enginestart"

var (
	// a rank down this long gets its first start; covers an engine that is still starting
	engineRestartGrace = 2 * time.Minute
	// between starts of the same rank, as DAOS's own auto-restart (engine_auto_restart_min_delay)
	engineRestartInterval = 5 * time.Minute
	// starts per rank before the operator stops trying and says so
	engineRestartMax = 3
)

// rank states whose engine may have died: errored (engine_died event) and excluded (SWIM,
// or an engine that crashed before it rejoined). stopped and adminexcluded are deliberate.
var engineRestartStates = map[string]bool{"errored": true, "excluded": true}

// In memory, per system and rank, like the probe hold: a restart of the operator starts over.
type engineRestartTrack struct {
	downSince time.Time
	lastTry   time.Time
	tries     int
	gaveUp    bool
}

var (
	engineRestartMu      sync.Mutex
	engineRestartRanks   = map[string]map[int32]*engineRestartTrack{} // system -> rank
	engineRestartRunning = map[string]string{}                        // system -> rank set of the Job in flight
)

func resetEngineRestarts() {
	engineRestartMu.Lock()
	defer engineRestartMu.Unlock()
	engineRestartRanks = map[string]map[int32]*engineRestartTrack{}
	engineRestartRunning = map[string]string{}
}

// engineRestartCandidates lists the ranks whose engine looks dead while its server pod is
// Ready. Nothing is a candidate while a human rank operation is requested or running (the
// annotation stays until its Job finished).
func engineRestartCandidates(sys *daosv1alpha1.DaosSystem, status *daosv1alpha1.DaosSystemStatus) []daosv1alpha1.RankStatus {
	if !serverEnabled(sys) || !membershipKnown(status) {
		return nil
	}
	if _, ok := sys.Annotations[daosv1alpha1.AnnotationRankOp]; ok {
		return nil
	}
	ready := map[string]bool{}
	for _, nc := range status.NodeConfigs {
		ready[nc.Node] = nc.Workload != "" && nc.ServerReady
	}
	var out []daosv1alpha1.RankStatus
	for _, rk := range status.Ranks {
		if engineRestartStates[rk.State] && ready[rk.Node] {
			out = append(out, rk)
		}
	}
	return out
}

// reconcileEngineRestart starts engines that died under a Ready server pod. It returns how
// soon to look again; 0 means nothing is pending.
func (r *DaosSystemReconciler) reconcileEngineRestart(ctx context.Context, sys *daosv1alpha1.DaosSystem, ns string, status *daosv1alpha1.DaosSystemStatus) (time.Duration, error) {
	if r.Dmg == nil {
		return 0, nil
	}
	now := time.Now()
	cands := engineRestartCandidates(sys, status)

	engineRestartMu.Lock()
	tracks := engineRestartRanks[sys.Name]
	if tracks == nil {
		tracks = map[int32]*engineRestartTrack{}
		engineRestartRanks[sys.Name] = tracks
	}
	// a rank that joined (or left the membership) starts over next time it goes down; one that
	// is only out of the candidates for now -- its pod restarting -- keeps its count
	stillDown := map[int32]bool{}
	for _, rk := range status.Ranks {
		if rk.State != memberJoined {
			stillDown[rk.Rank] = true
		}
	}
	for _, rk := range cands {
		if tracks[rk.Rank] == nil {
			tracks[rk.Rank] = &engineRestartTrack{downSince: now}
		}
	}
	for rank := range tracks {
		if !stillDown[rank] {
			delete(tracks, rank)
		}
	}
	running := engineRestartRunning[sys.Name]
	var due []int32
	var gaveUp []string
	next := time.Duration(0)
	if running == "" {
		for _, rk := range cands {
			t := tracks[rk.Rank]
			switch {
			case t.tries >= engineRestartMax:
				if !t.gaveUp {
					t.gaveUp = true
					gaveUp = append(gaveUp, fmt.Sprintf("rank %d (%s, on %s)", rk.Rank, rk.State, rk.Node))
				}
			case now.Sub(t.downSince) < engineRestartGrace:
				next = minRequeue(next, engineRestartGrace-now.Sub(t.downSince))
			case t.tries > 0 && now.Sub(t.lastTry) < engineRestartInterval:
				next = minRequeue(next, engineRestartInterval-now.Sub(t.lastTry))
			default:
				due = append(due, rk.Rank)
			}
		}
		if len(due) > 0 {
			sort.Slice(due, func(a, b int) bool { return due[a] < due[b] })
			running = rankList(due)
			engineRestartRunning[sys.Name] = running
			for _, rank := range due {
				tracks[rank].tries++
				tracks[rank].lastTry = now
			}
		}
	}
	tries := map[int32]int{}
	for rank, t := range tracks {
		tries[rank] = t.tries
	}
	engineRestartMu.Unlock()

	for _, g := range gaveUp {
		r.event(sys, corev1.EventTypeWarning, "EngineRestartGaveUp", fmt.Sprintf("%s still down after %d start attempt(s); "+
			"read the engine log on that node (%s). If its engine is running again, the rank waits for "+
			"kubectl daos rank reintegrate", g, engineRestartMax, "/var/log/daos/daos_engine.*.log"))
	}
	noteEngineRestarts(status, cands, tries)
	if running == "" {
		return next, nil
	}
	if len(due) > 0 {
		r.event(sys, corev1.EventTypeNormal, "EngineRestartRequested", fmt.Sprintf("dmg system start --ranks=%s: "+
			"the engine is down while its server pod is Ready", running))
	}

	res, err := r.runDmg(ctx, sys, ns, jobEngineStartSuffix, "system", "start", "--ranks="+running)
	if err != nil {
		return 0, fmt.Errorf("engine start job: %w", err)
	}
	if !res.Done {
		return requeueProbe, nil
	}
	engineRestartMu.Lock()
	delete(engineRestartRunning, sys.Name)
	engineRestartMu.Unlock()
	// the membership we report next comes from dmg system query, not from here
	clearProbeHold(sys.Name)

	msg, ok, stuck := engineStartOutcome(res, running)
	typ, reason := corev1.EventTypeNormal, "EngineRestartSent"
	if !ok {
		typ, reason = corev1.EventTypeWarning, "EngineRestartFailed"
	}
	r.event(sys, typ, reason, msg)
	if stuck {
		if err := r.restartStuckServers(ctx, sys, ns, status, running); err != nil {
			return 0, err
		}
	}
	return requeueProbe, nil
}

// restartStuckServers deletes the server pod of each node whose engines the start could not
// bring back, when no rank on that node is joined. The StatefulSet recreates the pod.
func (r *DaosSystemReconciler) restartStuckServers(ctx context.Context, sys *daosv1alpha1.DaosSystem, ns string, status *daosv1alpha1.DaosSystemStatus, ranks string) error {
	want := parseRankSet(ranks)
	stuck, healthy := map[string][]int32{}, map[string]bool{}
	for _, rk := range status.Ranks {
		if want[rk.Rank] {
			stuck[rk.Node] = append(stuck[rk.Node], rk.Rank)
		} else if rk.State == memberJoined {
			healthy[rk.Node] = true
		}
	}
	pods, err := r.serverPods(ctx, sys, ns)
	if err != nil {
		return err
	}
	nodes := make([]string, 0, len(stuck))
	for n := range stuck {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	for _, node := range nodes {
		rs := rankList(stuck[node])
		if healthy[node] {
			r.event(sys, corev1.EventTypeWarning, "EngineRestartNeedsPodRestart", fmt.Sprintf("rank(s) %s on %s did not start and "+
				"daos_server may be stuck on them; restarting the server pod would also stop the joined engine(s) there, "+
				"so the operator leaves it: delete the pod when that is acceptable", rs, node))
			continue
		}
		for i := range pods {
			p := &pods[i]
			if p.Labels[daosv1alpha1.LabelNode] != node || !p.DeletionTimestamp.IsZero() {
				continue
			}
			if err := r.Delete(ctx, p); client.IgnoreNotFound(err) != nil {
				return err
			}
			r.event(sys, corev1.EventTypeWarning, "EngineRestartPodDeleted", fmt.Sprintf("rank(s) %s on %s did not start: "+
				"daos_server does not recover an engine that died before it was ready, deleted %s so a new one starts it", rs, node, p.Name))
		}
		// the next start counts from the new pod, not from the start that timed out
		engineRestartMu.Lock()
		for _, rank := range stuck[node] {
			if t := engineRestartRanks[sys.Name][rank]; t != nil {
				t.lastTry = time.Now()
			}
		}
		engineRestartMu.Unlock()
	}
	return nil
}

// engineStartOutcome summarizes a finished `dmg system start` Job. stuck means DAOS was reached
// but did not start the engines: the request timed out or a rank reported an error.
func engineStartOutcome(res *dmg.Result, ranks string) (msg string, ok, stuck bool) {
	env, perr := parseResult(res)
	if perr != nil {
		return fmt.Sprintf("dmg system start --ranks=%s: %v", ranks, perr), false, false
	}
	if env.Error != nil {
		return fmt.Sprintf("dmg system start --ranks=%s: %s", ranks, *env.Error), false, strings.Contains(*env.Error, "timed out")
	}
	results, err := dmg.RankOp(env)
	if err != nil {
		return fmt.Sprintf("dmg system start --ranks=%s: %v", ranks, err), false, false
	}
	var failed []string
	for _, rr := range results {
		if rr.Errored {
			failed = append(failed, fmt.Sprintf("rank %d: %s", rr.Rank, rr.Msg))
		}
	}
	if len(failed) > 0 {
		return fmt.Sprintf("dmg system start --ranks=%s: %s", ranks, strings.Join(failed, "; ")), false, true
	}
	return fmt.Sprintf("dmg system start --ranks=%s: %d rank result(s), no errors; the next membership query shows whether the engine joined", ranks, len(results)), true, false
}

// noteEngineRestarts appends what the operator did to the EnginesDown message.
func noteEngineRestarts(status *daosv1alpha1.DaosSystemStatus, cands []daosv1alpha1.RankStatus, tries map[int32]int) {
	c := findCond(status, daosv1alpha1.ConditionEnginesReady)
	if c == nil || c.Reason != "EnginesDown" {
		return
	}
	var notes []string
	for _, rk := range cands {
		if n := tries[rk.Rank]; n > 0 {
			notes = append(notes, fmt.Sprintf("rank %d %d/%d", rk.Rank, n, engineRestartMax))
		}
	}
	if len(notes) > 0 {
		c.Message += "; engine start requested by the operator: " + strings.Join(notes, ", ")
	}
}

func rankList(ranks []int32) string {
	s := make([]string, len(ranks))
	for i, r := range ranks {
		s[i] = fmt.Sprintf("%d", r)
	}
	return strings.Join(s, ",")
}
