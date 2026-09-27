<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# exaci4-2 테스트베드: DAOS 서버를 파드로 운영 (2026-09-22)

[기존 DAOS 를 소비하는 배치](testbed-k8s-exaci4-2.md)의 다음 단계. 같은 VM 위에서
**operator 가 관리하는 DAOS 서버 파드**를 띄우고, 그 시스템에 풀·컨테이너·PV 를 만들어
파드까지 마운트시킨 기록이다. 네이티브 DAOS(`daos_flexa`)는 건드리지 않고 나란히 돌린다.

## 구성

| 항목 | 값 | 이유 |
|---|---|---|
| 시스템 이름 | `daos_k8s` | 같은 호스트의 네이티브 `daos_flexa` 와 공존 |
| 노드 | flexa-3423-1-b 한 대 (`daos.gluesys.com/pod-server=true`) | NVMe 는 네이티브 daos_server 의 SPDK 가 이미 점유 |
| 컨트롤 포트 | 10101 | 네이티브가 10001 사용 중 |
| provider / iface | `ofi+tcp` / ens18 | IB 주소가 A 노드에만 있다 |
| 스토리지 | scm `ram` 4 GiB + bdev **`file`** 20 GiB (`/var/daos/bdev0`) | NVMe 를 못 쓰므로 파일 백엔드(SPDK AIO) |
| 엔진 | targets 1, helpers 0, `systemRamReservedGiB: 2` | 16 GB VM 중 네이티브 엔진 2개가 이미 상주 |
| 휴지페이지 | 호스트 풀 2048 → **3072**, 파드 한도 `spec.server.hugepagesRequest: 2Gi` | 아래 참조 |

## 결과

| 단계 | 결과 |
|---|---|
| 서버 파드 기동 → rank join | `Formatted=True`, `Ready=True`, rank 0 `Joined` |
| `DaosPool` 생성 | `podpool` 8Gi(meta 520MB/data 8.1GB), `Ready` |
| PVC → 파드 마운트 | **30초**, `df` 가 `dfuse 8.1G` 로 보고 |
| 파드에서 쓰기 | `dd 64MiB` = 32 MB/s (file bdev + tcp 이므로 네이티브 NVMe 의 1/3) |

## 걸린 것과 원인

| 증상 | 원인 | 조치 |
|---|---|---|
| 엔진이 뜨자마자 종료, `dma_buffer_create() Failed to grow DMA buffer` → `DER_NOMEM` | kdev/file 클래스도 **SPDK 를 쓰므로 휴지페이지가 필요**한데 호스트 여유가 536장뿐이었다(네이티브가 1512장 점유) | 호스트 `vm.nr_hugepages=3072`, 파드 한도 2Gi. 기존 예약분은 건드리지 않으므로 네이티브 DAOS 는 무중단 |
| `libaio.so.1: cannot open shared object file` | file/kdev = SPDK AIO 경로 | daos-server 이미지에 libaio 추가 (`2.8.0-20260922`) |
| dRPC 소켓 생성 실패 | 파드에 `/var/run/daos_server` 없음 | emptyDir + 이미지 수정 |
| `daos cont query` 가 `DER_TIMEDOUT` | K8s 노드에서 agent 의 패브릭 스캔이 **CNI 인터페이스(cni0, flannel.1)까지** 후보로 올린다 | agent 설정에 `include_fabric_ifaces` 렌더 — 기본값은 엔진의 `fabricIface` |
| 마운트 안 된 스테이징 경로가 파드에 붙음 | daos-csi 결함 2건 (아래) | daos-csi `6203907` |

### daos-csi 에서 드러난 결함 (모두 수정)

- `Start` 가 타임아웃으로 dfuse 를 kill 해도 `procs` 항목은 수확 고루틴이 지울 때까지 남는다.
  그 사이의 재시도가 `Running()` 을 믿고 Start 를 건너뛰어, **DAOS 가 아닌 호스트 디렉터리**를 게시했다.
  파드는 그것을 dfuse 로 알고 64MiB 를 로컬 디스크에 썼다.
- `unbind` 의 `IsLikelyNotMountPoint` 는 부모와 st_dev 를 비교하므로 **같은 파일시스템 안의 bind 를 못 본다**.
  umount 를 건너뛰고 rmdir 이 EBUSY → NodeUnpublish 무한 재시도(파드 삭제 불가).
