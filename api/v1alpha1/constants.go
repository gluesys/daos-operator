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

// Node annotations that carry per-node discovery facts. Phase 2 step 1 reads
// them from the Node object; the host-preparation DaemonSet (#8) is what writes
// them. Until then they can be set by hand:
//
//	kubectl annotate node n1 daos.gluesys.com/fabric-iface=ens2 \
//	  daos.gluesys.com/bdev-list=0000:03:00.0,0000:04:00.0 \
//	  daos.gluesys.com/bdev-dsn=0000:03:00.0=6479A701A8C0D000,0000:04:00.0=6479A701A8C0D001
const (
	// AnnotationFabricIface is the NIC the engine binds (differs per host: ens2 vs ens2np0).
	AnnotationFabricIface = "daos.gluesys.com/fabric-iface"
	// AnnotationBdevList is the comma-separated list of VFIO-bound NVMe PCI addresses.
	AnnotationBdevList = "daos.gluesys.com/bdev-list"
	// AnnotationBdevDSN maps PCI address to the drive's PCI Device Serial Number:
	// "<pci>=<dsn>,...". Two nodes exposing the same DSN share one physical drive
	// (dual-port chassis); the operator refuses to render those nodes (2026-09-03 incident).
	AnnotationBdevDSN = "daos.gluesys.com/bdev-dsn"
	// AnnotationNumaNode optionally pins the engine (engine 0 only for now).
	AnnotationNumaNode = "daos.gluesys.com/numa-node"
	// AnnotationControlAddr overrides the address used for mgmt_svc_replicas / access
	// points / hostlist. Default is the node's InternalIP.
	AnnotationControlAddr = "daos.gluesys.com/control-addr"

	// AnnotationFormatApproved on a DaosSystem lets the operator run `dmg storage
	// format` exactly once. Formatting destroys whatever is on the drives, so it is
	// never automated: a human sets the value "true" (kubectl daos system format
	// will do this) after status.pendingFormat is reported; the operator removes
	// the annotation when the format Job finished, success or not.
	AnnotationFormatApproved = "daos.gluesys.com/format-approved"

	// AnnotationCertsRenewApproved on a DaosSystem lets the operator replace the
	// transport certificates. New certificates mean a new CA, so every engine and
	// client must reload them: the operator therefore performs the full-stop
	// restart of ADR-003 (stop, swap the Secret, restart pods, start, verify).
	// Setting this annotation approves that outage. It is removed when the
	// rotation finishes, successfully or not.
	AnnotationCertsRenewApproved = "daos.gluesys.com/certs-renew-approved"

	// AnnotationRankOp asks the operator to run one rank membership operation:
	// "<op>:<ranks>", e.g. "drain:2" or "exclude:1,3-4". Operations are
	// drain (migrate data off, graceful), exclude (mark down now; pools rebuild),
	// reintegrate (bring back; pools rebuild onto it) and clear-exclude (undo an
	// administrative exclusion). The operator runs it once and removes the
	// annotation; `kubectl daos rank ...` writes it after showing the impact.
	AnnotationRankOp = "daos.gluesys.com/rank-op"

	// AnnotationDestroyApproved on a DaosPool or DaosContainer lets the operator
	// run `dmg pool destroy` / `daos cont destroy` when the object is deleted.
	// Without it, deleting the Kubernetes object only forgets the DAOS object
	// (Event PoolOrphaned / ContainerOrphaned); data is never removed implicitly.
	AnnotationDestroyApproved = "daos.gluesys.com/destroy-approved"

	// FinalizerPool and FinalizerContainer hold deletion until the destroy decision is made.
	FinalizerPool      = "daos.gluesys.com/pool"
	FinalizerContainer = "daos.gluesys.com/container"

	// LabelPool and LabelContainer mark the dmg/daos Jobs run for an object.
	LabelPool      = "daos.gluesys.com/pool"
	LabelContainer = "daos.gluesys.com/container"

	// LabelSystem and LabelNode are put on every object the operator creates.
	LabelSystem = "daos.gluesys.com/system"
	LabelNode   = "daos.gluesys.com/node"
	LabelRole   = "daos.gluesys.com/role"
)

// Condition types on DaosSystem.status.conditions.
const (
	ConditionNodesSelected  = "NodesSelected"
	ConditionDriveConflict  = "DriveConflict"
	ConditionConfigRendered = "ConfigRendered"
	// ConditionServersReady is True when every rendered node's server pod is Ready.
	ConditionServersReady = "ServersReady"
	// ConditionFormatted mirrors what `dmg system query` says about the management
	// service: True once formatted, False (AwaitingApproval/Formatting/FormatFailed)
	// before, Unknown when no replica answered.
	ConditionFormatted = "Formatted"
	// ConditionTelemetry reports the metrics Service / ServiceMonitor state.
	ConditionTelemetry = "Telemetry"
	// ConditionClientAgent reports the per-node client agent DaemonSet.
	ConditionClientAgent = "ClientAgent"
	// ConditionCertificates reports the transport-certificate Secret (Generated/Valid/ExpiringSoon/Invalid/Insecure).
	ConditionCertificates = "Certificates"
	// ConditionUpgrading is True while the full-stop upgrade runs; False with
	// reason UpToDate / Pending / Completed / Failed otherwise.
	ConditionUpgrading = "Upgrading"
	ConditionReady     = "Ready"
	// ConditionSpaceLow is set on a DaosPool when usage crosses
	// spec.spaceWarningPercent. DAOS has no per-container quota, so a full pool
	// hits every container and PV in it at once.
	ConditionSpaceLow = "SpaceLow"
)
