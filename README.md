<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# daos-operator

Kubernetes operator for DAOS: `DaosSystem`, `DaosPool`, `DaosContainer`, `S3Service` CRD 와 reconcile 루프.
HPE K3000 의 CSC(`csc daos system create --nodecount 4`, `csc daos pool create`) 와 Enakta Platform 의
클러스터 뷰(health/version/fabric/capacity/ranks faulty·total/job queue) 가 하는 일을 K8s 관용구로 옮긴다.

이 저장소가 지키는 규칙(모든 exastor K8s 저장소 공통):
1. **CRD 가 유일한 관리 API.** 어플라이언스 REST/UI 와 코드를 공유하지 않는다.
2. **두 번째 SSoT 를 만들지 않는다.** 원하는 상태 = CR spec, 실제 상태 = DAOS MS DB·메트릭. operator 는 비교만 한다.
3. **파괴적 작업 자동화 금지.** `storage format`/wipe/재포맷은 사람 승인(어노테이션) 없이 실행하지 않는다.
4. **upstream-first.** 패치는 먼저 daos-stack / ai-dynamo/nixl / LMCache 로 보낸다.

## 상태
- Phase 0: kubebuilder v4 뼈대, CRD 3종(kind·envtest 검증).
- **Phase 3 #25 (2026-09-26): 노드 단위 클라이언트 agent `spec.clientAgent`.** 선택한 노드마다 daos_agent DaemonSet 을 띄우고 소켓을
  hostPath(`/var/run/daos_agent/<system>`)로 공개한다. 사이드카를 못 넣는 파드(vLLM production-stack 차트 등)가 hostPath 하나로 DAOS 에
  붙는다 — DAOS 본래의 "호스트당 agent 하나" 모델이다. agent 가 호스트 인터페이스 이름을 돌려주므로 클라이언트 파드도 hostNetwork 여야 한다.
  조건 `ClientAgent`.
- **Phase 3 #24 (2026-09-23): S3 게이트웨이 `S3Service`(ADR-004).** `spec.poolRef` 로 `DaosPool` 을 가리키면 versitygw-daos Deployment +
  Service 를 만들고 `--pool`/`--system`, daos_agent 네이티브 사이드카, agent ConfigMap, 인증서를 operator 가 채운다. 데이터 경로는 libdfs
  직결이라 PV 도 dfuse 도 쓰지 않는다. 루트 키는 Secret(`accessKey`/`secretKey`)으로만 받고, 내부 IAM 이 emptyDir 이면 `IAMDurable=False`
  로 "파드가 죽으면 S3 사용자·키가 사라진다"를 명시하며 그 상태의 `replicas > 1` 은 거부한다(사용자 목록이 조용히 갈라진다).
  차트의 `s3.services[]` 로 설치할 수 있고(루트 키는 기존 Secret 지정 또는 차트가 생성), 실장비 검증은
  `doc/testbed-k8s-pod-servers.md` 참조.
- **Phase 2 #7·#8 (2026-09-14): `DaosSystem` reconcile 1단계 + 호스트 준비 DaemonSet.** nodeSelector 로 노드 선택 → Node 어노테이션에서 노드별 사실
  (fabric NIC, VFIO NVMe 목록, 드라이브 DSN, NUMA) 읽기 → **같은 물리 드라이브(DSN)를 두 노드가 노출하면 그 노드들을 제외하고
  `DriveConflict=True`**(2026-09-03 손상 사고 재발 방지) → 관리 서비스 복제본 노드 선택(이름순 안정) → 노드별 `daos_server.yml`
  ConfigMap + `-agent`/`-control` ConfigMap 렌더 → `.status`(selectedNodes, msReplicaNodes, nodeConfigs, conditions).
- **Phase 2 #9 (2026-09-14): 노드 고정 서버 StatefulSet.** 렌더된 노드마다 `<sys>-server-<node>` StatefulSet(replicas 1, 필수 nodeAffinity,
  hostNetwork, privileged, hugepages-2Mi 리소스, hostPath 데이터/로그) 을 만든다.
- **Phase 2 #17 (2026-09-15): `kubectl daos` 플러그인.** 사람이 내려야 하는 결정(format·업그레이드·pool/cont 파괴)을 무엇이 지워지는지 보여주고
  확인을 받은 뒤 승인 어노테이션/필드로 쓴다. `system status` 는 조건·rank·대기 중인 결정을 요약한다.
- **Phase 2 #16 (2026-09-15): TLS 인증서.** `allowInsecure: false` 면 operator 가 upstream `gen_certificates.sh` 와 같은 CA(RSA 3072, SHA-512)와
  server/agent/admin 인증서를 Secret `<sys>-certs` 로 1회 생성하고 서버·agent 사이드카·dmg/daos Job 에 필요한 부분만 마운트한다.
- **Phase 2 #14 (2026-09-15): Helm 차트 `charts/daos-operator`.** CRD + operator Deployment/RBAC + hostprep ClusterRole + (옵션) Grafana 대시보드
  ConfigMap + (옵션) DaosSystem 한 개를 한 번에 설치. `make helm-sync` 가 생성 산출물(CRD, ClusterRole 규칙, JSON)을 차트로 복사하고 CI 가 drift 를 잡는다.
  `csi.enabled: true` 면 exastor/daos-csi 드라이버(controller + node DaemonSet + StorageClass)까지 같은 릴리스로 설치한다(2026-09-15).
