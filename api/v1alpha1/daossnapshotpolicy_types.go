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

// DaosSnapshotPolicySpec takes a DaosContainerSnapshot of one DaosContainer
// every interval and keeps the newest ones.
//
// The snapshots it takes carry the label daos.gluesys.com/snapshot-policy=<name>.
// Creating a policy approves destroying the DAOS snapshots it prunes: they are
// deleted with daos.gluesys.com/destroy-approved=true. Nothing else is ever
// pruned: a snapshot without the label (taken by hand, or by a CSI
// VolumeSnapshot) is not counted and not touched. To keep one of the policy's
// snapshots for good, remove the label from it; a snapshot exposed as a volume
// (spec.volume set) is skipped the same way.
//
// Deleting the policy stops taking and pruning; the snapshots it took stay as
// ordinary DaosContainerSnapshots.
//
// +kubebuilder:validation:XValidation:rule="!has(self.maxAge) || duration(self.maxAge) >= duration(self.interval)",message="maxAge must not be shorter than interval"
type DaosSnapshotPolicySpec struct {
	// ContainerRef is the DaosContainer, in this namespace, to snapshot.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="containerRef is immutable"
	ContainerRef string `json:"containerRef"`
	// Interval between snapshots (Go duration, e.g. 1h, 30m), at least 5m. A
	// snapshot is taken when the policy is created and then every interval; runs
	// missed while the operator was down are not made up.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('5m')",message="interval must be at least 5m"
	Interval metav1.Duration `json:"interval"`
	// Keep is how many of the policy's snapshots are kept; the oldest beyond
	// that are destroyed. Every snapshot keeps the space of what was overwritten
	// since it was taken, so this bounds the space the policy holds.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	Keep int32 `json:"keep"`
	// MaxAge, when set, also destroys the policy's snapshots older than this
	// (Go duration, e.g. 168h). The newest snapshot is always kept, so a policy
	// that cannot take new ones does not end up with none.
	// +kubebuilder:validation:Type=string
	// +optional
	MaxAge *metav1.Duration `json:"maxAge,omitempty"`
	// Suspend stops taking new snapshots; pruning goes on.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// DaosSnapshotPolicyStatus reports the snapshots the policy holds.
type DaosSnapshotPolicyStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// LastScheduleTime is when the policy last created a DaosContainerSnapshot.
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`
	// NextScheduleTime is when it creates the next one (unset while suspended).
	NextScheduleTime *metav1.Time `json:"nextScheduleTime,omitempty"`
	// LastSnapshot is the newest snapshot taken (Ready) under the policy.
	LastSnapshot string `json:"lastSnapshot,omitempty"`
	// Snapshots is how many taken snapshots the policy holds.
	Snapshots          int32 `json:"snapshots,omitempty"`
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=daossnappol
// +kubebuilder:printcolumn:name="Container",type=string,JSONPath=`.spec.containerRef`
// +kubebuilder:printcolumn:name="Interval",type=string,JSONPath=`.spec.interval`
// +kubebuilder:printcolumn:name="Keep",type=integer,JSONPath=`.spec.keep`
// +kubebuilder:printcolumn:name="Snapshots",type=integer,JSONPath=`.status.snapshots`
// +kubebuilder:printcolumn:name="Last",type=string,JSONPath=`.status.lastSnapshot`
// +kubebuilder:printcolumn:name="Next",type=date,JSONPath=`.status.nextScheduleTime`
// +kubebuilder:printcolumn:name="Suspend",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 40",message="name must be at most 40 characters (snapshot names add a 14-character timestamp)"

// DaosSnapshotPolicy takes periodic snapshots of a DaosContainer and destroys
// the oldest ones.
type DaosSnapshotPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DaosSnapshotPolicySpec   `json:"spec,omitempty"`
	Status DaosSnapshotPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DaosSnapshotPolicyList contains a list of DaosSnapshotPolicy.
type DaosSnapshotPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DaosSnapshotPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &DaosSnapshotPolicy{}, &DaosSnapshotPolicyList{})
		return nil
	})
}
