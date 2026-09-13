<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# daos-operator

Kubernetes operator for DAOS: `DaosSystem`, `DaosPool`, `DaosContainer` CRD 와 reconcile 루프.
HPE K3000 의 CSC(`csc daos system create --nodecount 4`, `csc daos pool create`) 와 Enakta Platform 의
클러스터 뷰(health/version/fabric/capacity/ranks faulty·total/job queue) 가 하는 일을 K8s 관용구로 옮긴다.

이 저장소가 지키는 규칙(모든 exastor K8s 저장소 공통):
1. **CRD 가 유일한 관리 API.** 어플라이언스 REST/UI 와 코드를 공유하지 않는다.
2. **두 번째 SSoT 를 만들지 않는다.** 원하는 상태 = CR spec, 실제 상태 = DAOS MS DB·메트릭. operator 는 비교만 한다.
3. **파괴적 작업 자동화 금지.** `storage format`/wipe/재포맷은 사람 승인(어노테이션) 없이 실행하지 않는다.
4. **upstream-first.** 패치는 먼저 daos-stack / ai-dynamo/nixl / LMCache 로 보낸다.

## 상태
Phase 0 (2026-09). kubebuilder v4 뼈대 + CRD 스키마 초안. 컨트롤러는 로그만.
설계 결정은 `doc/adr/` (ADR-001 배포 모델, ADR-002 디바이스·네트워크, ADR-003 업그레이드).

## 관리 표면 (v1 목표)
| 형태 | 담당 |
|---|---|
| 선언형 | CRD: 시스템 배치, 풀 생성/확장, 컨테이너, ACL |
| 명령형 | `kubectl daos` 플러그인: 디스크 교체, rank exclude/drain/reintegrate, `dmg check`, 업그레이드 승인 |
| 관측 | DAOS Prometheus 엔드포인트(9191) + ServiceMonitor, CR `.status`/conditions, K8s Events |
| 대시보드 | DAOS 공식 Grafana JSON 재사용 |

UI 는 없다. 필요해지면 CR 과 메트릭을 읽기만 하는 얇은 콘솔로 뒤에 얹는다.

## 이미지
`exastor/daos-images` 의 `daos-server/agent/client/admin` 을 소비한다.
