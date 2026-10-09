<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# ADR-001: Deployment model — dedicated storage nodes by default, GPU-node HCI as an option

- Status: **accepted** (2026-09-29)
- Date: 2026-09-14 (accepted 2026-09-29)
- Evidence for acceptance: four ranks on dedicated storage nodes (da1~4) with GPU-node clients
  worked on real hardware (`doc/testbed-da-ib-verbs.md`)

> This is the English record of the decision. The internal Korean original
> (`ADR-001-deployment-model.ko.md`) also carries commercial context that is not
> part of the technical decision.

## Context

A DAOS engine pins one target xstream per SSD to a core and polls it, keeps MD-on-SSD metadata in
tmpfs, and takes NVMe exclusively through SPDK/VFIO. Put that on a GPU node and it holds DRAM — the
same DRAM an LMCache G2 tier wants — and cores for as long as it runs. Worse, every GPU-node reboot
becomes a rank exclusion and a rebuild, because the node is rebooted for reasons that have nothing
to do with storage.

## Decision

- A `DaosSystem` is placed by default on **dedicated storage nodes** selected with `nodeSelector`:
  three or more nodes, an odd number of management-service replicas.
- GPU-node hyperconvergence is offered **only** through `spec.placement.mode: hyperconverged`, and
  that path enforces taints/tolerations and memory/cpu requests. It is documented as performance
  outside the supported envelope.

## Consequences and trade-offs

- The three-node minimum is a barrier for a small proof of concept. Phase 0 runs with a single
  management-service replica (no HA) for that reason.
- The HCI option stays, but it is not the default, so anything that describes the product has to say
  the same thing.

## Revisit when

An eight-GPU measurement shows HCI mode holding 10% or less of node DRAM.
