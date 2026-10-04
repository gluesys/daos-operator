<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# DAOS K8s CI Phase D — Tier 2 야간 잡 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 3노드 프로파일 3종(`kdev-3rank`·`mixed-2engine`·`tiers`)에서 Tier 1 의 1~3·7 을 반복하고, `kdev-3rank` 에서 복원력 케이스를 돌려 결과와 복구 시간을 야간 스케줄 파이프라인으로 보고한다.

**Architecture:** 기존 Ginkgo 스위트(`test/e2e`)를 그대로 쓴다. 프로파일은 Helm values 파일로 추가하고, 기대 rank 수만 `E2E_RANKS` 로 일반화한다. 복원력 케이스는 새 파일 `tier2_test.go` 의 `Label("tier2")` 컨테이너에 두고, 복구 시간은 CSV 로 남긴다. CI 는 프로파일마다 스케줄 잡 하나(`resource_group: daos-k8s-ci`).

**Tech Stack:** Go 1.24 / Ginkgo v2 / Gomega, Helm 4, kubectl, GitLab CI(shell 러너 3J, 태그 `daos-k8s`), Proxmox `qm`(hack/ci/env.sh 의 `qm()` 래퍼).

## Global Constraints

- 설계: `doc/ci-design-2026-10-02.md` 5절(프로파일 표)·7절(Tier 2 목록)·8절(Tier 2 는 보고만, MR 차단 없음).
- 노드: 저장 노드 3A·3B·4B(이미 `daos.gluesys.com/role=storage`), scale-out 노드 4A(클라이언트 `daos.gluesys.com/client=true`). VMID 3A=109, 3B=110, 4A=112, 4B=113.
- 워커 자원(2026-10-04 확인): hugepages 8Gi, 메모리 할당 가능 약 23Gi, `ib0` = ConnectX-4 VF EDR 100Gb/s ACTIVE(`mlx5_0`), kdev 디스크 `/dev/sdb`·`/dev/sdc`(각 100G), QEMU NVMe 2개.
- 공통 값: `scm` ram 4 GiB/엔진, `systemRamReservedGiB: 8`, `allowInsecure: true`, verbs 프로파일 `provider: "ofi+verbs;ofi_rxm"`, `fabricIface: ib0`.
- 이미지 태그는 CI 가 `E2E_SET` 으로 커밋 태그를 넣는다(!17). 프로파일의 고정 태그는 로컬 실행용.
- `dmg storage query usage` 는 혼합 bdev_list 구성의 daos_server 2.8 을 panic 시킨다(2026-09-15). **mixed 케이스 하나에서만** xfail 로 실행한다.
- 신규 파일은 SPDX 헤더 필수. 커밋·MR 은 한글.
- 클러스터를 로컬에서 쓰기 전에 실행 중·대기 중인 e2e CI 잡이 없는지 확인한다.

---

## 파일 구조

| 파일 | 책임 |
|---|---|
| `test/e2e/helpers_test.go` (수정) | `healthy` 상수 → `healthyWant()` (`E2E_RANKS`), `recordDuration()` (CSV) |
| `test/e2e/profiles/kdev-3rank.yaml` (신규) | 3노드 kdev 1엔진, MS 3, verbs |
| `test/e2e/profiles/mixed-2engine.yaml` (신규) | 노드당 엔진 2개(nvme + kdev), 6 rank |
| `test/e2e/profiles/tiers.yaml` (신규) | 엔진당 nvme[wal,meta] + kdev[data] |
| `test/e2e/tier2_test.go` (신규) | 복원력 케이스(`Label("tier2","resilience")`), mixed xfail(`Label("tier2","mixed")`) |
| `hack/ci/node-power.sh` (신규) | `qm stop/start` 로 워커 재부팅 |
| `.gitlab-ci.yml` (수정) | `e2e-tier2-{kdev,mixed,tiers}` 스케줄 잡, tier1 필터에 `!tier2` |
| `Makefile` (수정) | `E2E_LABELS` 기본값에 `!tier2` |
| `doc/ci-design-2026-10-02.md` (수정) | 10절 D 진행 표기 |

---

### Task 1: rank 수 일반화와 복구 시간 CSV

**Files:**
- Modify: `test/e2e/helpers_test.go:68-81`
- Modify: `test/e2e/tier1_test.go:329` (case 8 의 `healthy` 참조)
- Modify: `Makefile:89`, `.gitlab-ci.yml` (e2e-tier1 의 label-filter)

**Interfaces:**
- Produces: `func healthyWant() string`, `func recordDuration(name string, d time.Duration)`, 환경 변수 `E2E_RANKS`(기본 1), `E2E_TIMINGS`(CSV 경로, 비면 기록 안 함)

- [ ] **Step 1: `healthy` 상수를 함수로 바꾼다**

`test/e2e/helpers_test.go` 의 아래 블록을

