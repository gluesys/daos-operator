<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# K8s GPU 환경 배포 가이드 — vLLM KV 캐시를 DAOS 에 내리기

vLLM 이 만든 KV 캐시를 LMCache 가 DAOS 컨테이너에 저장하게 만드는 배포 절차다.
같은 프롬프트가 다시 오면 **파드가 재시작된 뒤에도** 캐시가 DAOS 에서 복원된다(§6 검증).

`helm install` 한 번으로 operator·CSI·S3 게이트웨이·vLLM 참조 워크로드를 함께 올릴 수 있다.

```
                    ┌─────────────── GPU 노드 ───────────────┐
   프롬프트 ──▶ vLLM(hostNetwork) ──▶ LMCache ──▶ lmcache-daos ──libdfs──┐
                    │      ▲                                 │          │
                    │      └── daos_agent (DaemonSet, 노드당 1개)       │
                    └─────────────────────────────────────────┘         ▼
                                                            DAOS 컨테이너(kvlmc)
                                                              in DaosPool
```

---

## 1. 전제 조건

| 항목 | 요구 | 확인 |
|---|---|---|
| Kubernetes | **1.29 이상** (네이티브 사이드카) | `kubectl version` |
| DAOS | 2.8 시스템 하나 (외부 또는 operator 가 띄운 파드 서버) | `kubectl get daossystem` |
| GPU | 노드에 NVIDIA GPU + 드라이버, `nvidia.com/gpu` 자원이 보일 것 | `kubectl get nodes -o custom-columns=N:.metadata.name,GPU:.status.capacity.nvidia\.com/gpu` |
| 이미지 저장소 | 서빙 이미지가 **on-disk 약 10 GB**. 노드 컨테이너 저장소에 그만한 여유 | `df -h`(§5.2) |
| 모델 | HF 캐시를 노드에 두고 hostPath 로 마운트(오프라인) 하거나 PVC 사용 | |

> **왜 hostNetwork 인가.** daos_agent 는 자기가 보는 인터페이스 이름을 클라이언트에 건네고, DAOS 서버는
> **클라이언트가 알린 주소로 되짚어 닿을 수 있어야** 한다. 파드 네트워크의 클라이언트는 CNI 의 마스커레이드
> (flannel 의 `-s 10.244.0.0/16 -j MASQUERADE` 등) 뒤에 있어 알린 주소와 실제 출발지가 어긋나고, 모든 RPC 가
> `crt_proto_query() ... DER_TIMEDOUT` 으로 끝난다. **실측으로 확인했고, 같은 시스템에 hostNetwork 로는
> 교차 노드 접속이 정상 동작한다.** CSI 노드 플러그인과 S3 게이트웨이가 hostNetwork 인 것도 같은 이유다.

---

## 2. GPU 런타임 — 흔한 세 가지 환경

vLLM 파드가 GPU 를 받으려면 (a) `nvidia.com/gpu` 자원을 광고하는 device plugin 과 (b) 컨테이너에 드라이버를
넣어주는 런타임이 필요하다. 환경에 따라 이 둘이 이미 갖춰져 있기도 하다.

### 2.1 NVIDIA GPU Operator (가장 흔하다 · 권장)

온프레미스 클러스터에서 가장 널리 쓰인다. 드라이버·toolkit·device plugin·RuntimeClass 를 한꺼번에 설치한다.

```bash
helm repo add nvidia https://nvidia.github.io/gpu-operator && helm repo update
helm install gpu-operator nvidia/gpu-operator -n gpu-operator --create-namespace \
  --set driver.enabled=true          # 노드에 드라이버가 이미 있으면 false
```

확인:
```bash
kubectl get runtimeclass                     # nvidia 가 있어야 한다
kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}={.status.capacity.nvidia\.com/gpu}{"\n"}{end}'
```

values 에 넣을 것:
```yaml
runtimeClassName: nvidia
nodeSelector: {"nvidia.com/gpu.present": "true"}     # GPU Operator 가 붙이는 라벨
tolerations: [{key: nvidia.com/gpu, operator: Exists, effect: NoSchedule}]
```

### 2.2 device plugin 단독 + nvidia-container-toolkit (이 문서의 검증 구성)

