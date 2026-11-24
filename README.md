<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# daos-operator

Run [DAOS](https://docs.daos.io/) on Kubernetes. The operator manages the DAOS
system, its pools and containers; a CSI driver mounts a container as a
`ReadWriteMany` volume; an S3 gateway serves a pool over S3; and a reference
vLLM + LMCache workload uses a DAOS container as its KV cache.

> **Version 0.2.0** is validated on our own hardware, not yet in
> production, and the API is `v1alpha1`: fields may still change between minor
> releases. See [what was measured](#what-has-been-measured).

## Install

```bash
helm install daos-operator oci://ghcr.io/gluesys/charts/daos-operator \
  --version 0.3.0 --namespace daos-system --create-namespace
```

Images are public at `ghcr.io/gluesys/daos-{operator,csi,server,agent,admin,client}`.
The chart installs the operator alone by default; the CSI driver, S3 gateways and
the vLLM workload are opt-in (`csi.enabled`, `s3.services[]`, `vllm.services[]`).
The vLLM workload is a reference for PoCs and tests; to use the DAOS KV cache from your
own serving stack, see [doc/serving-integration.md](doc/serving-integration.md). What has
been verified, and where, is in [doc/compatibility.md](doc/compatibility.md).

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
  `daos.gluesys.com/hostprep-status` records what the last run did, and its
  `warnings` list what stops DAOS on that node that hostprep did not fix (a
  filesystem signature to wipe, THP, a kubelet that has not seen the hugepages).
  `hostPrep.disableTHP: true` also sets transparent_hugepage to never, which
  uninstalling does not undo either.

If you already ran `helm uninstall` and are left with a `DaosSystem` and no
operator, bring the operator back alone and then delete it:

```bash
helm install daos-operator oci://ghcr.io/gluesys/charts/daos-operator \
  --version 0.3.0 -n daos-system --set system.create=false
kubectl delete daossystem <name> --wait
helm uninstall daos-operator -n daos-system
```

## Custom resources

| Kind | What it is |
|---|---|
| `DaosSystem` | the engines: node selection, fabric, storage tiers, management-service replicas |
| `DaosPool` | a pool, its redundancy factor, rank set and space warnings |
| `DaosContainer` | a container in a pool, POSIX or unstructured |
| `DaosContainerSnapshot` | one read-only snapshot (DAOS epoch) of a `DaosContainer` |
| `DaosSnapshotPolicy` | periodic snapshots of a `DaosContainer`, the oldest destroyed |
| `S3Service` | a versitygw gateway serving one pool |

Two rules the operator does not break:

- **Nothing destructive happens on its own.** `dmg storage format` and every
  destroy wait for a human to put an approval annotation on the resource. The
  operator consumes the annotation after one attempt, whatever the outcome.
- **There is no second source of truth.** Desired state is the CR spec, actual
  state is the DAOS management service. The operator only compares the two and
  reports what it sees.

### Snapshots are read-only views, not restore points

A `DaosContainerSnapshot` takes one `daos cont create-snap` of a container in its
namespace and records the epoch (`status.epoch`) and when it was taken:

```yaml
apiVersion: daos.gluesys.com/v1alpha1
kind: DaosContainerSnapshot
metadata: {name: ckpt-20261107, namespace: ml}
spec: {containerRef: train-data}
```

DAOS 2.8 cannot roll a container back (`daos_cont_rollback` is not implemented).
To get data back, mount the snapshot read-only and copy what you need out of it.
`spec.volume.claimName` does the mounting part: the operator creates a
`ReadOnlyMany` PVC of that name next to the snapshot, bound to a PV that daos-csi
mounts with `dfuse --snap-epoch` (daos-csi with `snapshotEpoch` support). Writes
fail with `EROFS`. The PV is `Retain`, so deleting the claim never touches the
container; removing `spec.volume`, or the snapshot, removes both.

```yaml
spec:
  containerRef: train-data
  volume: {claimName: ckpt-20261107}   # mount it in a pod like any PVC, readOnly
```

A snapshot keeps the space of everything overwritten or deleted after it, so old snapshots hold pool capacity until they are destroyed.

As with pools and containers, deleting the object keeps the DAOS snapshot unless
`daos.gluesys.com/destroy-approved: "true"` is set first (Event `SnapshotOrphaned`
gives the command to remove it by hand). DAOS 2.8 snapshots have no names (a named
create-snap fails with `DER_NOSYS`), so a snapshot is known by its epoch only. The
operator never takes a second snapshot for one object: if the answer of a
create-snap is lost, the snapshot taken after it that no other object holds is
adopted -- but only when exactly one fits and no other object of the container is
still taking its own; otherwise the object waits (`AdoptAmbiguous`, listing the
candidate epochs) for `daos.gluesys.com/adopt-epoch: "<epoch>"`. Because the time
is the only link, take snapshots of a container this operator manages only through
`DaosContainerSnapshot`: one taken by hand (`daos cont create-snap`) while an
object's create-snap answer is lost can be adopted as that object's, and an
adopted epoch is not re-matched (`adopt-epoch` only applies before adoption). If
that happens, delete the object without `destroy-approved` (both snapshots stay in
DAOS) and remove the unwanted one by hand. One destroyed by
hand is reported (`SnapshotMissing`), not re-taken. Deleting an object while its
create-snap runs waits for the epoch, so an approved delete destroys it too.
`spec.volume.claimName` cannot be changed in place: remove `spec.volume`, then set it again.

Users who only see their own namespace use the CSI path instead (`csi.snapshots.enabled`,
plus the VolumeSnapshot CRDs and snapshot-controller from kubernetes-csi/external-snapshotter):
a `VolumeSnapshot` of a DAOS PVC becomes a `DaosContainerSnapshot`. A PVC with
`dataSource: VolumeSnapshot` and only read-only access modes is the snapshot itself
(`ReadOnlyMany`, no copy, at once); with a writable access mode daos-csi creates a
`DaosContainer` with `restoreFrom` and the claim binds once the copy finished:

```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata: {name: ckpt, namespace: ml}
spec: {volumeSnapshotClassName: daos-snapshot, source: {persistentVolumeClaimName: train-data}}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: ckpt-ro, namespace: ml}
spec:
  accessModes: [ReadOnlyMany]
  storageClassName: daos-train
  dataSource: {apiGroup: snapshot.storage.k8s.io, kind: VolumeSnapshot, name: ckpt}
  resources: {requests: {storage: 1Gi}}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: ckpt-copy, namespace: ml}
spec:
  accessModes: [ReadWriteMany]          # a full, writable copy
  storageClassName: daos-train
  dataSource: {apiGroup: snapshot.storage.k8s.io, kind: VolumeSnapshot, name: ckpt}
  resources: {requests: {storage: 1Gi}}
```

For a writable copy, create a new `DaosContainer` with `spec.restoreFrom`:

```yaml
apiVersion: daos.gluesys.com/v1alpha1
kind: DaosContainer
metadata: {name: train-data-restored, namespace: ml}
spec: {poolRef: train, restoreFrom: {snapshotRef: ckpt-20261107}}
```

After `daos cont create` a Job mounts the snapshot read-only (`dfuse --snap-epoch`,
by pool and container UUID) and the new container, and copies with `cp -a`. (Not
`daos fs copy`: it resolves a dfuse path back to its container and reads the live
data, skipping the snapshot.) It
is a full copy: it takes as long and as much space as the data. The container is
not `Ready` until the copy finished (`RestoreWaiting` until the snapshot was taken,
`RestoreInProgress`, then `status.restoredFrom: <snapshot>@<epoch>`); a failed copy
is reported (`RestoreFailed`) and retried 5 minutes after it failed, which is safe
because the copy overwrites what it wrote; success needs the new container flushed
and unmounted cleanly. One attempt is bounded by
`restoreFrom.deadlineSeconds` (default 24 h). Each attempt pins the snapshot it
reads (`status.restoreSource`: UID, epoch, pool and container UUID), so a snapshot
re-created under the same name meanwhile is not mistaken for it. POSIX containers
only, and within one DAOS system (a snapshot in a pool of another `DaosSystem` is
`RestoreUnsupported`); `restoreFrom` cannot be added to or removed from an existing container. An
approved destroy of the snapshot waits until every copy from it finished.

#### Periodic snapshots and retention

A `DaosSnapshotPolicy` takes a snapshot of one container every `interval` and
keeps the `keep` newest; with `maxAge` it also drops those older than that (the
newest one is always kept):

```yaml
apiVersion: daos.gluesys.com/v1alpha1
kind: DaosSnapshotPolicy
metadata: {name: train-6h, namespace: ml}
spec: {containerRef: train-data, interval: 6h, keep: 8, maxAge: 168h}
```

Each run creates a `DaosContainerSnapshot` named `<policy>-<UTC yyyymmdd-hhmm>`
with the label `daos.gluesys.com/snapshot-policy: <policy>`. **Creating a policy
approves destroying the snapshots it prunes**: it sets
`daos.gluesys.com/destroy-approved: "true"` on them and deletes them (Event
`SnapshotPruned`). Only snapshots carrying its label are counted and pruned, so
snapshots taken by hand or through a `VolumeSnapshot` are never touched. To keep
one of its snapshots for good, remove the label
(`kubectl label daossnap <name> daos.gluesys.com/snapshot-policy-`); one exposed
with `spec.volume` is skipped as well.

One snapshot is taken at a time: while the last one is not taken yet (container
not ready, create-snap failing), the policy waits and reports `SnapshotPending`
instead of piling up more. Runs missed while the operator was down are not made
up; `suspend: true` stops new snapshots but keeps pruning. Deleting the policy
leaves its snapshots as ordinary `DaosContainerSnapshot`s.

### Capacity is per pool, not per PVC

Upstream DAOS 2.8 has no per-container quota (the extended edition does, see the
next section). On upstream, the size a PVC requests is recorded but
not enforced, and `df` inside the volume shows the whole pool: every PVC in a
pool shares that pool's size, and one full PVC fills them all.

To give teams or uses their own capacity, give each its own pool and
StorageClass. The chart does both from one list:

```yaml
csi:
  enabled: true
pools:
  - name: train
    size: 1Ti
    spec: {redundancyFactor: 1, spaceWarningPercent: 85}
    storageClass: {}                       # creates daos-train
  - name: kv
    size: 500Gi
    storageClass: {name: daos-kv, reclaimPolicy: Retain}
```

When a pool passes `spaceWarningPercent` the operator sets `SpaceLow` on the
`DaosPool` and records a `PoolSpaceLow` Event on every bound PVC in that pool, so users who
only see their own namespace (`kubectl describe pvc`) are told too. A PVC bound
while the pool is already low is warned on the next poll; `status.spaceLowClaims`
lists the PVCs told, and each gets `PoolSpaceRecovered` when the pool drops back.
A `storageClass` is only rendered for a pool in the DaosSystem daos-csi serves
(`csi.system`); the chart refuses one for a pool in another system. Pools created this way are
kept on `helm uninstall`, like any `DaosPool`.

### Container quota needs the extended edition of DAOS

Upstream DAOS 2.8 limits space per pool only. The exastor extended edition (see
ADR-005) adds a per-container hard limit, and `DaosContainer.spec.spaceLimit` sets it:

```yaml
spec: {poolRef: train, spaceLimit: 500Gi}
```

The operator does not assume an edition. It creates the container without the
limit and reads `daos cont query`: the extended edition reports space usage
(`status.edition: extended`, `status.spaceUsedBytes`), and only then is the limit
set with `daos cont set-prop space_limit:<bytes>` (plus `space_amp` for an EC file
object class). A new container becomes `Ready` once its limit is set; changing or
removing `spaceLimit` later is applied online without taking it out of service
(condition `SpaceLimit`: `Applied`, or `ApplyFailed`, retried when the spec changes).
On upstream DAOS the field is reported, not enforced (`SpaceLimit=Unsupported`,
`status.edition: upstream`) and the container is served as usual.

daos-csi passes each PVC's size as `spaceLimit` (StorageClass `spaceLimit: "false"`
to opt out), and with `csi.expansion.enabled` growing a PVC moves the limit.

`spaceLimit` must be positive (remove it for no limit). If a limit is changed while
the previous `set-prop` still runs, the value that Job set is recorded and the new
one is set right after.

> **Upgrading:** `spec.spaceLimit` and its status fields are new in the
> `DaosContainer` CRD. Helm never upgrades `crds/`, and an API server with the old
> CRD silently drops the field (so neither a `spaceLimit` nor a CSI PVC size would
> be enforced). Apply the CRDs first (see [Upgrade](#upgrade)).

### A pool with no redundancy stops serving when a rank leaves

`redundancyFactor: 0` is a valid choice — it is what a pool on a single fault
domain needs — but it has a consequence that is easy to meet by accident and
hard to read when you do.

**When a rank leaves (a node reboot, a drain, an engine failure), every
container in that pool becomes UNCLEAN and refuses to open**: `DER_RF`, *failures
exceed RF*. A dfuse mount of it returns `EIO` for everything. **This is sticky by
design: DAOS does not clear it, even after the rank is reintegrated and the pool
is whole again.** (The DAOS user docs say otherwise for rd_fac 0; the DAOS
maintainers confirmed the docs are wrong.)

**Getting the rank back can destroy the data.** Under DAOS 2.8's default
reintegration mode, `data_sync`, reintegration first discards everything on the
returning rank and then rebuilds it from other copies — and a pool with no
redundancy has none. The operator therefore **refuses to reintegrate a rank that
holds a `redundancyFactor: 0` pool with `data_sync` reintegration** (or a pool
whose mode it does not know). If the data on that rank is expendable, say so
explicitly:

```bash
kubectl daos rank reintegrate <sys> --ranks=<N> --discard-data
```

The operator creates `redundancyFactor: 0` pools with `reintegration:incremental`,
which does not discard the returning rank's data. Be aware that the DAOS
maintainers describe incremental reintegration as **not functional in DAOS
2.8**; in our reboot tests the data was intact afterwards, but that is not a
guarantee from DAOS.

A pool whose data is expendable (a cache, scratch) can choose the supported path
instead and declare the loss once, at creation:

```yaml
spec:
  redundancyFactor: 0
  properties: {reintegration: data_sync}   # a returning rank comes back empty
```

Such a pool is reintegrated without `--discard-data`. `no_data_sync` is refused
for these pools: it rejects every write.

**The choice is fixed.** Once the pool exists, `spec.redundancyFactor` and
`spec.properties.reintegration` cannot be edited (create a new DaosPool
instead), and the operator keeps the pool on that mode: if it is changed in
DAOS (`dmg pool set-prop`), the operator sets it back and emits
`ReintegrationModeDrift`. A redundant pool without the property is left on
whatever DAOS has. `kubectl get daospool -o wide` shows the mode DAOS reports.

After the rank is back, the containers stop with `Ready=False` and reason
`FailuresExceedRF`, and the status message carries the command. **Check the data
first**, then clear it by hand:

```bash
daos cont set-prop <pool> <cont> --properties status:healthy
```

If you accept that risk for a pool, annotate the DaosPool with
`daos.gluesys.com/auto-clear-unclean: "true"`. The operator then clears its
containers itself, but only when the pool uses incremental reintegration, every
target is back and rebuild has finished. Watch for the `ContainerUnclean` and
`ContainerHealthRestored` events.

> **Upgrading:** automatic clearing used to be on for every incremental pool; it
> is now off unless the pool carries the annotation above. It also relies on a
> `status` field that Helm will not add to an existing install, because Helm
> never upgrades `crds/`. Apply the CRDs first (see [Upgrade](#upgrade)).

### Pods that run as an account UID and open DAOS directly

File ownership on a CSI volume is numeric: dfuse `--multi-user` keeps the UID and GID of
each entry, so pods sharing a volume across nodes need the same numeric IDs per user, and
neither the driver nor the operator needs LDAP.

Opening a pool is different. `daos_agent` builds the caller's credential from the name
its own `/etc/passwd` gives the caller's UID, and the agent containers do not know the
cluster's accounts. dfuse behind the CSI driver is not affected (it connects as root). A pod
running as an account UID that talks to DAOS itself gets no credential:

- `ioil`: such a pod may not take dfuse's pool handle (dfuse hands it only to its own UID),
  so libioil connects itself; without a credential it quietly leaves the I/O on dfuse.
- `pil4dfs`, `pil4dfs-compat`: they never connect themselves from such a pod and always
  stay on dfuse, with or without a map.
- a libdfs application or the `daos` CLI: the connect fails.

`spec.client.clientUserMap` gives those UIDs names (`credential_config.client_user_map` in
`daos_agent.yml`). The pool and container ACLs then judge the pod by that name, so map each
approved UID to its own least-privilege principal and grant that principal only what it
needs; with no ACL entry the connect is refused and the pod falls back to dfuse.

```yaml
spec:
  client:
    clientUserMap:
      "1001": {user: alice, group: hpc, groups: [proj-a]}
```

```sh
daos pool update-acl <pool> -e A::alice@:r
daos cont update-acl <pool> <cont> -e A::alice@:rw
```

Do not map UIDs to `root`, and do not put a privileged name under `default`: a pool the
operator creates is owned by `root@`, so such a mapping gives every matching UID owner access
to all of its data and bypasses per-account ACLs. Leave `default` unset (unlisted UIDs stay
on dfuse, where file permissions still apply) or point it at a principal no ACL grants.

Measured on the CI lane (Tier 1 case 15, dfuse `--multi-user`): a UID-1001 `ioil` pod reads
through dfuse without a map, is intercepted with `1001` mapped to a principal the ACL grants
(`root` on that throwaway test pool), and falls back again with `default` mapped to `nobody`,
which no ACL grants. UIDs the agent can resolve itself never use the map. The client agent DaemonSet rolls on a change; CSI node plugin pods read the new map
only when they are recreated.

## Requirements

- Kubernetes 1.29+ (native sidecars)
- **hostNetwork for every DAOS client.** `daos_agent` hands the client an
  interface name and the servers must reach the client at the address it
  advertises. A pod-network client sits behind the CNI's masquerade, so its
  advertised address does not match its source address and every RPC ends in
  `DER_TIMEDOUT`. A cluster that enforces Pod Security needs a `privileged` exception for these pods.
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

- [`doc/architecture.html`](doc/architecture.html) — a one-page visual guide for
  developers: how the four reconcilers turn CRs into dmg Jobs, the DaosSystem
  reconcile steps, the approval gates, the repository map and the CI lanes
  *(Korean; open the file in a browser — GitLab and GitHub show it as source)*.
  Regenerate it from `doc/architecture.src.html` with the visual-explainer
  renderer (`node <visual-explainer>/kit/render.mjs doc/architecture.src.html`)
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