- **Phase 2 #13 (2026-09-15): 전체 중단 업그레이드(ADR-003).** 서버 파드 이미지 ≠ `spec.images.server` 이면 `Upgrading=False (Pending)` 로 멈추고,
  `spec.upgrade.approved: true` 뒤에만 `dmg system stop` → 파드 교체 → Ready 대기 → `dmg system start` → 전 rank joined 검증. 승인은 1회용.
- **Phase 2 #12 (2026-09-15): 텔레메트리.** `telemetry_port`(기본 9191) 렌더 + 헤드리스 `<sys>-metrics` Service + prometheus-operator CRD 가 있으면
  ServiceMonitor(15초). DAOS 공식 Grafana 대시보드 JSON 을 `config/grafana/` 에 동봉.
- **Phase 2 #11 (2026-09-15): DaosPool/DaosContainer reconcile.** `dmg pool ...`/`daos cont ...` 를 Job 으로 돌려 없으면 만들고(1회),
  rank 추가는 `pool extend`, `spec.acl` 변경은 `overwrite-acl` 로 반영한다. status 는 `dmg pool query`/`daos cont query` 미러. 삭제 시
  DAOS 객체 파괴는 `daos.gluesys.com/destroy-approved=true` 어노테이션이 있을 때만(없으면 남기고 Event `PoolOrphaned`/`ContainerOrphaned`).
- **Phase 2 #10 (2026-09-14): format 승인 게이트와 멤버십.** `dmg -j system query` 를 daos-admin Job 으로 돌려 미포맷이면
  `status.pendingFormat=true` 로 보고만 하고, 사람이 `daos.gluesys.com/format-approved=true` 어노테이션을 달아야 `dmg storage format`
  을 **1회** 실행한다. rank 목록·joined 수는 dmg 출력을 그대로 복사한다. `Ready=True` 는 서버 전부 Ready + 포맷 + 전 rank joined.
설계 결정은 `doc/adr/` (ADR-001 배포 모델, ADR-002 디바이스·네트워크, ADR-003 업그레이드).

## 노드 사실(facts) 계약과 호스트 준비 DaemonSet (#8)
`DaosSystem` 을 만들면 `spec.hostPrep.enabled`(기본 true)에 따라 `<sys>-hostprep` DaemonSet 이 선택된 노드마다 하나씩 뜬다
(privileged, hostNetwork, hostPID, ServiceAccount `daos-hostprep` ← 함께 배포되는 ClusterRole `daos-hostprep`: nodes get/patch).
`cmd/hostprep` 가 주기(`intervalSeconds`, 기본 300초)마다:

1. `/sys/bus/pci/devices` 에서 NVMe(class 0x010802)를 찾아 드라이버(nvme / vfio-pci / uio_pci_generic), NUMA, **PCI Device Serial
   Number**(확장 캡 ID 3; 에뮬레이션 NVMe 처럼 256B 설정공간이면 없음)를 읽고, 파티션·마운트·holder 가 있는 디스크는 "사용 중"으로 분류한다.
2. `/sys/class/net/*/device/infiniband` 가 있는 RDMA NIC 중 `fabricCIDR` 에 맞는(없으면 IPv4 가 있는 첫) 것을 fabric 으로 고른다.
3. `vm.nr_hugepages` 가 `spec.nrHugepages` 보다 작으면 올린다(0 이면 건드리지 않음).
4. `hostPrep.bindNvme: true` 일 때만 "사용 중이 아닌 커널 NVMe" 를 `daos_server nvme prepare <pci-allow-list>` 로 SPDK 에 넘긴다.
   기본값 false 에서는 탐색·어노테이션만 한다. 어떤 경우에도 format/wipe 는 하지 않는다.
5. 결과를 Node 어노테이션으로 기록한다.

| 어노테이션 | 값 |
|---|---|
| `daos.gluesys.com/fabric-iface` | 선택된 RDMA NIC (`ens2`, `ens2np0`, `ib0`) |
| `daos.gluesys.com/bdev-list` | SPDK 가 쓸 수 있게 이미 바인딩된 NVMe PCI 주소(콤마) — reconcile 은 이것만 `bdev_list` 에 넣는다 |
| `daos.gluesys.com/bdev-dsn` | `<pci>=<DSN>` 목록. **두 노드에 같은 DSN 이 보이면 reconcile 이 그 노드들을 제외**(9/3 손상 사고) |
| `daos.gluesys.com/numa-node` | fabric NIC(없으면 첫 NVMe)의 NUMA |
| `daos.gluesys.com/nvme-candidates` | 커널 NVMe 중 사용 중 아님 = `bindNvme` 가 가져갈 대상(정보용) |
| `daos.gluesys.com/nvme-in-use` | 파티션/마운트/holder 가 있어 절대 건드리지 않는 NVMe |
| `daos.gluesys.com/hostprep-status` | 마지막 실행 JSON(time, hugepages, bound/candidates/inUse 수, fabricAddr, fabricError) |
| `daos.gluesys.com/control-addr` | (사람이 선택적으로) mgmt_svc_replicas/access_points/hostlist 주소. 기본은 노드 InternalIP |

DaemonSet 이 켜져 있으면 **어노테이션의 정본은 hostprep** 이다(주기마다 덮어쓴다). 손으로 달아 쓰려면 `hostPrep.enabled: false` 로 끄고 아래처럼 단다.
spec.engines 에 `fabricIface`/`bdevList` 를 적으면 어노테이션보다 항상 우선한다.
```bash
kubectl label node cell1 daos.gluesys.com/role=storage
kubectl annotate node cell1 daos.gluesys.com/fabric-iface=ens2 daos.gluesys.com/bdev-list=0000:03:00.0 \
  daos.gluesys.com/bdev-dsn=0000:03:00.0=$(lspci -vvs 03:00.0 | awk '/Device Serial/{print $NF}' | tr -d -)
kubectl get daossys daos-dev -o yaml | yq .status      # conditions, nodeConfigs
kubectl -n daos-system get cm,ds -l daos.gluesys.com/system=daos-dev
```