```go
const healthy = "Ready=True ManagementService=True ranksJoined=1"

// waitReadyStable waits for systemHealthy and then requires it to hold for a minute.
func waitReadyStable(timeout time.Duration) {
	EventuallyWithOffset(1, systemHealthy).WithTimeout(timeout).Should(Equal(healthy))
	ConsistentlyWithOffset(1, systemHealthy).WithTimeout(time.Minute).Should(Equal(healthy))
}
```

다음으로 바꾼다.

```go
// healthyWant is systemHealthy's value for a healthy system of E2E_RANKS ranks (default 1).
func healthyWant() string {
	return "Ready=True ManagementService=True ranksJoined=" + envOr("E2E_RANKS", "1")
}

// waitReadyStable waits for systemHealthy and then requires it to hold for a minute.
func waitReadyStable(timeout time.Duration) {
	EventuallyWithOffset(1, systemHealthy).WithTimeout(timeout).Should(Equal(healthyWant()))
	ConsistentlyWithOffset(1, systemHealthy).WithTimeout(time.Minute).Should(Equal(healthyWant()))
}

// recordDuration reports a recovery time and appends "spec,name,seconds" to $E2E_TIMINGS (Tier 2
// trends recovery times, not performance: doc/ci-design-2026-10-02.md 7절).
func recordDuration(name string, d time.Duration) {
	AddReportEntry(name, d.Round(time.Second).String())
	path := os.Getenv("E2E_TIMINGS")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	Expect(err).NotTo(HaveOccurred())
	defer f.Close()
	_, err = fmt.Fprintf(f, "%q,%q,%.0f\n", CurrentSpecReport().LeafNodeText, name, d.Seconds())
	Expect(err).NotTo(HaveOccurred())
}
```

`helpers_test.go` import 에 `"os"` 가 없으면 추가한다.

- [ ] **Step 2: case 8 의 참조를 고친다**

`test/e2e/tier1_test.go` 의 `.Should(Equal(healthy), "the first system must not notice")` 를 `.Should(Equal(healthyWant()), "the first system must not notice")` 로 바꾼다.

- [ ] **Step 3: Tier 1 필터에서 tier2 를 뺀다**

`Makefile`: `E2E_LABELS ?= !upgrade && !regression` → `E2E_LABELS ?= !upgrade && !regression && !tier2`
`.gitlab-ci.yml` e2e-tier1: `-ginkgo.label-filter='!upgrade && !regression'` → `-ginkgo.label-filter='!upgrade && !regression && !tier2'`

- [ ] **Step 4: 빌드 확인**

Run: `GOFLAGS=-buildvcs=false go vet -tags e2e ./test/e2e/ && grep -rn 'Equal(healthy)' test/e2e/ ; echo rc=$?`
Expected: vet 출력 없음, grep 결과 없음(`rc=1`)

- [ ] **Step 5: 커밋**

```bash
git add test/e2e/helpers_test.go test/e2e/tier1_test.go Makefile .gitlab-ci.yml
git commit -m "test(e2e): 기대 rank 수를 E2E_RANKS 로, 복구 시간 CSV 헬퍼, tier1 필터에서 tier2 제외"
```

---

### Task 2: 3노드 프로파일 3종

**Files:**
- Create: `test/e2e/profiles/kdev-3rank.yaml`, `test/e2e/profiles/mixed-2engine.yaml`, `test/e2e/profiles/tiers.yaml`

**Interfaces:**
- Consumes: 노드 라벨 `daos.gluesys.com/role=storage`(스냅샷에 있음)
- Produces: 프로파일 이름 `kdev-3rank`(E2E_RANKS=3), `mixed-2engine`(E2E_RANKS=6), `tiers`(E2E_RANKS=3). CSI StorageClass 이름은 Tier 1 과 같은 `daos-e2e`

- [ ] **Step 1: `kdev-3rank.yaml`**

```yaml
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# Tier 2 프로파일: 저장 노드 3대(3A·3B·4B, role=storage), kdev 1엔진, MS 복제본 3, verbs/ib0.
# 다중 rank·MS 정족수·rank 재편입·drain·scale-out(4A). 설계 doc/ci-design-2026-10-02.md 5절.
image:
  repository: registry.gitlab.gluesys.com/exastor/daos-operator/daos-operator
  tag: "65bcaee5"
imagePullSecrets:
  - name: gitlab-registry
hostprep:
  defaultImage: registry.gitlab.gluesys.com/exastor/daos-operator/daos-hostprep:65bcaee5
system:
  create: true
  name: daos
  spec:
    version: "2.8.0"
    namespace: daos-system
    images:
      server: registry.gitlab.gluesys.com/exastor/daos-images/daos-server:2.8.0-20261003-g9927f7be
      agent:  registry.gitlab.gluesys.com/exastor/daos-images/daos-agent:2.8.0-20261003-g9927f7be
      admin:  registry.gitlab.gluesys.com/exastor/daos-images/daos-admin:2.8.0-20261003-g9927f7be
      client: registry.gitlab.gluesys.com/exastor/daos-images/daos-client:2.8.0-20261003-g9927f7be
    placement: dedicated
    nodeSelector:
      daos.gluesys.com/role: storage
    msReplicas: 3
    provider: "ofi+verbs;ofi_rxm"
    nrHugepages: 2048
    allowInsecure: true
    systemRamReservedGiB: 8
    engines:
      - targets: 4
        helpers: 0
        fabricIface: ib0
        bdevClass: kdev
        bdevList: [/dev/sdb]
        scmSizeGiB: 4
    hostPrep:
      enabled: true
      bindNvme: false
csi:
  enabled: true
  image:
    repository: registry.gitlab.gluesys.com/exastor/daos-csi/daos-csi
    tag: 55f554f9
  storageClasses:
    - name: daos-e2e
      reclaimPolicy: Delete
      parameters: {pool: e2epool, oclass: SX, dirOclass: S1, rdFac: "0", chunkSize: "4194304", csum: crc32}
```

