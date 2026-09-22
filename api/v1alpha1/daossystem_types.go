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
	"k8s.io/apimachinery/pkg/api/resource"
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

// A note on defaults: a field where 0 is a meaningful value must be a pointer.
// With `+kubebuilder:default` and `omitempty`, an explicit 0 is dropped by the
// client and the API server puts the default back, so the 0 never arrives. That
// bit us three times (redundancyFactor, spaceWarningPercent, nrHugepages).
//
// EngineSpec describes one daos_engine per node (one per CPU socket).
type EngineSpec struct {
	// Targets is the number of I/O targets (usually one per NVMe device).
	// +kubebuilder:default=8
	Targets int32 `json:"targets,omitempty"`
	// Helpers is nr_xs_helpers. 0 is a real setting (small engines run without
	// helper xstreams), so this is a pointer; unset means 2.
	Helpers *int32 `json:"helpers,omitempty"`
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
	// pod memory request so the scheduler sees it (ADR-002). DAOS refuses less
	// than 4 GiB per engine.
	// +kubebuilder:default=32
	ScmSizeGiB int32 `json:"scmSizeGiB,omitempty"`
	// BdevClass is the storage class of the data tier.
	//   nvme  SPDK-owned NVMe. The only class supported in production (ADR-002).
	//   kdev  kernel block devices (bdevList holds device paths). Test beds only.
	//   file  files on a filesystem, sized by BdevSizeGiB. Test beds only.
	// kdev and file need no hugepages, no VFIO and no IOMMU, which is what makes
	// a small VM able to run an engine at all; they are not a performance
	// configuration and must not be used to measure anything.
	// +kubebuilder:validation:Enum=nvme;kdev;file
	// +kubebuilder:default=nvme
	BdevClass string `json:"bdevClass,omitempty"`
	// BdevSizeGiB is the size of each backing file (class file only).
	BdevSizeGiB int32 `json:"bdevSizeGiB,omitempty"`
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
	// HugepagesRequest is what the server pod is allowed to use, as a quantity
	// ("1Gi"). It is deliberately separate from spec.nrHugepages: that one tells
	// DAOS how many hugepages to allocate on the host, while this one sets the
	// pod's hugetlb limit. On a host whose hugepages are managed outside DAOS
	// (nrHugepages: 0) the pod still needs this, or every SPDK call fails with
	// "Cannot allocate memory" even though the host has free pages.
	// Unset means nrHugepages * 2Mi.
	HugepagesRequest *resource.Quantity `json:"hugepagesRequest,omitempty"`
	// Resources overrides the computed requests/limits of the daos-server container.
	// By default memory request = sum(scmSizeGiB)+2Gi (tmpfs is charged to the pod),
	// cpu request = sum(targets+helpers+1) and hugepages-2Mi = nrHugepages*2Mi.
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
	// TerminationGracePeriodSeconds for the engine pod. Default 120.
	TerminationGracePeriodSeconds *int64 `json:"terminationGracePeriodSeconds,omitempty"`
}

// RankOpStatus is the outcome of the last rank membership operation (#20).
type RankOpStatus struct {
	Op          string       `json:"op,omitempty"`
	Ranks       string       `json:"ranks,omitempty"`
	RequestedAt *metav1.Time `json:"requestedAt,omitempty"`
	FinishedAt  *metav1.Time `json:"finishedAt,omitempty"`
	Succeeded   bool         `json:"succeeded,omitempty"`
	Message     string       `json:"message,omitempty"`
}

// CertificatesSpec tunes the transport certificates the operator generates
// when allowInsecure=false (#16, rotation #19).
type CertificatesSpec struct {
	// RenewBeforeDays is how long before expiry the Certificates condition
	// switches to ExpiringSoon. Rotation still needs a human approval
	// (daos.gluesys.com/certs-renew-approved=true). Default 30.
	// +kubebuilder:validation:Minimum=1
	RenewBeforeDays int32 `json:"renewBeforeDays,omitempty"`
}

// CertificatesStatus mirrors the certificate Secret.
type CertificatesStatus struct {
	SecretName string       `json:"secretName,omitempty"`
	NotAfter   *metav1.Time `json:"notAfter,omitempty"`
	RotatedAt  *metav1.Time `json:"rotatedAt,omitempty"`
	// PreviousSecret holds the bundle replaced by the last rotation, kept for
	// manual rollback; the operator never deletes it.
	PreviousSecret string `json:"previousSecret,omitempty"`
}

// TelemetrySpec exposes the engines' Prometheus endpoint (#12).
type TelemetrySpec struct {
	// Enabled sets telemetry_port in daos_server.yml and creates the
	// <sys>-metrics Service. Default true.
	Enabled *bool `json:"enabled,omitempty"`
	// Port is the daos_server telemetry_port. Default 9191.
	Port int32 `json:"port,omitempty"`
	// ServiceMonitor creates a monitoring.coreos.com/v1 ServiceMonitor when that
	// CRD is installed (prometheus-operator / kube-prometheus-stack). Default true;
	// silently skipped, with a condition message, when the CRD is absent.
	ServiceMonitor *bool `json:"serviceMonitor,omitempty"`
	// ServiceMonitorLabels are added to the ServiceMonitor so a Prometheus with a
	// serviceMonitorSelector picks it up (e.g. release: kube-prometheus-stack).
	ServiceMonitorLabels map[string]string `json:"serviceMonitorLabels,omitempty"`
	// Interval is the scrape interval. Default 15s (the DAOS dashboard uses rate(...[15s])).
	Interval string `json:"interval,omitempty"`
}

