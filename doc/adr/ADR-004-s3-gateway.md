<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# ADR-004: S3 게이트웨이 — versitygw-daos 를 `S3Service` CRD 로

- 상태: 제안
- 날짜: 2026-09-23
- 결정자:

## 배경

MinIO CE 가 2026-02 유지보수 중단·2026-04 저장소 아카이브로 끝나면서 국내에 CE 대체 수요가 생겼다(허브 c82997c1 §2.2, §4.2-4).
사내에는 이미 `versitygw` 포크의 DAOS 백엔드가 있다: S3 버킷 하나를 DAOS 풀 안의 DFS 컨테이너 하나에 대응시키고
`libdfs` 로 직접 붙는다(`versitygw daos --pool <label> [--system <name>]`).

업스트림 versitygw 에는 Helm 차트가 있지만 백엔드가 posix/s3/azure 뿐이고, DAOS 백엔드에 필요한 것 — 어느 풀인지,
`daos_agent` 사이드카, 클라이언트 설정 ConfigMap, TLS 인증서 — 을 사람이 직접 채워야 한다. 그 배선이 operator 가
이미 알고 있는 정보다.

## 결정

1. **네임스페이스 스코프 CRD `S3Service`** 를 추가한다. `spec.poolRef` 로 `DaosPool` 을 가리키면 operator 가
   Deployment + Service 를 만들고 `--pool`/`--system`, agent 사이드카, agent ConfigMap, 인증서를 채운다.
2. **데이터 경로는 `libdfs` 직결이고 dfuse 를 쓰지 않는다.** 따라서 이 파드에는 CSI PV 도, FUSE 도 필요 없다.
3. **루트 자격증명은 Secret 으로만 받는다**(`spec.rootCredentialsSecret`, 키 `accessKey`/`secretKey`).
   CRD 에 평문 필드를 두지 않는다.
4. **IAM 상태의 소유자를 명시한다.** 내부(flat-file) IAM 은 `--iam-dir` 이 가리키는 파드 로컬 디렉터리에 산다.
   - 기본값은 `replicas: 1` + PVC. PVC 를 주지 않으면 emptyDir 이고, 그때는 **파드가 죽으면 S3 사용자 계정이 사라진다**
     (객체 데이터는 DAOS 에 있으므로 무사하다). 이 경우 status 에 그렇게 적는다.
   - `replicas > 1` 은 외부 IAM(LDAP/Vault/S3) 을 지정했거나 RWX PVC 를 준 경우에만 허용한다.
     파일 IAM 을 여러 파드가 각자 로컬에 두면 사용자 목록이 조용히 갈라진다.
5. **버킷을 operator 가 만들지 않는다.** 버킷 = DFS 컨테이너지만, S3 클라이언트가 만드는 것이 정상 경로다.
   `DaosContainer` CR 과 S3 버킷을 양방향 동기화하지 않는다(두 번째 SSoT 금지 규칙).

## 근거

- dfuse 를 끼우면 POSIX 경유로 지연이 붙고 FUSE·privileged·마운트 수명 문제가 그대로 따라온다. 백엔드가 이미 libdfs 다.
- IAM 분기는 "선택"이 아니라 **데이터 소실 가능성**의 문제라 CRD 가 강제해야 한다. MinIO 대체 시나리오에서
  사용자/키가 사라지는 사고는 객체가 무사해도 장애로 취급된다.
- 버킷 동기화는 매력적으로 보이지만, S3 가 만든 컨테이너와 CR 이 어긋나는 순간 어느 쪽이 진실인지 답이 없다.

## 결과와 트레이드오프

- 게이트웨이는 **상태를 거의 갖지 않는다**(IAM 디렉터리 제외). 파드 재시작이 객체 데이터에 영향을 주지 않는다.
- `replicas > 1` 의 실질적 HA 는 외부 IAM 을 쓸 때만 성립한다. v1alpha1 은 외부 IAM 을 `spec.extraEnv` 로만 지원하고,
  전용 필드는 실제 수요가 확인되면 추가한다.
- 업스트림 차트와 기능이 겹치지만 대상이 다르다(차트: 임의 백엔드 수동 배선, CRD: operator 가 아는 DAOS 풀).

## 재검토 조건

- 외부 IAM 을 쓰는 고객이 생기면 `spec.iam` 에 LDAP/Vault 전용 필드를 추가한다.
- versitygw 가 DAOS 기반 IAM 저장소를 갖게 되면 4번 결정을 다시 본다(그때는 replicas 제약이 사라진다).
