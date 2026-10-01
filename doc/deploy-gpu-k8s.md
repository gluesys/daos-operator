<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# Deploying on a GPU Kubernetes cluster — vLLM KV cache on DAOS

How to make LMCache store the KV cache vLLM produces in a DAOS container. When the
same prompt comes back, the cache is restored from DAOS **even after the pod
restarted** (§6 verifies exactly that).

One `helm install` brings up the operator, the CSI driver, the S3 gateway and the
vLLM reference workload together.

```
                    ┌─────────────── GPU node ───────────────┐
     prompt ──▶ vLLM(hostNetwork) ──▶ LMCache ──▶ lmcache-daos ──libdfs──┐
                    │      ▲                                 │          │
                    │      └── daos_agent (DaemonSet, one per node)     │
                    └─────────────────────────────────────────┘         ▼
                                                            DAOS container (kvlmc)
                                                                in a DaosPool
```

---

## 1. Prerequisites

| Item | Requirement | Check |
|---|---|---|
| Kubernetes | **1.29 or later** (native sidecars) | `kubectl version` |
| DAOS | one 2.8 system (external, or pod servers run by the operator) | `kubectl get daossystem` |
| GPU | NVIDIA GPU + driver on the node, `nvidia.com/gpu` advertised | `kubectl get nodes -o custom-columns=N:.metadata.name,GPU:.status.capacity.nvidia\.com/gpu` |
| Image store | the serving image is **~10 GB on disk**; the node's container store needs that much free | `df -h` (§5.2) |
| Model | keep the HF cache on the node and mount it by hostPath (offline), or use a PVC | |

