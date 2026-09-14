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
  서버 파드는 아직 만들지 않으므로 `Ready=False (PodsNotManaged)` 로 정직하게 표시한다(#9).
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

`.status.conditions`: `NodesSelected`, `DriveConflict`, `ConfigRendered`(Partial 이면 nodeConfigs 의 message 에 이유), `Ready`.
렌더 결과는 `exastor/daos-images` 서버 엔트리포인트와 같은 2.8 키(`mgmt_svc_replicas`, agent `access_points`, control `hostlist`)를 쓴다.

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
