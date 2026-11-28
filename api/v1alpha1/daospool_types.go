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
// +kubebuilder:validation:XValidation:rule="!has(self.kvcache) || (has(self.profile) && self.profile == 'kvcache')",message="spec.kvcache only applies with profile: kvcache"
// +kubebuilder:validation:XValidation:rule="!has(self.profile) || self.profile != 'kvcache' || !has(self.redundancyFactor) || self.redundancyFactor == ((has(self.kvcache) && has(self.kvcache.redundancy) && self.kvcache.redundancy == 'ec') ? 1 : 0)",message="profile kvcache sets rd_fac from kvcache.redundancy (none: 0, ec: 1); leave redundancyFactor unset or make it agree"
// +kubebuilder:validation:XValidation:rule="!has(self.properties) || !('reintegration' in self.properties) || self.properties['reintegration'] in ['data_sync', 'incremental', 'no_data_sync']",message="properties.reintegration must be data_sync, incremental or no_data_sync"
// +kubebuilder:validation:XValidation:rule="!has(self.redundancyFactor) || self.redundancyFactor != 0 || !has(self.properties) || !('reintegration' in self.properties) || self.properties['reintegration'] != 'no_data_sync'",message="a pool with redundancyFactor 0 takes reintegration incremental (the default) or data_sync: no_data_sync refuses every write to the pool"
// +kubebuilder:validation:XValidation:rule="(has(self.external) && self.external) || (has(self.size) && string(self.size) != '0')",message="spec.size is required unless spec.external is true"
// +kubebuilder:validation:XValidation:rule="!has(self.external) || !self.external || (!has(self.acl) && !has(self.properties) && !has(self.redundancyFactor) && !has(self.memRatioPercent) && !has(self.profile) && !has(self.kvcache))",message="spec.external: the operator neither creates nor changes this pool, so acl, properties, redundancyFactor, memRatioPercent, profile and kvcache do not apply; set them where the pool is managed"
type DaosPoolSpec struct {
	// SystemRef names the DaosSystem this pool lives in.
	SystemRef string `json:"systemRef"`
	// External marks a pool that is owned and managed outside the operator: by
	// FlexA's console on a FlexA DAOS system (spec.externalMsReplicas), by
	// another cluster, or by hand with dmg. The operator only reads it: it
	// mirrors `dmg pool query` into status (space, state, SpaceLow) and serves
	// DaosContainers and CSI volumes in it, but never creates, extends, sets a
	// property or ACL on, or destroys the pool -- not even with
	// daos.gluesys.com/destroy-approved. A pool that does not exist is reported,
	// not created. spec.size and spec.ranks, when given, are checked against the
	// pool and a difference is reported on the SpecMismatch condition, never
	// corrected. Works on any DaosSystem, operator-run or external. Once the pool
	// has been seen (status.uuid), external cannot be turned off again.
	// +optional
	External bool `json:"external,omitempty"`
	// Label is the pool's DAOS label when it is not metadata.name, typically for
	// an external pool whose label is not a valid Kubernetes name (capitals,
	// underscores). Unset means metadata.name. It cannot change once the pool
	// exists.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_.:-]{1,127}$`
	// +optional
	Label string `json:"label,omitempty"`
	// Size is the total pool size (dmg pool create --size). Extension is done
	// by adding ranks, not by editing Size (DAOS 2.8 has no shrink). Required
	// unless the pool is external, where it is only compared.
	// +optional
	Size resource.Quantity `json:"size,omitempty"`
	// Ranks restricts the pool to these ranks; empty = all. Appending ranks
	// triggers `dmg pool extend`.
	Ranks []int32 `json:"ranks,omitempty"`
	// RedundancyFactor is rd_fac. Unset means 2; 0 is valid and means "no
	// redundancy", which is what a pool on a single fault domain needs. It is a
	// pointer because with a default an omitted 0 would come back as the default.
	// It cannot change once the pool exists (status.uuid is set).
	//
	// With 0 the reintegration mode is pinned: incremental unless
	// properties.reintegration says data_sync, which declares that a returning
	// rank's data may be discarded (no_data_sync is refused: it rejects every
	// write). The operator sets the mode back if it is changed in DAOS.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4
	RedundancyFactor *int32 `json:"redundancyFactor,omitempty"`
	// Properties are extra `--properties k:v` pairs (e.g. ec_cell_sz), applied
	// at creation. properties.reintegration cannot change once the pool exists,
	// and when it is set the operator keeps the pool on that mode.
	Properties map[string]string `json:"properties,omitempty"`
	// ACL entries in DAOS ACE syntax (A::user@:rw). Declarative.
	ACL []string `json:"acl,omitempty"`
	// SpaceWarningPercent is where the SpaceLow condition flips to True.
	// DAOS enforces capacity per pool, so this is the number that matters for
	// every container and PV in it. Unset means 85; 0 disables the check
	// (a pointer, because with a default an omitted 0 would be indistinguishable).
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:validation:Minimum=0
	SpaceWarningPercent *int32 `json:"spaceWarningPercent,omitempty"`
	// MemRatioPercent is `dmg pool create --mem-ratio`: how much of the pool's
	// metadata (which lives on SSD under MD-on-SSD) is kept in the engine's
	// memory file (RAM). The DAOS default is 100, which makes the RAM the hard
	// ceiling on pool size -- on a box with 34 GiB of memory file per engine
	// that caps the pool near 1 TiB no matter how much data-tier capacity the
	// HDDs have (2026-09-28, da1~3). Lowering it trades metadata cache for
	// addressable capacity. Only meaningful on MD-on-SSD systems.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	MemRatioPercent *int32 `json:"memRatioPercent,omitempty"`
	// Profile presets the pool for a purpose. kvcache is a loss-tolerant KV
	// cache for vLLM + LMCache with the lmcache-daos rank guard: a rebooting
	// node costs its share of the cache as misses (redundancy none) or is
	// served degraded (redundancy ec), and nothing in the data path stalls.
	// It fixes rd_fac, and for redundancy none pins self_heal to none so DAOS
	// never excludes a rank of this pool (an rd_fac 0 pool would lose that
	// rank's data and need a reintegrate). See FlexA Hub 1f09478a §5, §6.2.
	// It cannot change once the pool exists.
	// +kubebuilder:validation:Enum=kvcache
	Profile string `json:"profile,omitempty"`
	// KVCache tunes profile kvcache.
	KVCache *KVCacheSpec `json:"kvcache,omitempty"`
}

