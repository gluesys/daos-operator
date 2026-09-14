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
- Phase 0: kubebuilder v4 뼈대, CRD 3종(kind·envtest 검증).
- **Phase 2 #7·#8 (2026-09-14): `DaosSystem` reconcile 1단계 + 호스트 준비 DaemonSet.** nodeSelector 로 노드 선택 → Node 어노테이션에서 노드별 사실
  (fabric NIC, VFIO NVMe 목록, 드라이브 DSN, NUMA) 읽기 → **같은 물리 드라이브(DSN)를 두 노드가 노출하면 그 노드들을 제외하고
  `DriveConflict=True`**(2026-09-03 손상 사고 재발 방지) → 관리 서비스 복제본 노드 선택(이름순 안정) → 노드별 `daos_server.yml`
  ConfigMap + `-agent`/`-control` ConfigMap 렌더 → `.status`(selectedNodes, msReplicaNodes, nodeConfigs, conditions).
- **Phase 2 #9 (2026-09-14): 노드 고정 서버 StatefulSet.** 렌더된 노드마다 `<sys>-server-<node>` StatefulSet(replicas 1, 필수 nodeAffinity,
  hostNetwork, privileged, hugepages-2Mi 리소스, hostPath 데이터/로그) 을 만든다.
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

`.status.conditions`: `NodesSelected`, `DriveConflict`, `ConfigRendered`(Partial 이면 nodeConfigs 의 message 에 이유), `ServersReady`, `Formatted`, `Ready`.
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
| 미러 | uuid, state, total/free(티어 합), rebuild, disabledTargets, enabledRanks, lastQueryTime | uuid, poolUUID, health, type, ready |
| 실패 | `Ready=False (CreateFailed/ExtendFailed/AclFailed)` + Event, **spec 이 바뀔 때까지 재시도 안 함**(observedGeneration) | 동일 |
| 사라짐 | 이전에 uuid 를 알았는데 없어짐 → `PoolMissing`, **재생성 안 함**(데이터 손실은 사람이 봐야 한다) | `ContainerMissing`, 동일 |
| 삭제 | `destroy-approved=true` → `pool destroy --recursive`; 없으면 풀은 남기고 Event `PoolOrphaned` | `cont destroy`; 없으면 Event `ContainerOrphaned` |

- 크기(`spec.size`)는 생성 시에만 쓰인다. DAOS 2.8 은 풀 크기 변경이 없다(확장 = rank 추가).
- `overwrite-acl` 은 ACL 전체를 바꾼다. `spec.acl` 에 `A::OWNER@:...` 등 필요한 항목을 모두 적어야 한다.
- 컨테이너 Job 은 K8s ≥ 1.29 의 네이티브 사이드카(`initContainers[].restartPolicy: Always`)로 `daos_agent` 를 붙이고, 클라이언트가 fabric 을 쓰기
  위해 hostNetwork·`/dev`·privileged 로 뜬다(Phase 0; RDMA 디바이스 플러그인으로 대체 예정). `spec.images.client` 가 필요하다.
- 조회 주기 30초(홀드), 정상 60초 requeue. 한 reconcile 에 연산은 하나만 시작한다(`status.operation`).

```bash
kubectl get daospool,daoscont -A
kubectl annotate daospool kv daos.gluesys.com/destroy-approved=true && kubectl delete daospool kv   # 정말 파괴할 때만
kubectl -n daos-system get jobs -l daos.gluesys.com/pool=kv
```

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
