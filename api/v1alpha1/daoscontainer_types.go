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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ContainerType is the DAOS container layout.
// +kubebuilder:validation:Enum=POSIX;PYTHON;HDF5;UNKNOWN
type ContainerType string

// DaosContainerSpec maps to `daos cont create`. Defaults come from the
// lmcache-daos measurements (chunk 4 MiB, RP_2GX); the CSI StorageClass
// parameters are passed through here.
//
// RP_2GX / RP_2G1 place two replicas in two fault domains (= two server
// nodes). On a single-node system `daos cont create` fails with
// "grp size (2) is larger than domain nr (1)" (seen on the FlexA test bed,
// 2026-09-15): use fileOclass SX / dirOclass S1 and redundancyFactor 0 there.
type DaosContainerSpec struct {
	PoolRef string `json:"poolRef"`
	// +kubebuilder:default=POSIX
	Type ContainerType `json:"type,omitempty"`
	// +kubebuilder:default="RP_2GX"
	FileOclass string `json:"fileOclass,omitempty"`
	// +kubebuilder:default="RP_2G1"
	DirOclass string `json:"dirOclass,omitempty"`
	// ChunkSize in bytes (DFS chunk). 4194304 measured best for KV chunks.
	// +kubebuilder:default=4194304
	ChunkSize int64 `json:"chunkSize,omitempty"`
	// RedundancyFactor overrides the pool's rd_fac when set.
	RedundancyFactor *int32 `json:"redundancyFactor,omitempty"`
	// Checksum type (2.8 default crc32). "off" disables.
	// +kubebuilder:default="crc32"
	Checksum   string            `json:"checksum,omitempty"`
	Properties map[string]string `json:"properties,omitempty"`
	ACL        []string          `json:"acl,omitempty"`
	// Label is the DAOS container label; defaults to metadata.name.
	Label string `json:"label,omitempty"`
}

// DaosContainerStatus mirrors `daos cont query`.
type DaosContainerStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	UUID       string             `json:"uuid,omitempty"`
	PoolUUID   string             `json:"poolUUID,omitempty"`
	Ready      bool               `json:"ready,omitempty"`
	Health     string             `json:"health,omitempty"`
	Type       string             `json:"type,omitempty"`
	// Operation is the daos Job in flight (create, acl, destroy); empty when idle.
	Operation          string       `json:"operation,omitempty"`
	ObservedGeneration int64        `json:"observedGeneration,omitempty"`
	AppliedACLHash     string       `json:"appliedACLHash,omitempty"`
	LastQueryTime      *metav1.Time `json:"lastQueryTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=daoscont
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolRef`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Health",type=string,JSONPath=`.status.health`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// DaosContainer is a DAOS container; the CSI controller creates one per PV.
type DaosContainer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DaosContainerSpec   `json:"spec,omitempty"`
	Status DaosContainerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DaosContainerList contains a list of DaosContainer.
type DaosContainerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DaosContainer `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &DaosContainer{}, &DaosContainerList{})
		return nil
	})
}
