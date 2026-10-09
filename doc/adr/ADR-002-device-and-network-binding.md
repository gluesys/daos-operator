<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# ADR-002: Device and network binding — VFIO host preparation as a DaemonSet, hostNetwork, ofi+verbs

- Status: **accepted** (2026-09-29)
- Date: 2026-09-14 (accepted 2026-09-29)
- Evidence for acceptance: with `hostNetwork` and `ofi+verbs` on `ib0`, dfuse from the GPU node
  reached 587 MB/s write and 914 MB/s read, raw RDMA 11.4 GB/s. The `kdev`/`file` relaxation and
  the hugepages correction below are also from measurements.

## Context

A DAOS server (1) takes NVMe away from the kernel through VFIO so SPDK can drive it, (2) needs
hugepages, (3) binds its Mercury/libfabric address to a host NIC, and (4) keeps its superblock and
management-service database on node-local disk.

Kubernetes assumes a pod can be rescheduled freely. For a DAOS server pod that assumption is simply
false, and a design that pretends otherwise produces a system that looks healthy until the first
reschedule.

## Decision

- **Host preparation is a separate DaemonSet job**, not something the server pod does to its host:
  `vm.nr_hugepages`, `daos_server nvme prepare`, kernel parameters. The server pod then receives
  `/dev/vfio`, `/dev/hugepages`, `/sys/kernel/mm/hugepages` and `/sys/devices/system/node` as
  hostPaths, and declares a `hugepages-2Mi` resource. Kernel-NVMe (AIO) mode is not supported for
  production (a zvol pool measured 0.95x).
- **Server pods run with `hostNetwork: true`.** mlx5 devices are exposed through an RDMA device
  plugin. The default provider is `ofi+verbs;ofi_rxm`. UCX is excluded from the default: we could
  not make a two-engines-per-host configuration work with it.
- **Servers are a node-pinned StatefulSet** (nodeAffinity required), with the superblock and
  management-service database on a local PV (hostPath). No automatic rescheduling.
- **The tmpfs size for metadata is reflected in the pod's `memory` request** so the scheduler knows
  about it.
- **Per-node differences in `fabric_iface` (`ens2` vs `ens2np0`) are discovered by the operator**
  from node facts. Configuration is never copied between nodes.

## Rationale

DAOS documentation on hardware and deployment; the known-limitations notes from our LMCache DAOS
GPU-direct work; and the 2026-09-03 incident where two ranks sharing NVMe corrupted data.

## Consequences and trade-offs

Isolation is weaker and `privileged` is required. What we get back is performance and a simpler
operational story — one place that prepares a host, one place that owns a server's identity.

## Revisit when

DAOS 3.0 supports multiple providers, or Kubernetes DRA (Dynamic Resource Allocation) can express
RDMA and hugepages.

## Revision (2026-09-22): non-SPDK storage classes, for testing only

The decision above — *kernel NVMe (AIO) mode is not supported* — **stands for production**. Nothing
about the performance or stability evidence has changed.

What changes is that `spec.engines[].bdevClass` also accepts `kdev` (kernel block device) and `file`,
**for testing**. There is one reason. SPDK needs hugepages, an IOMMU and dedicated NVMe; without
hardware that has all three, **the path that brings a server up as a pod cannot be exercised at all**.
The flow from hostprep through the format approval gate to a pool and a PVC is only verified when a
server actually starts, and postponing that verification costs more than allowing AIO in tests.
`spec.systemRamReservedGiB`, `spec.disableVFIO` and `spec.controlPort` are added for the same reason:
small VMs, hosts without an IOMMU, and a second system on a host that already runs DAOS.

**The line we hold**

- The default is `nvme`, and both the CRD comments and the README say "do not use `kdev`/`file` for
  performance measurement".
- Published performance numbers come from an `nvme` configuration only. Testbed numbers are cited
  for functional confirmation, never as performance.
- "Is `bdevClass` `nvme`?" is on the production deployment checklist.

**A misconception the measurements corrected (2026-09-22).** We had written that `kdev`/`file` make
hugepages unnecessary. That is **wrong.** DAOS drives all three classes through SPDK — `file` and
`kdev` are SPDK's AIO backend — and DPDK initialisation needs hugepages. A pod with
`nr_hugepages: 0` failed to format with `spdk_env_init(): Cannot allocate memory`. What `kdev`/`file`
remove is the need for **dedicated NVMe and IOMMU/VFIO**, not hugepages.

Kubernetes adds one more: if the pod's hugetlb cgroup limit is 0, free hugepages on the host are
unusable. That is why `spec.nrHugepages` (how many the host pool DAOS manages should hold) and
`spec.server.hugepagesRequest` (how much the pod may use) are separate fields.

**Revisit when** dedicated hardware is available and tests can run on `nvme` too — then this
relaxation is no longer needed.
