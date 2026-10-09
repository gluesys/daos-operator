<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# ADR-003: Upgrade model — before 3.0, treat the full-stop upgrade as the first-class procedure

- Status: **accepted** (2026-09-29)
- Date: 2026-09-14 (accepted 2026-09-29)
- Evidence for acceptance: a full-stop restart (`dmg system stop` / `start`), adding a node, and
  recovering a failed rank all completed without data loss

> This is the English record of the decision. The internal Korean original
> (`ADR-003-upgrade-model.ko.md`) also carries commercial context that is not part
> of the technical decision.

## Context

From the DAOS Foundation: *"3.0 expects a communications protocol change which will not allow
backward compatibility with older versions."* 2.8 supports keeping clients across an upgrade, but
the servers have to move together. A rolling server upgrade is a 3.0 goal and a tech preview at
that. HPE's K3000 CSC automates the same shape — stop every server, upgrade, start.

Pretending otherwise would mean shipping a "rolling upgrade" that silently is not one.

## Decision

- When `DaosSystem.spec.version` changes, the operator performs a **full-stop upgrade**, and says so:
  confirm clients are drained → `dmg system stop` → swap the image tag → start the servers in order →
  `dmg system start` → verify.
- **No destructive step is ever part of it.** No format, no wipe. If the version field changes
  without the approval annotation, the system stops at `Pending` and waits.
- 2.8 → 3.0 is *not* this procedure. It needs a separate migration (data export/import, or whatever
  upstream tooling exists by then) and is designed in Phase 4.

## Consequences and trade-offs

The honest procedure is an outage, and it is visible as one. That is the point: an operator that
reports `Pending` and waits for a human is easier to trust than one that reports success while the
protocol underneath it changed.

## Revisit when

Rolling upgrade reaches GA in 3.0 — then add `strategy: Rolling`.
