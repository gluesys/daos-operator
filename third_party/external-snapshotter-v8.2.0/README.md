# external-snapshotter v8.2.0 (test only)

The VolumeSnapshot CRDs and snapshot-controller, from
https://github.com/kubernetes-csi/external-snapshotter at tag v8.2.0 (Apache-2.0, see
LICENSE). The e2e suite installs them on the CI cluster (`test/e2e`, before the chart:
the profiles turn on `csi.snapshots`, whose VolumeSnapshotClass needs the CRDs). A real
cluster installs them once, as its distribution says; the chart does not ship them.

Files are verbatim except one line: the v8.2.0 tag's `setup-snapshot-controller.yaml`
still names the v8.0.1 image, so it names v8.2.0 here, matching the chart's
csi-snapshotter sidecar.

| file | upstream path |
|---|---|
| `snapshot.storage.k8s.io_*.yaml` | `client/config/crd/` |
| `rbac-snapshot-controller.yaml`, `setup-snapshot-controller.yaml` | `deploy/kubernetes/snapshot-controller/` |
