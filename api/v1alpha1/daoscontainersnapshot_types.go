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

// DaosContainerSnapshotSpec maps to `daos cont create-snap`: a point-in-time,
// read-only view of one container (a DAOS epoch), taken once. DAOS 2.8 has no
// named snapshots (a name makes create-snap fail with -DER_NOSYS): a snapshot is
// known by its epoch only.
//
// DAOS 2.8 cannot roll a container back to a snapshot (daos_cont_rollback is
// not implemented, -DER_NOSYS). A snapshot is read by mounting it read-only
// (dfuse --snap) and copying what is needed out of it.
//
// A snapshot keeps the space of everything overwritten or deleted since it was
// taken: the pool does not get that space back until the snapshot is destroyed.
type DaosContainerSnapshotSpec struct {
	// ContainerRef is the DaosContainer, in this namespace, to snapshot.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="containerRef is immutable"
	ContainerRef string `json:"containerRef"`
	// Volume, when set, exposes the snapshot to pods as a read-only
	// PersistentVolumeClaim of that name in this namespace (ReadOnlyMany). The
	// operator creates the claim and a PersistentVolume bound to it, whose
	// reclaim policy is Retain: deleting either never touches the container.
	// Removing spec.volume removes them. Needs daos-csi with snapshotEpoch support.
	// +optional
	Volume *SnapshotVolumeSpec `json:"volume,omitempty"`
}

// SnapshotVolumeSpec names the read-only claim a snapshot is exposed through.
type SnapshotVolumeSpec struct {
	// ClaimName is the PersistentVolumeClaim to create in the snapshot's namespace.
	// It cannot be changed in place (the PV stays reserved for the first claim);
	// remove spec.volume, wait for the claim to go, then set it again.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="claimName is immutable; remove spec.volume and set it again"
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ClaimName string `json:"claimName"`
	// Driver is the daos-csi driver name.
	// +kubebuilder:default="daos.csi.gluesys.com"
	// +optional
	Driver string `json:"driver,omitempty"`
}

// DaosContainerSnapshotStatus mirrors `daos cont list-snaps` for this snapshot.
type DaosContainerSnapshotStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Epoch identifies the snapshot in DAOS (decimal). It is what
	// `daos cont destroy-snap --epc` and `dfuse --snap-epoch` take.
	Epoch string `json:"epoch,omitempty"`
	// CreationTime is when DAOS took the snapshot (the epoch's timestamp).
	CreationTime *metav1.Time `json:"creationTime,omitempty"`
	// ContainerUUID and PoolUUID pin the snapshot to the container it was taken
	// from, so a container re-created under the same name is not mistaken for it.
	ContainerUUID string `json:"containerUUID,omitempty"`
	PoolUUID      string `json:"poolUUID,omitempty"`
	Ready         bool   `json:"ready,omitempty"`
	// VolumeName is the PersistentVolume created for spec.volume.
	VolumeName string `json:"volumeName,omitempty"`
	// CreateRequestedAt is when create-snap was started. If its answer is lost
	// (operator restart, failed Job), the snapshot of the container taken after
	// it that no other DaosContainerSnapshot holds is adopted instead of taking
	// a second one.
	CreateRequestedAt *metav1.Time `json:"createRequestedAt,omitempty"`
	// Operation is the daos Job in flight (create, destroy); empty when idle.
	Operation          string       `json:"operation,omitempty"`
	ObservedGeneration int64        `json:"observedGeneration,omitempty"`
	LastQueryTime      *metav1.Time `json:"lastQueryTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=daossnap
// +kubebuilder:printcolumn:name="Container",type=string,JSONPath=`.spec.containerRef`
// +kubebuilder:printcolumn:name="Epoch",type=string,JSONPath=`.status.epoch`
// +kubebuilder:printcolumn:name="Taken",type=date,JSONPath=`.status.creationTime`
// +kubebuilder:printcolumn:name="Claim",type=string,JSONPath=`.spec.volume.claimName`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// DaosContainerSnapshot is one DAOS snapshot of a DaosContainer.
type DaosContainerSnapshot struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DaosContainerSnapshotSpec   `json:"spec,omitempty"`
	Status DaosContainerSnapshotStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DaosContainerSnapshotList contains a list of DaosContainerSnapshot.
type DaosContainerSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DaosContainerSnapshot `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &DaosContainerSnapshot{}, &DaosContainerSnapshotList{})
		return nil
	})
}
