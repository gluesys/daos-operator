<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# daos-operator

Run [DAOS](https://docs.daos.io/) on Kubernetes. The operator manages the DAOS
system, its pools and containers; a CSI driver mounts a container as a
`ReadWriteMany` volume; an S3 gateway serves a pool over S3; and a reference
vLLM + LMCache workload uses a DAOS container as its KV cache.

> **Pre-release.** Version 0.1.0 is validated on our own hardware, not in
> production. See [what was measured](#what-has-been-measured).

## Install

```bash
helm install daos-operator oci://ghcr.io/gluesys/charts/daos-operator \
  --version 0.1.0 --namespace daos-system --create-namespace
```

Images are public at `ghcr.io/gluesys/daos-{operator,csi,server,agent,admin,client}`.
The chart installs the operator alone by default; the CSI driver, S3 gateways and
the vLLM workload are opt-in (`csi.enabled`, `s3.services[]`, `vllm.services[]`).

## Custom resources

| Kind | What it is |
|---|---|
| `DaosSystem` | the engines: node selection, fabric, storage tiers, management-service replicas |
| `DaosPool` | a pool, its redundancy factor, rank set and space warnings |
| `DaosContainer` | a container in a pool, POSIX or unstructured |
| `S3Service` | a versitygw gateway serving one pool |

Two rules the operator does not break:

- **Nothing destructive happens on its own.** `dmg storage format` and every
  destroy wait for a human to put an approval annotation on the resource. The
  operator consumes the annotation after one attempt, whatever the outcome.
- **There is no second source of truth.** Desired state is the CR spec, actual
  state is the DAOS management service. The operator only compares the two and
  reports what it sees.

## Requirements

- Kubernetes 1.29+ (native sidecars)
- **hostNetwork for every DAOS client.** `daos_agent` hands the client an
  interface name and the servers must reach the client at the address it
  advertises. A pod-network client sits behind the CNI's masquerade, so its
  advertised address does not match its source address and every RPC ends in
  `DER_TIMEDOUT`. Managed Kubernetes needs a PodSecurity exception for these pods.
- **On an RDMA fabric** (`ofi+verbs`, `ucx`), clients also need the verbs devices:
  without `/dev/infiniband` the libfabric verbs provider fails to initialise.
- **Hugepages on every server node.** The `kdev` and `file` storage classes do
  not avoid this — DAOS drives all three classes through SPDK.
- **Clocks in sync across server nodes.** DAOS uses a hybrid logical clock. A node
  whose clock drifts is accepted into the system and then blocks aggregation,
  which surfaces much later as `DER_NOSPACE` on a pool that still looks empty.

Per-node facts that differ between hosts (NIC name, device paths) come from node
annotations rather than the chart: `daos.gluesys.com/fabric-iface`,
`daos.gluesys.com/bdev-list`, `daos.gluesys.com/bdev-list-<tier>`.

## What has been measured

On four physical nodes (32 cores, 62 GB, 100 Gb InfiniBand, one NVMe for
`[wal, meta]` and eight SAS disks for `[data]`) plus one GPU node:

| | |
|---|---|
| System | 4 ranks, management service replicated 3× |
| Fabric | `ofi+verbs` on IPoIB; raw RDMA 11.4 GB/s between server and client |
| dfuse from the GPU node | 587 MB/s write, 914 MB/s read (1 GiB) |
| CSI volume | 744 MB/s write (1 GiB) |
| HDD data tier | 143 MB/s write, 160 MB/s read, pool of 10 TiB |
| Fault injection | excluding a rank keeps a redundant pool readable and writable; the data survived exclude → rebuild → reintegrate |
| vLLM KV cache | 3840 tokens stored to DAOS, restored after a pod restart |

The same configuration over a 1 GbE management network reached 5 MiB/s and the
KV-cache workload failed outright: the fabric is not an implementation detail.

## Documentation

- [`doc/deploy-gpu-k8s.md`](doc/deploy-gpu-k8s.md) — deploying the vLLM KV-cache
  path on common GPU setups, with the traps each one hides
- [`doc/adr/`](doc/adr/) — the decisions behind the deployment, device-binding and
  upgrade models, with the evidence that approved them *(Korean)*
- [`doc/testbed-*.md`](doc/) — what was run on real hardware and what broke *(Korean)*
- [`doc/design-notes.ko.md`](doc/design-notes.ko.md) — internal design notes per
  feature *(Korean)*

## License

Apache-2.0. DAOS itself is BSD-2-Clause-Patent and is installed from the
[upstream packages](https://packages.daos.io/); the runtime images in this
project are stock DAOS 2.8 RPMs on a Rocky 9 base.
