<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# 단일 노드 DAOS 에서 S3 와 DFS 성능 (client-6, 2026-10-02)

operator 가 세운 **단일 노드 DAOS** 위에서 같은 풀을 대상으로 두 접근 경로를 쟀다.
`#26` 작업으로 세운 client-6 클러스터를 그대로 썼다.

## 구성

| 항목 | 값 |
|---|---|
| 노드 | client-6 — 192 코어, RAM 1 TB, H100 NVL(다른 작업이 사용 중), RoCE 400G |
| 쿠버네티스 | 1.31.14 / CRI-O 1.31.5 (단일 노드) |
| operator | **공개 차트** `oci://ghcr.io/gluesys/charts/daos-operator:0.1.2` → `ghcr.io/gluesys/daos-operator:v0.1.0-rc.4` |
| DAOS | 2.8.0, 1 rank, targets 4, SCM `ram` 16 GiB, 데이터 티어 `file` 100 GiB |
| 데이터 티어 위치 | `/home`(SATA SSD, LVM/XFS) — `server.dataHostPath` |
| 풀 | `perfpool` 60 GiB, rd_fac 0 |
| 컨테이너 | `dfsc` POSIX, chunk 4 MiB, SX/S1 |
| S3 | `S3Service` CRD → versitygw-daos `2.8.0-20260923-fix28`, NodePort 30707 |

**클라이언트와 서버가 같은 노드다.** 네트워크가 경로에 없다.

## 결과

DFS 는 in-process `libdfs`(lmcache-daos 바인딩), S3 는 boto3. 각 3회 중 최선값.

| 크기 | DFS 쓰기 | DFS 읽기 | S3 PUT | S3 GET | 쓰기 배수 | 읽기 배수 |
|---|---|---|---|---|---|---|
| 1 MiB | 959 | 547 | 85 | 70 | 11x | 8x |
| 16 MiB | 1801 | 4252 | 150 | 159 | 12x | 27x |
| 64 MiB | 2498 | 6648 | 164 | 181 | 15x | 37x |
| 256 MiB | **2681** | **7094** | 164 | 182 | 16x | 39x |

(단위 MB/s)

병렬:

| | DFS | S3 |
|---|---|---|
| 4 x 64 MiB 쓰기 | 2649 | 600 |
| 16 x 64 MiB 쓰기 | 2731 | 682 |

## 읽을 점

- **S3 단일 스트림은 객체 크기와 무관하게 164~182 MB/s 에서 멎는다.** 16 MiB 에서 이미
  천장이고 256 MiB 에서도 같다. 대역폭이 아니라 **연결당 한계**다.
- **병렬이 답이다.** 16 스레드로 682 MB/s — 단일 스트림의 4배. DFS 도 같은 배수(2.7 GB/s)이니
  게이트웨이가 병렬성을 못 살리는 것은 아니다. 다만 출발점이 15배 낮다.
- **읽기 격차가 쓰기보다 크다.** DFS 읽기는 7 GB/s 까지 가는데(페이지 캐시) S3 GET 은
  182 MB/s 다. HTTP 본문을 한 번 더 복사하는 비용이 캐시 이득을 전부 먹는다.
- 결론: **용량·호환성이 필요하면 S3, 대역폭이 필요하면 DFS.** 같은 데이터를 두 경로로 열어두는
  구성(S3 로 넣고 DFS 로 읽기)이 성립한다는 것이 이 숫자의 쓸모다.

## 측정에서 틀리기 쉬운 것 — `aws` CLI 로 재면 안 된다

처음에 `aws s3 cp` 로 쟀더니 **1 MiB 에서 3 MB/s** 가 나왔다. 게이트웨이가 아니라
**CLI 프로세스 기동 시간**이다. 같은 대상을 boto3 로 재면 85 MB/s 다 — **28배 차이**가
측정 도구에서 나온다.

| 크기 | `aws s3 cp` PUT | boto3 PUT |
|---|---|---|
| 1 MiB | 3 | 85 |
| 16 MiB | 44 | 150 |
| 64 MiB | 107 | 164 |
| 256 MiB | 152 | 164 |

객체가 커질수록 좁혀지는 것이 바로 고정비의 모양이다. **작은 객체 성능을 CLI 로 보고하면
우리 게이트웨이를 30배 과소평가하게 된다.**

## 이 숫자의 한계 — 저장장치 성능이 아니다

**RAM 1 TB 노드에서 데이터 티어가 SSD 위의 파일이다.** 100 GiB bdev 에 대고 256 MiB 를
쓰고 읽었으니 페이지 캐시가 거의 전부 흡수한다. DFS 읽기 7 GB/s 는 SATA SSD 가 낼 수 있는
수치가 아니다 — **메모리 속도에 가깝다.**

따라서 이 표는 **스택(라이브러리·게이트웨이·프로토콜)의 상대 비용**을 보여주는 것이지
저장장치 성능이 아니다. 절대값이 필요하면 NVMe 를 SPDK 로 물린 구성에서 다시 재야 한다
(`doc/testbed-da-ib-verbs.md` 의 da1~4 가 그 구성이다).

## 곁다리로 확인된 것

- **공개 차트가 끝까지 동작한다.** `helm install oci://ghcr.io/gluesys/charts/daos-operator`
  한 줄로 operator 가 뜨고, 사내 자격증명 없이 `ghcr.io/gluesys/daos-operator` 를 받았다.
  어제 발행한 것(#27)이 실제로 설치 가능하다는 확인이다.
- **`S3Service` CRD 는 20초만에 게이트웨이를 띄운다.** 풀 이름과 자격증명 Secret 만 준다.
- **`hostPrep.enabled=false` 로 두면 사람이 할 일이 생긴다.** 이번에 걸린 것:
  - `transparent_hugepage` 를 꺼야 한다 — 안 끄면 서버가 `code = 623` 으로 기동 실패
  - hugepages 를 미리 잡아야 한다(`vm.nr_hugepages`)
  - 작은 호스트에서는 `systemRamReservedGiB` 를 낮춰야 한다(기본 64 GiB)
  - `file` bdev 는 `server.dataHostPath` 로 여유 있는 파일시스템에 둬야 한다
  이 목록이 배포 가이드에 없다. 넣을 값이 있다.