- [ ] **Step 2: `mixed-2engine.yaml`** — `kdev-3rank.yaml` 을 복사한 뒤 머리말과 `nrHugepages`, `engines`, `hostPrep` 만 다음으로 바꾼다.

```yaml
# Tier 2 프로파일: 저장 노드 3대, 노드당 엔진 2개 — engine0 nvme(QEMU NVMe, uio_pci_generic), engine1 kdev
# (/dev/sdb). 6 rank, ib0 공유, fabric 포트 31316/31416(operator 가 엔진마다 +100). 혼합 bdev_list 에서
# `dmg storage query usage` 패닉을 xfail 로 고정한다. 설계 5절.
    nrHugepages: 3072
    engines:
      - targets: 4
        helpers: 0
        fabricIface: ib0
        bdevClass: nvme
        bdevList: []            # hostprep 이 발견한 QEMU NVMe
        scmSizeGiB: 4
      - targets: 4
        helpers: 0
        fabricIface: ib0
        bdevClass: kdev
        bdevList: [/dev/sdb]
        scmSizeGiB: 4
    disableVFIO: true
    hostPrep:
      enabled: true
      bindNvme: true
```

- [ ] **Step 3: `tiers.yaml`** — 같은 방식으로 다음으로 바꾼다.

```yaml
# Tier 2 프로파일: 저장 노드 3대, 엔진당 nvme[wal,meta] + kdev[data](/dev/sdb,/dev/sdc). CRD 주석의 "출하 형태"
# 이지만 CI·수동 검증 기록이 없던 bdevTiers 렌더링과 포맷을 본다. 설계 5절.
    nrHugepages: 2048
    engines:
      - targets: 4
        helpers: 0
        fabricIface: ib0
        scmSizeGiB: 4
        bdevTiers:
          - class: nvme
            roles: [wal, meta]
            bdevList: []        # hostprep 의 bdev-list(-0) 어노테이션
          - class: kdev
            roles: [data]
            bdevList: [/dev/sdb, /dev/sdc]
    disableVFIO: true
    hostPrep:
      enabled: true
      bindNvme: true
```

- [ ] **Step 4: 스키마 확인 (클러스터 dry-run, 실행 중 e2e 잡 없을 때)**

Run:
```bash
for p in kdev-3rank mixed-2engine tiers; do
  helm template daos-operator charts/daos-operator -n daos-system -f test/e2e/profiles/$p.yaml \
    | kubectl apply --dry-run=server -f - >/dev/null && echo "$p ok"
done
```
Expected: `kdev-3rank ok`, `mixed-2engine ok`, `tiers ok` (DaosSystem enum·필드 위반이 있으면 server dry-run 이 거부한다)

- [ ] **Step 5: 커밋**

```bash
git add test/e2e/profiles/kdev-3rank.yaml test/e2e/profiles/mixed-2engine.yaml test/e2e/profiles/tiers.yaml
git commit -m "test(e2e): Tier 2 프로파일 kdev-3rank, mixed-2engine, tiers"
```

---

### Task 3: 프로파일별 기동 검증 (Tier 1 의 1~3·7 반복)

**Files:** 없음(코드 변경 없음, 실행으로 검증)

**Interfaces:**
- Consumes: Task 1 의 `E2E_RANKS`, Task 2 의 프로파일

- [ ] **Step 1: kdev-3rank** (클러스터가 비어 있을 때)

Run:
```bash
cd test/e2e && E2E_RESET=1 E2E_PROFILE=kdev-3rank E2E_RANKS=3 \
  go test -tags e2e . -count=1 -timeout 90m -v -args -ginkgo.v \
  -ginkgo.focus='case [1237]\)' -ginkgo.label-filter='!upgrade && !regression && !tier2'
```
Expected: `Ran 4 of N Specs`, `4 Passed`

- [ ] **Step 2: mixed-2engine** — 위에서 `E2E_PROFILE=mixed-2engine E2E_RANKS=6`. Expected: 4 Passed
- [ ] **Step 3: tiers** — `E2E_PROFILE=tiers E2E_RANKS=3`. Expected: 4 Passed

