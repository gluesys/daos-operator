<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# ADR-002: 디바이스·네트워크 결합 — VFIO 호스트 준비 DaemonSet, hostNetwork, ofi+verbs

- 상태: 제안
- 날짜: 2026-09-14

## 배경
DAOS 서버는 (1) NVMe 를 VFIO 로 커널에서 분리해 SPDK 가 잡고, (2) hugepages 를 요구하며,
(3) Mercury/libfabric 주소가 호스트 NIC 에 묶이고, (4) superblock/MS DB 가 노드 로컬 디스크에 있다.
K8s 의 "파드는 자유롭게 재스케줄된다" 가정이 서버 파드에는 성립하지 않는다.

## 결정
- 호스트 준비(`vm.nr_hugepages`, `daos_server nvme prepare`, 커널 파라미터)는 **별도 DaemonSet Job** 이 수행.
  서버 파드는 `/dev/vfio`, `/dev/hugepages`, `/sys/kernel/mm/hugepages`, `/sys/devices/system/node` 를 hostPath 로 받고
  `hugepages-2Mi` 리소스를 선언한다. 커널 NVMe(AIO) 모드는 지원하지 않는다(zvol 풀 0.95x 실측).
- 서버 파드는 `hostNetwork: true`. RDMA 디바이스 플러그인으로 mlx5 를 노출. 기본 provider 는
  `ofi+verbs;ofi_rxm`. UCX 는 호스트당 2엔진 구성이 불가함이 사내 확인되어 기본에서 제외.
- 서버는 노드 고정 StatefulSet(nodeAffinity 필수), superblock/MS DB 는 local PV(hostPath). 자동 재스케줄 금지.
- tmpfs(메타데이터) 크기를 파드 `memory` request 에 그대로 반영해 스케줄러가 알게 한다.
- 노드별 `fabric_iface` 이름 차이(`ens2` vs `ens2np0`)는 operator 가 노드 탐색으로 채운다. 설정 복사 금지.

## 근거
DAOS docs(hardware/deployment), lmcache-daos `gpudirect/README.md` "알려진 한계", 2026-09-03 공유 NVMe 손상 사건.

## 결과와 트레이드오프
격리 이점이 줄고 privileged 가 필요하다. 대신 성능·운영 단순성을 얻는다.

## 재검토 조건
DAOS 3.0 multi-provider 지원, K8s DRA(Dynamic Resource Allocation) 로 RDMA/hugepages 를 표현할 수 있게 되면.
