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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// PlacementMode selects where engines run (ADR-001).
// +kubebuilder:validation:Enum=dedicated;hyperconverged
type PlacementMode string

const (
	PlacementDedicated      PlacementMode = "dedicated"
	PlacementHyperconverged PlacementMode = "hyperconverged"
)

// EngineSpec describes one daos_engine per node (one per CPU socket).
type EngineSpec struct {
	// Targets is the number of I/O targets (usually one per NVMe device).
	// +kubebuilder:default=8
	Targets int32 `json:"targets,omitempty"`
	// Helpers is nr_xs_helpers.
	// +kubebuilder:default=2
	Helpers int32 `json:"helpers,omitempty"`
	// FabricIface is the NIC name. Empty means the operator discovers it per node
	// (names differ per host: ens2 vs ens2np0 -- never copy configs between hosts).
	FabricIface string `json:"fabricIface,omitempty"`
	// +kubebuilder:default=31316
	FabricPort int32 `json:"fabricPort,omitempty"`
	// BdevList is the NVMe PCI address list. Empty means the operator discovers
	// VFIO-bound devices on the node. Dual-port chassis: the operator must verify
	// PCI DSN uniqueness across nodes before format (2026-09-03 incident).
	BdevList []string `json:"bdevList,omitempty"`
	// ScmSizeGiB is the tmpfs (MD-on-SSD metadata) size; reflected 1:1 into the
	// pod memory request so the scheduler sees it (ADR-002).
	// +kubebuilder:default=32
	ScmSizeGiB int32 `json:"scmSizeGiB,omitempty"`
	// PinnedNumaNode pins the engine; nil lets DAOS choose.
	PinnedNumaNode *int32 `json:"pinnedNumaNode,omitempty"`
}

// ImagesSpec pins the container images from exastor/daos-images.
type ImagesSpec struct {
	Server string `json:"server"`
	Agent  string `json:"agent"`
	Admin  string `json:"admin"`
	// Client is the daos-client image (daos CLI, dfuse); DaosContainer Jobs run it
	// with the Agent image as a sidecar.
	Client string `json:"client,omitempty"`
	// HostPrep is the host-preparation DaemonSet image (daos-server + hostprep binary).
	// Empty uses the operator's built-in default.
	HostPrep string `json:"hostPrep,omitempty"`
}

// HostPrepSpec controls the per-node preparation DaemonSet (#8). It discovers
// NVMe/DSN/RDMA facts into Node annotations, raises hugepages, and only when
// BindNvme is true hands unused NVMe to SPDK. It never touches devices that
// have partitions, mounts or holders, and never formats anything.
type HostPrepSpec struct {
	// Enabled deploys the DaemonSet. Default true.
	Enabled *bool `json:"enabled,omitempty"`
	// BindNvme lets hostprep unbind unused NVMe from the kernel (daos_server nvme prepare).
	// Default false: discover and annotate only.
	BindNvme bool `json:"bindNvme,omitempty"`
	// FabricCIDR picks the RDMA NIC whose IPv4 address is in this network, e.g. 172.28.136.0/24.
	// Empty = first RDMA NIC with an address.
	FabricCIDR string `json:"fabricCIDR,omitempty"`
	// IntervalSeconds between discovery runs. Default 300.
	IntervalSeconds int32 `json:"intervalSeconds,omitempty"`
}

// ServerSpec controls the per-node server workloads (#9). Each rendered node
// gets its own StatefulSet (replicas=1, pinned with a required nodeAffinity)
// because a DAOS rank is bound to the node that holds its superblock and NVMe:
// Kubernetes must never reschedule it elsewhere (ADR-002).
type ServerSpec struct {
	// Enabled creates the server StatefulSets. Default true. Set false to render
	// configuration only (e.g. manual bring-up with exastor/daos-images compose).
	Enabled *bool `json:"enabled,omitempty"`
	// DataHostPath is the node directory that holds control_metadata (management
	// service DB, superblock records). Mounted at /var/daos. Default /var/daos/<system>.
	DataHostPath string `json:"dataHostPath,omitempty"`
	// LogHostPath is the node directory for daos_server/engine logs. Default /var/log/daos/<system>.
	LogHostPath string `json:"logHostPath,omitempty"`
	// Resources overrides the computed requests/limits of the daos-server container.
	// By default memory request = sum(scmSizeGiB)+2Gi (tmpfs is charged to the pod),
	// cpu request = sum(targets+helpers+1) and hugepages-2Mi = nrHugepages*2Mi.
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
	// TerminationGracePeriodSeconds for the engine pod. Default 120.
	TerminationGracePeriodSeconds *int64 `json:"terminationGracePeriodSeconds,omitempty"`
}

// UpgradeSpec implements ADR-003: before DAOS 3.0 only a full-stop upgrade
// exists and it never runs without an explicit approval.
type UpgradeSpec struct {
	// Approved must be set to true by a human for the operator to execute
	// a version change. It is reset to false after the upgrade completes.
	Approved bool `json:"approved,omitempty"`
}