실패하면 operator 결함일 수 있다. 설계 13절 표에 행을 추가하고(재현 조건·잡 번호), 고친 뒤 이어간다. 추측으로 고치지 않는다.

---

### Task 4: 노드 전원 스크립트

**Files:**
- Create: `hack/ci/node-power.sh`

**Interfaces:**
- Produces: `hack/ci/node-power.sh reboot <node-name>` — VM 을 `qm stop` 후 `qm start`, SSH 가 돌아올 때까지 대기. 종료 코드 0 = 성공

- [ ] **Step 1: 스크립트 작성**

```bash
#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# 워커 VM 전원 제어(Tier 2 노드 재부팅 케이스). env.sh 의 qm() 은 3J 에서는 jenkins-ci@vdi5, 개발 PC 에서는
# `ssh vdi5` 로 간다. 사용: node-power.sh reboot exaci5-3b
set -euo pipefail
. "$(dirname "$0")/env.sh"
op=${1:?op}; node=${2:?node}
vmid=""
for v in "${!VM_NAME[@]}"; do [ "${VM_NAME[$v]}" = "$node" ] && vmid=$v; done
[ -n "$vmid" ] || { echo "node-power: unknown node $node" >&2; exit 2; }
case $op in
  reboot)
    qm stop "$vmid" --timeout 120
    qm start "$vmid"
    wait_ssh "${VM_IP[$vmid]}" 300
    echo "node-power: $node ($vmid) back"
    ;;
  *) echo "node-power: unknown op $op" >&2; exit 2 ;;
esac
```

- [ ] **Step 2: 문법 확인**

Run: `chmod +x hack/ci/node-power.sh && bash -n hack/ci/node-power.sh && hack/ci/node-power.sh bogus exaci5-3b; echo rc=$?`
Expected: `node-power: unknown op bogus`, `rc=2`

- [ ] **Step 3: 커밋**

```bash
git add hack/ci/node-power.sh
git commit -m "ci: 워커 VM 재부팅 스크립트(node-power.sh, Tier 2)"
```

---

### Task 5: 복원력 케이스 (kdev-3rank)

**Files:**
- Create: `test/e2e/tier2_test.go`

**Interfaces:**
- Consumes: `healthyWant()`, `recordDuration()`(Task 1), `hack/ci/node-power.sh`(Task 4), 기존 헬퍼 `kubectl`, `kubectlE`, `jsonpath`, `condStatus`, `condReason`, `systemHealthy`, `waitReadyStable`, `apply`, `run`, `envOr`, `serverPod(node)`, `expectKnownBug`
- Produces: `Label("tier2","resilience")` 스펙 7개, `Label("tier2","mixed")` 스펙 1개

케이스 순서(Ordered): 풀·PVC 준비 → rank 재편입 → MS 정족수 → MS 리더 kill → CSI node plugin 재시작 → 노드 재부팅 → drain/uncordon → scale-out(마지막: rank 수가 4로 바뀐다).

- [ ] **Step 1: 파일 뼈대와 공통 헬퍼**

