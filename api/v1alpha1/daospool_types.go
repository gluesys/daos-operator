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

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// DaosPoolSpec maps to `dmg pool create`.
type DaosPoolSpec struct {
	// SystemRef names the DaosSystem this pool lives in.
	SystemRef string `json:"systemRef"`
	// Size is the total pool size (dmg pool create --size). Extension is done
	// by adding ranks, not by editing Size (DAOS 2.8 has no shrink).
	Size resource.Quantity `json:"size"`
	// Ranks restricts the pool to these ranks; empty = all. Appending ranks
	// triggers `dmg pool extend`.
	Ranks []int32 `json:"ranks,omitempty"`
	// RedundancyFactor is rd_fac (2.8 default 3; lmcache-daos used 0..2).
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4
	// +kubebuilder:default=2
	RedundancyFactor int32 `json:"redundancyFactor,omitempty"`
	// Properties are extra `--properties k:v` pairs (e.g. ec_cell_sz).
	Properties map[string]string `json:"properties,omitempty"`
	// ACL entries in DAOS ACE syntax (A::user@:rw). Declarative.
	ACL []string `json:"acl,omitempty"`
}

// DaosPoolStatus mirrors `dmg pool query`.
type DaosPoolStatus struct {
	Conditions      []metav1.Condition `json:"conditions,omitempty"`
	UUID            string             `json:"uuid,omitempty"`
	Label           string             `json:"label,omitempty"`
	State           string             `json:"state,omitempty"`
	FreeBytes       int64              `json:"freeBytes,omitempty"`
	TotalBytes      int64              `json:"totalBytes,omitempty"`
	RebuildState    string             `json:"rebuildState,omitempty"`
	DisabledTargets int32              `json:"disabledTargets,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=daospool
// +kubebuilder:printcolumn:name="System",type=string,JSONPath=`.spec.systemRef`
// +kubebuilder:printcolumn:name="Size",type=string,JSONPath=`.spec.size`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Rebuild",type=string,JSONPath=`.status.rebuildState`

// DaosPool is a DAOS pool.
type DaosPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DaosPoolSpec   `json:"spec,omitempty"`
	Status DaosPoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DaosPoolList contains a list of DaosPool.
type DaosPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DaosPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &DaosPool{}, &DaosPoolList{})
		return nil
	})
}
