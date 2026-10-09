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
  --version 0.1.5 --namespace daos-system --create-namespace
```

Images are public at `ghcr.io/gluesys/daos-{operator,csi,server,agent,admin,client}`.
The chart installs the operator alone by default; the CSI driver, S3 gateways and
the vLLM workload are opt-in (`csi.enabled`, `s3.services[]`, `vllm.services[]`).

## Upgrade

Helm installs `crds/` once and never upgrades them, so apply the new CRDs first,
then upgrade the release:

```bash
helm pull oci://ghcr.io/gluesys/charts/daos-operator --version <new> --untar -d /tmp/daos-operator-<new>
kubectl apply --server-side --force-conflicts -f /tmp/daos-operator-<new>/daos-operator/crds/
helm upgrade daos-operator oci://ghcr.io/gluesys/charts/daos-operator \
  --version <new> --namespace daos-system --reuse-values
```

Upgrading the release replaces the operator; it does not restart the engines. A new
`spec.images.server` only reports `Upgrading=Pending`. The full-stop engine upgrade
starts when you approve it with `kubectl daos system upgrade <system> [--image I] --yes`, after
draining clients. The approval is consumed whatever happens, including when there
is nothing to upgrade.

## Uninstall

**Delete the custom resources before the release, while the operator is still
running to act on them.** The chart marks `DaosSystem` with
`helm.sh/resource-policy: keep`, so `helm uninstall` on its own leaves the system
and its server pods behind and takes away the operator that could clean them up.
Everything the system owns — the server StatefulSet, the agent and `hostprep`
DaemonSets, its ConfigMaps, Secrets and Services — is garbage-collected with it,
so deleting the `DaosSystem` first is what actually stops the pods.

```bash
# 1. stop whatever is using the storage (workloads, PVCs)
# 2. containers and pools, if you want the DAOS data gone
kubectl annotate daoscontainer <name> daos.gluesys.com/destroy-approved=true
kubectl delete daoscontainer <name> --wait
kubectl annotate daospool <name> daos.gluesys.com/destroy-approved=true
kubectl delete daospool <name> --wait
# 3. the system, while the operator is still there
kubectl delete daossystem <name> --wait
# 4. the release
helm uninstall daos-operator -n daos-system
# 5. optional: Helm never removes crds/
kubectl delete crd daossystems.daos.gluesys.com daospools.daos.gluesys.com \
  daoscontainers.daos.gluesys.com s3services.daos.gluesys.com
```

What this does and does not remove:

- **Deleting a `DaosSystem` does not touch the data.** The operator does no wipe
  and no reformat on deletion; the DAOS data on the devices stays as it was.
- **Deleting a `DaosPool` or `DaosContainer` does not destroy it either**, unless
  you set `daos.gluesys.com/destroy-approved=true` first. Without the annotation
  the DAOS object is kept, the Kubernetes object just forgets it, and the operator
  emits `PoolOrphaned` / `ContainerOrphaned` telling you what to destroy by hand
  if that is what you meant. Step 2 above is the destructive path — skip it to
  keep the data for a later system.
- **`hostprep` does not revert what it changed on the nodes.** It raises
  `vm.nr_hugepages`, and with `hostPrep.bindNvme: true` it hands unused NVMe to
  SPDK. Neither is undone by uninstalling. The default is `bindNvme: false`, so a
  default install leaves only the raised hugepages. The node annotation
  `daos.gluesys.com/hostprep-status` records what the last run did.

If you already ran `helm uninstall` and are left with a `DaosSystem` and no
operator, bring the operator back alone and then delete it:

```bash
helm install daos-operator oci://ghcr.io/gluesys/charts/daos-operator \
  --version 0.1.5 -n daos-system --set system.create=false
kubectl delete daossystem <name> --wait
helm uninstall daos-operator -n daos-system
```

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
- [`doc/adr/`](doc/adr/) — the decisions behind the deployment, device-binding,
  upgrade and S3-gateway models, with the evidence that approved them. In English,
  except ADR-005; [`doc/adr/README.md`](doc/adr/README.md) says why
- [`doc/testbed-*.md`](doc/) — what was run on real hardware and what broke *(Korean)*
- [`doc/ci-design-2026-10-02.md`](doc/ci-design-2026-10-02.md) — the CI design: a six-VM
  cluster on ExaCI5 slots 3/4, snapshot-rollback resets, nvme/kdev/mixed profiles, tier
  mapping; [`doc/ci-handoff-2026-10-02.md`](doc/ci-handoff-2026-10-02.md) lists the real
  regressions it must catch *(Korean)*
- [`hack/ci/`](hack/ci/) — the scripts that build and reset that CI cluster
  (`vm-reshape.sh` once, `prep-all.sh`/`cluster-init.sh`/`snapshot.sh` to rebuild the
  base, `rollback.sh` before every run) *(Korean comments)*
- [`doc/design-notes.ko.md`](doc/design-notes.ko.md) — internal design notes per
  feature *(Korean)*

## License

Apache-2.0. DAOS itself is BSD-2-Clause-Patent and is installed from the
[upstream packages](https://packages.daos.io/); the runtime images in this
project are stock DAOS 2.8 RPMs on a Rocky 9 base.
