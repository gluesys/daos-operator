<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# Host-path mounts

Some software does not take a PVC. It takes a directory on the node and bind-mounts
that into the containers it starts itself: Backend.AI (the vfolder mount on its
storage-proxy and agent nodes), Slurm, plain Docker. For these the chart can mount a
DAOS POSIX container at a path on the node, with one dfuse per node.

Pods should keep using the CSI driver. A host-path mount has no per-pod lifecycle, no
quota per claim and no way to be scheduled around.

## 1. Chart values

```yaml
system:
  spec:
    clientAgent:
      enabled: true
      nodeSelector: {daos.gluesys.com/client: "true"}
hostMounts:
  - name: vfroot
    pool: bai
    container: vfroot
    hostPath: /vfroot/daos
    nodeSelector: {daos.gluesys.com/client: "true"}
```

Each entry becomes a DaemonSet `<release>-hostmount-<name>` in the release namespace.
Every selected node gets the same container at the same path.

| Key | Default | Meaning |
|---|---|---|
| `name` | required | names the DaemonSet |
| `pool`, `container` | required | label or UUID. The container must be POSIX |
| `hostPath` | required | mount point on the node. It needs a parent directory below `/` (`/vfroot/daos`, not `/vfroot`): the pod reaches the host through that parent |
| `systemName` | `system.spec.systemName` when the chart creates the system | DAOS system name |
| `multiUser` | `true` | dfuse `--multi-user`. Without it only root can enter the mount; with it each caller owns what it creates |
| `dfuseArgs` | `[]` | extra dfuse flags |
| `image` | `system.spec.images.client` | needs `dfuse`, `daos`, `findmnt`, `umount` |
| `agentSocketHostPath` | the node agent of `system.name` | set it for another DaosSystem or an agent you run yourself |
| `nodeSelector`, `tolerations` | all nodes, none | where the mount exists |
| `updateStrategy` | `{type: OnDelete}` | see section 3. `RollingUpdate` takes `maxUnavailable` only: a surge pod would detach the mount the old pod still serves |
| `resources` | requests 100m / 256Mi, no limit | a memory limit that kills dfuse cuts every user of the mount |

The pod is privileged and on the host network. Privileged is what lets the mount
propagate to the host (`Bidirectional`), and with an RDMA provider it is also how dfuse
reaches the verbs devices. The node needs the node agent (`system.spec.clientAgent`).

The container is not created by this entry. Create it with a `DaosContainer` or
`daos container create`; until it exists the pod waits and logs
`waiting for container <pool>/<container>`, without restarting.

## 2. What the pod does

1. Detaches a stale mount at the path. A dfuse that was killed leaves a dead mount
   behind, and nothing can be mounted or even stat()ed there until it is detached.
2. Waits for the node agent socket and for the container.
3. Runs dfuse. When dfuse exits it starts again from step 1 after 2 seconds. Five exits
   within a minute fail the pod, so a mount that cannot work shows as a crash loop.
4. On termination it stops dfuse and leaves no mount behind.

Two probes, neither of which touches the file system:

| Probe | Checks | Why |
|---|---|---|
| liveness | the mount is in the mount table (or the pod is still waiting in step 2) | catches a mount removed from under a running dfuse |
| readiness | the mount answered a `statfs` within the last 20 seconds, from a stamp one process in the pod keeps | a node whose mount does not answer shows not ready |

A probe must not run `stat` on the mount itself. While the DAOS servers are unreachable
a `stat` on a dfuse mount blocks in the kernel and cannot be interrupted, not even by
`timeout`: it stays in D state until the servers are back. A probe that did that would
leave one stuck process behind every period. For the same reason nothing restarts dfuse
because the mount is slow. During a server outage dfuse has to stay up: its users block,
and continue when the servers return.

## 3. Users of the mount, and what a dfuse restart does to them

A bind mount is a reference to the mount that existed when the container started.
When dfuse restarts, the path on the host gets a new mount and the old one is dead.
What a consumer sees depends on how it bound the path:

| The consumer bound | After dfuse restarts |
|---|---|
| a directory inside the mount (`-v /vfroot/daos/x:/data`), the default private propagation | `ENOTCONN` until the consumer is restarted. Backend.AI binds each vfolder into its session containers this way |
| the parent of the mount point with `rslave` (`-v /vfroot:/vfroot:rslave`, in a pod `mountPropagation: HostToContainer`) | sees the new mount without a restart |
| nothing, a host process that opens paths as it goes (the Backend.AI storage-proxy) | works again as soon as the mount is back; files it held open are lost |

This is why the DaemonSets default to `updateStrategy: OnDelete`. A rolling update
would restart dfuse on every node as soon as a value changes. To roll a change: drain
the consumers of a node, delete that node's pod, let the consumers back.

A server outage is different: dfuse stays up, so the mount stays the same one. Its
users block and then continue, whatever they bound.

Measured behaviour is in section 6.

## 4. Backend.AI

Backend.AI starts its session containers itself and bind-mounts vfolders from a host
path, on Kubernetes too (its Kubernetes agent uses `hostPath` volumes), so the CSI
driver is not on that path. What it needs from DAOS:

- the same container mounted at the same path on the storage-proxy node and on every
  agent node: one `hostMounts[]` entry whose `nodeSelector` covers them;
- `multiUser: true`, since session containers run as the user's UID;
- dfuse caching left at its defaults. Another node sees a change up to 1 second late;
  with caching off that delay is gone, but small-file work is 5 to 10 times slower;
