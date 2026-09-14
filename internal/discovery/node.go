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

// Package discovery reads per-node DAOS facts from Node annotations and checks
// them for the one mistake that has already cost us data: two nodes claiming
// the same physical NVMe through a dual-port chassis.
package discovery

import (
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	daosv1alpha1 "gitlab.gluesys.com/exastor/daos-operator/api/v1alpha1"
)

// NodeFacts is what the operator knows about one node.
type NodeFacts struct {
	Name        string
	ControlAddr string            // InternalIP unless overridden
	FabricIface string            // may be empty
	Bdevs       []string          // PCI addresses, may be empty
	DSN         map[string]string // pci -> serial, may be empty
	NumaNode    *int32
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// FromNode extracts facts; missing annotations leave fields empty.
func FromNode(n *corev1.Node) NodeFacts {
	f := NodeFacts{Name: n.Name, DSN: map[string]string{}}
	ann := n.Annotations
	f.FabricIface = strings.TrimSpace(ann[daosv1alpha1.AnnotationFabricIface])
	f.Bdevs = splitList(ann[daosv1alpha1.AnnotationBdevList])
	for _, kv := range splitList(ann[daosv1alpha1.AnnotationBdevDSN]) {
		if i := strings.IndexByte(kv, '='); i > 0 {
			f.DSN[strings.TrimSpace(kv[:i])] = strings.TrimSpace(kv[i+1:])
		}
	}
	if v := strings.TrimSpace(ann[daosv1alpha1.AnnotationNumaNode]); v != "" {
		var numa int32
		for _, c := range v {
			if c < '0' || c > '9' {
				numa = -1
				break
			}
			numa = numa*10 + int32(c-'0')
		}
		if numa >= 0 {
			f.NumaNode = &numa
		}
	}
	if v := strings.TrimSpace(ann[daosv1alpha1.AnnotationControlAddr]); v != "" {
		f.ControlAddr = v
	} else {
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP {
				f.ControlAddr = a.Address
				break
			}
		}
	}
	if f.ControlAddr == "" {
		f.ControlAddr = n.Name
	}
	return f
}

// DriveConflicts returns dsn -> sorted node names for every serial seen on more
// than one node.
func DriveConflicts(facts []NodeFacts) map[string][]string {
	seen := map[string][]string{}
	for _, f := range facts {
		for _, dsn := range f.DSN {
			if dsn == "" {
				continue
			}
			seen[dsn] = append(seen[dsn], f.Name)
		}
	}
	out := map[string][]string{}
	for dsn, nodes := range seen {
		if len(nodes) > 1 {
			sort.Strings(nodes)
			out[dsn] = nodes
		}
	}
	return out
}

// Resolve merges the engine spec with node facts. Spec values win when set;
// otherwise the annotation is used. Missing lists the fields still unknown.
func Resolve(spec daosv1alpha1.EngineSpec, f NodeFacts) (fabric string, bdevs []string, numa *int32, missing []string) {
	fabric = spec.FabricIface
	if fabric == "" {
		fabric = f.FabricIface
	}
	bdevs = spec.BdevList
	if len(bdevs) == 0 {
		bdevs = f.Bdevs
	}
	numa = spec.PinnedNumaNode
	if numa == nil {
		numa = f.NumaNode
	}
	if fabric == "" {
		missing = append(missing, daosv1alpha1.AnnotationFabricIface)
	}
	if len(bdevs) == 0 {
		missing = append(missing, daosv1alpha1.AnnotationBdevList)
	}
	return
}
