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

## 노드 선행 조건 (실측으로 드러난 것)

GPU 노드(현재 cxl2)에서 **호스트 설정 두 가지**가 먼저다. 둘 다 컨테이너 재시작을 동반하므로 장비 소유자와 맞춘다.

1. **GPU 를 CRI-O 에 알리기.** CRI-O 1.31 은 `cdi.k8s.io/*` 애너테이션을 **무시한다**(실측: 애너테이션을 단 파드에
   `/dev/nvidia*` 도 NVML 도 들어오지 않았고 crio 로그에 CDI 처리 흔적이 없다). CDI 스펙(`/etc/cdi/nvidia.yaml`)이
   등록돼 있어도 마찬가지다. 문서화된 경로인 nvidia 런타임 핸들러를 쓴다:
   ```bash
   nvidia-ctk runtime configure --runtime=crio      # /etc/crio/crio.conf.d 에 nvidia 핸들러 추가
   systemctl restart crio
   ```
   클러스터 쪽 `RuntimeClass nvidia` 와 `servingEngineSpec.runtimeClassName: nvidia` 는 이미 준비돼 있다.
   nvidia device plugin DaemonSet 도 같은 RuntimeClass 를 쓴다(이것이 없으면 NVML 을 못 봐 CrashLoop 한다).

2. **컨테이너 저장소에 여유.** 서빙 이미지는 on-disk 약 10 GB 다. cxl2 루트는 70 GB 라 podman/CRI-O 가 공유하는
   `/var/lib/containers`(18 GB)와 함께 두면 풀 도중 `disk-pressure` taint 가 걸려 아무 것도 스케줄되지 않는다.
   `/mnt/nvme1`(1.3 TB)로 옮긴다:
   ```bash
   systemctl stop kubelet crio; podman stop --all
   rsync -aHAX /var/lib/containers/ /mnt/nvme1/containers/
   sed -i 's|graphroot = "/var/lib/containers/storage"|graphroot = "/mnt/nvme1/containers/storage"|' /etc/containers/storage.conf
   mv /var/lib/containers /var/lib/containers.old && mkdir /var/lib/containers

   # podman 은 DB 에 static_dir 경로를 박아두고 기동 때 대조한다. graphroot 만
   # 옮기면 "database configuration mismatch" 로 컨테이너를 못 띄운다(실측).
   # 옛 경로를 그대로 쓰도록 고정하고, 원본 DB 를 그 자리에 둔다.
   mkdir -p /var/lib/containers/storage/libpod /etc/containers/containers.conf.d
   printf '[engine]\nstatic_dir = "/var/lib/containers/storage/libpod"\n' \
     > /etc/containers/containers.conf.d/99-static-dir.conf
   cp -a /mnt/nvme1/containers/storage/db.sql /var/lib/containers/storage/libpod/db.sql

   systemctl start crio kubelet
   podman ps -a                       # 원래 컨테이너들이 보여야 한다
   podman start <원래 컨테이너들>
   # 확인 후 rm -rf /var/lib/containers.old
   ```
   CRI-O 는 이 문제가 없다(자체 DB 를 쓰지 않는다). 이미지와 레이어는 양쪽이 공유하므로
   한 번만 옮기면 된다.

## 순서

1. GPU 노드를 클러스터에 넣고 라벨: `kubectl label node <gpu-node> daos.gluesys.com/gpu=true`
   (nvidia device plugin 과 `daos-k8s-agent` DaemonSet 이 이 라벨을 본다)
2. `kubectl apply -f lmcache-config.yaml`
3. `helm repo add vllm https://vllm-project.github.io/production-stack && helm install vllm-daos vllm/vllm-stack -n daos-system -f values.yaml`
4. 확인: `kubectl -n daos-system get daossystem daos-k8s -o jsonpath='{.status.conditions[?(@.type=="ClientAgent")]}'` 가 `Ready`,
   vLLM 파드 로그에 `DaosConnector` 초기화, 같은 프롬프트 두 번째 요청에서 LMCache hit.

`modelURL` 은 GPU 노드의 로컬 HF 캐시(`/mnt/nvme1/hf_cache`)를 hostPath 로 보여 오프라인으로 읽는다.
GPU 메모리 0.45 는 같은 노드의 데모(22 GB)와 공존하기 위한 값이다.
