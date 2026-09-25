<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# 참조 배포: vLLM production-stack + LMCache + DAOS (#25)

vLLM 이 생성한 KV 캐시를 LMCache 가 DAOS 컨테이너에 내리는 구성이다. 데이터 경로는
`lmcache-daos` 커넥터가 libdfs 로 직접 붙고(ctypes), 파드에 필요한 DAOS 는 **agent 소켓 하나**다.

| 조각 | 무엇 | 어디서 |
|---|---|---|
| DAOS 시스템·풀 | `daos-k8s` / `podpool` | operator (DaosSystem, DaosPool) |
| KV 캐시 컨테이너 | `kvlmc` (POSIX, chunk 4 MiB, SX/S1, rd_fac 0) | `DaosContainer` CR |
| 노드 단위 agent | `daos-k8s-agent` DaemonSet, **파드 네트워크**, 소켓 hostPath `/var/run/daos_agent/daos-k8s` | `spec.clientAgent{enabled, hostNetwork: false, nodeSelector: daos.gluesys.com/gpu=true}` |
| 서빙 이미지 | `vllm-lmcache-daos:0.30.0-0.5.5-20260926` (daos-client + vllm 0.30.0 + lmcache 0.5.5 + lmcache-daos) | exastor/daos-images |
| LMCache 설정 | `lmcache-config.yaml` (ConfigMap) | 이 디렉터리 |
| vLLM 파드 | production-stack 차트 + `values.yaml` | 이 디렉터리 |

## 왜 agent 가 파드 네트워크인가

production-stack 차트에는 hostNetwork 도, 재시작되는(native) 사이드카도 넣을 자리가 없다. daos_agent 는
자기가 보는 인터페이스 이름을 클라이언트에 건네므로, 파드 네트워크의 vLLM 을 섬기려면 agent 도 파드
네트워크에 있어야 한다(`eth0`). 그래서 `spec.client.includeFabricIfaces` 에 `eth0` 를 넣고
`spec.clientAgent.hostNetwork: false` 로 둔다. 엔진(hostNetwork)은 flannel 을 통해 파드 IP 에 닿는다.

## 순서

1. GPU 노드를 클러스터에 넣고 라벨: `kubectl label node <gpu-node> daos.gluesys.com/gpu=true`
   (nvidia device plugin 과 `daos-k8s-agent` DaemonSet 이 이 라벨을 본다)
2. `kubectl apply -f lmcache-config.yaml`
3. `helm repo add vllm https://vllm-project.github.io/production-stack && helm install vllm-daos vllm/vllm-stack -n daos-system -f values.yaml`
4. 확인: `kubectl -n daos-system get daossystem daos-k8s -o jsonpath='{.status.conditions[?(@.type=="ClientAgent")]}'` 가 `Ready`,
   vLLM 파드 로그에 `DaosConnector` 초기화, 같은 프롬프트 두 번째 요청에서 LMCache hit.

`modelURL` 은 GPU 노드의 로컬 HF 캐시(`/mnt/nvme1/hf_cache`)를 hostPath 로 보여 오프라인으로 읽는다.
GPU 메모리 0.45 는 같은 노드의 데모(22 GB)와 공존하기 위한 값이다.
