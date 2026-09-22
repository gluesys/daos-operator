
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