// DaosSystemSpec defines the desired state of a DAOS system.
type DaosSystemSpec struct {
	// Version is the DAOS version (image tag prefix), e.g. "2.8.0".
	Version string `json:"version"`
	// Namespace is where the operator creates this system's ConfigMaps, Secrets and pods.
	// +kubebuilder:default="daos-system"
	Namespace string     `json:"namespace,omitempty"`
	Images    ImagesSpec `json:"images"`
	// +kubebuilder:default=dedicated
	Placement PlacementMode `json:"placement,omitempty"`
	// NodeSelector picks storage nodes. Required for dedicated placement.
	NodeSelector map[string]string   `json:"nodeSelector,omitempty"`
	Tolerations  []corev1.Toleration `json:"tolerations,omitempty"`
	// MsReplicas is the number of management-service replicas (odd). Phase 0
	// test beds with two hosts run 1 and are therefore not HA.
	// +kubebuilder:validation:Enum=1;3;5
	// +kubebuilder:default=3
	MsReplicas int32 `json:"msReplicas,omitempty"`
	// Provider is the fabric provider. UCX is excluded by default (ADR-002).
	// +kubebuilder:default="ofi+verbs;ofi_rxm"
	Provider string `json:"provider,omitempty"`
	// +kubebuilder:default=8192
	NrHugepages int32 `json:"nrHugepages,omitempty"`
	// Engines per node; usually one per socket.
	// +kubebuilder:validation:MinItems=1
	Engines []EngineSpec `json:"engines"`
	// AllowInsecure disables TLS between dmg/agent and servers. Phase 0 only.
	AllowInsecure bool         `json:"allowInsecure,omitempty"`
	HostPrep      HostPrepSpec `json:"hostPrep,omitempty"`
	Server        ServerSpec   `json:"server,omitempty"`
	Upgrade       UpgradeSpec  `json:"upgrade,omitempty"`
}

// RankStatus mirrors `dmg system query -v` for one rank. The operator copies
// it; it never keeps its own membership database (no second source of truth).
type RankStatus struct {
	Rank  int32  `json:"rank"`
	Node  string `json:"node"`
	State string `json:"state"`
}

// NodeConfigStatus reports what the operator rendered for one selected node.
type NodeConfigStatus struct {
	Node        string `json:"node"`
	ConfigMap   string `json:"configMap,omitempty"`
	ControlAddr string `json:"controlAddr,omitempty"`
	FabricIface string `json:"fabricIface,omitempty"`
	BdevCount   int32  `json:"bdevCount,omitempty"`
	Ready       bool   `json:"ready"`
	Message     string `json:"message,omitempty"`
	// Workload is the server StatefulSet created for this node (#9).
	Workload string `json:"workload,omitempty"`
	// ServerReady mirrors the server pod's Ready condition (control port listening).
	ServerReady bool `json:"serverReady,omitempty"`
}

// DaosSystemStatus is read back from the DAOS management service and metrics.
type DaosSystemStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// SelectedNodes are the nodes matching spec.nodeSelector, sorted.
	SelectedNodes []string `json:"selectedNodes,omitempty"`
	// MsReplicaNodes are the nodes chosen to host management-service replicas.
	MsReplicaNodes  []string           `json:"msReplicaNodes,omitempty"`
	NodeConfigs     []NodeConfigStatus `json:"nodeConfigs,omitempty"`
	Formatted       bool               `json:"formatted,omitempty"`
	RanksJoined     int32              `json:"ranksJoined,omitempty"`
	RanksTotal      int32              `json:"ranksTotal,omitempty"`
	Ranks           []RankStatus       `json:"ranks,omitempty"`
	ObservedVersion string             `json:"observedVersion,omitempty"`
	// PendingFormat is set when storage is unformatted; a human must create the
	// approval (kubectl daos system format) -- never automated.
	PendingFormat bool `json:"pendingFormat,omitempty"`
	// FormatTime is when the operator's format Job completed successfully.
	FormatTime *metav1.Time `json:"formatTime,omitempty"`
	// LastQueryTime is when `dmg system query` last answered (ranks are as of then).
	LastQueryTime *metav1.Time `json:"lastQueryTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=daossys
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Ranks",type=string,JSONPath=`.status.ranksJoined`
// +kubebuilder:printcolumn:name="Formatted",type=boolean,JSONPath=`.status.formatted`
// +kubebuilder:printcolumn:name="PendingFormat",type=boolean,JSONPath=`.status.pendingFormat`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// DaosSystem is a DAOS storage system (one `daos_server` system, N ranks).
type DaosSystem struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DaosSystemSpec   `json:"spec,omitempty"`
	Status DaosSystemStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DaosSystemList contains a list of DaosSystem.
type DaosSystemList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DaosSystem `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &DaosSystem{}, &DaosSystemList{})
		return nil
	})
}
