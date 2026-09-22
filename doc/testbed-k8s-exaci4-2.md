<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# exaci4-2 테스트베드: K8s 가 기존 DAOS 를 소비하는 배치 (2026-09-22)

Phase 2 종료 기준("helm install 후 PV 가 파드에 마운트")을 **실장비에서 통과**시킨 기록이다.
DAOS 서버는 기존대로 VM 에 네이티브로 두고, K8s 는 그것을 **소비만** 한다(`spec.externalMsReplicas`, #22).

## 구성

| 노드 | 역할 | 비고 |
|---|---|---|
| exaci4-2j (192.168.34.20) | K8s 컨트롤 플레인 | 5.7 GB RAM. DAOS 안 씀 |
| flexa-3423-1-a (.21) | 워커 + **DAOS 클라이언트** | daos_flexa 의 엔진 2개와 MS(127.0.0.100), ib0 172.28.178.118 이 여기에만 있다 |
| flexa-3423-1-b (.22) | 워커 | IB 주소가 없어 DAOS 접근 불가 |

- Kubernetes v1.31.14, CRI-O 1.31.5, flannel(`--iface=ens18`), cgroup v1.
- **DAOS 는 A 노드에서만 쓸 수 있다**: MS 가 127.0.0.100(로컬 dummy0)에만 열려 있고 ib0 주소도 A 에만 있다.
  그래서 `nodeSelector: daos.gluesys.com/client=true` 를 A 에만 붙이고 dmg/daos Job 과 CSI 노드 플러그인을 거기로 고정한다.

## 노드 준비에서 걸린 것 (EL8 특이사항)

1. `kubeadm` preflight 가 `conntrack` 없음으로 실패 → `conntrack-tools iproute-tc socat` 설치.
2. CRI-O 가 컨테이너 생성에서 `writing file 'devices.allow': Operation not permitted`.
   SELinux 는 무관(permissive 로도 동일). pkgs.k8s.io 의 CRI-O 는 SLE 빌드라 EL8 의 **cgroup v1 + systemd 239** 와 맞지 않는다.
   → CRI-O 와 kubelet 을 **cgroupfs** 드라이버로 바꾸면 해결(`/etc/crio/crio.conf.d/10-cgroupfs.conf`, kubeadm `KubeletConfiguration.cgroupDriver`).
   장기적으로는 cgroup v2 로 부팅하거나(재부팅 필요) containerd 를 쓰는 편이 낫다.
3. CRI-O 1.31 의 기본 런타임은 `crun` 인데 EL8 에는 없다 → `dnf install crun`.
4. 컨트롤 플레인 노드가 NotReady: CNI 설정을 CRI-O 가 늦게 집는다 → `systemctl restart crio` 후 Ready.
5. 노드 이름: 호스트명에 `_` 와 대문자가 있어 K8s 이름으로 못 쓴다 → `--node-name flexa-3423-1-a` 로 명시.

## 배포

```bash
kubectl apply --server-side -f charts/daos-operator/crds/
kubectl -n daos-system create secret docker-registry gitlab-registry \
  --docker-server=registry.gitlab.gluesys.com --docker-username=<user> --docker-password=<token>
kubectl -n daos-system patch sa default -p '{"imagePullSecrets":[{"name":"gitlab-registry"}]}'   # operator 가 만드는 Job 파드까지 덮는다
kubectl label node flexa-3423-1-a daos.gluesys.com/client=true daos.gluesys.com/role=storage
helm install daos-operator charts/daos-operator -n daos-system -f deploy/exaci4-2-values.yaml
```

## 결과

| 단계 | 결과 |
|---|---|
| operator 가 기존 시스템 인식 | `FORMATTED=true, RANKS=2, READY=True` (rank 0·1 joined) |
| `DaosPool` 생성 | `k8spool` 3Gi, rank 0, `Ready`, `USED% 2` |
| PVC → 파드 마운트(깨끗한 2회차) | **30초** (기준 15분) |
| 파드에서 쓰기 | `dd 64MiB` = 100 MB/s, 읽기·md5 확인, `df` 가 dfuse 3.1G 로 보고 |
| PVC 삭제 | 파드 정상 삭제 후 76초 내 DAOS 컨테이너 파괴까지 완료 |

## 이 배포에서 찾아 고친 결함

| 증상 | 원인 | 수정 |
|---|---|---|
| 풀 생성이 `DER_INVAL` | `redundancyFactor: 0` 이 CRD 기본값 2 로 덮임(omitempty+default) | `*int32` 로 변경 |
| 컨테이너 reconcile 이 계속 실패 | Job 이름이 63자 초과(PV 이름) → 파드 템플릿 라벨 거부 | 이름 축약 + 키 해시 |
| `daos` 가 `DER_MISC` 로 API 초기화 실패 | agent 사이드카에 `/dev` 가 없어 fabric(ib0→mlx5_0) 열거 불가 | 사이드카에 `/dev` + privileged (operator·차트·daos-csi 모두) |
| 외부 모드에서 rank 를 못 읽음 | 서버 파드가 없다고 조회를 건너뜀 | 외부 모드는 조회 수행 |
| 고아 마운트 후 unstage 무한 실패 | 경로가 사라진 뒤에도 `fusermount3` 오류를 그대로 반환 | 없으면 성공 처리 |
| 사용 중 컨테이너 파괴 7초마다 재시도 | 끝난 Job 삭제 이벤트가 즉시 다음 시도를 부름 | 30초 홀드 + "아직 마운트 중" 설명 |

## 알아둘 것

- **파드를 `--force --grace-period=0` 로 지우면** kubelet 이 unstage 를 건너뛰어 dfuse 마운트가 남고, 그 컨테이너는
  `DER_BUSY` 로 파괴되지 않는다. 노드에서 `mount | grep dfuse` 로 확인하고 `fusermount3 -u <경로>` 로 정리한다.
- 이 테스트베드의 DAOS 는 rank 별 여유가 달라 풀을 만들 때 `ranks: [0]` 이 필요했다. 전체 rank 로는 `DER_NOSPACE`.
- 장애 도메인이 하나(엔진 2개가 같은 호스트)이므로 컨테이너 오브젝트 클래스는 `SX`/`S1`, `rdFac: 0` 이어야 한다.
