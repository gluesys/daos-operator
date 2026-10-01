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
	"strings"
	"testing"

	daosv1alpha1 "gitlab.gluesys.com/exastor/daos-operator/api/v1alpha1"
)

// The gate exists to refuse a reintegrate that would only time out. It must not
// refuse the one case reintegrate is for: a rank that DAOS excluded and whose
// engine is back. The rank stays "excluded" until something reintegrates it, so
// gating on the rank state alone makes the documented recovery -- restart the
// server pod, then reintegrate -- impossible to finish (seen on cxl2, 2026-10-02).
func TestRankOpPrecondition(t *testing.T) {
	st := func(state, node string, ready bool) *daosv1alpha1.DaosSystemStatus {
		return &daosv1alpha1.DaosSystemStatus{
			Ranks:       []daosv1alpha1.RankStatus{{Rank: 1, Node: node, State: state}},
			NodeConfigs: []daosv1alpha1.NodeConfigStatus{{Node: node, ServerReady: ready}},
		}
	}

	cases := []struct {
		name, op, state string
		ready           bool
		wantRefusal     string // "" means it must pass the gate
	}{
		{"excluded, engine back -> allowed", "reintegrate", "excluded", true, ""},
		{"errored, engine back -> allowed", "reintegrate", "errored", true, ""},
		{"excluded, pod not ready -> refused", "reintegrate", "excluded", false, "no running engine"},
		{"awaitformat, pod not ready -> refused", "reintegrate", "awaitformat", false, "no running engine"},
		{"adminexcluded -> refused even with the engine up", "reintegrate", "adminexcluded", true, "administratively excluded"},
		{"joined -> allowed", "reintegrate", "joined", true, ""},
		{"other ops are never gated", "drain", "excluded", false, ""},
	}
	for _, c := range cases {
		got := rankOpPrecondition(c.op, "1", st(c.state, "n1", c.ready))
		switch {
		case c.wantRefusal == "" && got != "":
			t.Errorf("%s: expected the gate to pass, got refusal %q", c.name, got)
		case c.wantRefusal != "" && !strings.Contains(got, c.wantRefusal):
			t.Errorf("%s: expected a refusal mentioning %q, got %q", c.name, c.wantRefusal, got)
		}
	}

	// A rank not named by the request is not consulted at all.
	s := st("excluded", "n1", false)
	s.Ranks = append(s.Ranks, daosv1alpha1.RankStatus{Rank: 2, Node: "n2", State: "joined"})
	if got := rankOpPrecondition("reintegrate", "2", s); got != "" {
		t.Errorf("rank 2 requested, rank 1 unhealthy: expected pass, got %q", got)
	}
}
