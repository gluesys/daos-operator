<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# Compatibility

This page lists what was run, where, when, and the evidence for it. It says nothing about
what *should* work. A row without a date is not a claim.

| Status | Meaning |
|---|---|
| **verified** | brought up and exercised end to end on the date shown. The evidence is linked |
| **CI** | exercised on every main pipeline: the e2e lane or the render checks |
| **rendered** | manifests render and pass kubeconform. Never run |
| **unverified** | not run. Only the configuration is written down, or nothing is |

**Rule.** At each chart release, a **verified** row older than 90 days moves to
*re-check needed* unless it was run again. New rows need a date and evidence.

## 1. Storage and client layer (operator, CSI, client agent)

| Kubernetes / runtime | Fabric | DAOS servers | Status | Date | Where · evidence |
|---|---|---|---|---|---|
| 1.31.14 / CRI-O 1.31.5 | ofi+tcp | operator pods, kdev and nvme engines, 1–3 ranks | **CI** (e2e tier 1) | every main pipeline (last 2026-11-14, pipeline 66466) | lane B (vdi5 VMs), `test/e2e`, doc/ci-design-2026-10-02.md |
| 1.31.14 / CRI-O 1.31.5 | ofi+tcp | operator pods | verified | 2026-09-22 | exaci4-2, doc/testbed-k8s-pod-servers.md |
| 1.31.14 / CRI-O 1.31.5 | ofi+verbs, IB 100G | operator pods, 3 ranks, MS ×3 | verified | 2026-09-28 | da1~da4, doc/testbed-da-ib-verbs.md |
| 1.32.6 (kubeadm) / not recorded | ofi+verbs, IB 100G | operator pods, 2 ranks, MD-on-SSD, NVMe via SPDK | verified | 2026-11-13 | moon (gpu-node1~3), FlexA Hub `3ee2473f` |
| 1.29 · 1.31 · 1.34 | — | — | **CI** (rendered) | every main pipeline | `hack/ci/chart-schema.sh` (kubeconform) |
| containerd (any version) | any | any | unverified | — | — |
| managed (EKS · GKE · AKS) | any | external DAOS only | unverified | — | configuration in deploy-gpu-k8s §2.3 |
| any | any | `clientAgent.hostNetwork: false` (pod-network agent) | unverified | — | serving-integration.md §2 |

## 2. GPU node and serving (reference workload, serving-integration.md)

| GPU · how it is wired | RDMA access | LMCache mode · model | Status | Date | Where · evidence |
|---|---|---|---|---|---|
| RTX A6000 · device plugin + nvidia-container-toolkit, CRI-O | — (tcp) | in-process · Llama-3.1-8B | verified | 2026-09-27 | deploy-gpu-k8s §2.2, §6; [note A](#a-rtx-a6000-2026-09-27) |
| H100 NVL · GPU Operator v26.7.1 (`driver.enabled=false`) | `/dev/infiniband` + privileged, RoCE 400G | agent + DAOS pools seen from the GPU pod | verified | 2026-10-01 | [note B](#b-h100--gpu-operator-2026-10-01) |
| H100 NVL · GPU Operator | RDMA shared device plugin (Network Operator 26.4.2) + `IPC_LOCK` | 64 MiB round trip, 2777 / 12162 MB/s | verified | 2026-10-01 | deploy-gpu-k8s "When the fabric is RDMA" |
| A100 80GB · device plugin (CDI) | `/dev/infiniband` + privileged, IB 100G | **MP** · Qwen3.8-27B INT4, hand-written Deployment | verified | 2026-11-13 | moon, Hub `3ee2473f` §4 |
| any | any | MP via the chart (`lmcacheMode: mp`) | rendered (+ launcher test) | 2026-11-14 | `hack/ci/chart-schema.sh` |
| Rebellions NPU · `accelerator: rbln` | any | in-process (lmcache-rbln) | rendered | 2026-10-13 | chart 0.1.6 |
| GPU Operator managing the NIC driver (MOFED/DOCA) | any | any | unverified | — | touches the NIC driver the DAOS client uses; needs its own test |

## 3. Images in the rows above

| Image | Tag used |
|---|---|
| DAOS servers / agent / admin / client | `2.8.0-20261110-gc1645462` (lane B, moon) |
| vllm-lmcache-daos | `0.30.0-0.5.5-20260927` (A6000), `0.30.0-0.5.5-20261028` (moon) |

## 4. Notes on the verified rows

### A. RTX A6000, 2026-09-27

Kubernetes 1.31.14 / CRI-O 1.31.5 (cgroup v2). NVIDIA RTX A6000 (sm_86), driver 610.43.02,
nvidia-container-toolkit 1.20.1. DAOS 2.8.0 in operator pods over ofi+tcp. Image
`vllm-lmcache-daos:0.30.0-0.5.5-20260927`, Llama-3.1-8B-Instruct, bf16, max-model-len 16384.
The KV cache was restored from DAOS after the pod restarted (deploy-gpu-k8s §6).

### B. H100 + GPU Operator, 2026-10-01

GPU Operator v26.7.1 with `driver.enabled=false` (host driver 610.57.04) and
`toolkit.enabled=true`, on Kubernetes 1.31.14 / CRI-O 1.31.5 / Rocky 10.2, one H100 NVL and a
RoCE 400G link. One pod held `nvidia.com/gpu: 1` together with `hostNetwork`, `privileged`, a
`/dev/infiniband` hostPath and the agent socket. From that pod it saw the GPU, the CUDA devices,
the DAOS pools and the IB devices. The GPU Operator adds its own `99-nvidia.conf` drop-in and
leaves a custom CRI-O storage root (deploy-gpu-k8s §5.2) alone.

## 5. Adding a row

Add a row when something is brought up in a new combination, or when an existing row is
run again (then update its date). Link the evidence: a pipeline, a testbed note in `doc/`,
or a Hub document. Keep the columns. A new dimension needs a reason that a real
environment differs on it.
