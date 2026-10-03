<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# DAOS K8s CI Phase C (Tier 1 e2e) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:executing-plans. Steps use `- [ ]`.

**Goal:** 설계 [`../ci-design-2026-10-02.md`](../ci-design-2026-10-02.md) 7절 Tier 1 의 13 케이스를 Ginkgo 스펙으로 만들어 MR 마다 CI 클러스터(ExaCI5 슬롯 3·4)에서 돌린다.

**Architecture:** `test/e2e` 의 kubebuilder 스캐폴드(kind, metrics 2건)를 걷어내고 다시 쓴다. 스펙은 `KUBECONFIG` 가 가리키는 클러스터와 `E2E_PROFILE`(`test/e2e/profiles/<name>.yaml`) 만으로 대상을 고른다(계획서 4절: 같은 스펙이 다른 클러스터에서 돈다). 클러스터 조작은 사람이 하던 그대로 `kubectl`/`helm` 실행으로 하고, 모든 대기는 `Eventually`, "상태가 유지되는가"는 `Consistently` 로 본다(재검증 관찰 (c): Ready 가 30초가량 0/0 로 내려갔다 복귀). 실행 전 리셋은 `hack/ci/rollback.sh`(`E2E_RESET=1`).

**Tech Stack:** Ginkgo v2.27 / Gomega 1.39 (go.mod 에 이미 있음), build tag `e2e`, kubectl·helm, daos-csi.

## Global Constraints

- 스펙 하나가 다른 스펙의 잔여물에 기대지 않는다. 순서가 필요한 묶음은 `Ordered` + 명시적 정리.
- 실패 시 진단(`kubectl get/describe`, 서버·operator 로그 꼬리)을 `GinkgoWriter` 와 JUnit 첨부로 남긴다.
- 조용히 건너뛰는 스펙 금지. 전제(KUBECONFIG, 프로파일)가 없으면 `Fail`.
- 이미지 태그는 `hack/ci/images.txt` 와 같은 값(프로파일), CI 에서는 변수로 덮어쓴다.
- 새 파일 SPDX 헤더, 커밋 한글.

## 슬라이스

### 슬라이스 1 (이번)
- [ ] 스위트: 전제 확인, 리셋, `helm upgrade --install`(프로파일), 포맷 승인, `Ready` 가 60초 연속 유지될 때까지 대기, 실패 진단 수집, AfterSuite 는 언인스톨하지 않음(케이스 7 이 한다)
- [ ] 케이스 1: DaosSystem Ready, ManagementService True, ranksJoined=1 이 **60초 유지**
- [ ] 케이스 2: DaosPool → PVC(StorageClass, CSI) → 파드에서 dd 64 MiB + sha256 → 파드 재생성 후 다시 읽어 같은 sha256 → 전부 삭제 → DaosContainer·finalizer 잔여 0
- [ ] 케이스 5: reconcile 도중 operator 파드 삭제 → 새 파드가 뜬 뒤 서버 StatefulSet·ConfigMap 이 바뀌지 않음(resourceVersion 비교), Ready 유지
- [ ] 케이스 13: 엔진 프로세스만 kill → `ServersReady` 또는 `Ready` 가 False 로 바뀐다(#37). 서버 파드 재시작 → 복귀
- [ ] 케이스 7: `helm uninstall` → 네임스페이스에 operator 소유 리소스 0, CRD 인스턴스 0
- [ ] `nvme-1rank` 프로파일에 CSI 와 StorageClass 추가
- [ ] Makefile `test-e2e` 를 클러스터 대상으로 바꿈, CI 잡 `e2e-tier1`(태그 `daos-k8s`, `resource_group`, MR 수동 → 안정화 후 자동)

### 슬라이스 2
- [ ] 케이스 3 S3Service put/get/list, 케이스 4 `kubectl-daos` 전 서브커맨드, 케이스 6 `helm upgrade` v0.1.0-rc.4 → HEAD(데이터 보존)

### 슬라이스 3
- [ ] 케이스 8~12: hugepages 합산 초과, THP always + hostPrep off, systemRamReservedGiB 기본값, file bdev 루트, rank 내린 채 풀 삭제 → DestroyStalled. 각자 프로파일 오버라이드, 노드 상태 변경은 SSH, 복구는 롤백