// UpgradeSpec implements ADR-003: before DAOS 3.0 only a full-stop upgrade
// exists and it never runs without an explicit approval.
type UpgradeSpec struct {
	// Approved must be set to true by a human for the operator to execute
	// a version change. Setting it asserts that clients are drained (the
	// operator cannot see client handles). It is reset to false when the
	// upgrade completes or fails.
	Approved bool `json:"approved,omitempty"`
	// TimeoutMinutes bounds the wait for pods and ranks in each phase. Default 30.
	TimeoutMinutes int32 `json:"timeoutMinutes,omitempty"`
}

// UpgradeStatus is the full-stop upgrade state machine (ADR-003, #13).
// Phases: Pending (image differs, waiting for approval) -> Stopping (dmg
// system stop) -> Updating (server pods deleted, StatefulSets already carry the
// new image) -> Starting (waiting for pods) -> StartingSystem (dmg system start)
// -> Verifying (dmg system query, all ranks joined) -> Completed | Failed.
type UpgradeStatus struct {
	// Trigger says why the system is being restarted: ImageChange (spec.images.server)
	// or CertificateRotation (daos.gluesys.com/certs-renew-approved).
	Trigger    string       `json:"trigger,omitempty"`
	Phase      string       `json:"phase,omitempty"`
	Message    string       `json:"message,omitempty"`
	FromImage  string       `json:"fromImage,omitempty"`
	ToImage    string       `json:"toImage,omitempty"`
	StartedAt  *metav1.Time `json:"startedAt,omitempty"`
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
}

// DaosSystemSpec defines the desired state of a DAOS system.
type DaosSystemSpec struct {
	// Version is the DAOS version (image tag prefix), e.g. "2.8.0".
	Version string `json:"version"`
	// SystemName is the DAOS system name (daos_server.yml `name`, agent/control
	// `name`). Default "daos_server". Changing it after format is not supported.
	// +kubebuilder:default="daos_server"
	SystemName string `json:"systemName,omitempty"`
	// Namespace is where the operator creates this system's ConfigMaps, Secrets and pods.
	// +kubebuilder:default="daos-system"
	Namespace string     `json:"namespace,omitempty"`
	Images    ImagesSpec `json:"images"`
	// +kubebuilder:default=dedicated
	Placement PlacementMode `json:"placement,omitempty"`
	// ExternalMsReplicas attaches this DaosSystem to a DAOS system the operator
	// does NOT run: the addresses of an existing management service. With it set
	// the operator manages no server pods and no host preparation; it renders the
	// client (agent) and admin (control) configuration from these addresses,
	// mirrors membership, and runs pool/container/rank operations as Jobs.
	// Use it when DAOS runs on bare metal and Kubernetes only consumes it.
	// The Jobs still run on nodes matching NodeSelector, so those nodes must be
	// able to reach these addresses and the fabric.
	ExternalMsReplicas []string `json:"externalMsReplicas,omitempty"`
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
	// NrHugepages is the SPDK hugepage count for all engines on a host. 0 means
	// none, which is right (and required) for the kdev and file classes, so this
	// is a pointer; unset means 8192.
	NrHugepages *int32 `json:"nrHugepages,omitempty"`
	// SystemRamReservedGiB is DAOS's `system_ram_reserved`: memory it leaves to
	// the operating system. DAOS defaults to 64 GiB, which no small test host
	// can meet; lowering it is how those hosts run an engine at all. Unset keeps
	// the DAOS default.
	SystemRamReservedGiB int32 `json:"systemRamReservedGiB,omitempty"`
	// DisableVFIO makes SPDK use uio_pci_generic instead of VFIO, for hosts
	// without an IOMMU. Ignored by the kdev and file classes.
	DisableVFIO bool `json:"disableVFIO,omitempty"`
	// ControlPort is the daos_server control-plane port (server, agent and dmg
	// must agree). Default 10001; change it to run a second system on hosts that
	// already have one.
	// +kubebuilder:default=10001
	ControlPort int32 `json:"controlPort,omitempty"`
	// Engines per node; usually one per socket. Required unless
	// ExternalMsReplicas is set (then the engines run elsewhere).
	Engines []EngineSpec `json:"engines,omitempty"`
	// AllowInsecure disables TLS between dmg/agent and servers. Phase 0 only.
	AllowInsecure bool             `json:"allowInsecure,omitempty"`
	HostPrep      HostPrepSpec     `json:"hostPrep,omitempty"`
	Server        ServerSpec       `json:"server,omitempty"`
	Certificates  CertificatesSpec `json:"certificates,omitempty"`
	Telemetry     TelemetrySpec    `json:"telemetry,omitempty"`
	Upgrade       UpgradeSpec      `json:"upgrade,omitempty"`
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
	// Upgrade is the state of the last/current full-stop upgrade (#13).
	Upgrade *UpgradeStatus `json:"upgrade,omitempty"`
	// Certificates mirrors the transport-certificate Secret (#16, #19).
	Certificates *CertificatesStatus `json:"certificates,omitempty"`
	// LastRankOp is the result of the last drain/exclude/reintegrate request (#20).
	LastRankOp *RankOpStatus `json:"lastRankOp,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=daossys
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Ranks",type=string,JSONPath=`.status.ranksJoined`
// +kubebuilder:printcolumn:name="Formatted",type=boolean,JSONPath=`.status.formatted`
// +kubebuilder:printcolumn:name="PendingFormat",type=boolean,JSONPath=`.status.pendingFormat`
// +kubebuilder:printcolumn:name="Upgrade",type=string,JSONPath=`.status.upgrade.phase`
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
