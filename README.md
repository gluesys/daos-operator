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
- **Phase 2 #7 (2026-09-14): `DaosSystem` reconcile 1단계 구현.** nodeSelector 로 노드 선택 → Node 어노테이션에서 노드별 사실
  (fabric NIC, VFIO NVMe 목록, 드라이브 DSN, NUMA) 읽기 → **같은 물리 드라이브(DSN)를 두 노드가 노출하면 그 노드들을 제외하고
  `DriveConflict=True`**(2026-09-03 손상 사고 재발 방지) → 관리 서비스 복제본 노드 선택(이름순 안정) → 노드별 `daos_server.yml`
  ConfigMap + `-agent`/`-control` ConfigMap 렌더 → `.status`(selectedNodes, msReplicaNodes, nodeConfigs, conditions).
  서버 파드는 아직 만들지 않으므로 `Ready=False (PodsNotManaged)` 로 정직하게 표시한다(#9).
설계 결정은 `doc/adr/` (ADR-001 배포 모델, ADR-002 디바이스·네트워크, ADR-003 업그레이드).

## 노드 사실(facts) 계약
호스트 준비 DaemonSet(#8)이 쓰기 전까지는 손으로 어노테이션을 단다. spec.engines 의 값이 있으면 그것이 우선한다.

| 어노테이션 | 값 | 예 |
|---|---|---|
| `daos.gluesys.com/fabric-iface` | 엔진이 바인드할 NIC 이름(호스트마다 다름) | `ens2`, `ens2np0`, `ib0` |
| `daos.gluesys.com/bdev-list` | VFIO 바인딩된 NVMe PCI 주소, 콤마 구분 | `0000:03:00.0,0000:04:00.0` |
| `daos.gluesys.com/bdev-dsn` | `<pci>=<PCI Device Serial Number>` 목록 | `0000:03:00.0=6479A701A8C0D000,...` |
| `daos.gluesys.com/numa-node` | (선택) 엔진 0 고정 NUMA | `1` |
| `daos.gluesys.com/control-addr` | (선택) mgmt_svc_replicas/access_points/hostlist 에 쓸 주소. 기본은 노드 InternalIP | `172.28.136.184` |

```bash
kubectl label node cell1 daos.gluesys.com/role=storage
kubectl annotate node cell1 daos.gluesys.com/fabric-iface=ens2 \
  daos.gluesys.com/bdev-list=0000:03:00.0 daos.gluesys.com/bdev-dsn=0000:03:00.0=$(lspci -vvs 03:00.0 | awk '/Device Serial/{print $NF}')
kubectl apply -f config/samples/daos_v1alpha1_daossystem.yaml
kubectl get daossys daos-dev -o yaml | yq .status      # conditions, nodeConfigs
kubectl -n daos-system get cm -l daos.gluesys.com/system=daos-dev
```

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