```go
//go:build e2e

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Gluesys Co., Ltd.

package e2e

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Tier 2 (doc/ci-design-2026-10-02.md 7절): what multi-rank systems do when parts of them fail.
// Run against kdev-3rank: E2E_PROFILE=kdev-3rank E2E_RANKS=3 -ginkgo.label-filter='tier2 && resilience'.
// Recovery times go to $E2E_TIMINGS (recordDuration) so their trend is visible night to night.
var _ = Describe("Tier 2 resilience", Label("tier2", "resilience"), Ordered, func() {
	// rankOn returns the rank number of node's first engine, from status.ranks.
	rankOn := func(node string) int {
		out := jsonpath("daossystem", sysName, `.status.ranks[?(@.node=="`+node+`")].rank`)
		f := strings.Fields(out)
		Expect(f).NotTo(BeEmpty(), "no rank on %s", node)
		n, err := strconv.Atoi(f[0])
		Expect(err).NotTo(HaveOccurred())
		return n
	}
	rankState := func(rank int) string {
		return jsonpath("daossystem", sysName, fmt.Sprintf(`.status.ranks[?(@.rank==%d)].state`, rank))
	}
	// signalServer sends sig to daos_server inside node's server pod (STOP/CONT/KILL).
	signalServer := func(node, sig string) {
		out := kubectl("exec", serverPod(node), "-c", "daos-server", "--", "sh", "-c",
			`for p in /proc/[0-9]*; do [ "$(cat $p/comm 2>/dev/null)" = daos_server ] && kill -`+sig+` ${p#/proc/} && echo signalled ${p#/proc/}; done; true`)
		Expect(out).To(ContainSubstring("signalled"), "no daos_server in %s", serverPod(node))
	}
	// reintegrateIfNeeded is the documented recovery once the engine is back: reintegrate the rank
	// unless it rejoined on its own (kubectl daos waits for the result, daos-operator !14).
	reintegrateIfNeeded := func(rank int) {
		Eventually(func() string { return condStatus("ServersReady") }).WithTimeout(10 * time.Minute).Should(Equal("True"))
		time.Sleep(90 * time.Second)
		if rankState(rank) != "joined" {
			out := run(envOr("KUBECTL_DAOS", "kubectl-daos"), "rank", "reintegrate", sysName,
				fmt.Sprintf("--ranks=%d", rank), "--yes", "--wait=8m", "-n", ns)
			Expect(out).To(ContainSubstring("done: "))
		}
	}
	nodes := strings.Split(envOr("E2E_STORAGE_NODES", "exaci5-3a,exaci5-3b,exaci5-4b"), ",")
	victim := nodes[1] // not the client node, not necessarily the MS leader

	BeforeAll(func() {
		By("a pool and a volume with a checksummed file, kept for the whole container")
		apply(`apiVersion: daos.gluesys.com/v1alpha1
kind: DaosPool
metadata: {name: e2epool}
spec: {systemRef: daos, size: 8Gi, redundancyFactor: 0}`)
		Eventually(func() string { return jsonpath("daospool", "e2epool", `.status.conditions[?(@.type=="Ready")].status`) }).
			WithTimeout(5 * time.Minute).Should(Equal("True"))
		apply(`apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: e2e-t2}
spec: {accessModes: [ReadWriteMany], storageClassName: daos-e2e, resources: {requests: {storage: 1Gi}}}`)
		apply(fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: e2e-t2-app}
spec:
  nodeSelector: {daos.gluesys.com/client: "true"}
  containers:
    - name: c
      image: %s
      command: [sh, -c, "head -c 16777216 /dev/urandom > /data/blob && sha256sum /data/blob > /data/blob.sha256 && sleep infinity"]
      volumeMounts: [{name: d, mountPath: /data}]
  volumes: [{name: d, persistentVolumeClaim: {claimName: e2e-t2}}]`, jsonpath("daossystem", sysName, ".spec.images.client")))
		Eventually(func() string { return jsonpath("pod", "e2e-t2-app", ".status.phase") }).WithTimeout(15 * time.Minute).Should(Equal("Running"))
		Eventually(func() error {
			_, err := kubectlE("exec", "e2e-t2-app", "--", "test", "-s", "/data/blob.sha256")
			return err
		}).WithTimeout(2 * time.Minute).Should(Succeed())
	})
	checksumOK := func() {
		out, err := kubectlE("exec", "e2e-t2-app", "--", "sha256sum", "-c", "/data/blob.sha256")
		Expect(err).NotTo(HaveOccurred(), out)
		Expect(out).To(ContainSubstring("OK"))
	}
```

- [ ] **Step 2: rank 재편입 (서버 파드 삭제)**

```go
	It("brings a rank back after its server pod is deleted", func() {
		rank := rankOn(victim)
		start := time.Now()
		kubectl("delete", "pod", serverPod(victim), "--wait=false")
		Eventually(func() string { return rankState(rank) }).WithTimeout(5 * time.Minute).ShouldNot(Equal("joined"))
		recordDuration("rank down detected", time.Since(start))
		reintegrateIfNeeded(rank)
		waitReadyStable(10 * time.Minute)
		recordDuration("rank back to healthy", time.Since(start))
		checksumOK()
	})
```

- [ ] **Step 3: MS 정족수 상실 (복제본 3 중 2 정지)**

파드 삭제는 곧바로 재기동돼 정족수 상실이 잠깐만 보인다. daos_server 를 SIGSTOP 으로 멈춰 두고 조건을 본 뒤 SIGCONT 로 돌린다.

```go
	It("reports NoQuorum while two of three MS replicas are frozen (#34)", func() {
		frozen := []string{nodes[1], nodes[2]}
		DeferCleanup(func() {
			for _, n := range frozen {
				_, _ = kubectlE("exec", serverPod(n), "-c", "daos-server", "--", "sh", "-c",
					`for p in /proc/[0-9]*; do [ "$(cat $p/comm 2>/dev/null)" = daos_server ] && kill -CONT ${p#/proc/}; done; true`)
			}
		})
		start := time.Now()
		for _, n := range frozen {
			signalServer(n, "STOP")
		}
		Eventually(func() string { return condReason("ManagementService") }).WithTimeout(5 * time.Minute).Should(Equal("NoQuorum"))
		recordDuration("NoQuorum reported", time.Since(start))
		for _, n := range frozen {
			signalServer(n, "CONT")
		}
		start = time.Now()
		waitReadyStable(10 * time.Minute)
		recordDuration("quorum back to healthy", time.Since(start))
	})
