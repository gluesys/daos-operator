<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# ADR-003: 업그레이드 모델 — 3.0 이전에는 "전체 중단 업그레이드"를 1급 절차로

- 상태: 제안
- 날짜: 2026-09-14

## 배경
DAOS 재단 공식 문구: "3.0 expects a communications protocol change which will not allow backward
compatibility with older versions." 2.8 은 클라이언트 유지만 지원하고 서버는 일괄 업그레이드해야 한다.
서버 롤링 업그레이드는 3.0 목표(tech preview). HPE K3000 의 CSC 도 같은 방식(전 서버 stop → upgrade → start)을 자동화했다.

## 결정
- operator 는 `DaosSystem.spec.version` 변경 시 **전체 중단 업그레이드**를 정직하게 1급 절차로 수행한다:
  클라이언트 드레인 확인 → `dmg system stop` → 이미지 태그 교체 → 순차 기동 → `dmg system start` → 검증.
- 파괴적 단계(format, wipe)는 절대 포함하지 않는다. 승인 어노테이션 없이 버전 필드만 바뀌면 `Pending` 상태로 멈춘다.
- 2.8→3.0 은 별도 마이그레이션 절차(데이터 export/import 또는 upstream 도구)로 다루며 Phase 4 에서 설계한다.
- 첫 유료 배포는 3.0 이후로 잡거나, 2.8 계약서에 전면 재설치 업그레이드를 명시한다.

## 근거
Lombardi DUG25 "Rolling Upgrade Preparation", 허브 문서 §1.3.

## 재검토 조건
3.0 에서 롤링 업그레이드가 GA 되면 `strategy: Rolling` 추가.