> **Why hostNetwork.** daos_agent hands the client the interface names it sees, and
> the DAOS servers must be able to **reach back at the address the client
> announced**. A client on the pod network sits behind the CNI's masquerade
> (flannel's `-s 10.244.0.0/16 -j MASQUERADE`, for instance), so the announced
> address and the real source address disagree and every RPC ends in
> `crt_proto_query() ... DER_TIMEDOUT`. **We measured this, and cross-node access
> to the same system works when the client is on hostNetwork.** The CSI node plugin
> and the S3 gateway are on hostNetwork for the same reason.

---

## 2. GPU runtime — the three common setups

For a vLLM pod to get a GPU you need (a) a device plugin advertising
`nvidia.com/gpu` and (b) a runtime that injects the driver into the container.
Depending on the environment, both may already be in place.

### 2.1 NVIDIA GPU Operator (the most common · recommended)

The most widely used option on on-premises clusters. It installs the driver, the
toolkit, the device plugin and the RuntimeClass in one go.

```bash
helm repo add nvidia https://nvidia.github.io/gpu-operator && helm repo update
helm install gpu-operator nvidia/gpu-operator -n gpu-operator --create-namespace \
  --set driver.enabled=true          # false if the node already has the driver
```

Check:
```bash
kubectl get runtimeclass                     # nvidia must be there
kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}={.status.capacity.nvidia\.com/gpu}{"\n"}{end}'
```

What to put in values:
```yaml
runtimeClassName: nvidia
nodeSelector: {"nvidia.com/gpu.present": "true"}     # label the GPU Operator adds
tolerations: [{key: nvidia.com/gpu, operator: Exists, effect: NoSchedule}]
```

### 2.2 Device plugin alone + nvidia-container-toolkit (the setup verified here)

For nodes whose driver you manage yourself. **CRI-O has one trap** (§5.1).

```bash
# on the node (GPU nodes only)
dnf install -y nvidia-container-toolkit
nvidia-ctk runtime configure --runtime=crio     # --runtime=containerd for containerd
systemctl restart crio                          # or containerd

# in the cluster
kubectl apply -f - <<'YAML'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: {name: nvidia}
handler: nvidia
YAML
kubectl create -f https://raw.githubusercontent.com/NVIDIA/k8s-device-plugin/v0.17.1/deployments/static/nvidia-device-plugin.yml
```

Run the device plugin DaemonSet with `runtimeClassName: nvidia` — that is what lets
it see NVML. Keep the default `DEVICE_LIST_STRATEGY=envvar` strategy; on CRI-O,
avoid `cdi-cri` / `cdi-annotations` because of §5.1.

### 2.3 Managed Kubernetes (EKS · GKE · AKS)

Create a GPU node pool and the device plugin is usually already there. Only the
labels differ.

| Environment | nodeSelector example | Note |
|---|---|---|
| EKS (GPU AMI) | `{"nvidia.com/gpu.present": "true"}` or the instance-type label | device plugin included by default |
| GKE | `{"cloud.google.com/gke-accelerator": "nvidia-tesla-a100"}` | apply the driver DaemonSet separately |
| AKS | `{"accelerator": "nvidia"}` | installed when the GPU node pool is created |

```bash
kubectl get nodes -o json | jq -r '.items[] | .metadata.name + " " + (.status.capacity["nvidia.com/gpu"] // "none")'
```

> Managed environments sometimes **block hostNetwork and hostPath by policy**
> (PSA/OPA). A DAOS client needs both, so the namespace needs an exemption
> (`pod-security.kubernetes.io/enforce: privileged`). The cloud VPC also has to
> reach the DAOS servers at L3.

---

## 3. Preparing the DAOS side

### 3.1 System, pool and KV container

```bash
kubectl get daossystem                      # is it Ready
kubectl apply -f - <<'YAML'
apiVersion: daos.gluesys.com/v1alpha1
kind: DaosPool
metadata: {name: kvpool}
spec: {systemRef: <DaosSystem name>, size: 100Gi, redundancyFactor: 0}
---
apiVersion: daos.gluesys.com/v1alpha1
kind: DaosContainer
metadata: {name: kvlmc, namespace: daos-system}
spec:
  poolRef: kvpool
  type: POSIX
  chunkSize: 4194304        # 4 MiB -- most of lmcache-daos performance is decided here
  fileOclass: SX            # use replication (RP_2GX etc.) with two or more nodes
  dirOclass: S1
  redundancyFactor: 0
YAML
```

The 1 MiB default `chunkSize` costs an RPC per chunk and drops read throughput
badly. **Use 4 MiB.**

### 3.2 Installing the client agent on the GPU nodes

Run one daos_agent per GPU node and expose its socket through a hostPath. Serving
pods only need to mount that socket.

```yaml
# DaosSystem spec
clientAgent:
  enabled: true
  nodeSelector: {"nvidia.com/gpu.present": "true"}   # GPU nodes
  # hostSocketDir: /var/run/daos_agent/<system>      # default; several systems can coexist
  # hostNetwork: true                                # default. Leave it alone
client:
  includeFabricIfaces: ["ens18"]   # the fabric interface names the servers use
```

```bash
kubectl get daossystem <name> -o jsonpath='{.status.conditions[?(@.type=="ClientAgent")].message}{"\n"}'
# "N node agent(s), host network; clients mount hostPath ... as /var/run/daos_agent"
```

Leave `includeFabricIfaces` empty and the agent's fabric scan will offer the
**CNI interfaces (cni0, flannel.1)** as candidates, after which the client cannot
reach the engines. Name the interfaces the engines use.

---

## 4. Install

```bash
helm install daos daos-operator/charts/daos-operator -n daos-system --create-namespace -f my-values.yaml
```

`my-values.yaml` (for a GPU Operator environment):

```yaml
vllm:
  services:
    - name: vllm-daos
      image: registry.gitlab.gluesys.com/exastor/daos-images/vllm-lmcache-daos:0.30.0-0.5.5-20260927
      poolRef: kvpool
      containerRef: kvlmc
      systemName: daos_k8s                       # DaosSystem spec.systemName
      agentSocketHostPath: /var/run/daos_agent/daos-k8s
      model:
        path: /data/hf_cache/hub/models--meta-llama--Llama-3.1-8B-Instruct/snapshots/<hash>
        servedName: llama31-8b
        cacheHostPath: /mnt/nvme/hf_cache        # the node's HF cache -> /data/hf_cache
      port: 8100                                 # hostNetwork: a free port on the node
      gpus: 1
      gpuMemoryUtilization: "0.90"
      maxModelLen: 16384
      runtimeClassName: nvidia
      nodeSelector: {"nvidia.com/gpu.present": "true"}
      tolerations: [{key: nvidia.com/gpu, operator: Exists, effect: NoSchedule}]
      resources: {requests: {cpu: "8", memory: 32Gi}}
```

To keep the model in a PVC instead of a hostPath, drop `model.cacheHostPath`, set
`HF_HOME` through `extraEnv` and attach the PVC yourself — the chart only renders
the hostPath form today.

---

## 5. Traps, per environment (all measured)

### 5.1 CRI-O ignores `cdi.k8s.io/*` annotations — when you wire CDI by hand

Write the CDI spec (`/etc/cdi/nvidia.yaml`), annotate the pod with
`cdi.k8s.io/gpu: nvidia.com/gpu=all`, and neither `/dev/nvidia*` nor NVML shows up
— the crio log holds no trace of CDI being processed at all. The device plugin then
CrashLoops with `CDI --device-list-strategy options are only supported on
NVML-based systems`.
→ Use the **nvidia runtime handler + RuntimeClass** and run the device plugin with
the `envvar` strategy (§2.2).

**This applies to the hand-wired setup of §2.2, not to the GPU Operator.** Measured
on 2026-10-01 (CRI-O 1.31.5, the same version, Rocky 10.2, H100 NVL): the GPU
Operator's device plugin runs with `DEVICE_LIST_STRATEGY=cdi-annotations,cdi-cri`
and `CDI_ENABLED=true` and it works — a pod that names no `runtimeClassName` at all
gets the GPU. Its toolkit container installs the CDI specs and the runtime drop-in
that a hand-wired setup has to get right itself. So the difference is not CRI-O; it
is who did the wiring.

### 5.2 The serving image fills up the node disk

It is ~10 GB on disk. If the root partition is small and shared with another
container runtime (podman, say), the pull can trip the `disk-pressure` taint, after
which nothing schedules on the node and the kubelet image GC **starts deleting other
images.**

When moving to a bigger disk, **move CRI-O alone**:
```toml
# /etc/crio/crio.conf.d/98-storage.toml
[crio]
root = "/mnt/nvme/crio"
runroot = "/run/crio"
```
Moving podman's graphroot instead does not work — podman's database stores an
absolute path per container, so containers fail to start with `database
configuration mismatch`. Separating CRI-O also removes the risk of the kubelet GC
touching another runtime's images.

### 5.3 The image ships no CUDA toolchain

The flashinfer sampler tries to JIT-compile a kernel, fails with `Could not find
nvcc` and **takes the whole engine startup down with it.** The chart sets
`VLLM_USE_FLASHINFER_SAMPLER=0` by default (no measurable cost on sm_86; only
sampling moves to the PyTorch path).

### 5.4 hostNetwork means the port is shared with the node

The default is 8100. Change `port:` if the node already uses it. Two services on one
node need different ports.

### 5.5 The per-node agent and its socket

The socket lives on a hostPath, so **a socket file left behind by a previous agent
can block the new one** (`Configured dRPC socket file is already in use`). The
operator adds an initContainer that cleans up before start — build your own
DaemonSet and you have to do the same.

---

## 6. Verification

```bash
POD=$(kubectl -n daos-system get pod -l app=vllm-daos -o name | head -1)
NODE=$(kubectl -n daos-system get $POD -o jsonpath='{.spec.nodeName}')

# 1) did the connector attach
kubectl -n daos-system logs $POD | grep -E "Creating connector|DaosConnector"
#   Creating connector for URL: plugin://daos/kvpool/kvlmc?sys=daos_k8s

# 2) cold: send one request and confirm the store
curl -s -X POST http://<node IP>:8100/v1/completions -H 'Content-Type: application/json' \
  -d '{"model":"llama31-8b","prompt":"'"$(python3 -c 'print("DAOS is an object store. "*120)')"'","max_tokens":16,"temperature":0}' >/dev/null
kubectl -n daos-system logs $POD | grep "Stored"
#   Stored 1024 out of total 1024 tokens. size: 0.1250 GB
kubectl get daospool kvpool -o jsonpath='used={.status.usedPercent}%{"\n"}'

# 3) throw away the local cache, send the same prompt -- is it restored from DAOS (the point)
kubectl -n daos-system rollout restart deploy/vllm-daos
#   (once it is ready, send the same request again)
kubectl -n daos-system logs $POD | grep -E "hit tokens|Retrieved"
#   LMCache hit tokens: 1024
#   Retrieved 1024 out of 1024 required tokens
```

If step 3 passes, the KV cache lives in DAOS, outside the GPU node.

---

## 7. Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `crt_proto_query() ... DER_TIMEDOUT` | client on the pod network (masquerade) | put the pod on `hostNetwork: true` |
| `daos_init failed: rc=-1011` | the agent is not up, or its socket is a ghost | `kubectl logs <agent pod>`, §5.5 |
| agent says `socket file is already in use` | an old socket on the hostPath | clean it in an initContainer (the operator does) |
| device plugin CrashLoop, `only supported on NVML-based systems` | CDI annotations do nothing | RuntimeClass nvidia + `envvar` strategy (§5.1) |
| `Could not find nvcc` | no CUDA toolchain in the image | `VLLM_USE_FLASHINFER_SAMPLER=0` (chart default) |
| pod `Pending`, node has `disk-pressure` | the 10 GB image does not fit | move the CRI-O store to a bigger disk (§5.2) |
| `ImagePullBackOff` 403 | internal registry authentication | add an `imagePullSecret` in that namespace |
| reads are slow | the container's default 1 MiB `chunkSize` | recreate it with 4 MiB (§3.1) |

---

## 8. The combination that was verified

| Item | Value |
|---|---|
| Kubernetes / runtime | 1.31.14 / CRI-O 1.31.5 (cgroup v2) |
| GPU | NVIDIA RTX A6000 (sm_86), driver 610.43.02, nvidia-container-toolkit 1.20.1 |
| DAOS | 2.8.0 (pod servers, ofi+tcp) |
| Image | `vllm-lmcache-daos:0.30.0-0.5.5-20260927` (vLLM 0.30.0 + LMCache 0.5.5 + lmcache-daos) |
| Model | Llama-3.1-8B-Instruct, bf16, max-model-len 16384 |

The GPU Operator path was verified separately on 2026-10-01: GPU Operator v26.7.1
with `driver.enabled=false` (host driver 610.57.04) and `toolkit.enabled=true`, on
Kubernetes 1.31.14 / CRI-O 1.31.5 / Rocky 10.2 with one H100 NVL and a RoCE 400G
link. A single pod took `nvidia.com/gpu: 1` together with `hostNetwork`,
`privileged`, a `/dev/infiniband` hostPath and the agent socket, and saw the GPU,
the CUDA devices, the DAOS pools and the IB devices at once. The operator adds its
own `99-nvidia.conf` drop-in and leaves a custom CRI-O storage root (§5.2) alone.

Not verified: letting the GPU Operator manage the **network** driver (MOFED/DOCA).
That path touches the NIC driver the DAOS client depends on, so it needs its own
test. containerd and managed Kubernetes also remain unverified — only how to
configure them is written down.

## When the fabric is RDMA (ofi+verbs, ucx)

If the DAOS system runs an RDMA provider, **the client pod has to see the verbs
devices directly.** Without `/dev/infiniband`, libfabric's verbs provider fails to
initialize (a fatal `na_ofi_provider_check`) and the connector never comes up. It is
a requirement that stays hidden over tcp, so you meet it the moment you switch
fabrics.

In the chart, give the workload `rdma: true`:

```yaml
vllm:
  services:
    - name: vllm-daos
      rdma: true          # mounts /dev/infiniband + privileged
      ...
```

The CSI node plugin and the S3 gateway are under the same condition (both already
mount `/dev` and run privileged). On a cluster with an RDMA device plugin, swap the
hostPath for that resource request.

Measured (2026-09-28, 100 Gb IB, 3 ranks on da1~3 + one GPU node):

| Path | 1 GbE management network | IB/verbs |
|---|---|---|
| dfuse write / read (1 GiB) | 5.2 / 10.5 MiB/s | **587 / 914 MB/s** |
| CSI PVC write (1 GiB) | 26 MB/s | **744 MB/s** |
| vLLM 3840-token KV cache | failed | **`Retrieved 3840/3840`** |

**A note on the LMCache connector**: if the first DAOS connection takes more than
3 seconds, the health monitor goes degraded and skips lookup and store entirely. On
a slow path, or a system with many ranks, raise `DAOS_PING_TIMEOUT` to about 15
seconds.
