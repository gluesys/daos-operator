<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# Security policy

## Reporting

Email **kpkim@gluesys.com** with `[daos-operator]` in the subject. Please do not
open a public issue for a suspected vulnerability.

Useful things to include: what an attacker — or an unlucky operator — can do, the
chart and operator versions, the Kubernetes version, the DAOS release, and the
smallest reproduction you have. A `DaosSystem`/`DaosPool` manifest and the
operator log around the event are usually enough. If you have a patch, attach it
rather than opening a pull request, since pull requests are public.

We will acknowledge within five working days. This is a small team with no
dedicated on-call, so please read that as an honest expectation rather than a
service level. We will tell you what we intend to do and when, and we will credit
you in the fix unless you ask us not to.

## What is in scope

This is a Kubernetes operator for a distributed storage system. The interesting
failure is usually **something destructive happening without the human approval
that was supposed to gate it**, or the operator handing out more privilege than
it needs.

- **A destructive operation running without its approval annotation.** The
  operator never formats or destroys on its own: `dmg storage format` waits for
  `daos.gluesys.com/format-approved`, destroying a pool or container waits for
  `daos.gluesys.com/destroy-approved`, and replacing certificates waits for
  `daos.gluesys.com/certs-renew-approved`. The annotation is dropped after one
  attempt, whatever the outcome. **A path that formats, destroys or re-keys
  without that gate, or that replays an annotation, is a security issue** even
  when it looks like a plain bug — the result is data loss with no operator
  action behind it.
- **Reconciling the wrong object.** Adopting a DAOS pool that belongs to another
  `DaosPool`, or pointing an engine at a device that another rank already owns.
  Two ranks writing the same drive corrupts data silently.
- **Privilege escalation through the pods the operator creates.** It already asks
  for a lot — `hostNetwork` on every DAOS client, `/dev/infiniband` and
  `privileged` on an RDMA fabric unless an RDMA device plugin supplies the
  devices, and a `hostprep` DaemonSet that raises hugepages and can hand NVMe to
  SPDK. A change that widens any of that, or lets a workload pod inherit it, is
  in scope.
- **Leaking a credential the operator handles.** `S3Service` root credentials,
  image pull secrets, and the DAOS agent/admin certificates it mounts.
- **The operator's RBAC.** Anything that lets a user who can create a custom
  resource reach beyond what that resource is about.

## What is out of scope

- **DAOS itself, and the container images' contents.** Report those to the
  [DAOS project](https://github.com/daos-stack/daos). If the issue is in how
  *this* operator drives DAOS, it is in scope.
- **Misconfiguration of the underlying storage.** Notably, two DAOS ranks
  formatting the same dual-port drives will corrupt data, and no amount of care
  in this code prevents it. Compare PCI device serial numbers across nodes before
  you trust any data; the operator publishes them as
  `daos.gluesys.com/bdev-dsn` node annotations so you can.
- **Access control inside a DAOS pool or container.** Multi-tenant isolation is
  the deployer's job. The operator creates pools and containers; it does not
  enforce per-tenant boundaries within them.
- **Chart values that are insecure by choice**, e.g. `tls: false`, or putting
  `s3.services[].rootCredentials` inline instead of naming an existing Secret.
  Helm values end up in the release Secret, which `values.yaml` warns about at
  that key. We will take reports about the warning being wrong or missing.

## Supported versions

There is no stable release yet. The chart is published with
`artifacthub.io/prerelease: "true"`, and fixes land on `main`.

| Version | Supported |
|---|---|
| `main` | yes |
| latest `v0.1.0-rc.*` | best effort |
| anything else | no |

Once releases begin, this section will name the supported branches.

## Hardening notes for deployers

These are not vulnerabilities in the code, but they decide whether a deployment
is safe:

- **Verify that no two DAOS ranks own the same physical drive before you trust
  any data.** Compare `daos.gluesys.com/bdev-dsn` across nodes and confirm each
  rank reports distinct device serial numbers. Dual-port backplanes make this
  easy to get wrong and the damage is silent.
- **Keep the approval annotations manual.** They exist so that a format or a
  destroy is a decision somebody made. Automating them in a controller or a
  pipeline removes the only thing standing between a reconcile loop and your
  data.
- **Prefer an RDMA device plugin to `privileged`.** With `rdmaResource` the
  plugin injects only the verbs devices the pod needs, which drops both the
  `/dev/infiniband` hostPath and `privileged`, leaving `IPC_LOCK`. Throughput is
  unchanged; `values.yaml` documents the setting next to `rdma`.
- **Name an existing Secret for S3 root credentials** rather than letting the
  chart create one from values.
- **Turn on `tls`** for anything beyond a testbed, so the agent and admin
  certificates are mounted rather than the system running without them.
- **Deleting a custom resource does not delete data.** Removing a `DaosSystem`
  leaves the storage untouched, and removing a `DaosPool` without
  `daos.gluesys.com/destroy-approved=true` keeps the DAOS pool and emits a
  `PoolOrphaned` event. Plan decommissioning deliberately — see the README's
  Uninstall section.