```

- [ ] **Step 4: MS 리더 kill**

설계는 "제어 평면 10초 내 복구"지만, operator 조건은 멤버십 조회 주기로만 갱신된다. 그래서 새 리더가 보고되기까지의 시간을 기록하고, 단언은 2분으로 둔다. 데이터 경로 정지는 xfail 로 시간만 남긴다.

```go
	It("elects a new MS leader after the leader's daos_server is killed", func() {
		leaderRe := regexp.MustCompile(`leader ([0-9.]+):`)
		leaderIP := func() string {
			m := leaderRe.FindStringSubmatch(jsonpath("daossystem", sysName, `.status.conditions[?(@.type=="ManagementService")].message`))
			if m == nil {
				return ""
			}
			return m[1]
		}
		old := leaderIP()
		Expect(old).NotTo(BeEmpty())
		node := jsonpath("daossystem", sysName, `.status.nodeConfigs[?(@.controlAddr=="`+old+`")].node`)
		Expect(node).NotTo(BeEmpty(), "no node with control address %s", old)
		start := time.Now()
		signalServer(node, "KILL")
		Eventually(func() string { return leaderIP() }).WithTimeout(2 * time.Minute).
			Should(And(Not(BeEmpty()), Not(Equal(old))), "a new leader must be reported")
		recordDuration("new MS leader reported", time.Since(start))
		expectKnownBug("data path stalls ~2 min after an MS leader change (design 7절)", func() {
			out, err := kubectlE("exec", "e2e-t2-app", "--", "sh", "-c", "timeout 10 sha256sum -c /data/blob.sha256")
			Expect(err).NotTo(HaveOccurred(), out)
		})
		reintegrateIfNeeded(rankOn(node))
		waitReadyStable(10 * time.Minute)
		recordDuration("leader kill back to healthy", time.Since(start))
		checksumOK()
	})
```

- [ ] **Step 5: CSI node plugin 재시작**

```go
	It("keeps a mounted volume readable across a CSI node plugin restart", func() {
		node := jsonpath("pod", "e2e-t2-app", ".spec.nodeName")
		plugin := kubectl("get", "pod", "-n", ns, "-l", "app.kubernetes.io/component=csi-node",
			"--field-selector", "spec.nodeName="+node, "-o", "jsonpath={.items[0].metadata.name}")
		Expect(plugin).NotTo(BeEmpty())
		kubectl("delete", "pod", "-n", ns, plugin, "--wait=true", "--timeout=3m")
		Eventually(func() string {
			return kubectl("get", "pod", "-n", ns, "-l", "app.kubernetes.io/component=csi-node",
				"--field-selector", "spec.nodeName="+node, "-o", `jsonpath={.items[0].status.conditions[?(@.type=="Ready")].status}`)
		}).WithTimeout(3 * time.Minute).Should(Equal("True"))
		checksumOK()
	})