- a volume in `storage-proxy.toml` on that path. The stock `vfs` backend passes the
  storage-proxy contract tests on dfuse. The `daos` backend plugin
  (`backendai-storage-daos`) adds a refusal to start on a path that is not dfuse, and
  pool, container and lost-mount health in `get_hwinfo`.

dfuse does not support hard links, `flock` and POSIX locks, FIFOs and UNIX sockets
inside the mount, or reading a file after it was unlinked. A vfolder holding an SQLite
database, for one, will not work.

## 5. Nodes outside Kubernetes

A Backend.AI node that is not a worker of the cluster running the operator needs the
DAOS client on the host: the `daos-client` package of the servers' version, an agent,
and a dfuse unit. `deploy/host-client/` has the two pieces that are specific to an
operator-managed system.

```sh
# where kubectl reaches the cluster: the agent config the operator keeps, and the
# agent certificates when the system uses them
deploy/host-client/fetch-config.sh <DaosSystem name> daos-system ./daos-client

# on the node
cp daos-client/daos_agent.yml /etc/daos/        # and certs/ to /etc/daos/certs, owner daos_agent
systemctl enable --now daos_agent
install -m 0644 deploy/host-client/daos-dfuse@.service /etc/systemd/system/
mkdir -p /etc/daos/dfuse
cat > /etc/daos/dfuse/vfroot.conf <<'EOF'
POOL=bai
CONTAINER=vfroot
MOUNTPOINT=/vfroot/daos
DFUSE_ARGS=--multi-user --sys-name=daos_k8s
EOF
systemctl enable --now daos-dfuse@vfroot
```

The node must reach the servers on the fabric, and the servers must reach the node at
the address its agent advertises. `include_fabric_ifaces` in the fetched file names
interfaces of the cluster's nodes; edit it when this node's interface has another name.
With servers on the host network (`placement: dedicated`) the access points in the file
are node addresses and work from outside the cluster as they are.

The unit follows the rules of section 2: stale mount detached first, dfuse restarted
after 2 seconds, nothing restarted because the mount is slow. It has no start limit:
while the servers, the agent or the container are away it keeps trying every 2 seconds
and mounts by itself once they are back. `fetch-config.sh` wants an output directory
of its own: it replaces the directory as a whole, only with a complete set, and refuses
one that holds other files.

## 6. Measured

2026-11-27, one node (Rocky 9.8, kernel 5.14.0-570, Kubernetes 1.31.14 / CRI-O 1.31.5, RTX A6000)
as client of 4 ranks of DAOS 2.8.0 over ofi+verbs (IB 100G), an external system
(`externalMsReplicas`) with the node agent from `spec.clientAgent`. The DaemonSet was
the chart's own render of the entry in section 1, applied with nothing edited. Two
podman containers stood in for consumers: one bound a directory inside the mount with
private propagation, one bound the parent with `rslave`.

| Event | Host path | Pod | Private bind | `rslave` bind |
|---|---|---|---|---|
| DaemonSet created before the container exists | not mounted | waiting, 0 restarts | — | — |
| container created | mounted 3.4 s later | ready | — | — |
| `kill -9` dfuse | `ENOTCONN`, back after 3.7 s | no restart | `ENOTCONN` until restarted | continues |
| `kill -9` twice more in a row | back after 3.8 s each | no restart | | |
| `umount -l` of the path under a running dfuse | back after 27 s | restarted by liveness | `ENOTCONN` until restarted | continues |
| all DAOS servers stopped for 82 s, then started | blocks, then continues | not ready during the outage, ready 11 s after the start, no restart, dfuse not restarted | continues | continues |
| pod deleted (the `OnDelete` update path) | unmounted in 0.6 s, back 2.7 s after the delete | new pod | `ENOTCONN` until restarted | continues |
| DaemonSet deleted | no mount left, directory empty | — | — | — |

Other checks in the same run:

- A file created by UID 1000 in the mount was owned by 1000:1000. UID 2000 could read
  it and could not write to it.
- The storage-proxy 26.9.8 contract tests (`test_volume.py`, `test_quota.py`) passed on
  the mount through the `daos` backend plugin, and its `get_hwinfo` reported `healthy`
  with the pool and container read from the mount.
- During the server outage the plugin reported `degraded` (its `daos` query timed out),
  then `unavailable` (the mount had not answered a `statvfs` for 5 s), and `healthy`
  again after the start, in one process that was not restarted.
- An earlier render ran `timeout 5 stat -f` on the mount as its readiness probe. During
  a server outage each run stayed in D state for the whole outage (7 minutes), 15 of
  them by the time the servers were started. All returned when the servers came back.
  That is the reason for the stamp in section 2.
- Section 5: `fetch-config.sh` wrote the operator's `daos_agent.yml`; a `daos_agent`
  started on the host with that file served `daos container create` and a
  `daos-dfuse@` unit. The unit mounted in 1.2 s, came back 3.1 s after `kill -9` of its
  dfuse, and left no mount behind on stop. With a stub in place of dfuse that exited at
  once, the unit was at its tenth try after 22 s and mounted by itself, with no manual
  start, once the stub stopped failing. The system allowed insecure transport, so the
  certificate branch of the script was not run. The node was a worker of the cluster as
  well; a node that is not has not been tried.

Not run: a Backend.AI manager or agent on top of the mount, more than one client node,
and a rolling update of the DaemonSet.
