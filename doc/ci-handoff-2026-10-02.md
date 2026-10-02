<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# CI 작업 인수인계 — 테스트베드에서 실제로 깨진 것들 (2026-10-02)

허브의 [DAOS K8s 패키징 전체 테스트 CI 계획](https://ac2repo.gluesys.com/document-hub/?document=7b362953-1969-40b2-ba4f-fd5e5ac15110)
(`7b362953`)을 구현할 때 쓰라고 남긴다. 계획서가 **"무엇을 테스트할 것인가"** 를 채워야 하는데,
2026-09-29 ~ 10-02 사이 실장비에서 **실제로 밟은 실패**가 그 답이다. 얕은 e2e 대신 진짜 회귀를
막는 스펙을 쓰려면 이 목록에서 출발하는 편이 빠르다.

## 1. CI 가 잡아야 할 실패 — 전부 실제로 당한 것

| 실패 | 어떻게 드러났나 | 스펙으로 쓸 때 |
|---|---|---|
| **hugepages 부족** | 엔진이 기동 직후 조용히 죽는다. `dma_buffer_create() Failed to grow DMA buffer` → `DER_NOMEM(-1009)`. 파드는 Ready, MS 는 Leader, `ranksJoined=0` 만 단서 | 한 노드에 시스템 둘을 올려 합산 초과를 만든다. **먼저 뜬 쪽이 다 가져간 뒤 두 번째가 죽는** 순서면 스케줄러가 못 걸러낸다 |
| **THP 켜짐** | 서버가 `code = 623 "transparent hugepage (THP) enabled"` 로 기동 실패 | `hostPrep.enabled=false` 경로의 선행 조건. hostPrep 을 켠 경로와 끈 경로를 둘 다 돌려야 의미가 있다 |
| **`systemRamReservedGiB` 기본값** | DAOS 기본 64 GiB 가 노드 메모리보다 커서 `insufficient ram to meet minimum requirements` | 작은 노드(32~64 GiB) 프로파일을 Tier 1 에 하나 둔다 |
| **`file` bdev 위치** | `/var/daos`(루트)에 떨어져 `fallocate: no space`. `server.dataHostPath` 로 옮겨야 한다 | 루트가 좁은 노드에서 bdevSizeGiB 를 크게 잡아 재현 |
| **`excluded` rank 재편입 차단** | 가드가 rank 상태만 보고 엔진이 돌아와도 거부 → 문서의 복구 절차가 끝날 수 없었다. `0b9f544` 로 수정, `TestRankOpPrecondition` 추가 | **복구 절차 e2e**: 파드 재시작 → reintegrate → joined. 단위 테스트는 있으니 e2e 로 한 번 더 |
| **삭제 못 하는 풀의 무한 루프** | destroy Job 이 나흘간 재생성 반복, 알림 없음 (#36) | rank 를 내린 상태에서 풀 삭제를 걸고, N회 뒤 상태가 `DestroyStalled` 로 바뀌는지 |
| **엔진 사망이 안 보임** | `ServersReady`·`ManagementService` 둘 다 초록색 (#37) | 엔진만 죽이고 조건이 그 사실을 말하는지 |
| **MS 정족수 상실이 안 보임** | 멤버십 질의는 정족수 없이도 답한다 (#34, `74cc176` 로 수정) | MS 복제본 과반을 내리고 `ManagementService=NoQuorum` 이 뜨는지 |
| **warm-up 이 이중화 컨테이너에서 전면 실패** | SX 프로브가 `rd_fac>=1` 컨테이너에서 `DER_INVAL`. lmcache-daos `dc07594` 로 수정 | 컨테이너 oclass 를 바꿔가며 warm-up 성공을 확인 |

`#34`·`#36`·`#37` 은 **상태가 초록색인데 동작하지 않는다**는 한 계열이다. CI 가 조건만 보고
통과시키면 전부 놓친다 — **데이터 경로를 실제로 한 번 태우는 단계**가 Tier 1 에 반드시 있어야 한다.

## 2. 지금 쓸 수 있는 장비

| 장비 | 상태 | CI 에서의 쓸모 |
|---|---|---|
| **client-6** | **단일 노드 k8s 1.31.14 / CRI-O 1.31.5 가 살아 있다.** GPU Operator v26.7.1 + Network Operator 26.4.2 + 단일 rank DAOS(`daos-c6`) + S3 게이트웨이. hugepages 8 GiB, RAM 1 TB, H100, RoCE 400G | Tier 1 의 kind 대안으로 **바로 쓸 수 있다.** 계획서의 "kind 안에서 DAOS 엔진이 도는지 확인(1일)"은 절반이 이미 답이 나왔다 — 실클러스터에서는 돈다 |
| **da1~4** | daos-ib 4 rank, IB 400G verbs, NVMe. 정상 | Tier 2 실장비 야간 |
| **exaci4** | daos-flexa(2 rank)·daos-k8s(1/2, rank1 excluded) | Tier 2. daos-k8s 는 풀 서비스가 물려 있어 정리 필요 |
| **cxl2** | A6000, exaci4 멤버. **시계는 2026-09-28 이후 정상**(이전 기록은 낡았다). 다만 GPU 를 남이 쓰고 hugepages 3 GiB 가 한계라 엔진 하나까지 | 클라이언트 쪽 검증용 |

**주의**: client-6 과 cxl2 는 **공용 장비**다. client-6 변경 내역과 되돌리는 법은
그 호스트의 `/root/k8s-UNDO.md` 에 있다(swap·SELinux·firewalld·THP·hugepages 포함).

## 3. CI 를 걸기 전에 처리할 것

1. ~~토큰 평문 노출~~ — **2026-10-02 처리 완료.** 노출 범위가 계획서가 지목한
   `daos-ci-lane` 한 곳이 아니라 더 넓었다: GitHub classic PAT 1개(`public_repo`,
   `~/.bash_history` 에도 잔존), GitLab PAT `gitlab_flexa_mr`(id 226,
   `jenkins_lib`·`samba-flexa`), 그리고 **프로젝트 액세스 토큰** `kkp_ndmp`(id 237,
   `ndmp4`). 마지막 것은 봇 계정(`project_111_bot_…`) 소유라 개인 설정 화면에 안 보여
   찾기 어려웠고, `manage_runner`·`k8s_proxy`·`write_registry`·`self_rotate` 까지
   가진 가장 위험한 토큰이었다 — **프로젝트 111 → Settings → Access Tokens** 에 있다.
   셋 다 폐기 확인했고, 세 저장소 remote 를 **SSH 로 전환**해 URL 에 자격증명을 넣는
   방식 자체를 없앴다(`jenkins_lib` 은 fetch 만 SSH 이고 push 만 토큰 URL 인 비대칭이
   원인이었다). 51곳 재스캔에서 흔적 0건.
2. **릴리스 태그** — 2026-10-02 에 `v0.1.0-rc.3`·`v0.1.0-rc.4` 를 달아 푸시했다.
   Tier 3 릴리스 게이트가 비교할 기준이 이제 있다.
3. **아직 남은 것** — `~/.docker/config.json` 의 레지스트리 자격증명(base64 는 인코딩일
   뿐 암호화가 아니다)과 `~/src/Flexa/tokens/*` 평문 파일. 의도적인 보관소라 성격이
   다르지만, **CI 러너가 그 파일을 읽을 수 있는 환경**이 되므로 자격증명 전달 방식을
   정해야 한다 — GitLab CI 변수(masked/protected) 또는 외부 시크릿 저장소.

## 4. 계획서의 열린 결정 3개에 대한 참고

- **exaci4-2·da1~4 를 야간 전용으로 쓸 수 있나** — 지금은 수동 테스트베드와 공용이다.
  da1~4 는 `daos-ib` 가 상시 떠 있어 야간 잡이 포맷을 돌리면 충돌한다. **예약/잠금이 필요하다**는
  쪽에 무게가 실린다. client-6 을 Tier 1 전용으로 떼어두는 편이 현실적이다.
- **머지 차단 정책** — 의견 없음(사람 결정).
- **kind 대안** — client-6 이 이미 그 역할을 하고 있다. 스냅샷 리셋보다 **`kubeadm reset` +
  재초기화 스크립트**가 이 노드에는 더 맞다(VM 이 아니라 물리 서버라 스냅샷이 없다).

## 5. 관련 기록

- `doc/testbed-client6-s3-vs-dfs.md` — 단일 노드 S3/DFS 성능과 측정 함정
- `doc/testbed-da-ib-verbs.md` — da1~4 4 rank, MS 이중화 장애 시험
- `doc/deploy-gpu-k8s.md` — GPU 환경 배포, §5 함정 목록
- 이슈 #26(GPU Operator 공존), #34, #36, #37
