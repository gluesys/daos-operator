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