이미지: `make hostprep-image HOSTPREP_BASE=<daos-server 이미지>` → daos-server 위에 정적 `hostprep` 바이너리. 기본 참조는
`registry.gitlab.gluesys.com/exastor/daos-operator/daos-hostprep`, `spec.images.hostPrep` 으로 바꿀 수 있다.

`.status.conditions`: `NodesSelected`, `DriveConflict`, `ConfigRendered`(Partial 이면 nodeConfigs 의 message 에 이유), `ServersReady`, `Certificates`, `Formatted`, `Telemetry`, `Upgrading`, `Ready`.
렌더 결과는 `exastor/daos-images` 서버 엔트리포인트와 같은 2.8 키(`mgmt_svc_replicas`, agent `access_points`, control `hostlist`)를 쓴다.

## 서버 워크로드 (#9)
렌더에 성공한 노드마다 **StatefulSet 하나(replicas 1)** 를 만든다. 한 시스템에 StatefulSet 하나나 DaemonSet 을 쓰지 않는 이유:

- rank 는 superblock·NVMe 가 있는 노드에 묶여 있어 파드가 옮겨 다니면 안 된다 → `requiredDuringScheduling` nodeAffinity 로
  `metadata.name` 고정, 데이터는 그 노드의 hostPath(ADR-002). 재스케줄은 없다.
- 노드별 `daos_server.yml` 이 다르다(fabric_iface, bdev_list) → 워크로드마다 자기 ConfigMap 을 `/etc/daos/daos_server.yml` 로 마운트.
- StatefulSet 은 같은 superblock 위에 엔진이 둘 뜨는 일을 막고, `updateStrategy: OnDelete` 로 이미지·설정이 바뀌어도 **엔진을 스스로
  재시작하지 않는다**. 재시작·업그레이드는 ADR-003 절차가 단계별로 한다.

