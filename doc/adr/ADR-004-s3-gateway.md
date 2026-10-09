<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# ADR-004: S3 gateway — versitygw-daos behind an `S3Service` CRD

- Status: proposed
- Date: 2026-09-23
- Deciders:

## Context

MinIO's community edition stopped being maintained in 2026-02 and its repository was archived in
2026-04, which left people looking for a replacement.

We already have a DAOS backend in a `versitygw` fork: it maps one S3 bucket onto one DFS container
inside a DAOS pool and attaches directly with `libdfs`
(`versitygw daos --pool <label> [--system <name>]`).

Upstream versitygw ships a Helm chart, but its backends are posix/s3/azure only, and everything a
DAOS backend needs — which pool, a `daos_agent` sidecar, the client configuration ConfigMap, TLS
certificates — has to be filled in by hand. That wiring is information the operator already holds.

## Decision

1. **A namespace-scoped CRD, `S3Service`.** Point `spec.poolRef` at a `DaosPool` and the operator
   creates a Deployment and a Service and fills in `--pool`/`--system`, the agent sidecar, the agent
   ConfigMap and the certificates.
2. **The data path is `libdfs` directly; dfuse is not used.** So this pod needs neither a CSI PV nor
   FUSE.
3. **Root credentials arrive only as a Secret** (`spec.rootCredentialsSecret`, keys `accessKey` /
   `secretKey`). The CRD has no plaintext field for them.
4. **State the owner of the IAM data.** The internal (flat-file) IAM lives in a pod-local directory
   that `--iam-dir` points at.
   - The default is `replicas: 1` with a PVC. Without a PVC it is an emptyDir, and then **S3 user
     accounts disappear when the pod dies** (object data is in DAOS, so that survives). The status
     says so when this is the case.
   - `replicas > 1` is allowed only with an external IAM (LDAP/Vault/S3) or an RWX PVC. Several pods
     each keeping a local file IAM means the user list diverges silently.
5. **The operator does not create buckets.** A bucket is a DFS container, but having the S3 client
   create it is the normal path. `DaosContainer` CRs and S3 buckets are not synchronised in both
   directions (the rule against a second source of truth).

## Rationale

- Putting dfuse in the path adds POSIX-translation latency and brings FUSE, `privileged` and mount
  lifetime along with it. The backend is already `libdfs`.
- The IAM branch is not a preference, it is **the possibility of losing data**, so the CRD has to
  force the choice. In a MinIO-replacement scenario, users and keys vanishing is an outage even when
  every object is intact.
- Bucket synchronisation looks attractive until an S3-created container and a CR disagree, and then
  there is no answer to which one is right.

## Consequences and trade-offs

- The gateway holds **almost no state** (the IAM directory aside). Restarting the pod does not touch
  object data.
- `replicas > 1` is only really HA with an external IAM. v1alpha1 supports external IAM through
  `spec.extraEnv` alone; dedicated fields wait for a real request.
- This overlaps with the upstream chart in function but not in audience — that chart wires an
  arbitrary backend by hand, this CRD drives a DAOS pool the operator already knows about.

## Revisit when

- Someone runs an external IAM: add LDAP/Vault fields under `spec.iam`.
- versitygw grows a DAOS-backed IAM store: revisit decision 4, since the replicas constraint
  disappears with it.
