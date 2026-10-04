<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# DAOS K8s CI Phase B (Tier 0) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:executing-plans. Steps use `- [ ]`.

**Goal:** 설계 [`../ci-design-2026-10-02.md`](../ci-design-2026-10-02.md) 6절 Tier 0 를 MR 마다 10분 안에 도는 잡으로 만든다. 그리고 그 결과로 kmod 가 들어간 daos-server 이미지를 CI 가 빌드해, Phase A 의 A-3 를 **수동 패치 없이** 다시 통과시킨다.

**Architecture:** operator 저장소에 잡 3개(`go-test`, `chart-schema`, `render-validate`), daos-images 저장소에 잡 2개(`images`, `sbom-scan`). 이미지 빌드·config 검증은 podman 이 있는 exa-build 러너(태그 `daos-build`, Exastor 그룹 러너)에서, 나머지는 기존 `Flexa` docker 러너에서 돈다. 교차 저장소 접근(operator 잡이 daos-images 의 `validate-config.sh` 와 이미지를 읽음)은 daos-images 의 CI job token 허용 목록에 daos-operator 를 넣어 해결한다.

**Tech Stack:** GitLab CI, golang:1.26 + setup-envtest, Helm 3, kubeconform, podman, syft/trivy(컨테이너로 실행).

## Global Constraints

- 새 파일 SPDX 헤더, 커밋 한글, Co-Authored-By 줄.
- Tier 0 잡은 MR 차단(설계 8절)이지만 `sbom-scan` 은 `allow_failure: true`(보고만).
- 조용히 건너뛰는 잡 금지(두 저장소 `.gitlab-ci.yml` 머리 주석).
- 이미지 태그: `2.8.0-<YYYYMMDD>-g<short sha>`. `main` 에서만 푸시하고, MR 에서는 빌드만.
- kubeconform 스키마 버전: 1.29.0, 1.31.0, 1.34.0(chart `kubeVersion >=1.29`).
- 차트 프로파일 7개: tcp, verbs, external-ms, pod-servers, s3, csi, vllm.

---

### Task 1: operator `go-test` 잡 (unit + envtest)
**Files:** Modify `.gitlab-ci.yml`
- [ ] 잡 추가: image golang:1.26, `make test`, artifacts `cover.out`, coverage regex.
- [ ] 로컬 `make test` 통과 확인(이미 통과: 2026-10-02 fix/spdk-host-modules).
- [ ] GitLab ci/lint valid.
- [ ] 커밋.

### Task 2: 차트 프로파일과 `chart-schema` 잡
**Files:** Create `charts/daos-operator/ci/{tcp,verbs,external-ms,pod-servers,s3,csi,vllm}-values.yaml`, `hack/ci/chart-schema.sh`; Modify `.gitlab-ci.yml`
- [ ] `chart-schema.sh`: 프로파일마다 `helm template` → kubeconform `-strict -summary -kubernetes-version {1.29.0,1.31.0,1.34.0}`. CRD 는 기본 스키마로, 커스텀 리소스(DaosSystem 등)는 CRD 에서 뽑은 JSON 스키마로 검증한다(`-schema-location default -schema-location 'dist/schemas/{{ .ResourceKind }}_{{ .ResourceAPIVersion }}.json'`). 스키마는 `charts/daos-operator/crds/*.yaml` 의 `openAPIV3Schema` 를 python 으로 꺼낸다.
- [ ] 의도적으로 틀린 값(예: `system.spec.engines[0].bdevClass: nvmee`)으로 실패하는지 확인 후 되돌린다.
- [ ] 잡 추가(alpine/helm 이미지 + kubeconform 바이너리 다운로드 + python3), 커밋.

### Task 3: operator `render-validate` 잡
**Files:** Create `hack/ci/render-validate.sh`; Modify `.gitlab-ci.yml`
- [ ] `go run ./hack/render-samples` → daos-images 를 `CI_JOB_TOKEN` 으로 clone → `DOCKER=podman scripts/validate-config.sh {server|agent|control} <file>` 를 9개 파일에 실행. 하나라도 INVALID 면 실패.
- [ ] 이미지 태그는 변수 `DAOS_IMAGE_TAG`(기본: daos-images `main` 최신 빌드 태그, 없으면 `2.8.0-20260922`).
- [ ] daos-images(819) job token 허용 목록에 daos-operator(820) 추가(API).
- [ ] 잡 추가(태그 `daos-build`), 커밋.

### Task 4: daos-images `images` 잡
**Files (daos-images):** Modify `.gitlab-ci.yml`, `Makefile`(필요 시 `DOCKER=podman` 지원만)
- [ ] `make all DOCKER=podman IMAGE_NSP=$REG IMAGE_TAG=$TAG` 로 base + server/agent/client/admin 빌드.
- [ ] `main` 에서만 `podman push`(재시도·재로그인, operator 의 `push()` 패턴 그대로).
- [ ] 빌드된 server 이미지 안에 `modprobe` 가 있는지 단언(`podman run --rm --entrypoint sh $IMG -c 'command -v modprobe'`).
- [ ] 커밋, MR, 파이프라인 통과 확인.

### Task 5: daos-images `sbom-scan` 잡 (보고만)
- [ ] syft(`anchore/syft`)로 5개 이미지 SPDX JSON, trivy(`aquasec/trivy`)로 HIGH/CRITICAL 표 → artifacts. `allow_failure: true`.

### Task 6: 머지 후 A-3 재검증 (수동 패치 없이)
**Files:** Modify `hack/ci/images.txt`, `test/e2e/profiles/nvme-1rank.yaml`, `doc/ci-design-2026-10-02.md`
- [ ] daos-images `main` 파이프라인이 푸시한 태그와 operator `main` 의 hostprep `latest`(kmod 포함)를 images.txt·프로파일에 반영.
- [ ] `cluster-init.sh` 의 사전 pull 부분만 다시 돌린 뒤 `snapshot.sh`.
- [ ] `verify-nvme.sh` → `NVME_CLASS_OK`. 설계 5절에 "이미지 수정 후 무패치 통과" 기록.
- [ ] 커밋, MR.