드라이버를 직접 관리하는 노드에 쓴다. **CRI-O 에서는 함정이 하나 있다**(§5.1).

```bash
# 노드에서 (GPU 노드에만)
dnf install -y nvidia-container-toolkit
nvidia-ctk runtime configure --runtime=crio     # containerd 면 --runtime=containerd
systemctl restart crio                          # 또는 containerd

# 클러스터에
kubectl apply -f - <<'YAML'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata: {name: nvidia}
handler: nvidia
YAML
kubectl create -f https://raw.githubusercontent.com/NVIDIA/k8s-device-plugin/v0.17.1/deployments/static/nvidia-device-plugin.yml
```

device plugin DaemonSet 은 `runtimeClassName: nvidia` 로 돌려야 한다(그래야 NVML 을 본다). 전략은 기본값
`DEVICE_LIST_STRATEGY=envvar` 를 쓴다 — CRI-O 에서 `cdi-cri`/`cdi-annotations` 는 §5.1 때문에 피한다.

### 2.3 관리형 쿠버네티스 (EKS · GKE · AKS)

GPU 노드풀을 만들면 device plugin 은 대개 이미 있다. 라벨만 환경에 맞춘다.

| 환경 | nodeSelector 예 | 비고 |
|---|---|---|
| EKS (GPU AMI) | `{"nvidia.com/gpu.present": "true"}` 또는 인스턴스 타입 라벨 | device plugin 기본 포함 |
| GKE | `{"cloud.google.com/gke-accelerator": "nvidia-tesla-a100"}` | 드라이버 DaemonSet 을 따로 적용 |
| AKS | `{"accelerator": "nvidia"}` | GPU 노드풀 생성 시 설치 |

```bash
kubectl get nodes -o json | jq -r '.items[] | .metadata.name + " " + (.status.capacity["nvidia.com/gpu"] // "없음")'
```

> 관리형 환경에서는 **hostNetwork 와 hostPath 를 정책(PSA/OPA)이 막는 경우**가 있다. DAOS 클라이언트에는 둘 다
> 필요하므로 네임스페이스에 예외가 필요하다(`pod-security.kubernetes.io/enforce: privileged`).
> 또한 클라우드 VPC 에서 DAOS 서버까지 L3 로 닿아야 한다.

---

## 3. DAOS 쪽 준비

### 3.1 시스템·풀·KV 컨테이너

```bash
kubectl get daossystem                      # Ready 인지
kubectl apply -f - <<'YAML'
apiVersion: daos.gluesys.com/v1alpha1
kind: DaosPool
metadata: {name: kvpool}
spec: {systemRef: <DaosSystem 이름>, size: 100Gi, redundancyFactor: 0}
---
apiVersion: daos.gluesys.com/v1alpha1
kind: DaosContainer
metadata: {name: kvlmc, namespace: daos-system}
spec:
  poolRef: kvpool
  type: POSIX
  chunkSize: 4194304        # 4 MiB — lmcache-daos 성능의 대부분이 여기서 결정된다
  fileOclass: SX            # 노드가 2대 이상이면 RP_2GX 등 복제 사용
  dirOclass: S1
  redundancyFactor: 0
YAML
```

`chunkSize` 기본값 1 MiB 는 청크당 RPC 오버헤드로 읽기가 크게 떨어진다. **4 MiB 를 쓴다.**

### 3.2 GPU 노드에 클라이언트 agent 깔기

GPU 노드마다 daos_agent 를 하나 두고 소켓을 hostPath 로 공개한다. 서빙 파드는 그 소켓만 마운트하면 된다.

```yaml
# DaosSystem spec
clientAgent:
  enabled: true
  nodeSelector: {"nvidia.com/gpu.present": "true"}   # GPU 노드
  # hostSocketDir: /var/run/daos_agent/<system>      # 기본값, 여러 시스템 공존 가능
  # hostNetwork: true                                # 기본값. 그대로 둔다
client:
  includeFabricIfaces: ["ens18"]   # 서버가 쓰는 패브릭 인터페이스 이름
```