```

실행 전 `kubectl -n daos-system get pod --show-labels | grep csi-node` 로 라벨을 확인한다. 다르면 셀렉터를 실제 라벨로 바꾼다.

- [ ] **Step 6: 노드 재부팅**

```go
	It("keeps the pool and its data across a storage node reboot", func() {
		rank := rankOn(victim)
		start := time.Now()
		run(filepath.Join(repoRoot, "hack", "ci", "node-power.sh"), "reboot", victim)
		Eventually(func() string {
			return kubectl("get", "node", victim, "-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`)
		}).WithTimeout(10 * time.Minute).Should(Equal("True"))
		recordDuration("node Ready after reboot", time.Since(start))
		reintegrateIfNeeded(rank)
		waitReadyStable(15 * time.Minute)
		recordDuration("reboot back to healthy", time.Since(start))
		Expect(jsonpath("daospool", "e2epool", `.status.conditions[?(@.type=="Ready")].status`)).To(Equal("True"))
		checksumOK()
	})
```

- [ ] **Step 7: drain / uncordon**

```go
	It("recovers after a storage node is drained and uncordoned", func() {
		rank := rankOn(victim)
		start := time.Now()
		DeferCleanup(func() { _, _ = kubectlE("uncordon", victim) })
		kubectl("drain", victim, "--ignore-daemonsets", "--delete-emptydir-data", "--force", "--timeout=5m")
		Eventually(func() string { return rankState(rank) }).WithTimeout(5 * time.Minute).ShouldNot(Equal("joined"))
		kubectl("uncordon", victim)
		reintegrateIfNeeded(rank)
		waitReadyStable(10 * time.Minute)
		recordDuration("drain/uncordon back to healthy", time.Since(start))
		checksumOK()
	})
```

- [ ] **Step 8: scale-out (4번째 rank)** — 마지막에 둔다.

```go
	It("formats and joins a fourth rank when a node is added (scale-out)", func() {
		node := envOr("E2E_SCALEOUT_NODE", "exaci5-4a")
		start := time.Now()
		kubectl("label", "node", node, "daos.gluesys.com/role=storage", "--overwrite")
		Eventually(func() string {
			return jsonpath("daossystem", sysName, `.status.conditions[?(@.type=="Formatted")].message`)
		}).WithTimeout(10 * time.Minute).Should(ContainSubstring("not formatted yet"))
		kubectl("annotate", "daossystem", sysName, "daos.gluesys.com/format-approved=true", "--overwrite")
		Eventually(systemHealthy).WithTimeout(15 * time.Minute).Should(Equal("Ready=True ManagementService=True ranksJoined=4"))
		recordDuration("scale-out to 4 ranks", time.Since(start))
		checksumOK()
	})
})
```

- [ ] **Step 9: mixed xfail** — 같은 파일 끝에 별도 컨테이너로 둔다.

```go
// mixed-2engine only: `dmg storage query usage` panics daos_server 2.8 when a node mixes nvme and
// kdev bdev lists (2026-09-15). Pinned as a known bug; the system must still come back afterwards.
var _ = Describe("Tier 2 mixed", Label("tier2", "mixed"), func() {
	It("runs dmg storage query usage on mixed bdev lists (known panic)", func() {
		admin := jsonpath("daossystem", sysName, ".spec.images.admin")
		apply(fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: e2e-usage}
spec:
  restartPolicy: Never
  hostNetwork: true
  nodeSelector: {daos.gluesys.com/role: storage}
  containers:
    - name: c
      image: %s
      command: [dmg, -j, -o, /etc/daos/daos_control.yml, storage, query, usage]
      volumeMounts: [{name: ctl, mountPath: /etc/daos}]
  volumes: [{name: ctl, configMap: {name: %s-control}}]`, admin, sysName))
		DeferCleanup(func() { _, _ = kubectlE("delete", "pod", "e2e-usage", "--wait=false") })
		Eventually(func() string { return jsonpath("pod", "e2e-usage", ".status.phase") }).WithTimeout(5 * time.Minute).
			Should(Or(Equal("Succeeded"), Equal("Failed")))
		expectKnownBug("dmg storage query usage panics daos_server on mixed bdev lists (2026-09-15)", func() {
			Consistently(func() string { return condStatus("ServersReady") }).WithTimeout(2 * time.Minute).Should(Equal("True"))
		})
		waitReadyStable(15 * time.Minute)
	})
})
```

컨트롤 ConfigMap 이름과 키는 실행 전에 확인한다: `kubectl -n daos-system get cm daos-control -o jsonpath='{.data}' | head -c 200`. 키가 `daos_control.yml` 이 아니면 `-o` 경로를 맞춘다.

- [ ] **Step 10: 빌드 확인**

Run: `GOFLAGS=-buildvcs=false go vet -tags e2e ./test/e2e/`
Expected: 출력 없음

- [ ] **Step 11: 클러스터 실행 (비어 있을 때)**

Run:
```bash
cd test/e2e && E2E_RESET=1 E2E_PROFILE=kdev-3rank E2E_RANKS=3 E2E_TIMINGS=/tmp/t2.csv \
  go test -tags e2e . -count=1 -timeout 180m -v -args -ginkgo.v -ginkgo.label-filter='tier2 && resilience'
cat /tmp/t2.csv
```
Expected: `7 Passed`, CSV 에 행 10개 이상. 실패는 Task 3 과 같이 설계 13절에 기록한 뒤 고친다.

- [ ] **Step 12: 커밋**

```bash
git add test/e2e/tier2_test.go
git commit -m "test(e2e): Tier 2 복원력 케이스(rank 재편입, MS 정족수·리더, CSI 재시작, 재부팅, drain, scale-out)와 mixed xfail"
```

---

### Task 6: 야간 스케줄 잡

**Files:**
- Modify: `.gitlab-ci.yml` (e2e-regression 잡 아래에 추가)

**Interfaces:**
- Consumes: Task 1~5 전부, 기존 `.daos-k8s` 앵커(before_script 에 `E2E_SET`), `e2e-build`·`image-*` 잡
- Produces: 스케줄 변수 `TIER=2` 로만 도는 잡 3개, 아티팩트 `dist/tier2-*.csv`, JUnit

- [ ] **Step 1: 잡 추가**

```yaml
# Tier 2 (doc/ci-design-2026-10-02.md 7절): nightly schedule with TIER=2. Reports only (8절), never
# blocks a merge. Each profile repeats Tier 1 cases 1-3 and 7; kdev-3rank also runs the resilience cases.
.e2e-tier2: &e2e-tier2
  <<: *daos-k8s
  needs: [e2e-build, image-operator, image-hostprep]
  timeout: 4h
  allow_failure: true
  artifacts:
    when: always
    paths: [dist/tier2-*.csv]
    reports:
      junit: dist/e2e-tier2-*.xml
    expire_in: 30 days
  rules:
    - if: $CI_PIPELINE_SOURCE == "schedule" && $TIER == "2"

e2e-tier2-kdev:
  <<: *e2e-tier2
  script:
    - cd test/e2e
    - E2E_RESET=1 E2E_PROFILE=kdev-3rank E2E_RANKS=3 KUBECTL_DAOS=$CI_PROJECT_DIR/dist/kubectl-daos ../../dist/e2e.test -test.timeout 80m -ginkgo.v -ginkgo.focus='case [1237]\)' -ginkgo.label-filter='!upgrade && !regression && !tier2' -ginkgo.junit-report=$CI_PROJECT_DIR/dist/e2e-tier2-kdev-t1.xml
    - E2E_RESET=1 E2E_PROFILE=kdev-3rank E2E_RANKS=3 E2E_TIMINGS=$CI_PROJECT_DIR/dist/tier2-kdev.csv KUBECTL_DAOS=$CI_PROJECT_DIR/dist/kubectl-daos ../../dist/e2e.test -test.timeout 150m -ginkgo.v -ginkgo.label-filter='tier2 && resilience' -ginkgo.junit-report=$CI_PROJECT_DIR/dist/e2e-tier2-kdev-res.xml

e2e-tier2-mixed:
  <<: *e2e-tier2
  script:
    - cd test/e2e
    - E2E_RESET=1 E2E_PROFILE=mixed-2engine E2E_RANKS=6 KUBECTL_DAOS=$CI_PROJECT_DIR/dist/kubectl-daos ../../dist/e2e.test -test.timeout 80m -ginkgo.v -ginkgo.focus='case [1237]\)' -ginkgo.label-filter='!upgrade && !regression && !tier2' -ginkgo.junit-report=$CI_PROJECT_DIR/dist/e2e-tier2-mixed-t1.xml
    - E2E_RESET=1 E2E_PROFILE=mixed-2engine E2E_RANKS=6 ../../dist/e2e.test -test.timeout 40m -ginkgo.v -ginkgo.label-filter='tier2 && mixed' -ginkgo.junit-report=$CI_PROJECT_DIR/dist/e2e-tier2-mixed-xfail.xml

e2e-tier2-tiers:
  <<: *e2e-tier2
  script:
    - cd test/e2e
    - E2E_RESET=1 E2E_PROFILE=tiers E2E_RANKS=3 KUBECTL_DAOS=$CI_PROJECT_DIR/dist/kubectl-daos ../../dist/e2e.test -test.timeout 80m -ginkgo.v -ginkgo.focus='case [1237]\)' -ginkgo.label-filter='!upgrade && !regression && !tier2' -ginkgo.junit-report=$CI_PROJECT_DIR/dist/e2e-tier2-tiers-t1.xml
```

스케줄 파이프라인에서는 MR·main 용 e2e 잡이 돌지 않아야 한다. e2e-tier1·e2e-upgrade·e2e-regression 의 `rules` 맨 앞에 `- if: $CI_PIPELINE_SOURCE == "schedule"` + `when: never` 를 넣는다.

`e2e-build`·`image-operator`·`image-hostprep` 의 `rules` 에는 `- if: $CI_PIPELINE_SOURCE == "schedule"` 을 추가한다(스케줄 파이프라인에서도 빌드돼야 needs 가 성립한다).

- [ ] **Step 2: CI lint**

Run: GitLab CI lint API 에 `.gitlab-ci.yml` 을 보낸다(`POST /projects/820/ci/lint`).
Expected: `valid: true`

- [ ] **Step 3: 커밋**

```bash
git add .gitlab-ci.yml
git commit -m "ci: Tier 2 야간 스케줄 잡(kdev-3rank·mixed-2engine·tiers), 스케줄에서 Tier 1 잡 제외"
```

- [ ] **Step 4: MR 머지 후 스케줄 등록** (사용자 승인 후)

GitLab API `POST /projects/820/pipeline_schedules` — `description: "Tier 2 nightly"`, `ref: main`, `cron: "0 1 * * *"`, `cron_timezone: "Asia/Seoul"`, 이어서 `POST .../pipeline_schedules/<id>/variables` 로 `TIER=2`. 첫 실행은 `POST .../pipeline_schedules/<id>/play` 로 수동 시작해 결과를 확인한다.

---

### Task 7: 설계 문서 갱신

**Files:**
- Modify: `doc/ci-design-2026-10-02.md` (10절 표의 D 행, 13절에 Tier 2 가 찾은 결함)

- [ ] **Step 1:** 10절 D 행 작업 칸 앞에 `(진행: !<MR>)` 를 넣고, Task 3·5 실행에서 나온 결함을 13절 표에 행으로 추가한다(재현 조건·잡 번호·케이스).
- [ ] **Step 2: 커밋**

```bash
git add doc/ci-design-2026-10-02.md
git commit -m "docs(ci): Phase D 진행 표기와 Tier 2 발견 결함"
```

---

## 자체 점검

- 설계 7절 Tier 2 항목 대응: 1~3·7 반복 → Task 3·6, rank 재편입 → Task 5 Step 2, MS 정족수 → Step 3, MS 리더 kill + 데이터 경로 xfail → Step 4, 노드 재부팅 → Step 6, drain/uncordon → Step 7, CSI 재시작 → Step 5, 4A scale-out → Step 8, mixed 패닉 xfail → Step 9, JUnit + 소요 시간 CSV → Task 1·6.
- 전제 조건: 레지스트리 rate limit(레드마인 #13798)이 풀려야 클러스터 실행(Task 3, Task 5 Step 11)과 스케줄 잡이 의미가 있다. 코드 작성(Task 1·2·4·5·6 의 커밋 단계)은 그와 무관하다.