// KVCacheSpec tunes the kvcache pool profile.
type KVCacheSpec struct {
	// Redundancy is none (rd_fac 0, self_heal none; clients use S1 buckets and
	// a per-rank breaker) or ec (rd_fac 1, DAOS's default self_heal; clients
	// use EC_2P1G1 buckets and a pool-wide breaker, and need at least three
	// fault domains). Measured on a 3-node lane with one node powered off
	// for 240 s: none misses that node's share during the outage and is back
	// at once; ec keeps the hits but rebuilds the node's share, needs a
	// reintegrate afterwards and 1.5x the space.
	// +kubebuilder:validation:Enum=none;ec
	// +kubebuilder:default=none
	Redundancy string `json:"redundancy,omitempty"`
}

// SpaceLowClaim is one PVC warned that its pool is low on space.
type SpaceLowClaim struct {
	// Volume is the PV name.
	Volume string `json:"volume"`
	// ClaimUID is the UID of the PVC the PV was bound to when it was warned.
	ClaimUID string `json:"claimUID"`
}

// DaosPoolStatus mirrors `dmg pool query`. Nothing here is a second source of
// truth: every field is copied from dmg output.
type DaosPoolStatus struct {
	Conditions      []metav1.Condition `json:"conditions,omitempty"`
	UUID            string             `json:"uuid,omitempty"`
	Label           string             `json:"label,omitempty"`
	State           string             `json:"state,omitempty"`
	FreeBytes       int64              `json:"freeBytes,omitempty"`
	TotalBytes      int64              `json:"totalBytes,omitempty"`
	RebuildState    string             `json:"rebuildState,omitempty"`
	DisabledTargets int32              `json:"disabledTargets,omitempty"`
	// EnabledRanks are the ranks the pool spans (dmg pool query --show-enabled).
	EnabledRanks []int32 `json:"enabledRanks,omitempty"`
	// UsedPercent is (total-free)/total from the last query, rounded down.
	UsedPercent int32 `json:"usedPercent,omitempty"`
	// SpaceLowClaims are the PVCs that have been told the pool is low on space.
	// A PVC bound while the pool is already low (including a PV re-bound to a new
	// PVC) is warned on the next poll, and the ones listed here get
	// PoolSpaceRecovered when it drops back.
	// +optional
	SpaceLowClaims []SpaceLowClaim `json:"spaceLowClaims,omitempty"`
	// Operation is the dmg Job in flight (create, extend, acl, destroy); empty when idle.
	Operation string `json:"operation,omitempty"`
	// ObservedGeneration is the spec generation the last create attempt was made for;
	// a failed create is not retried until the spec changes.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// AppliedACLHash identifies the spec.acl last written with overwrite-acl.
	AppliedACLHash string       `json:"appliedACLHash,omitempty"`
	LastQueryTime  *metav1.Time `json:"lastQueryTime,omitempty"`
	// ReintegrationMode is the pool's reintegration property (data_sync,
	// incremental, no_data_sync) as dmg pool get-prop last reported it; empty when
	// it could not be read. Containers are only cleared of
	// "Failures exceed RF" automatically on incremental pools annotated
	// daos.gluesys.com/auto-clear-unclean=true.
	ReintegrationMode string `json:"reintegrationMode,omitempty"`
	// SelfHeal is the pool's self_heal property as dmg pool get-prop last
	// reported it; empty when it could not be read. Profile kvcache with
	// redundancy none keeps it at none.
	SelfHeal string `json:"selfHeal,omitempty"`
	// DestroyAttempts counts failed dmg pool destroy runs since the DaosPool was
	// deleted; after a few the condition turns DestroyStalled (#36).
	DestroyAttempts int32 `json:"destroyAttempts,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=daospool
// +kubebuilder:printcolumn:name="System",type=string,JSONPath=`.spec.systemRef`
// +kubebuilder:printcolumn:name="Size",type=string,JSONPath=`.spec.size`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Used%",type=integer,JSONPath=`.status.usedPercent`
// +kubebuilder:printcolumn:name="Rebuild",type=string,JSONPath=`.status.rebuildState`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="RF",type=integer,JSONPath=`.spec.redundancyFactor`,priority=1
// +kubebuilder:printcolumn:name="Reintegration",type=string,JSONPath=`.status.reintegrationMode`,priority=1
// +kubebuilder:printcolumn:name="External",type=boolean,JSONPath=`.spec.external`,priority=1
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.uuid) || (has(self.spec.external) && self.spec.external) || ((has(self.spec.profile) ? self.spec.profile : '') == (has(oldSelf.spec.profile) ? oldSelf.spec.profile : '') && (has(self.spec.kvcache) && has(self.spec.kvcache.redundancy) ? self.spec.kvcache.redundancy : 'none') == (has(oldSelf.spec.kvcache) && has(oldSelf.spec.kvcache.redundancy) ? oldSelf.spec.kvcache.redundancy : 'none'))",message="spec.profile and spec.kvcache.redundancy cannot change once the pool exists; create a new DaosPool"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.uuid) || (has(self.spec.external) && self.spec.external) || (has(self.spec.redundancyFactor) == has(oldSelf.spec.redundancyFactor) && (!has(self.spec.redundancyFactor) || self.spec.redundancyFactor == oldSelf.spec.redundancyFactor))",message="spec.redundancyFactor cannot change once the pool exists; create a new DaosPool for a different redundancy"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.uuid) || (has(self.spec.external) && self.spec.external) || (has(self.spec.properties) && 'reintegration' in self.spec.properties ? self.spec.properties['reintegration'] : '') == (has(oldSelf.spec.properties) && 'reintegration' in oldSelf.spec.properties ? oldSelf.spec.properties['reintegration'] : '')",message="spec.properties.reintegration cannot change once the pool exists"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.uuid) || !has(oldSelf.spec.external) || !oldSelf.spec.external || (has(self.spec.external) && self.spec.external)",message="spec.external cannot be turned off once the pool has been seen: the operator would start changing a pool someone else manages. Create a new DaosPool for a pool the operator should manage"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.uuid) || (has(self.spec.label) ? self.spec.label : '') == (has(oldSelf.spec.label) ? oldSelf.spec.label : '')",message="spec.label cannot change once the pool exists"

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