```bash
kubectl get daossystem <이름> -o jsonpath='{.status.conditions[?(@.type=="ClientAgent")].message}{"\n"}'
# "N node agent(s), host network; clients mount hostPath ... as /var/run/daos_agent"
```

`includeFabricIfaces` 를 비워두면 agent 의 패브릭 스캔이 **CNI 인터페이스(cni0, flannel.1)까지** 후보로 올려
클라이언트가 엔진에 닿지 못한다. 엔진이 쓰는 인터페이스 이름을 적는다.

---

## 4. 설치

```bash
helm install daos daos-operator/charts/daos-operator -n daos-system --create-namespace -f my-values.yaml
```

`my-values.yaml` (GPU Operator 환경 기준):

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
        path: /data/hf_cache/hub/models--meta-llama--Llama-3.1-8B-Instruct/snapshots/<해시>
        servedName: llama31-8b
        cacheHostPath: /mnt/nvme/hf_cache        # 노드의 HF 캐시 → /data/hf_cache
      port: 8100                                 # hostNetwork: 노드에서 비어 있는 포트
      gpus: 1
      gpuMemoryUtilization: "0.90"
      maxModelLen: 16384
      runtimeClassName: nvidia
      nodeSelector: {"nvidia.com/gpu.present": "true"}
      tolerations: [{key: nvidia.com/gpu, operator: Exists, effect: NoSchedule}]
      resources: {requests: {cpu: "8", memory: 32Gi}}