- **노드 플러그인을 재시작하면 자식이던 dfuse 가 모두 죽는다.** 남은 마운트는 stat 이 ENOTCONN 인데,
  `unbind` 는 그 오류로 포기해 NodeUnstage 가 무한 재시도됐고, `Start` 의 `MkdirAll` 은 "file exists" 로 실패했다
  (스테일 마운트 정리가 MkdirAll 뒤에 있었다) → 그 볼륨을 다시 스테이지할 방법이 없었다.
  수동 복구는 노드에서 `fusermount3 -uz <스테이징 경로>`.

## 알아둘 것

- **scm `ram` 이어도 풀은 재시작을 견딘다**(실측 2026-09-23). operator 가 렌더하는 설정이 이미 MD-on-SSD 이기 때문이다:
  `control_metadata.path` 와 bdev 파일이 hostPath 에 있고 tier 에 `bdev_roles: [wal, meta, data]` 가 붙는다.
  램디스크는 캐시라서 기동 때마다 다시 포맷되지만(로그의 `starting format of SCM (ram:...)`), 엔진은 WAL/meta 에서
  복구한다 — 재시작 후 `rank 0 became pool service leader 2`, `pool podpool: service ranks set to 0`, PV 안 파일 3개 md5 동일.
- 풀이 실제로 사라진 경우(예: bdev 파일까지 지운 경우) operator 는 **다시 만들지 않고** `PoolMissing` 으로 멈춘다.
  `DaosPool` 의 `systemRef` 를 다른 시스템으로 바꿔도 같은 증상이 나온다 — 그 시스템에는 그 풀이 없기 때문이고, 원래 풀은 무사하다.
- `DaosPool` 을 지워도 `daos.gluesys.com/destroy-approved=true` 가 없으면 DAOS 풀은 **남는다**(`PoolOrphaned` 경고).
  CR 의 `systemRef` 를 잘못 바꿔도 다른 시스템의 풀을 파괴하지 않는다는 뜻이기도 하다.
- 한 시스템당 클라이언트(agent) 설정이 하나이므로, 네이티브용과 파드용 CSI 스택은 **드라이버 이름을 달리해** 따로 띄운다
  (`daos.csi.gluesys.com` / `pod.daos.csi.gluesys.com`).
- `daos_agent` 는 설정을 **기동 시에만** 읽는다. ConfigMap 을 고쳤으면 노드 플러그인 파드를 재시작해야 한다.
- 손으로 만든 CSI 스택에는 `imagePullSecrets: [{name: gitlab-registry}]` 를 빠뜨리지 말 것(차트가 넣어주는 값이다).
  빠지면 노드에 캐시가 없는 쪽에서만 `403 Forbidden` 으로 갈린다.

## 추가 기록: S3 게이트웨이(2026-09-23)

