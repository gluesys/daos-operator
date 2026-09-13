<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# Security policy

## Reporting

Email **kpkim@gluesys.com** with `[lmcache-daos]` in the subject. Please do not
open a public issue for a suspected vulnerability.

Useful things to include: what an attacker can do, the versions and transport
involved (`ofi+verbs`, `ucx+rc_v`, `ofi+tcp`), the DAOS release, and the smallest
reproduction you have. If you have a patch, attach it rather than opening a pull
request, since pull requests are public.

We will acknowledge within five working days. This is a small team with no
dedicated on-call, so please read that as an honest expectation rather than a
service level. We will tell you what we intend to do and when, and we will credit
you in the fix unless you ask us not to.

## What is in scope

This project is a storage backend for an LLM inference cache. The interesting
failure is usually not a crash.

- **Returning the wrong KV cache** — a hit that serves bytes belonging to another
  request, another tenant, or an older version of the same key. This is treated
  as a security issue even when it looks like a plain bug, because it is silent:
  sizes and return codes stay correct and the model produces fluent, wrong text.
  The repository has a documented case of exactly this
  (`gpudirect/DAOS-CONCURRENT-READ-CORRUPTION.md`).
- **Crossing a tenant boundary** — reading or overwriting objects that belong to
  another key namespace or DAOS container.
- **Anything that turns cache content into code execution** in the serving
  process, e.g. through the metadata path.
- Denial of service that a client can trigger cheaply against the cache server.

## What is out of scope

- **DAOS, LMCache, vLLM, UCX, Mercury and libfabric themselves.** Report those to
  their projects. If the issue is in how *this* code uses them, it is in scope.
- **The GPU-direct backend (`DaosGdsBackend`).** It is experimental, unsupported,
  and depends on an unmerged DAOS draft plus out-of-tree patches. We will read
  reports about it and fix what we can, but it carries no security promise and
  must not be run in production. See the README section "모드별 성숙도".
- **Misconfiguration of the underlying storage.** Notably, two DAOS ranks
  formatting the same dual-port drives will corrupt data, and no amount of care
  in this code prevents it. `gpudirect/DAOS-CONCURRENT-READ-CORRUPTION.md` §62
  documents how to detect that before it bites.
- Access control on the DAOS pool and container. Multi-tenant isolation is
  currently the deployer's job, not this backend's; there is no per-tenant
  enforcement here beyond the key namespace.

## Supported versions

There has been no tagged release yet. Only `main` is supported, and fixes land
there. Once releases begin, this section will name the supported branches.

| Version | Supported |
|---|---|
| `main` | yes |
| anything else | no |

## Hardening notes for deployers

These are not vulnerabilities in the code, but they decide whether a deployment
is safe:

- Verify that no two DAOS ranks own the same physical drive before you trust any
  data: compare PCI DSNs across nodes and confirm each rank reports distinct
  device UUIDs.
- `PYTHONHASHSEED=0` is required for cross-node cache sharing to be correct, not
  merely faster. Without it the key derivation differs between processes.
- Objects carry no payload checksum. Integrity on the wire is the transport's
  (RoCE ICRC) and the length check's; if you need more, use a DAOS object class
  with redundancy and verify at the application layer.
