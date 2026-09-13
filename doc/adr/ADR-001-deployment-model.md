<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# ADR-001: 배포 모델 — 전용 스토리지 노드 기본, GPU 노드 HCI는 옵션

- 상태: 제안
- 날짜: 2026-09-14
- 결정자: 별동대(미정)

## 배경
DAOS 엔진은 SSD당 target xstream 을 코어에 고정해 폴링하고, MD-on-SSD 메타데이터를 tmpfs 에 두며,
SPDK/VFIO 로 NVMe 를 독점한다. GPU 노드에 상주시키면 DRAM(LMCache G2 계층과 경쟁)과 코어를 상시 점유하고,
GPU 노드 재부팅마다 rank 제외/rebuild 가 발생한다. 총판 인센티브도 전용 노드 쪽에 있다(허브 문서 §2.2).

## 결정
- `DaosSystem` 의 기본 배치는 `nodeSelector` 로 지정한 **전용 스토리지 노드**(3노드 이상, MS 복제본 홀수).
- GPU 노드 HCI 는 `spec.placement.mode: hyperconverged` 옵션으로만 제공하고, taint/toleration 과
  memory/cpu request 를 강제한다. 문서에 "지원 범위 외 성능"으로 표기.

## 근거
Mooncake 비교(허브 §1.4), DGX B200 자원 계산, 총판 채널 분석.

## 결과와 트레이드오프
- 최소 3노드 요구가 소규모 PoC 진입 장벽이 된다. Phase 0 은 MS 복제본 1개(비 HA)로 시험한다.
- HCI 옵션은 유지하되 기본값이 아니므로 마케팅 문구와 일치시켜야 한다.

## 재검토 조건
8-GPU 실측에서 HCI 모드의 DRAM 점유가 10% 이하로 확인되면 기본값 재검토.
