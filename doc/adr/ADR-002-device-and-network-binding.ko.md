<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# ADR-002: 디바이스·네트워크 결합 — VFIO 호스트 준비 DaemonSet, hostNetwork, ofi+verbs

- 상태: **승인** (2026-09-29, kpkim)
- 날짜: 2026-09-14 (승인 2026-09-29)
- 승인 근거: hostNetwork·ofi+verbs(ib0) 구성으로 GPU 노드 dfuse 587/914 MB/s, RDMA 11.4 GB/s 실측. kdev/file 완화와 hugepages 정정도 실측 반영

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

## 개정 (2026-09-22): 테스트 전용 비SPDK 저장 클래스 허용

위 결정("커널 NVMe(AIO) 모드는 지원하지 않는다")은 **운영 배포에 대해 그대로 유지한다.** 성능과 안정성의 근거가 바뀐 것이 없다.

다만 `spec.engines[].bdevClass` 에 `kdev`(커널 블록 장치)와 `file`(파일)을 **테스트 전용으로** 추가한다. 이유는 하나다:
SPDK 는 hugepages·IOMMU·전용 NVMe 를 요구하고, 그것을 갖춘 장비가 없으면 **서버를 파드로 올리는 경로 자체를 한 번도 실행해 볼 수 없다**.
hostprep → format 승인 게이트 → 풀 → PVC 로 이어지는 흐름은 서버가 실제로 떠야 검증되며, 그 검증을 미루는 비용이 AIO 를 테스트에 허용하는
비용보다 크다. 함께 추가하는 `spec.systemRamReservedGiB`, `spec.disableVFIO`, `spec.controlPort` 도 같은 목적이다(작은 VM, IOMMU 없는 호스트,
이미 DAOS 가 도는 호스트에 두 번째 시스템).

**지키는 선**
- 기본값은 `nvme` 이고, CRD 주석과 README 에 "kdev/file 은 성능 측정에 쓰지 말 것"을 명시한다.
- 제품 문서·영업 자료의 성능 수치는 `nvme` 구성에서만 나온다. 테스트베드 수치는 기능 확인용으로만 인용한다.
- 운영 배포 점검표에 "bdevClass=nvme 인가"를 넣는다.

**실측으로 정정한 오해(2026-09-22)**: `kdev`/`file` 이면 hugepages 가 필요 없다고 적었는데 **틀렸다.** DAOS 는 세 클래스 모두 SPDK 를 통해
다루고(`file`·`kdev` 는 SPDK 의 AIO 백엔드), DPDK 초기화에 hugepages 가 필요하다. 실제로 `nr_hugepages: 0` 인 파드에서
`spdk_env_init(): Cannot allocate memory` 로 포맷이 실패했다. kdev/file 이 없애 주는 것은 **전용 NVMe 와 IOMMU/VFIO** 이지 hugepages 가 아니다.
쿠버네티스에서는 한 가지가 더 있다: 파드의 hugetlb cgroup 한도가 0 이면 호스트에 여유 hugepage 가 있어도 쓸 수 없다. 그래서
`spec.nrHugepages`(DAOS 가 호스트 풀을 관리할 개수)와 `spec.server.hugepagesRequest`(파드가 쓸 수 있는 양)를 분리했다.

**재검토 조건**: 전용 장비가 확보되어 테스트도 `nvme` 로 돌릴 수 있게 되면 이 완화의 필요성이 사라진다.