`S3Service`(#24) + `versitygw-daos:2.8.0-20260923`(daos-images#6) 를 이 시스템의 `podpool` 앞에 띄웠다.

| 단계 | 결과 |
|---|---|
| S3Service 기동 | `PoolReady/Deployed/Ready=True`, 1/1, 엔드포인트 `http://s3.daos-system.svc:7070 (nodePort 30707)` |
| 버킷 생성·목록 | **200 OK** (`PUT /testbucket` 261 ms, `GET /`) |
| 객체 쓰기(수정 전) | 실패 — DAOS 엔진 SIGSEGV(`vos_fetch_begin` ← `ds_obj_rw_handler`), daos-operator#28 |
| 객체 쓰기(수정 후) | **성공** — PUT 8 MiB 1.50s, GET md5 일치, 동시 PUT 24개·같은 키 6개 전부 성공, 엔진 무사 |

엔진이 죽어도 서버 파드를 재시작하면 rank 가 재조인하고 **풀과 기존 PV 데이터는 무사했다**(md5 동일).
**원인**: 조건부 KV 수정(`DAOS_COND_KEY_INSERT`/`_REMOVE`)을 **명시적 트랜잭션(`daos_tx_open`) 안에서** 실행하면
엔진이 죽는다. 조건 플래그 단독도 트랜잭션 단독도 정상이고 조합만 죽는다 — 게이트웨이 없이 재현된다
(`hack/repro/txmode.c`). 포크의 `ReleasePublicationLock` 이 그 조합을 쓰고 있었다(acquire 는 `DAOS_TX_NONE` 이라
버킷 조작은 멀쩡했다). 트랜잭션 안에서 조건 플래그를 빼면 해결되고, 트랜잭션이 이미 원자성을 준다.

## 추가 기록: 2호스트 확장 시도 (2026-09-27)

테스트베드 VM 두 대는 메모리가 없어 2번째 rank 를 못 띄웠다(daos-images #1). 같은 클러스터의
워커 `cxl2`(62 GB, 192.168.0.193)를 서버 노드로 넣어 2호스트를 만들려다 operator 결함 두 개가 드러났다.

준비(호스트별로 다른 것):
- NIC 이름이 다르다 → `spec.engines[].fabricIface` 를 비우고 노드 주석 `daos.gluesys.com/fabric-iface`
  로 내린다(`ens18` / `enp1s0f0np0`). 기존 노드는 렌더 결과가 같아 재시작하지 않는다.
- 루트 파일시스템이 작다 → `/var/daos/<system>` 과 `/var/log/daos/<system>` 을 `/mnt/nvme1` 로
  bind mount(fstab 등록). `dataHostPath` 는 시스템 단위라 노드별로 다르게 줄 수 없다.
- `vm.nr_hugepages` 를 올린 뒤 **kubelet 을 재시작**해야 `hugepages-2Mi` 용량이 광고된다.

결함 1 — 포맷 전 노드는 멤버 목록에 없다. `dmg system query` 는 rank 가 있는 멤버만 보여준다.
추가된 노드는 `awaitformat` 도 아니고 아예 없으므로 `pendingFormat` 이 서지 않았고, 서더라도
포맷 경로가 `!status.Formatted` 에 막혀 있었다. → 렌더된 노드와 멤버의 차집합으로 감지하고,
이미 뜬 시스템이면 `dmg -l <주소> storage format` 으로 그 노드만 포맷한다.

결함 2 — MS 복제본을 이름순으로 매번 다시 뽑았다. `cxl2` 가 들어오자 `mgmt_svc_replicas` 가
`192.168.34.22` → `192.168.0.193` 으로 바뀌었고, 포맷된 cxl2 는 자기 자신을 MS 로 보고 합류 대신
**별도 시스템을 부트스트랩**했다(로그: `MS leader running on memfs...`, `rank 0`). 동시에 데이터를
쥔 기존 rank 의 설정도 남의 MS 를 가리키게 다시 쓰였다 — 그 파드가 재시작했다면 그대로 깨진다.
→ `status.msReplicaNodes` 에 기록된 노드가 역할을 유지한다.

운영에서 같이 배운 것: **operator 를 두 개 돌리지 말 것.** 9/22 에 띄워둔 로컬 `make run` 프로세스가
클러스터 배포본과 동시에 같은 CR 을 재조정하고 있었다. 옛 코드가 상태를 계속 되돌려 진단을 흐렸다.
`kubectl -n daos-system scale deploy/daos-operator --replicas=0` 으로도 재조정이 멈추지 않으면
로컬 프로세스를 먼저 의심한다(`pgrep -af 'exe/main --metrics-bind-address'`).

### 결과 (2026-09-27, 수용 기준 4/4)

| 기준 | 결과 |
|---|---|
| 전 rank Joined | ✅ `2/2` — rank0 flexa-3423-1-b(192.168.34.22), rank1 cxl2(192.168.0.193). 두 호스트, 서로 다른 서브넷, `ofi+tcp` |
| dfuse 왕복 | ✅ `duopool`(4 GiB, rd_fac 1, ranks 0·1) → PVC `duo-pvc`(`RP_2G1`) 에 cxl2 에서 64 MiB 쓰고 flexa-3423-1-b 에서 md5 일치 확인 (마운트 ~10초) |
| 서버 재시작 후 풀 생존 | ✅ rank1 파드 삭제 → 재조인 `2/2`, 같은 파일 md5 일치 |
| 타인 재현 | ✅ `DaosSystem`/`DaosPool`/`StorageClass` CR + 이 문서 |

기존 워크로드(vLLM KV 캐시, S3, CSI PV 3개)는 작업 내내 유지됐다. MS 복제본은 1개(비 HA)
그대로이며, rd_fac 1 풀은 fault domain 2개(호스트 2개)로 처음 성립했다.

복구 사례 하나: 결함 2 를 고치기 전 cxl2 가 옛 슈퍼블록으로 **rank 0 을 주장하며 진짜 MS 에
join** 을 시도하자, MS 는 그 실패를 자기 rank 0 의 사망으로 해석해 `Joined→Errored` 로 내렸고
`podpool` 이 service rank 를 잃었다. 엔진 프로세스는 살아 있었고 `dmg system start --ranks=0`
으로는 풀리지 않았다(컨트롤 플레인 입장에선 시작할 게 없다). **서버 파드 재시작**으로 재조인해
복구했다 — 데이터 손실 없음. 남의 슈퍼블록을 들고 온 노드는 붙이기 전에 반드시 비울 것.