| 항목 | 값 |
|---|---|
| 이름 | `<sys>-server-<node>`, 라벨 `daos.gluesys.com/{system,node,role=server}` |
| 파드 | hostNetwork, `system-node-critical`, privileged, `terminationGracePeriodSeconds` 120(`spec.server.terminationGracePeriodSeconds`) |
| 리소스 | memory request = Σ`scmSizeGiB` + 2Gi(tmpfs 는 파드에 과금), cpu = Σ(targets+helpers+1), `hugepages-2Mi` = nrHugepages×2Mi(request=limit). `spec.server.resources` 로 전체 대체 |
| 볼륨 | ConfigMap → `/etc/daos/daos_server.yml`; hostPath `spec.server.dataHostPath`(기본 `/var/daos/<sys>`) → `/var/daos`(control_metadata, MS DB); `logHostPath`(기본 `/var/log/daos/<sys>`) → `/var/log/daos`; `/dev/hugepages`, `/dev`, `/sys`(uio_pci_generic 테스트베드의 `/dev/uio*` 와 daos_server 의 `/sys/bus/pci` 바인딩 때문에 통째로) |
| 프로브 | readiness = TCP 10001(제어 포트, format 전에도 열림). **liveness 없음**: 느린 엔진을 프로브가 죽이면 안 된다 |
| 삭제 | 사람이 `spec.server.enabled: false` 로 끄거나 DaosSystem 을 지울 때만. 노드가 selector 에서 빠지거나 facts 를 잃어도 워크로드는 남긴다(rank 제외 절차 #10+ 전까지 자동 정지 금지) |

`.status.nodeConfigs[].workload/serverReady` 와 `ServersReady` condition(`PodsNotReady` 면 파드 상태 요약)으로 본다. 파드 상태는 30초 주기로 다시 읽는다.

```bash
kubectl -n daos-system get sts,pods -l daos.gluesys.com/system=daos-dev -o wide
kubectl -n daos-system logs daos-dev-server-<node>-0        # "DAOS Server config loaded from /etc/daos/daos_server.yml"
```
kind 같은 RDMA 없는 환경에서는 `scan fabric ... DER_HG_FATAL` 로 종료한다(정상: 설정 마운트까지는 검증됨). `nrHugepages: 0` 과
`spec.server.resources` 를 작게 주면 스케줄까지는 된다.

## format 승인 게이트와 멤버십 (#10)
포맷은 드라이브를 지운다. operator 는 **포맷 여부를 스스로 결정하지 않는다.**

1. 서버 워크로드가 있으면 `spec.images.admin` 이미지로 `<sys>-dmg-query` Job(`dmg -j system query -v`, `<sys>-control` ConfigMap 마운트,
   hostNetwork, 스토리지 노드 selector/toleration)을 돌리고 파드 로그의 JSON 을 읽는다. 끝난 Job 은 지우고 다음 주기(미포맷/불명 30초,
   정상 60초)에 다시 돈다.
2. dmg 가 `system is uninitialized (storage format required?)`(또는 `raft service unavailable`) 을 돌려주면 `status.pendingFormat=true`,
   `Formatted=False (AwaitingApproval)` 로 **보고만 한다.**
3. 사람이 승인한다: `kubectl annotate daossystem <sys> daos.gluesys.com/format-approved=true` (나중의 `kubectl daos system format` 이 이걸 붙인다).
4. 승인이 있고 `pendingFormat` 이 관측됐고 **렌더된 노드의 서버 파드가 전부 Ready** 일 때만 `<sys>-dmg-format` Job(`dmg -j storage format`)을
   1회 실행한다. 일부 노드가 빠진 채 포맷하면 그 rank 가 영영 빠지므로 `AwaitingServers` 로 거부한다.
5. Job 이 끝나면 성공·실패와 무관하게 어노테이션을 지운다(one-shot). 성공이면 `status.formatted=true`, `formatTime`, Event `Formatted`;
   실패면 `Formatted=False (FormatFailed)` + host_errors 메시지 + Event `FormatFailed`, 다시 승인해야 재시도.
6. 포맷된 뒤에는 `system query` 결과를 `status.ranks[]{rank,node,state}`, `ranksJoined/ranksTotal`, `lastQueryTime` 으로 복사한다.
   `awaitformat` 상태 rank(노드 증설)가 보이면 `pendingFormat` 이 다시 켜져 같은 게이트를 거친다. 포맷된 시스템에 남은 승인 어노테이션은
   지우고 Event `ApprovalIgnored` 를 남긴다.
7. MS 에 닿지 못하면(`unable to contact the DAOS Management Service`, connection refused) `Formatted=Unknown (ManagementUnreachable)` 로
   마지막 상태를 유지한다. `--force`/`--reformat` 은 어떤 경로에도 없다.

`Ready` 사유: `ServersDisabled` → `ServersNotReady` → `AwaitingFormat` → `FormatUnknown` → `RanksNotJoined` → `Ready`.

```bash
kubectl get daossys                                   # FORMATTED / PENDINGFORMAT / READY 열
kubectl get daossys <sys> -o jsonpath='{.status.conditions[?(@.type=="Formatted")].message}{"\n"}'
kubectl annotate daossystem <sys> daos.gluesys.com/format-approved=true     # 사람만
kubectl -n daos-system get jobs,pods -l app.kubernetes.io/name=daos-dmg
kubectl get events --field-selector involvedObject.kind=DaosSystem
```
TLS 인증서(`allowInsecure: false`)의 admin 인증서 Secret 마운트는 아직 없다(Phase 0 은 insecure).

## 풀과 컨테이너 (#11)
`DaosPool`(클러스터 스코프, 라벨 = `metadata.name`)과 `DaosContainer`(네임스페이스, 라벨 = `spec.label` 또는 이름)는 각각 Job 으로 dmg/daos 를 부른다.
Job 은 DaosSystem 네임스페이스에 만들어지고, 풀 Job 은 `pool-<name>-dmg-<op>`, 컨테이너 Job 은 `cont-<ns>-<name>-daos-<op>` 이름을 쓴다.

| 단계 | DaosPool (`dmg`, admin 이미지) | DaosContainer (`daos`, client 이미지 + agent 사이드카) |
|---|---|---|
| 전제 | DaosSystem `status.formatted=true` | DaosPool `status.uuid` 있음 |
| 조회 | `pool query --show-enabled <label>` | `cont query <pool> <label>` |
| 없음 | `pool create -z <bytes>B -P rd_fac:N[,k:v] [-r ranks] [-a acl] <label>` (1회) | `cont create <pool> <label> --type --file-oclass --dir-oclass --chunk-size --properties rd_fac,cksum[,k:v] [--acl-file]` (1회) |
| 드리프트 | `spec.ranks` 에 새 rank → `pool extend --ranks=<빠진 것>`; `spec.acl` 해시 변경 → `pool overwrite-acl` | `spec.acl` 해시 변경 → `cont overwrite-acl` |
| 미러 | uuid, state, total/free(티어 합), **usedPercent**, rebuild, disabledTargets, enabledRanks, lastQueryTime | uuid, poolUUID, health, type, ready |
| 실패 | `Ready=False (CreateFailed/ExtendFailed/AclFailed)` + Event, **spec 이 바뀔 때까지 재시도 안 함**(observedGeneration) | 동일 |
| 사라짐 | 이전에 uuid 를 알았는데 없어짐 → `PoolMissing`, **재생성 안 함**(데이터 손실은 사람이 봐야 한다) | `ContainerMissing`, 동일 |
| 삭제 | `destroy-approved=true` → `pool destroy --recursive`; 없으면 풀은 남기고 Event `PoolOrphaned` | `cont destroy`; 없으면 Event `ContainerOrphaned` |

- 크기(`spec.size`)는 생성 시에만 쓰인다. DAOS 2.8 은 풀 크기 변경이 없다(확장 = rank 추가).
- **용량 경고(#21)**: DAOS 는 용량을 풀에서 강제한다. 풀이 차면 그 안의 컨테이너·PV 가 한꺼번에 영향을 받으므로, 질의마다
  `status.usedPercent`(printcolumn `USED%`)를 갱신하고 `spec.spaceWarningPercent`(미지정=85, `0`=끔)를 넘으면 `SpaceLow=True` 와
  Event `PoolSpaceLow` 를 **상태가 바뀔 때 한 번만** 낸다(복구 시 `PoolSpaceRecovered`). CSI 는 같은 수치를 `GetCapacity` 로 보고한다.
- `overwrite-acl` 은 ACL 전체를 바꾼다. `spec.acl` 에 `A::OWNER@:...` 등 필요한 항목을 모두 적어야 한다.
- 컨테이너 Job 은 K8s ≥ 1.29 의 네이티브 사이드카(`initContainers[].restartPolicy: Always`)로 `daos_agent` 를 붙이고, 클라이언트가 fabric 을 쓰기
  위해 hostNetwork·`/dev`·privileged 로 뜬다(Phase 0; RDMA 디바이스 플러그인으로 대체 예정). `spec.images.client` 가 필요하다.
- 조회 주기 30초(홀드), 정상 60초 requeue. 한 reconcile 에 연산은 하나만 시작한다(`status.operation`).

```bash
kubectl get daospool,daoscont -A
kubectl annotate daospool kv daos.gluesys.com/destroy-approved=true && kubectl delete daospool kv   # 정말 파괴할 때만
kubectl -n daos-system get jobs -l daos.gluesys.com/pool=kv
```

## 테스트베드용 비SPDK 구성 (#23, 성능 측정 금지)
SPDK 가 요구하는 hugepages·IOMMU·전용 NVMe 가 없는 장비에서도 **서버를 파드로 올려** 전체 흐름(hostprep → format 승인 → 풀 → PVC)을
검증할 수 있게 한 설정이다. ADR-002 개정으로 테스트 전용으로만 허용한다.

| 필드 | 뜻 |
|---|---|
| `spec.engines[].bdevClass` | `nvme`(기본, 운영) / `kdev`(커널 블록 장치, `bdevList` 에 경로) / `file`(파일, `bdevSizeGiB` 로 크기) |
| `spec.nrHugepages` | DAOS 가 **호스트 hugepage 풀을 관리할 개수**. 0 = 건드리지 않음(밖에서 관리하는 호스트) |
| `spec.server.hugepagesRequest` | **파드가 쓸 수 있는 hugepage 양**(예 `1Gi`). 미지정이면 `nrHugepages × 2Mi`. 0 이면 파드가 hugepage 를 전혀 못 쓴다 |
| `spec.systemRamReservedGiB` | DAOS 기본 64 GiB 예약을 낮춘다. 작은 호스트는 이걸 낮추지 않으면 엔진이 뜨지 않는다 |
| `spec.disableVFIO` | IOMMU 없는 호스트에서 uio_pci_generic 사용 |
| `spec.controlPort` | 기본 10001. 이미 DAOS 가 도는 호스트에 두 번째 시스템을 올릴 때 바꾼다 |

**중요**: `kdev`·`file` 도 DAOS 는 SPDK(AIO)로 다루므로 **hugepages 는 여전히 필요하다**(실측: 없으면 `spdk_env_init(): Cannot allocate memory`).
이 클래스가 없애 주는 것은 전용 NVMe 와 IOMMU/VFIO 다. 호스트 hugepage 를 DAOS 밖에서 관리한다면 `nrHugepages: 0` + `server.hugepagesRequest` 로 준다.

```yaml
spec:
  systemName: daos_k8s
  controlPort: 10101
  provider: ofi+tcp          # IB 주소가 없는 호스트
  nrHugepages: 0             # 호스트 풀은 밖에서 관리
  server:
    hugepagesRequest: 1Gi    # 파드가 쓸 수 있는 양(없으면 SPDK 초기화 실패)
  systemRamReservedGiB: 2
  engines:
    - targets: 2
      helpers: 0
      scmSizeGiB: 4          # DAOS 최소값
      bdevClass: file
      bdevSizeGiB: 20
      bdevList: ["/var/daos/bdev0"]
```
**이 구성으로 성능을 재지 말 것.** 숫자는 `nvme` 구성에서만 의미가 있다. 렌더 결과는 `make validate-configs` 로 실제 2.8 바이너리 검증을 거친다.

## 이미 도는 DAOS 에 붙기 (#22, spec.externalMsReplicas)
DAOS 가 베어메탈이나 다른 클러스터에서 돌고 있고 K8s 는 그걸 **쓰기만** 하는 배치가 실제로 있다. `spec.externalMsReplicas` 에 그 시스템의
관리 서비스 주소를 적으면 operator 는 남의 시스템을 운영하려 들지 않는다.

| 하는 것 | 하지 않는 것 |
|---|---|
| agent·control 설정 렌더(그 주소로), 멤버십 미러(`status.ranks`), 풀·컨테이너 reconcile, rank 연산, CSI 연동 | 서버 StatefulSet, hostprep DaemonSet, 메트릭 Service, 인증서 생성, 업그레이드, **포맷**(승인 어노테이션이 붙어도 거부하고 지운다) |

```yaml
spec:
  systemName: daos_flexa            # 그 시스템의 이름(기본 daos_server 가 아닐 수 있다)
  externalMsReplicas: ["127.0.0.100"]
  nodeSelector: {daos.gluesys.com/client: "true"}   # dmg/daos Job 이 뜰 노드 = MS 와 fabric 에 닿는 노드
  allowInsecure: true
```
`spec.engines` 는 이 모드에서 비워 둔다. Job 은 `nodeSelector` 노드에 hostNetwork 로 뜨므로 **그 노드가 MS 주소와 fabric 에 닿아야 한다**
(로컬 주소에만 MS 가 열려 있으면 그 호스트에서만 가능).

## rank 멤버십 조작 (#20)
rank 를 빼고 넣는 일은 데이터가 어디 있느냐를 바꾸는 결정이라 operator 가 스스로 하지 않는다. 죽은 rank 는 **보고**할 뿐 쫓아내지 않는다.

```bash
kubectl daos rank drain daos-dev --ranks=2          # 또는 annotate daossystem ... daos.gluesys.com/rank-op=drain:2
kubectl daos system status daos-dev                 # last rank op: drain 2 -> ok ...
```

| 요청 | 실행 | 뜻 |
|---|---|---|
| `drain:<ranks>` | `dmg system drain --ranks=` | 데이터를 먼저 옮긴다(점진적, 풀 중복도 유지) |
| `exclude:<ranks>` | `dmg system exclude --ranks=` | 즉시 down 처리 → 해당 풀 전부 리빌드 |
| `reintegrate:<ranks>` | `dmg system reintegrate --ranks=` | 다시 합류시키고 데이터를 되돌린다 |
| `clear-exclude:<ranks>` | `dmg system clear-exclude --ranks=` | 관리자 제외 상태만 해제 |

- 어노테이션은 1회용이다. operator 가 Job 을 한 번 돌리고 rank 별 결과를 `status.lastRankOp{op,ranks,succeeded,message}` 에 적은 뒤 지운다.
  실패한 rank 가 하나라도 있으면 `succeeded=false` 와 함께 어느 rank 가 왜 실패했는지 남는다(Event `RankOpFailed`).
- 잘못된 요청(알 수 없는 연산, rank 표기 오류, 미포맷 시스템)은 실행하지 않고 그 이유를 status 에 적는다.
- 멤버십 자체는 여기서 추측하지 않는다. 다음 `dmg system query` 결과가 `status.ranks` 를 갱신한다.

## 텔레메트리 (#12)
`spec.telemetry`(기본 enabled, port 9191, serviceMonitor true, interval 15s):

- 서버 설정에 `telemetry_port` 를 넣어 각 엔진 호스트가 `:9191/metrics` 로 `engine_*` 시리즈를 낸다(hostNetwork 라 노드 IP).
- 헤드리스 Service `<sys>-metrics`(selector `daos.gluesys.com/system,role=server`, 포트 `metrics`)를 만든다. 서버 파드가 Ready 여야 endpoint 가 생긴다.
- `monitoring.coreos.com/v1 ServiceMonitor` CRD 가 서빙되면(RESTMapper 로 확인) 같은 이름의 ServiceMonitor 를 만든다. `serviceMonitorLabels`
  (예 `release: kube-prometheus-stack`)로 Prometheus 의 selector 에 맞춘다. CRD 가 없으면 `Telemetry=True (NoPrometheusOperator)` 로 알리고 넘어간다.
- Grafana: `config/grafana/DAOS-Grafana-Dashboard.json`(upstream 원본, BSD-2-Clause-Patent). `kustomize build config/grafana` 가
  `grafana_dashboard: "1"` ConfigMap 을 만들고 Helm 차트의 `grafana.dashboard.enabled` 도 같은 것을 설치한다.

```bash
kubectl -n daos-system get svc,servicemonitor daos-dev-metrics
kubectl get daossys daos-dev -o jsonpath='{.status.conditions[?(@.type=="Telemetry")].message}{"\n"}'
```

## 업그레이드 (#13, ADR-003)
DAOS 2.x 서버는 롤링 업그레이드가 없다. operator 는 그 사실을 감추지 않고 **전체 중단 업그레이드**를 상태 머신으로 수행한다.

- 트리거: 서버 **파드**가 실행 중인 이미지 ≠ `spec.images.server`. StatefulSet 은 `OnDelete` 라 spec 을 바꿔도 파드는 그대로다.
  이 상태에서 `status.upgrade.phase=Pending`, `Upgrading=False (Pending)`. 시스템은 계속 동작한다.
- `spec.upgrade.approved: true` 는 사람이 **클라이언트를 드레인했음을 보증**하는 것이다(operator 는 클라이언트 핸들을 볼 수 없다).
- 단계(`status.upgrade.phase`): `Stopping`(`dmg system stop`) → `Updating`(구 이미지 서버 파드 삭제; STS 가 새 이미지로 재생성) →
  `Starting`(렌더 노드 수만큼 새 이미지 파드 Ready 대기) → `StartingSystem`(`dmg system start`) → `Verifying`(`dmg system query -v` 전 rank joined)
  → `Completed`. 진행 중엔 `Ready=False (Upgrading)` 이고 format/멤버십 프로브는 쉰다.
- 어느 단계든 dmg 오류나 타임아웃(`spec.upgrade.timeoutMinutes`, 기본 30)이면 `Failed` + Event `UpgradeFailed`, 승인은 false 로 되돌린다.
  다시 승인하면 `Stopping` 부터 다시 한다. `--force`, format, wipe 는 어디에도 없다.
- 완료 시 `status.observedVersion = spec.version`, 승인 false 로 리셋, Event `UpgradeCompleted`.
- 2.8 → 3.0 은 프로토콜 비호환이라 이 절차의 대상이 아니다(별도 마이그레이션, Phase 4).

```bash
kubectl get daossys                      # UPGRADE 열 = phase
kubectl patch daossys daos-dev --type merge -p '{"spec":{"images":{"server":"...:2.8.1"},"version":"2.8.1"}}'
kubectl patch daossys daos-dev --type merge -p '{"spec":{"upgrade":{"approved":true}}}'   # 드레인 뒤, 사람만
kubectl get daossys daos-dev -o jsonpath='{.status.upgrade}{"\n"}'
```

## `kubectl daos` 플러그인 (#17)
`make kubectl-daos` 로 `bin/kubectl-daos` 를 만들어 PATH 에 두면 `kubectl daos ...` 로 부른다. 선언형 CR 이 못 하는 "사람의 결정" 만 다룬다.

| 명령 | 하는 일 |
|---|---|
| `kubectl daos system status <sys>` | 조건 9종, 노드별 렌더/서버 상태, rank, **DECISION PENDING**(format 대기·업그레이드 Pending) 요약 |
| `kubectl daos system format <sys>` | `status.pendingFormat` 일 때만. 지워질 노드·디바이스 수를 보여주고 `yes` 입력 후 `daos.gluesys.com/format-approved=true` |
| `kubectl daos system upgrade <sys> [--image I] [--version V]` | 새 서버 이미지/버전을 적고 전체 중단 업그레이드 승인(`spec.upgrade.approved=true`). 클라이언트 드레인 보증 문구 표시 |
| `kubectl daos system certs <sys>` | 인증서 교체 승인. 만료일과 "전체 중단 재시작 + 클라이언트 재시작 필요" 를 보여주고 `yes` 후 `certs-renew-approved=true` |
| `kubectl daos rank drain\|exclude\|reintegrate\|clear-exclude <sys> --ranks=N[,M-O]` | rank 멤버십 조작 요청. 영향(데이터 이동·리빌드)과 현재 rank 목록을 보여주고 `yes` 후 `rank-op` 어노테이션 |
| `kubectl daos pool destroy <pool>` | 사용량을 보여주고 `destroy-approved=true` + `DaosPool` 삭제 → operator 가 `dmg pool destroy --recursive` |
| `kubectl daos cont destroy -n <ns> <cont>` | 동일, 컨테이너 |

`--yes` 로 프롬프트를 건너뛴다(스크립트). `--kubeconfig`/`--context` 는 kubectl 과 같다.

**설치**: 태그를 푸시하면 CI 가 linux/amd64·arm64, darwin/amd64·arm64 바이너리를 만들어 GitLab generic package `kubectl-daos/<tag>` 와
Release 에 올린다. 내려받아 PATH 에 두면 `kubectl daos` 로 불린다. krew-index 제출은 공개 URL 이 필요해 아직 미결이다 — `krew/README.md` 참고
(매니페스트 템플릿 `krew/daos.yaml.tmpl`, 생성기 `hack/krew-manifest.sh`).

## TLS 인증서 (#16)
`spec.allowInsecure: false`(운영 기본)이면 operator 가 Secret `<sys>-certs` 를 만든다. 내용과 배치는 upstream `utils/certs/gen_certificates.sh` 와 같다.

| 파일 | 서버 파드 | agent 사이드카 | dmg/daos Job |
|---|---|---|---|
| `daosCA.crt` | ✓ | ✓ | ✓ |
| `server.crt` / `server.key`(0400) | ✓ | | |
| `agent.crt` / `agent.key`(0400) | `clients/agent.crt` 만 | ✓ | |
| `admin.crt` / `admin.key`(0400) | `clients/admin.crt` 만 | | ✓ |

- CA: O=DAOS, CN="DAOS CA", RSA 3072, SHA-512, pathlen 1, 1095일. 리프: CN server(serverAuth+clientAuth) / agent(clientAuth) / admin(clientAuth).
- 이미 있는 Secret 은 검증만 한다(`Certificates=Valid|ExpiringSoon|Invalid`). 사람이 넣은 인증서를 덮어쓰지 않으며, 재생성은 Secret 삭제로 한다.
  인증서 교체 뒤에는 전체 중단 재시작이 필요하다(ADR-003 절차와 같은 방식; 자동화는 아직 없음).
- 컨테이너 Job 의 `daos` CLI 는 admin 세트를, 그 사이드카 agent 는 agent 세트를 받는다. hostprep 은 인증서가 필요 없다.
- `allowInsecure: true` 면 `Certificates=False (Insecure)` 로 표시만 한다(Phase 0).

### 교체 (#19)
`spec.certificates.renewBeforeDays`(기본 30) 안으로 만료가 다가오면 `Certificates=True (ExpiringSoon)` 과 함께 승인 방법을 알린다. 교체는 CA 가 바뀌는 일이라
**모든 엔진과 클라이언트가 새 CA 를 다시 읽어야 한다**. 그래서 승인은 곧 중단 승인이다:

```bash
kubectl daos system certs <sys>        # 또는: kubectl annotate daossystem <sys> daos.gluesys.com/certs-renew-approved=true
```
승인되면 ADR-003 의 전체 중단 절차를 그대로 쓴다: `dmg system stop` → **엔진이 멈춘 상태에서** Secret 교체(이전 번들은 `<sys>-certs-previous` 로 보존) →
서버 파드 전부 삭제·재기동 → `dmg system start` → 전 rank joined 검증 → `Completed`. 어노테이션은 성공·실패와 무관하게 소거된다(1회용).
`status.upgrade.trigger` 가 `CertificateRotation` 으로 구분되고, `status.certificates{secretName,notAfter,rotatedAt,previousSecret}` 에 결과가 남는다.

**operator 가 하지 않는 것**: 클라이언트 재시작. 교체 후 CSI 노드 DaemonSet 과 `daos_agent` 사이드카를 쓰는 파드는 사람이 다시 시작해야 한다
(`kubectl -n daos-system rollout restart ds/daos-csi-node`). Event `CertificatesRotated`/`CertificateRotationCompleted` 메시지에도 적어 둔다.
롤백은 `<sys>-certs-previous` 의 데이터를 `<sys>-certs` 로 되돌린 뒤 같은 절차를 한 번 더 도는 것이다(자동화 없음).

## 이미지 (#15)
| 이미지 | 태그 | 만드는 곳 |
|---|---|---|
| `registry.gitlab.gluesys.com/exastor/daos-operator/daos-operator` | `<short sha>`, `<VERSION>`(=차트 appVersion), `latest` | GitLab CI `image-operator`(main 푸시·태그 시, exa-build 러너 podman) |
| `registry.gitlab.gluesys.com/exastor/daos-operator/daos-hostprep` | 동일 | CI `image-hostprep`(multi-stage: golang 빌드 → daos-server 베이스) |
| `exastor/daos-images/daos-{server,agent,client,admin}` | `2.8.0-<date>` | exastor/daos-images |

- `VERSION` 파일이 차트 `appVersion` 과 같은 값이어야 한다(`image.tag` 기본값). 릴리스는 `VERSION` 을 올리고 태그를 푸시한다.
- 로컬: `make operator-image operator-push hostprep-image hostprep-push DOCKER="sudo -n docker" IMAGE_TAG=dev`.
- 레지스트리는 internal 프로젝트라 pull 에 인증이 필요하다. kind 는 `kind load docker-image`, 실클러스터는 `imagePullSecrets`(차트 `imagePullSecrets`).

## 설치: Helm 차트 (#14)
```bash
make helm-sync helm-lint                       # 생성 산출물 → 차트 동기화 + lint (bin/helm)
helm install daos-operator charts/daos-operator -n daos-system --create-namespace \
  --set image.tag=<operator 태그> \
  --set grafana.dashboard.enabled=true \
  --set system.create=true --set system.name=daos      # values.yaml 의 system.spec 을 편집해서
kubectl get daossys daos -w
```
- 차트가 설치하는 것: `crds/`(3종, 최초 설치 시에만 — 업그레이드 시 `kubectl apply --server-side -f charts/daos-operator/crds/`), ServiceAccount,
  ClusterRole(`templates/clusterrole.yaml` 은 `make helm-sync` 가 `config/rbac/role.yaml` 에서 **생성**, 손으로 고치지 말 것)·Binding,
  leader-election Role, ClusterRole `daos-hostprep`, Deployment, 옵션 Grafana ConfigMap(`grafana.dashboard.enabled`), 옵션 DaosSystem(`system.create`,
  `helm.sh/resource-policy: keep` 이라 uninstall 이 엔진을 멈추지 않는다).
- operator 가 만드는 것(차트 밖): hostprep DaemonSet, 서버 StatefulSet, 메트릭 Service/ServiceMonitor, dmg/daos Job.
- `hostprep.defaultImage` → env `DAOS_HOSTPREP_DEFAULT_IMAGE`(spec.images.hostPrep 이 비었을 때의 기본 이미지).
- `csi.enabled: true`: exastor/daos-csi 의 CSIDriver·RBAC·controller Deployment(csi-provisioner)·node DaemonSet(registrar + `daos_agent` 사이드카,
  `csi.system` 의 `<sys>-agent` ConfigMap, `csi.tls` 면 `<sys>-certs`)·`csi.storageClasses[]` 를 설치한다(`templates/csi.yaml`, daos-csi `deploy/` 와 동일 내용).
  kind 검증(2026-09-15): operator + CSI 를 한 릴리스로 설치 → CSINode 에 드라이버 등록 → PVC 생성 시 CSI 가 `DaosContainer` CR 을 StorageClass 파라미터로
  만들고 operator 가 Ready 로 올릴 때까지 `Unavailable` 재시도(kind 엔 DAOS 가 없어 여기까지). 실 노드에서 PV 마운트까지가 Phase 2 종료 기준.
- CI: `go-build` 잡이 `make helm-sync && git diff --exit-code -- charts/` 로 drift 를 막고, `helm` 잡(alpine/helm)이 lint + 전체 옵션 렌더를 한다.
- kind 검증(2026-09-15): `daos-operator:dev` 이미지로 `helm install` → Deployment Ready → 클러스터 내부 RBAC 으로 daos-dev 시스템 reconcile(DaemonSet·StatefulSet·Service·ServiceMonitor·Job 생성, forbidden 없음) → `helm uninstall`.

## 렌더된 설정의 2.8 스키마 검증
`make validate-configs DOCKER="sudo -n docker"` 는 `hack/render-samples` 로 대표 설정 7종(단일 엔진, 2엔진+TLS, 커스텀 systemName,
agent·control 각각 TLS/insecure)을 렌더한 뒤 **실제 DAOS 2.8 바이너리**로 검사한다(exastor/daos-images `scripts/validate-config.sh`).
컨트롤 플레인이 YAML 을 `UnmarshalStrict` 로 읽으므로 2.8 에 없는 키·잘못된 타입은 바로 실패한다.

2026-09-15 결과: 7종 전부 OK(부정 대조군 2종은 기대대로 INVALID). 하드웨어 없는 검사 범위는 파싱 → fabric 인터페이스 → 호스트 메모리까지고,
그 뒤 엔진·bdev 의미 검증은 설정에 적힌 NIC·메모리를 가진 호스트가 필요하다.

## 실장비 K8s 배포 (Phase 2 종료 기준 통과)
`doc/testbed-k8s-exaci4-2.md`: exaci4-2 테스트베드(3노드 K8s v1.31)에서 **helm install → PVC → 파드 dfuse 마운트**를 실제로 통과시킨 기록.
DAOS 는 기존대로 VM 에 네이티브로 두고 K8s 가 소비만 하는 배치(`spec.externalMsReplicas`)다. 깨끗한 PVC→마운트 **30초**, 삭제까지 76초.
EL8 노드 준비의 함정(cgroupfs 드라이버, crun, conntrack)과 이 과정에서 찾아 고친 결함 6건이 같은 문서에 있다.

## 실장비 검증
`doc/testbed-2026-09-15.md`: daos_ci 에서 operator 의 dmg/daos Job 명령 전부, CSI 노드 dfuse 마운트(재시작 복구 포함), hostprep 탐색을 실장비로 확인.
주의 두 가지 — `spec.systemName`(기본 daos_server; 테스트베드는 daos_flexa), 컨테이너 기본 oclass RP_2GX/RP_2G1 은 서버 노드 2대 이상 필요(1대면 SX/S1, rd_fac 0).

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
