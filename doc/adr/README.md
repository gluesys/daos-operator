<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# Architecture decision records

| ADR | What it decides | English |
|---|---|---|
| [000](ADR-000-template.md) | the template for a new record | **yes** |
| [001](ADR-001-deployment-model.md) | dedicated storage nodes by default; GPU-node hyperconvergence as an option | **yes** |
| [002](ADR-002-device-and-network-binding.md) | VFIO host preparation as a DaemonSet, `hostNetwork`, `ofi+verbs`; and the 2026-09-22 revision that allows `kdev`/`file` for testing only | **yes** |
| [003](ADR-003-upgrade-model.md) | the full-stop upgrade is the first-class procedure until DAOS 3.0 | **yes** |
| [004](ADR-004-s3-gateway.md) | `S3Service` drives a versitygw gateway straight through `libdfs`, no dfuse | **yes** |
| [005](ADR-005-daos-baseline.ko.md) | the DAOS baseline is the upstream RPM; our differentiation sits outside DAOS | — *(Korean, internal)* |

## How to read this directory

These records were written inside Gluesys, in Korean. Each one that has an
English `.md` keeps the Korean original beside it as `.ko.md`.

**The English file is the technical decision and is the one to cite.** It is not
a translation of the whole document: several of the originals mix the decision
with commercial context — channel economics, contract shape, edition planning —
that is not part of the technical decision and is not maintained in English. The
`.ko.md` stays as the internal record.

`ADR-005` has no English version by intent. It is an internal record about which
DAOS build the product is based on, and most of it is planning rather than a
decision about this operator's behaviour.

New records are written in English, from `ADR-000-template.md`.
