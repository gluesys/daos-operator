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

package discovery

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	daosv1alpha1 "gitlab.gluesys.com/exastor/daos-operator/api/v1alpha1"
)

func node(name, ip string, ann map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: ann},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}}}}
}

func TestFromNodeAndConflicts(t *testing.T) {
	a := FromNode(node("cell1", "10.0.0.1", map[string]string{
		daosv1alpha1.AnnotationFabricIface: "ens2",
		daosv1alpha1.AnnotationBdevList:    "0000:02:00.0, 0000:03:00.0",
		daosv1alpha1.AnnotationBdevDSN:     "0000:02:00.0=DSN-A,0000:03:00.0=DSN-B",
		daosv1alpha1.AnnotationNumaNode:    "1",
	}))
	b := FromNode(node("cell2", "10.0.0.2", map[string]string{
		daosv1alpha1.AnnotationFabricIface: "ens2np0",
		daosv1alpha1.AnnotationBdevList:    "0000:02:00.0",
		daosv1alpha1.AnnotationBdevDSN:     "0000:02:00.0=DSN-A", // same drive as cell1 (dual-port chassis)
	}))
	if a.FabricIface != "ens2" || len(a.Bdevs) != 2 || a.NumaNode == nil || *a.NumaNode != 1 || a.ControlAddr != "10.0.0.1" {
		t.Fatalf("bad facts: %+v", a)
	}
	c := DriveConflicts([]NodeFacts{a, b})
	if nodes, ok := c["DSN-A"]; !ok || len(nodes) != 2 {
		t.Fatalf("expected DSN-A conflict between two nodes, got %v", c)
	}
	if _, ok := c["DSN-B"]; ok {
		t.Fatalf("DSN-B must not conflict")
	}
	fabric, bdevs, _, missing := Resolve(daosv1alpha1.EngineSpec{}, FromNode(node("bare", "", nil)))
	if fabric != "" || len(bdevs) != 0 || len(missing) != 2 {
		t.Fatalf("expected two missing facts, got fabric=%q bdevs=%v missing=%v", fabric, bdevs, missing)
	}
	fabric, _, _, _ = Resolve(daosv1alpha1.EngineSpec{FabricIface: "ib0"}, a)
	if fabric != "ib0" {
		t.Fatalf("spec must override annotation")
	}
}
