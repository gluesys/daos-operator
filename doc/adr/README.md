<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# Architecture decision records

| ADR | What it decides | English |
|---|---|---|
| [000](ADR-000-template.md) | the template | — *(Korean)* |
| [001](ADR-001-deployment-model.md) | dedicated storage nodes by default; GPU-node hyperconvergence as an option | **yes** |
| [002](ADR-002-device-and-network-binding.md) | VFIO host preparation as a DaemonSet, `hostNetwork`, `ofi+verbs`; and the 2026-09-22 revision that allows `kdev`/`file` for testing only | — *(Korean)* |
| [003](ADR-003-upgrade-model.md) | the full-stop upgrade is the first-class procedure until DAOS 3.0 | **yes** |
| [004](ADR-004-s3-gateway.md) | `S3Service` drives a versitygw gateway straight through `libdfs`, no dfuse | — *(Korean)* |
| [005](ADR-005-daos-baseline.ko.md) | the DAOS baseline is the upstream RPM; our differentiation sits outside DAOS | — *(Korean, internal)* |

## How to read this directory

These records were written inside Gluesys, in Korean. Where an ADR now has an
English `.md`, **that file is the technical decision on its own and is the one to
cite.** It is not a translation of the whole document: the `.ko.md` beside it
keeps the original, which also carries commercial context — channel economics,
contract shape, edition planning — that is not part of the technical decision and
is not maintained in English.

`ADR-005` has no English version by intent. It is an internal record about which
DAOS build the product is based on, and most of it is planning rather than a
decision about this operator's behaviour.

`ADR-002` and `ADR-004` are technical throughout and are good candidates for an
English version; nobody has written one yet.
