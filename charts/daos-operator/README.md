<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# daos-operator

Run [DAOS](https://docs.daos.io/) on Kubernetes: an operator for the DAOS system,
pools and containers, a CSI driver that mounts a container as a `ReadWriteMany`
volume, an S3 gateway, and a reference vLLM + LMCache workload that uses a DAOS
container as its KV cache.

## What it installs

| Resource | Purpose |
|---|---|
| `DaosSystem` | the engines: node selection, fabric, storage tiers, management-service replicas |
| `DaosPool` | a pool (`dmg pool create`), redundancy factor, rank set, space warnings |
| `DaosContainer` | a container in a pool (POSIX or unstructured) |
| `S3Service` | a versitygw gateway serving one pool over S3 |
| CSI driver | `dfuse` mounts as RWX PVs, one container per volume |

Destructive steps never happen on their own. `dmg storage format` and every
destroy wait for a human to put an approval annotation on the resource; the
operator drops the annotation after one attempt, whatever the outcome.

## Requirements

- Kubernetes 1.29+ (native sidecars)
- DAOS 2.8 server/client images the operator can pull
- **hostNetwork for every DAOS client.** `daos_agent` hands the client an
  interface name and the servers must reach the client at the address it
  advertises; a pod-network client sits behind the CNI's masquerade and every RPC
  ends in `DER_TIMEDOUT`. Managed Kubernetes therefore needs a PodSecurity
  exception for these pods.
- **On an RDMA fabric** (`ofi+verbs`, `ucx`) clients also need the verbs devices:
  without `/dev/infiniband` the libfabric verbs provider fails to initialise.
  Set `rdma: true` on the workloads that need it.
- Hugepages on every server node. `kdev` and `file` storage classes do not avoid
  this: DAOS drives all three classes through SPDK.
- Clocks in sync across server nodes. DAOS uses a hybrid logical clock; a node
  whose clock drifts is accepted into the system and then blocks aggregation,
  which surfaces much later as `DER_NOSPACE` on a pool that looks empty.

## Install

```bash
helm install daos-operator oci://<registry>/charts/daos-operator --version 0.1.0 \
  --namespace daos-system --create-namespace
```

The chart's default image references point at the registry this project is built
in. Override them for your own registry:

```yaml
image:
  repository: <your-registry>/daos-operator
system:
  spec:
    images:
      server: <your-registry>/daos-server:2.8.0
      agent:  <your-registry>/daos-agent:2.8.0
      admin:  <your-registry>/daos-admin:2.8.0
      client: <your-registry>/daos-client:2.8.0
```

## Upgrade

Helm installs `crds/` once and never upgrades them, so apply the new CRDs first,
then upgrade the release:

```bash
helm pull oci://<registry>/charts/daos-operator --version <new> --untar -d /tmp/daos-operator-<new>
kubectl apply --server-side --force-conflicts -f /tmp/daos-operator-<new>/daos-operator/crds/
helm upgrade daos-operator oci://<registry>/charts/daos-operator \
  --version <new> --namespace daos-system --reuse-values
```

Upgrading the release replaces the operator; it does not restart the engines. A new
`spec.images.server` only reports `Upgrading=Pending`. The full-stop engine upgrade
starts when you approve it with `kubectl daos system upgrade <system> [--image I] --yes`, after
draining clients. The approval is consumed whatever happens, including when there
is nothing to upgrade.

## Values

See `values.yaml`. The parts most installations touch:

| Key | Meaning |
|---|---|
| `system.spec.nodeSelector` | which nodes run DAOS servers |
| `system.spec.engines[]` | targets, SCM size, fabric port, storage tiers |
| `system.spec.engines[].bdevTiers[]` | split roles across devices, e.g. NVMe `[wal, meta]` + HDDs `[data]` |
| `system.spec.msReplicas` | management-service replicas (3 for a redundant MS) |
| `csi.enabled` | the CSI driver and its StorageClasses |
| `s3.services[]` | S3 gateways, one per pool |
| `vllm.services[]` | reference serving workload; `rdma: true` on an RDMA fabric |

Per-node facts that differ between hosts (NIC name, device paths) come from node
annotations, not from the chart: `daos.gluesys.com/fabric-iface`,
`daos.gluesys.com/bdev-list`, `daos.gluesys.com/bdev-list-<tier>`.

## Documentation

Project documentation lives at
<https://github.com/gluesys/daos-operator>. The engineering notes (ADRs, testbed
records, the GPU deployment guide) are written in Korean; the README there is in
English.
