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
| 1.32.6 (kubeadm) / containerd 2.2.1, Ubuntu 24.04 | ofi+verbs, IB 100G | operator pods, 2 ranks, MD-on-SSD, NVMe via SPDK | verified | 2026-11-13 | moon (gpu-node1~3), FlexA Hub `3ee2473f` |
| 1.29 · 1.31 · 1.34 | — | — | **CI** (rendered) | every main pipeline | `hack/ci/chart-schema.sh` (kubeconform) |
| containerd other than 2.2.1 · CRI-O other than 1.31 | any | any | unverified | — | — |
| managed (EKS · GKE · AKS) | any | external DAOS only | unverified | — | configuration in deploy-gpu-k8s §2.3 |
| 1.31.14 / CRI-O 1.31.5, flannel (vxlan) | ofi+tcp | `clientAgent.hostNetwork: false` (pod-network agent), ordinary pod: no SNAT to the fabric subnet + `rp_filter=2` | verified (fails with `DER_TIMEDOUT` as installed) | 2026-11-17 | lane A; serving-integration.md §2; [note E](#e-pod-network-agent-2026-11-17) |
| any | ofi+verbs, ucx | `clientAgent.hostNetwork: false` | unverified | — | — |

## 2. GPU node and serving (reference workload, serving-integration.md)

| GPU · how it is wired | RDMA access | LMCache mode · model | Status | Date | Where · evidence |
|---|---|---|---|---|---|
| RTX A6000 · device plugin + nvidia-container-toolkit, CRI-O | — (tcp) | in-process · Llama-3.1-8B | verified | 2026-09-27 | deploy-gpu-k8s §2.2, §6; [note A](#a-rtx-a6000-2026-09-27) |
| H100 NVL · GPU Operator v26.7.1 (`driver.enabled=false`) | `/dev/infiniband` + privileged, RoCE 400G | agent + DAOS pools seen from the GPU pod | verified | 2026-10-01 | [note B](#b-h100--gpu-operator-2026-10-01) |
| H100 NVL · GPU Operator | RDMA shared device plugin (Network Operator 26.4.2) + `IPC_LOCK` | 64 MiB round trip, 2777 / 12162 MB/s | verified | 2026-10-01 | deploy-gpu-k8s "When the fabric is RDMA" |
| A100 80GB · device plugin (CDI), containerd 2.2.1 | `/dev/infiniband` + privileged, IB 100G | **MP** · Qwen3.8-27B INT4, hand-written Deployment | verified | 2026-11-13 | moon, Hub `3ee2473f` §4 |
| RTX A6000 · podman on one host (no Kubernetes), local DAOS 4 ranks | — | **MP** + MTP 3 · Qwen3.8-27B, FP8 KV, chunk 1600, moon's arguments | verified (restored = cold, 96/96) | 2026-11-15 | cxl2; [note C](#c-mtp--lmcache-mp-2026-11-15) |
| NVIDIA A2 · device plugin + nvidia-container-toolkit, CRI-O | `/dev/infiniband` + privileged, ofi+verbs | **MP via the chart** (`lmcacheMode: mp`) · Qwen2.5-1.5B (not hybrid) | verified | 2026-11-15 | lane A; [note D](#d-chart-mp-mode-on-a-gpu-2026-11-15) |
| NVIDIA A2 · GPU Operator v26.7.1 (host driver and toolkit), CRI-O | — (tcp) | **MP via the chart, hybrid model** · Qwen3.5-4B (GDN + full attention), bf16, chunk 528 | verified | 2026-11-17 | lane A; [note G](#g-chart-mp-mode-with-a-hybrid-model-2026-11-17) |
| any | any | MP via the chart, Qwen3.8-27B | rendered (+ launcher test) | 2026-11-14 | `hack/ci/chart-schema.sh`; moon runs it with a hand-written Deployment |
| Rebellions NPU · `accelerator: rbln` | any | in-process (lmcache-rbln) | rendered | 2026-10-13 | chart 0.1.6 |
| NVIDIA A2 · GPU Operator v26.7.1 (`driver.enabled=false`, `toolkit.enabled=false`: toolkit on the host), CRI-O, **DAOS server on the same node** (NVMe via SPDK, hugepages) | — (tcp) | **MP via the chart** · Qwen2.5-1.5B, restored from DAOS after a restart | verified | 2026-11-17 | lane A; [note F](#f-gpu-operator-next-to-a-daos-server-2026-11-17) |
| GPU Operator managing the NIC driver (MOFED/DOCA) | any | any | unverified | — | touches the NIC driver the DAOS client uses; needs its own test |

## 3. Images in the rows above

| Image | Tag used |
|---|---|
| DAOS servers / agent / admin / client | `2.8.0-20261110-gc1645462` (lane B, moon) |
| vllm-lmcache-daos | `0.30.0-0.5.5-20260927` (A6000), `0.30.0-0.5.5-20261028` (moon, 2026-11-13), `0.32.0-0.5.6-20261114` (moon from 2026-11-14, note C) |

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

### C. MTP + LMCache MP, 2026-11-15

The question was whether a reply restored from the cache matches the cold reply when MTP is on.
The image and arguments were moon's (`vllm-lmcache-daos:0.32.0-0.5.6-20261114`, FLASHINFER, FP8
KV, MTP 3, chunk 1600, `--separate-object-groups`), with `--max-model-len 40000` to fit 48 GB, on
an RTX A6000 (sm_86; moon is sm_80). Greedy replies to prompts of 4321, 16321 and 32321 random
words, a Korean document question, a needle and a code question were compared byte for byte: one
cold reply against two GPU hits and six alternating storage and GPU hits. The storage hits read
from DAOS L2. MTP on, chunk 1600: 48/48 equal. MTP off, chunk 1568: 48/48 equal. MTP off with
chunk 1600 does not start (serving-integration §3.2).

Earlier, vLLM 0.30 + LMCache 0.5.5 and 0.5.7 with bf16 KV (MTP block 800) gave 27/27 different
replies, the symptom of LMCache#5675. vLLM, LMCache and the KV dtype all changed between the two
runs, so which one made the difference is not known. Treat MTP + LMCache as per image: run the
check again whenever one of the three changes.

### D. Chart MP mode on a GPU, 2026-11-15

Lane A, Kubernetes 1.31.14 / CRI-O 1.31.5. DAOS 3 ranks (kdev-3rank). The pool and the container came
from a `DaosPool` (30Gi) and a `DaosContainer` (POSIX, 4 MiB). vLLM came from `vllm.services[]` with
`lmcacheMode: mp` and nothing edited by hand. The image was `vllm-lmcache-daos:0.30.0-0.5.5-20261028`
and the GPU an NVIDIA A2 (16 GB), so the model was Qwen2.5-1.5B-Instruct, not a hybrid one.

All three checks of serving-integration §4 passed:

1. The launcher reported the MP server listening after 10 s, then started vLLM, which created its
   `LMCacheMPConnector`.
2. Two cold prompts of about 9.6k tokens were stored.
3. After `kubectl rollout restart` (a new pod with an empty L1) the same prompts were served from DAOS:
   `37/37 keys (0 L1, 37 L2)`, 259 MiB in 228 ms. The time to answer dropped from 3.2 s to 0.9 s, and
   the text was the same as the cold replies.

The `nvidia` RuntimeClass used by the device plugin is not created by the chart.

### E. Pod-network agent, 2026-11-17

Lane A, Kubernetes 1.31.14 / CRI-O 1.31.5, flannel vxlan with `ipMasq`. DAOS 1 rank over ofi+tcp
(kdev-1rank), the agent on another node with `hostNetwork: false` and
`includeFabricIfaces: [ens19, eth0]`. The client was an ordinary pod (no hostNetwork, not
privileged) that mounted the agent socket. The agent handed it `eth0`, `ofi+tcp`.

As installed, every call ended in `DER_TIMEDOUT`. Two causes, each enough on its own: the pod's
traffic to the fabric subnet was masqueraded, and strict reverse path filtering dropped the
handshake on both nodes, since the fabric interface carries the packets but the route to the pod
subnet goes through `flannel.1`. With a `RETURN` rule for the fabric subnet ahead of flannel's
masquerade on the client node and `rp_filter=2` on both nodes, `daos pool query` worked, and a
1 GiB random file copied in with `daos fs copy` and back out had the same sha256.

### F. GPU Operator next to a DAOS server, 2026-11-17

Lane A, node exaci5-4a: Kubernetes 1.31.14 / CRI-O 1.31.5 on EL8 (cgroup v1, `cgroupfs`), NVIDIA A2,
host driver 610.57.04 and nvidia-container-toolkit 1.20.2. GPU Operator v26.7.1 with
`driver.enabled=false`. On the same node: one DAOS engine (`nvme-1rank`: two NVMe bound to
`uio_pci_generic` for SPDK, 2 MiB hugepages, hostNetwork, privileged), the client agent and the
reference workload from the chart (`lmcacheMode: mp`, `runtimeClassName: nvidia`, `nvidia.com/gpu: 1`).

- With `toolkit.enabled=true` the toolkit pod looped in CrashLoopBackOff. Each start restarted CRI-O,
  and on this node that killed every container: conmon ran inside `crio.service`'s cgroup, and
  systemd stops the whole cgroup. The node feature discovery worker and DCGM exporter went down with
  it each time (deploy-gpu-k8s §2.1). The toolkit on the host with `toolkit.enabled=false` fixed it.
- After that, the operator's pods (feature discovery, device plugin, DCGM exporter, validators)
  stayed up while hostprep bound the NVMe and set the hugepages and the engine formatted and joined.
  DAOS and the GPU Operator did not touch each other's resources.
- The checks of serving-integration §4 passed as in note D: two cold prompts of ~9.6k tokens stored,
  then after `kubectl rollout restart` `37/37 keys (0 L1, 37 L2)` read from the local engine in about
  0.1 s, the time to answer down from 3.2 s to 0.75 s, text equal (2/2).

### G. Chart MP mode with a hybrid model, 2026-11-17

The node of note F (A2 16 GB, GPU Operator, a DAOS engine on the same node). Qwen3.5-4B in bf16, the
smallest Qwen hybrid model (linear GDN layers with a full attention layer every fourth). Image
`vllm-lmcache-daos:0.30.0-0.5.5-20261028`. Everything came from `vllm.services[]` with
`lmcacheMode: mp`. Two settings had to follow the model and the GPU, and vLLM named both at start-up:

- `lmcacheChunkSize: 528`. vLLM's block for this model is 528, and 256 stops it ("must be a
  multiple of 528").
- `--max-num-seqs=32` in `extraArgs`. On 16 GB only 80 Mamba cache blocks fit, and the default of
  256 sequences stops CUDA graph capture.

The checks of serving-integration §4 passed. Two cold prompts of ~9.3k tokens took 7.7 s each and
were stored. After `kubectl rollout restart` the same prompts were served from DAOS:
`17/17 keys (0 L1, 17 L2)` in about 0.64 s, 8976 tokens retrieved. The time to answer dropped to 2.3 s,
and the text was the same as the cold replies (2/2).

## 5. Adding a row

Add a row when something is brought up in a new combination, or when an existing row is
run again (then update its date). Link the evidence: a pipeline, a testbed note in `doc/`,
or a Hub document. Keep the columns. A new dimension needs a reason that a real
environment differs on it.
