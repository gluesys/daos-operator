<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# da1~4 테스트베드: 다중 rank DAOS 를 파드로, tcp → IB/verbs (2026-09-28)

exaci4 테스트베드(VM 2대)로는 메모리가 없어 다중 rank 를 못 받았고, GPU 워커 cxl2 로 2호스트를
만들었더니 그 호스트의 시계 결함에 막혔다(exastor/daos#5). 물리 장비 `da1~da4` 를 비우고 다시 세웠다.

## 구성

- 장비: da1~da4, Rocky 8.10, 32코어/62 GB, NVMe 1 TB(WAL/meta 용) + SAS 2.7 TB × 8(데이터 용),
  ConnectX-5/6 100 Gb IB(`mlx5_0`), 1 GbE 관리 NIC `eno1`.
- 기존 네이티브 DAOS 시스템 `daos_flexa`(풀 `smx`, 2 TB)는 **소유자 승인 후 전부 파괴**했다.
- K8s: 기존 exaci4 클러스터에 워커로 조인(v1.31.14, CRI-O 1.31.5, cgroupfs, flannel).
- DAOS: operator 가 관리하는 서버 파드. 시스템 `daos-ib`(3 rank, **MS 복제본 3개**).

## 두 번 세웠고, 두 번째가 맞았다

| | 1차 `daos-da` | 2차 `daos-ib` |
|---|---|---|
| provider / iface | `ofi+tcp` / `eno1`(1 GbE) | **`ofi+verbs;ofi_rxm` / `ib0`** |
| rank | 4 (da1~4) | 3 (da1~3, da4 제외) |
| GPU 노드 dfuse 쓰기/읽기 | 5.2 / 10.5 MiB/s | **587 / 914 MB/s** |
| CSI PVC 쓰기 1 GiB | 26 MB/s | **744 MB/s** |
| vLLM 긴 프롬프트(3840 토큰) | 실패(`Retrieved 0`, HTTP 500) | **`Retrieved 3840 out of 3840`** |

원시 RDMA 대역(`ib_write_bw`, da1↔cxl2)은 **11.4 GB/s**. 1차 구성은 기능은 다 통과했지만 대역이
1 GbE 관리망이라 KV 캐시 같은 대용량 경로가 성립하지 않았다. **다중 rank 를 검증할 때 패브릭을
관리망으로 잡으면 기능 통과가 성능 실패를 가린다.**

## IPoIB 주소를 옮긴 이유

cxl2(GPU 노드)와 da1~3 은 **같은 IB 패브릭**이다(`sm_lid` 가 모두 `0x8`, da1 의 `ibhosts` 에
cxl2 가 `memfs mlx5_0` 로 보인다). IPoIB 서브넷만 10.120.0.x / 100.100.33.x 로 갈라져 있었다.
da 쪽을 cxl2 대역으로 옮겨(`100.100.33.91~94/24`, 사전에 미사용 확인) 클라이언트가 IB 로 붙게 했다.
`nmcli con mod <ib0 연결> ipv4.addresses ... ipv4.method manual` 로 영구 반영.

## 걸린 것

**da4 는 IB 링크가 죽어 있다.** `mlx5_0`/`mlx5_1` 모두 `Physical state: Disabled`, `Base lid 65535`,
`ibhosts` 에도 나타나지 않고 `ib0` NM 연결 자체가 없다. 물리(케이블/포트) 문제라 원격 복구가 안 돼
verbs 시스템에서 제외했다 — 그래서 4 rank 가 아니라 3 rank 다. **점검 필요.**

**vLLM 파드가 verbs 를 못 썼다.** 이미지에 libfabric verbs 는 있는데(`fi_info -l` 에 verbs) 런타임에
`na_ofi_provider_check` 가 치명 오류를 냈다. 원인은 이미지가 아니라 **파드에 `/dev/infiniband` 가
없고 privileged 가 아니었던 것**. hostPath 마운트와 권한을 주자 해결됐다. tcp 로 붙을 때는 드러나지
않던 요구사항이라, RDMA 패브릭에서는 **DAOS 클라이언트 파드 전부**에 해당한다(CSI 노드·S3·vLLM).

**LMCache 커넥터 ping 타임아웃 3초가 짧다.** 첫 연결에서 타임아웃하면 health monitor 가 degraded 로
들어가 lookup/store 를 통째로 건너뛴다. `DAOS_PING_TIMEOUT=15` 로 회피했다(lmcache-daos 보고 대상).

**flannel 이 da 노드에서 크래시했다.** DS 인자가 `--iface=ens18 --iface=enp1s0f0np0` 로 고정돼
`eno1` 인 노드가 인터페이스를 못 잡았다. `--iface=eno1` 추가로 해결. NIC 이름이 다른 노드를 넣을 때는
DAOS `fabric-iface` 주석뿐 아니라 **CNI 쪽도 같이** 봐야 한다.

## rank 장애 주입에서 배운 순서

1차 구성(4 rank)에서 `rank-op: exclude:0` 으로 장애를 주입했다. 결과와 순서:

- 제외된 rank 의 **엔진은 죽는다**(재조인을 거부당해 종료). 풀은 `TargetsExcluded` 로 가고 리빌드가 돈다.
- 그 상태에서 **다른 노드의 읽기·쓰기는 정상**이었다(rd_fac 1, md5 일치).
- `reintegrate` 를 바로 부르면 `rank [0] is administratively excluded` 로 거부된다.
- `clear-exclude` 만으로도 안 된다 — 엔진이 죽어 있어 `SystemDrainReq` 가 5분 타임아웃한다.
- **맞는 순서: `clear-exclude` → 서버 파드 재시작(엔진 기동) → `reintegrate`.** 그 뒤 풀이 `Ready` 로
  돌아오고 데이터는 md5 일치로 보존됐다.

`kubectl daos rank` 안내와 문서에 이 순서를 넣어야 한다.

## 남은 제품 과제

- **bdev 티어가 하나뿐이다.** operator 는 `bdev_roles: [wal, meta, data]` 를 한 티어에 고정 렌더한다.
  이 장비의 원래 구성인 **NVMe=wal/meta + SAS=data 2티어**를 표현할 수 없어 NVMe 1발만 썼다.
  노드당 SAS 8발(≈21 TB)이 놀고 있다. DAOS-HDD 하이브리드가 제품 방향인 만큼 우선순위가 높다.
- MS 복제본 3개는 이번에 처음 세웠다. **MS 복제본 장애(리더 kill) 시험은 아직 안 했다.**

## 2티어(NVMe wal/meta + SAS data) 실장비 결과 (2026-09-28)

operator#29 를 구현해 원래 네이티브 구성과 같은 2티어로 다시 세웠다. 렌더된 설정과 SPDK 설정이
의도대로 나온다 — `daos_nvme.conf` 에 NVMe 가 역할코드 `_1_6`(wal|meta), SAS 8발이 `_2_1`(data)로
붙는다. 3 rank, MS 3복제, 노드당 9개 장치.

**HDD 데이터 티어는 실제로 쓰인다.** 8 GiB 쓰기 143 MB/s, 읽기 160 MB/s (rd_fac 1 이라 디스크에는
2배가 내려간다. 24 스핀들 기준).

**그런데 풀 크기는 HDD 가 아니라 메타를 담는 램이 정한다.**

| `scmSizeGiB` | 만들어지는 풀 |
|---|---|
| 16 | 600 GiB 성공, **1 TiB 실패**(`DER_NOSPACE`) |
| 34 (네이티브 구성과 같은 값) | **1 TiB 성공**(실측 1.00 TiB), 2 TiB 실패 |

노드당 SAS 는 21 TB 인데 쓸 수 있는 풀은 1 TiB 수준이었다. md-on-SSD 에서 풀 메타데이터의 일부가
RAM 메모리 파일에 상주하고, 그 비율(`--mem-ratio`)의 DAOS 기본값이 100% 라 **RAM 이 그대로 상한**이
되기 때문이다.

### mem-ratio 노출로 풀린다 (2026-09-28)

`dmg storage query usage` 로 본 실제 용량 — 모든 장치가 비-PCI(AIO)라 알려진 혼합 구성 panic 조건은
아니었고, 명령은 정상 동작했다:

```
Tier Roles       Rank  T1-Total T1-Free  T2-Total T2-Free
T1   meta,wal    0~2   1.0 TB   1.0 TB   24 TB    24 TB     <- 데이터 티어 총 72 TB
T2   data
```

`DaosPoolSpec.memRatioPercent` 를 추가해(`--mem-ratio`) 다시 재면:

| 설정 | 결과 |
|---|---|
| mem-ratio 미설정(=DAOS 기본 100%), scm 34 | 1 TiB 성공, 2 TiB 실패 |
| **mem-ratio 10%**, scm 34 | **10 TiB 성공**(실측 10.00 TiB, `df` 11T), 4 GiB 쓰기 155 MB/s |

즉 2티어 렌더 + mem-ratio 두 가지가 같이 있어야 HDD 용량이 실제로 쓰인다.

**주의**: 메모리 파일은 시스템 전체에서 공유된다. 이미 만든 풀이 쓰고 있으면 새 풀이 `DER_NOSPACE` 로
거부된다 — 용량이 남아 있어도 그렇다. 한계를 잴 때는 기존 풀을 먼저 비우고 재야 한다(이 함정 때문에
mem-ratio 가 안 듣는 것처럼 보였다).

### 주의: 같은 이름 풀을 지웠다 다시 만들면 예전 풀을 물 수 있다

`DaosPool` CR 을 지우고 같은 이름으로 곧바로 다시 만들면, 파괴가 끝나기 전에 새 CR 이 조회를 돌려
**살아 있는 동명 DAOS 풀을 그대로 Ready 로 보고**한다. 그때 `spec.size` 와 실제 크기가 다른데도
조건은 Ready 다(3Ti 스펙에 실제 404 GB). 크기 사다리를 잴 때 이걸 모르면 결과를 통째로 잘못 읽는다 —
실제로 한 번 잘못 읽었다. 풀마다 새 이름을 쓰거나 파괴 완료를 확인하고 만들 것. 별도 이슈로 올린다.

## da4 를 나중에 붙이기 — 스케일아웃 포맷 실검증 (2026-09-29)

IB 케이블을 고친 뒤 da4 를 **가동 중인 시스템에 추가**했다. 이것이 `8865ad3`(돌아가는 시스템에
붙은 노드를 그 노드만 포맷) 이 데이터가 있는 실제 시스템에서 처음 끝까지 검증된 사례다.

절차: ib0 를 `100.100.33.94/24` 로, 옛 시스템 슈퍼블록 wipe(9개 장치), 휴지페이지 5120 + kubelet
재시작, 티어 주석 2개, `da-server=true` 라벨. 그러면 operator 가 스스로 `not formatted yet:
192.168.59.94` 로 사람 승인을 요구하고, 승인 후 **그 주소만** 포맷해 rank3 으로 붙였다.
결과 `4/4 joined`, 10 TiB 풀 `big10` 의 데이터 md5 그대로.

### 걸린 것 1: 호스트에 옛 `daos_server` 가 살아 포트를 잡고 있었다

포맷이 `request system does not match running system (daos_ib != daos_flexa)` 로 실패했다.
da4 의 **네이티브 `daos_server` 프로세스가 10101 을 점유**하고 있었기 때문이다. 부팅 자동시작은
`disabled` 인데도 프로세스는 떠 있었다.

- da3 에도 같은 유휴 프로세스가 있었다. 거기서는 파드가 포트를 먼저 잡아 겉보기엔 정상이었지만,
  **다음 재시작 때 네이티브 쪽이 먼저 잡으면 그 rank 가 조용히 깨진다.**
- 조치: 네 노드 모두 `systemctl mask daos_server daos_agent`. 호스트에 DAOS 패키지가 깔린 노드를
  파드 서버로 쓸 때는 **정지·비활성화로는 부족하고 mask 가 필요하다.**

### 걸린 것 2: 호스트 `daos_agent` 정지가 파드 에이전트의 소켓을 지운다

`daos_agent.service` 에 `RuntimeDirectory=daos_agent` 가 있어 **서비스가 멈출 때 systemd 가
`/var/run/daos_agent` 를 통째로 삭제**한다. operator 는 파드 에이전트의 hostPath 소켓을
`/var/run/daos_agent/<system>/` 에 두므로, 호스트 서비스를 건드리는 순간 그 아래 소켓이 같이
날아가고 모든 클라이언트가 `DER_NONEXIST` 로 떨어진다. 에이전트 파드를 재시작하면 복구된다.

파드 에이전트가 살아 있는데 소켓만 사라지므로 `kubectl get pods` 로는 정상으로 보인다. 소켓 경로를
`/var/run/daos_agent` 밖으로 옮기는 편이 안전하다(이슈 등록).

### 걸린 것 3: 포맷 실패 사유가 다음 질의에 덮인다

포맷 Job 이 실패했는데 조건 메시지는 곧바로 `... not formatted yet: <주소> (a human must approve ...)`
로 돌아갔다. 실패 사유(`system does not match`)는 Job 로그에만 있었고 Job 은 삭제돼 사라졌다.
승인을 다시 넣어도 같은 자리에서 실패할 뿐 이유를 알 수 없다 — 진단에 시간을 썼다.

## 관리 서비스(MS) 3중화 장애 시험 (2026-09-29)

`msReplicas: 3` 을 세워만 두고 실제로 죽여 보지는 않았었다. da1~4(4 rank, 복제본 da1·da2·da3,
풀 `big10` rd_fac 1)에서 세 가지를 넣었다. 측정은 두 갈래로 동시에 돌렸다 — da4 에서 dfuse 로
16 MiB 쓰기를 1초 간격으로(데이터), da1 에서 `dmg system leader-query` 를 2초 간격으로(제어).

| 시험 | 조작 | 제어 평면 | 데이터 경로 |
|---|---|---|---|
| ① 리더 1대 | da2 정지 | da2→da3 재선출, 공백 **3초** | **121초 정지**, 실패 0 |
| ② 리더 1대 | da3 정지 | da3→da2, **7초** 만에 새 리더 | **121초 정지**, 실패 0 |
| ③ 정족수 상실 | da1+da3 동시 정지(3중 2) | **리더 없음**, 쓰기 거부 | **132초 정지**, 실패 0 |

전체 I/O 194건 중 실패 0건, 제어 탐침 263건 중 실패 31건(③ 구간에 집중).

### 정족수는 정확히 동작한다

복제본 2/3 을 내린 상태(파드 자동 복구를 막기 위해 operator 를 0으로 내리고 StatefulSet 을 0으로):

```
leader-query  -> Current Leader: (비어 있음)
                 Unresponsive Replicas: 192.168.59.93, 192.168.59.91
system query  -> [0-3] Joined
pool create   -> "unable to contact the DAOS Management Service"
```

복구 후 15초 만에 서버 4대가 Running 이 되고 리더가 다시 섰다(da1). 같은 `pool create` 가
이번에는 **연결 실패가 아니라 크기 검증 오류**(server code 604, 이어서 `DER_INVAL`)로 답한다 —
MS 가 요청을 처리한다는 뜻이다.

### 남길 것 두 가지

**"관리 서비스 이중화 = 무중단"이 아니다.** 세 번 모두 제어는 3~7초에 넘어갔지만 데이터 경로는
매번 2분 남짓 멎었다. 죽인 노드가 rank 이기도 해서, 클라이언트가 그 rank 로 가던 I/O 를 재시도하며
`DER_TIMEDOUT` 을 내고 약 120초 뒤 성공한다. 복제본 수를 늘려도 이 시간은 줄지 않는다 —
DAOS 가 rank 를 죽었다고 판정하는 시간에 달렸다. KV 캐시·추론에서 2분 무응답은 사실상 장애다.

**읽기는 정족수 없이도 답한다.** ③ 구간 내내 `dmg system query` 는 `[0-3] Joined` 를 반환했다.
operator 의 멤버십 조회가 바로 이 명령이므로, **MS 가 정족수를 잃어도 status 는 정상으로 보인다.**
헬스체크는 `leader-query` 의 리더 유무나 쓰기 가능 여부로 판단해야 한다(별도 이슈).