```

모델을 hostPath 대신 PVC 로 두려면 `model.cacheHostPath` 를 빼고 `extraEnv` 로 `HF_HOME` 을 지정한 뒤
PVC 를 직접 붙인다(현재 차트는 hostPath 만 렌더한다).

---

## 5. 환경별 함정 (실측)

### 5.1 CRI-O 는 `cdi.k8s.io/*` 애너테이션을 무시한다

CDI 스펙(`/etc/cdi/nvidia.yaml`)을 만들어두고 파드에 `cdi.k8s.io/gpu: nvidia.com/gpu=all` 을 달아도
`/dev/nvidia*` 도 NVML 도 들어오지 않는다(crio 로그에 CDI 처리 흔적 자체가 없다). device plugin 은
`CDI --device-list-strategy options are only supported on NVML-based systems` 로 CrashLoop 한다.
→ **nvidia 런타임 핸들러 + RuntimeClass** 를 쓰고, device plugin 은 `envvar` 전략으로 돌린다(§2.2).
containerd 에서는 CDI 가 동작하지만, 이 가이드는 양쪽 모두에서 통하는 RuntimeClass 방식을 권한다.

### 5.2 서빙 이미지가 노드 디스크를 채운다

on-disk 약 10 GB 다. 루트 파티션이 작고 다른 컨테이너 런타임(podman 등)과 저장소를 공유하면 풀 도중
`disk-pressure` taint 가 걸려 노드에 아무 것도 스케줄되지 않고, kubelet 이미지 GC 가 **다른 이미지를 지우려 든다.**

큰 디스크로 옮길 때 **CRI-O 만 따로 옮긴다**:
```toml
# /etc/crio/crio.conf.d/98-storage.toml
[crio]
root = "/mnt/nvme/crio"
runroot = "/run/crio"
```
podman 의 graphroot 를 옮기는 방식은 실패한다 — podman DB 에 컨테이너별 절대경로가 박혀 있어
`database configuration mismatch` 로 컨테이너가 뜨지 않는다. CRI-O 만 분리하면 kubelet GC 가 다른 런타임의
이미지를 건드릴 위험도 함께 사라진다.

### 5.3 이미지에 CUDA 툴체인이 없다

flashinfer 샘플러가 커널을 JIT 컴파일하려다 `Could not find nvcc` 로 **엔진 기동 전체를 실패시킨다.**
차트가 `VLLM_USE_FLASHINFER_SAMPLER=0` 을 기본으로 넣는다(sm_86 기준 성능 영향 없음, 샘플링만 PyTorch 경로).

### 5.4 hostNetwork 라 포트가 노드와 공유된다

기본 8100 이다. 노드에서 이미 쓰는 포트면 `port:` 를 바꾼다. 한 노드에 두 개를 올리려면 서로 다른 포트여야 한다.

### 5.5 노드 단위 agent 와 소켓

소켓이 hostPath 에 있어 **이전 agent 가 남긴 소켓 파일이 새 agent 를 막을 수 있다**
(`Configured dRPC socket file is already in use`). operator 가 기동 전에 정리하는 initContainer 를 넣는다 —
직접 DaemonSet 을 만든다면 같은 처리를 해야 한다.

---

## 6. 검증

```bash
POD=$(kubectl -n daos-system get pod -l app=vllm-daos -o name | head -1)
NODE=$(kubectl -n daos-system get $POD -o jsonpath='{.spec.nodeName}')

# 1) 커넥터가 붙었는지
kubectl -n daos-system logs $POD | grep -E "Creating connector|DaosConnector"
#   Creating connector for URL: plugin://daos/kvpool/kvlmc?sys=daos_k8s

# 2) 콜드: 한 번 보내고 저장을 확인
curl -s -X POST http://<노드IP>:8100/v1/completions -H 'Content-Type: application/json' \
  -d '{"model":"llama31-8b","prompt":"'"$(python3 -c 'print("DAOS is an object store. "*120)')"'","max_tokens":16,"temperature":0}' >/dev/null
kubectl -n daos-system logs $POD | grep "Stored"
#   Stored 1024 out of total 1024 tokens. size: 0.1250 GB
kubectl get daospool kvpool -o jsonpath='used={.status.usedPercent}%{"\n"}'

# 3) 로컬 캐시를 버리고 같은 프롬프트 → DAOS 에서 복원되는지 (핵심)
kubectl -n daos-system rollout restart deploy/vllm-daos
#   (준비되면 같은 요청을 한 번 더)
kubectl -n daos-system logs $POD | grep -E "hit tokens|Retrieved"
#   LMCache hit tokens: 1024
#   Retrieved 1024 out of 1024 required tokens
```

3번이 통과하면 KV 캐시가 GPU 노드 바깥의 DAOS 에 살아 있다는 뜻이다.

---

## 7. 문제 해결

| 증상 | 원인 | 조치 |
|---|---|---|
| `crt_proto_query() ... DER_TIMEDOUT` | 파드 네트워크 클라이언트(마스커레이드) | 파드를 `hostNetwork: true` 로 |
| `daos_init failed: rc=-1011` | agent 가 안 떴거나 소켓이 유령 | `kubectl logs <agent 파드>`, §5.5 |
| agent 가 `socket file is already in use` | hostPath 의 이전 소켓 | initContainer 로 정리(operator 가 처리) |
| device plugin CrashLoop, `only supported on NVML-based systems` | CDI 애너테이션 미동작 | RuntimeClass nvidia + `envvar` 전략(§5.1) |
| `Could not find nvcc` | 이미지에 CUDA 툴체인 없음 | `VLLM_USE_FLASHINFER_SAMPLER=0`(차트 기본값) |
| 파드가 `Pending`, 노드에 `disk-pressure` | 이미지 10 GB 가 안 들어감 | CRI-O 저장소를 큰 디스크로(§5.2) |
| `ImagePullBackOff` 403 | 사내 레지스트리 인증 | 해당 네임스페이스에 `imagePullSecret` |
| 읽기가 느리다 | 컨테이너 `chunkSize` 기본 1 MiB | 4 MiB 로 다시 만든다(§3.1) |

---

## 8. 검증된 조합

| 항목 | 값 |
|---|---|
| Kubernetes / 런타임 | 1.31.14 / CRI-O 1.31.5 (cgroup v2) |
| GPU | NVIDIA RTX A6000 (sm_86), 드라이버 610.43.02, nvidia-container-toolkit 1.20.1 |
| DAOS | 2.8.0 (파드 서버, ofi+tcp) |
| 이미지 | `vllm-lmcache-daos:0.30.0-0.5.5-20260927` (vLLM 0.30.0 + LMCache 0.5.5 + lmcache-daos) |
| 모델 | Llama-3.1-8B-Instruct, bf16, max-model-len 16384 |

GPU Operator·containerd·관리형 쿠버네티스 조합은 **이 문서 기준으로 미검증**이다(구성 방법만 적었다).
