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
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	daosv1alpha1 "github.com/gluesys/daos-operator/api/v1alpha1"
)

// engineDownHint is where the cause of a dead engine is: daos_server keeps running and the
// pod stays Ready, so neither pod status nor the server's exit message carries it (#37).
const engineDownHint = "the server pod is up but its engine is not: read /var/log/daos/daos_engine.*.log " +
	"in that pod (kubectl exec). A common cause is hugepages exhausted on the node (DER_NOMEM)"

// membershipKnown reports whether status.ranks is an answer from the management service and
// not the placeholder left right after a format.
func membershipKnown(status *daosv1alpha1.DaosSystemStatus) bool {
	c := findCond(status, daosv1alpha1.ConditionFormatted)
	return c != nil && c.Status == metav1.ConditionTrue && !strings.HasSuffix(c.Message, "querying membership")
}

// setEnginesCondition compares, per node with a Ready server pod, the engines the spec runs
// there with the ranks that joined from that node.
func setEnginesCondition(sys *daosv1alpha1.DaosSystem, status *daosv1alpha1.DaosSystemStatus, serversAllReady bool) {
	switch {
	case !serverEnabled(sys):
		setCond(status, daosv1alpha1.ConditionEnginesReady, metav1.ConditionFalse, "ServersDisabled", "spec.server.enabled=false")
		return
	case !serversAllReady:
		setCond(status, daosv1alpha1.ConditionEnginesReady, metav1.ConditionUnknown, "ServersNotReady", "not every server pod is ready; see condition ServersReady")
		return
	case !membershipKnown(status):
		setCond(status, daosv1alpha1.ConditionEnginesReady, metav1.ConditionUnknown, "MembershipUnknown", "the management service has not reported the ranks; see condition Formatted")
		return
	}
	want := len(sys.Spec.Engines)
	joined, others := map[string]int{}, map[string][]string{}
	for _, r := range status.Ranks {
		if r.State == memberJoined {
			joined[r.Node]++
		} else {
			others[r.Node] = append(others[r.Node], fmt.Sprintf("rank %d %s", r.Rank, r.State))
		}
	}
	var down []string
	nodes, total := 0, 0
	for _, nc := range status.NodeConfigs {
		if nc.Workload == "" || !nc.ServerReady {
			continue
		}
		nodes++
		total += joined[nc.Node]
		if joined[nc.Node] >= want {
			continue
		}
		why := "no rank registered: not formatted yet, or the engine died before joining"
		if o := others[nc.Node]; len(o) > 0 {
			why = strings.Join(o, ", ")
		}
		down = append(down, fmt.Sprintf("%s: %d/%d engine(s) joined (%s)", nc.Node, joined[nc.Node], want, why))
	}
	if len(down) > 0 {
		sort.Strings(down)
		setCond(status, daosv1alpha1.ConditionEnginesReady, metav1.ConditionFalse, "EnginesDown", strings.Join(down, "; ")+"; "+engineDownHint)
		return
	}
	setCond(status, daosv1alpha1.ConditionEnginesReady, metav1.ConditionTrue, "AllJoined", fmt.Sprintf("%d/%d engine(s) joined on %d node(s)", total, want*nodes, nodes))
}
