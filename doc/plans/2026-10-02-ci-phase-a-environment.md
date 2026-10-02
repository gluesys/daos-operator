<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright 2026 Gluesys Co., Ltd. -->

# DAOS K8s CI Phase A (환경 구축) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** ExaCI5 슬롯 3·4 의 VM 6대를 설계 문서([`../ci-design-2026-10-02.md`](../ci-design-2026-10-02.md)) 1~4절대로 바꿔, `k8s-ready` 스냅샷 롤백으로 3분 안에 깨끗한 K8s 클러스터가 나오고, 그 위에서 SPDK `nvme` 클래스 DaosSystem 이 뜨는지(A-3) 판정하며, 3J 러너에서 빈 GitLab 파이프라인이 롤백까지 end-to-end 로 도는 상태를 만든다.

**Architecture:** 모든 작업은 `hack/ci/*.sh` 셸 스크립트로 저장소에 남긴다. 스크립트는 개발 PC 또는 3J 러너에서 실행되며, Proxmox 조작은 `ssh vdi5 "sudo qm …"`, 노드 조작은 `ssh root@<node>` 로 한다. 상태는 스냅샷이 들고 있으므로 스크립트는 멱등일 필요가 없고 "실패하면 멈춘다"(`set -euo pipefail`)가 원칙이다. 검증은 각 Task 의 `check-*.sh` 또는 명시된 명령의 출력으로 한다.

**Tech Stack:** Proxmox VE 8.0 `qm`, Rocky 8.10, kubeadm/kubelet/kubectl v1.31.14, CRI-O 1.31, flannel, Helm 3, daos-operator chart(`charts/daos-operator`), GitLab Runner(shell executor).

## Global Constraints

- VMID: 3J=108(러너, **롤백 안 함**), 4J=111(컨트롤 플레인), 3A=109, 3B=110, 4A=112(GPU·클라이언트), 4B=113. 클러스터 = 111 109 110 112 113.
- mgmt IP(ens18, /18): 3J 192.168.35.30, 4J .40, 3A .31, 3B .32, 4A .41, 4B .42. 보조망 ens19: 10.10.35.<같은 끝자리>/24. IB `ib0`: 10.10.36.<같은 끝자리>/24.
- 노드 이름: `exaci5-3j exaci5-4j exaci5-3a exaci5-3b exaci5-4a exaci5-4b`(소문자, `_` 없음).
- 자원: 워커 코어 12 / 32768 MB, 4J 8 / 8192 MB, 3J 4 / 8192 MB. VM 메모리는 반드시 `hugepages: 2` 설정을 유지한다(호스트 hugepage 풀 안에서만 증설).
- 워커 디스크: `nvme0`·`nvme1`·`scsi1`·`scsi2` 각 `local-SSD:100`(ZFS zvol 100 GB). `remote-NVMe` 위의 디스크는 전부 분리한다(파일은 지우지 않는다. B 노드와 공유 파일이다).
- GPU: `qm set 112 --hostpci2 0000:b1:00.0` 만. `31:00.0` 은 건드리지 않는다.
- K8s v1.31.14, CRI-O 1.31, cgroup 드라이버 **cgroupfs**(EL8 cgroup v1), `crun`, flannel `--iface=ens18`, pod CIDR 10.244.0.0/16.
- 노드 설정: swap off, SELinux permissive, firewalld off, THP never, `vm.nr_hugepages=4096`(워커), `uio_pci_generic` 로드.
- 비밀은 스크립트에 쓰지 않는다. 초기 root 암호는 환경변수 `CI_SSH_PASS`(값은 `jenkins_lib/t/CI/env/ExaCI5-3.src` 의 `SSHPASS`), 레지스트리 자격증명은 `REG_USER`/`REG_TOKEN`, GitLab 변수는 masked file 타입.
- 새 파일은 SPDX 헤더 필수(`# SPDX-License-Identifier: Apache-2.0`, `# Copyright 2026 Gluesys Co., Ltd.`). 커밋 메시지는 한글.
- `main` 브랜치에서 직접 작업하지 않는다. 브랜치 `ci/phase-a-environment`.
- `git push` 는 사용자가 명시적으로 요청할 때만.

---

### Task 0: 브랜치와 디렉터리

**Files:**
- Create: `hack/ci/` (디렉터리)

- [ ] **Step 1: 브랜치 생성**

```bash
cd ~/src/Flexa/daos-operator && git checkout -b ci/phase-a-environment main && mkdir -p hack/ci && git status --short
```
Expected: 출력 없음(깨끗), 브랜치 `ci/phase-a-environment`.

---

### Task 1: 공통 환경 파일 `hack/ci/env.sh`

**Files:**
- Create: `hack/ci/env.sh`
- Create: `hack/ci/check-env.sh`

**Interfaces:**
- Produces: 셸 변수 `VDI_SSH`, `CLUSTER_VMIDS`, `WORKER_VMIDS`, `CP_VMID`, `RUNNER_VMID`, 연관배열 `VM_NAME[vmid]`, `VM_IP[vmid]`, `VM_IP2[vmid]`(ens19), `VM_IBIP[vmid]`, 함수 `qm()`(vdi5 에서 `sudo qm "$@"`), `node_ssh <ip> <cmd…>`, `wait_ssh <ip> [sec]`. 이후 모든 스크립트가 `source "$(dirname "$0")/env.sh"` 로 쓴다.

- [ ] **Step 1: 검사 스크립트를 먼저 쓴다**

```bash
cat > hack/ci/check-env.sh <<'EOF'
#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
# env.sh 가 정의하는 것들이 설계 문서 1절과 일치하는지 확인한다.
set -euo pipefail
source "$(dirname "$0")/env.sh"
[ "$CP_VMID" = 111 ] && [ "$RUNNER_VMID" = 108 ]
[ "${CLUSTER_VMIDS[*]}" = "111 109 110 112 113" ]
[ "${WORKER_VMIDS[*]}" = "109 110 112 113" ]
[ "${VM_IP[112]}" = 192.168.35.41 ] && [ "${VM_IP2[112]}" = 10.10.35.41 ] && [ "${VM_IBIP[112]}" = 10.10.36.41 ]
[ "${VM_NAME[113]}" = exaci5-4b ]
qm list >/dev/null          # vdi5 에 sudo qm 이 된다
echo "check-env: OK"
EOF
chmod +x hack/ci/check-env.sh
```

- [ ] **Step 2: 실패 확인**

Run: `hack/ci/check-env.sh`
Expected: `env.sh: No such file or directory` 로 종료 코드 1.

- [ ] **Step 3: env.sh 작성**

```bash
cat > hack/ci/env.sh <<'EOF'
#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# DAOS K8s CI 환경 상수. 설계: doc/ci-design-2026-10-02.md 1절.
# 개발 PC 에서는 `ssh vdi5`(root) 가, 3J 러너에서는 jenkins-ci 계정이 Proxmox 에 닿는다.
# VDI_SSH 를 바꾸면 두 경우를 모두 덮는다.
VDI_SSH=${VDI_SSH:-"ssh -o BatchMode=yes -o ConnectTimeout=10 vdi5"}

CP_VMID=111
RUNNER_VMID=108
CLUSTER_VMIDS=(111 109 110 112 113)
WORKER_VMIDS=(109 110 112 113)

declare -A VM_NAME=([108]=exaci5-3j [111]=exaci5-4j [109]=exaci5-3a [110]=exaci5-3b [112]=exaci5-4a [113]=exaci5-4b)
declare -A VM_IP=([108]=192.168.35.30 [111]=192.168.35.40 [109]=192.168.35.31 [110]=192.168.35.32 [112]=192.168.35.41 [113]=192.168.35.42)
declare -A VM_IP2 VM_IBIP
for v in "${!VM_IP[@]}"; do
    last=${VM_IP[$v]##*.}
    VM_IP2[$v]=10.10.35.$last
    VM_IBIP[$v]=10.10.36.$last
done

SNAP=${SNAP:-k8s-ready}
SSH_OPTS=(-o BatchMode=yes -o StrictHostKeyChecking=no -o ConnectTimeout=10)

qm() { $VDI_SSH "export LC_ALL=C; sudo qm $*"; }
node_ssh() { local ip=$1; shift; ssh "${SSH_OPTS[@]}" "root@$ip" "$@"; }
wait_ssh() {
    local ip=$1 limit=${2:-300} t=0
    until ssh "${SSH_OPTS[@]}" -o ConnectTimeout=3 "root@$ip" true 2>/dev/null; do
        sleep 5; t=$((t+5)); [ $t -ge $limit ] && { echo "wait_ssh $ip: timeout" >&2; return 1; }
    done
}
EOF
```

- [ ] **Step 4: 통과 확인**

Run: `bash -n hack/ci/env.sh && hack/ci/check-env.sh`
Expected: `check-env: OK`

- [ ] **Step 5: 커밋**

```bash
git add hack/ci/env.sh hack/ci/check-env.sh
git commit -m "ci(env): 슬롯 3·4 VM 상수와 Proxmox/노드 접속 헬퍼를 둔다"
```

---

### Task 2: VM 재구성 `hack/ci/vm-reshape.sh`

**Files:**
- Create: `hack/ci/vm-reshape.sh`
- Create: `hack/ci/check-vm-shape.sh`

**Interfaces:**
- Consumes: `env.sh` 의 `qm`, `CLUSTER_VMIDS`, `WORKER_VMIDS`, `RUNNER_VMID`.
- Produces: 6대가 `init` 상태 + 설계 2절 자원·디스크·GPU 로 **정지** 상태. (기동은 Task 3.)

주의: 이 Task 는 **되돌릴 수 없다.** 4J 의 기존 스냅샷 체인(`a2-driver-ready`~`lustre-nfsrdma-gds`)은 `init` 롤백 후 그대로 남지만 사용하지 않는다(사용자 결정 2026-10-02). `qm rollback` 은 실행 중인 VM 에서 실패하므로 먼저 정지한다.

- [ ] **Step 1: 검사 스크립트**

```bash
cat > hack/ci/check-vm-shape.sh <<'EOF'
#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
# 6대의 qm config 가 설계 2절과 일치하는지 본다. 어긋나면 그 VM 과 항목을 찍고 1 로 끝난다.
set -euo pipefail
source "$(dirname "$0")/env.sh"
fail=0
chk() { local v=$1 key=$2 want=$3 got; got=$(qm config "$v" | awk -F': ' -v k="$key" '$1==k{print $2}'); [ "$got" = "$want" ] || { echo "VM $v $key: got '$got' want '$want'"; fail=1; }; }
for v in "${WORKER_VMIDS[@]}"; do
    chk "$v" cores 12; chk "$v" memory 32768; chk "$v" hugepages 2
    for d in nvme0 nvme1 scsi1 scsi2; do
        qm config "$v" | grep -qE "^$d: local-SSD:vm-$v-disk-[0-9]+,.*size=100G" || { echo "VM $v $d: not a 100G local-SSD zvol"; fail=1; }
    done
    qm config "$v" | grep -q 'remote-NVMe' && { echo "VM $v: remote-NVMe disk still attached"; fail=1; }
done
chk 112 hostpci2 0000:b1:00.0
chk 111 cores 8;  chk 111 memory 8192
chk 108 cores 4;  chk 108 memory 8192
[ $fail = 0 ] && echo "check-vm-shape: OK"
exit $fail
EOF
chmod +x hack/ci/check-vm-shape.sh
```

- [ ] **Step 2: 실패 확인**

Run: `hack/ci/check-vm-shape.sh; echo "exit=$?"`
Expected: `VM 109 cores: got '8' want '12'` 등 여러 줄과 `exit=1`.

- [ ] **Step 3: vm-reshape.sh 작성**

```bash
cat > hack/ci/vm-reshape.sh <<'EOF'
#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# 슬롯 3·4 VM 6대를 설계 2절 모양으로 만든다(정지 상태로 끝난다).
#   1) 정지 → `init` 롤백   2) remote-NVMe 디스크 전부 분리   3) 워커에 zvol 4개
#   4) 코어·메모리   5) 4A 에 A2 GPU
# 되돌릴 수 없다. 한 번 실행한다. 이후의 리셋은 rollback.sh(k8s-ready) 가 맡는다.
set -euo pipefail
source "$(dirname "$0")/env.sh"

ALL=("$RUNNER_VMID" "${CLUSTER_VMIDS[@]}")

echo "== stop + rollback init"
for v in "${ALL[@]}"; do
    qm stop "$v" --timeout 90 || true
    qm rollback "$v" init
done

echo "== detach every disk that is not the root qcow2"
for v in "${ALL[@]}"; do
    for key in $(qm config "$v" | awk -F: '/^(scsi|nvme|virtio|sata|ide)[0-9]+:/ && !/qcow2/ && !/cdrom/ {print $1}'); do
        qm set "$v" --delete "$key"
    done
    for key in $(qm config "$v" | awk -F: '/^unused[0-9]+:/ {print $1}'); do
        qm set "$v" --delete "$key"      # 참조만 지운다. 파일은 남는다(B 노드와 공유).
    done
done

echo "== workers: zvols, cpu, memory"
for v in "${WORKER_VMIDS[@]}"; do
    qm set "$v" --nvme0 local-SSD:100 --nvme1 local-SSD:100 --scsi1 local-SSD:100 --scsi2 local-SSD:100
    qm set "$v" --cores 12 --memory 32768 --hugepages 2 --balloon 0
done
echo "== control plane / runner"
qm set "$CP_VMID" --cores 8 --memory 8192 --hugepages 2 --balloon 0
qm set "$RUNNER_VMID" --cores 4 --memory 8192 --hugepages 2 --balloon 0
echo "== GPU on 4A"
qm set 112 --hostpci2 0000:b1:00.0
echo "vm-reshape: done (VMs stopped)"
EOF
chmod +x hack/ci/vm-reshape.sh
```

- [ ] **Step 4: 실행 전 마지막 확인**

Run: `hack/ci/check-env.sh && qm list | awk '$1>=108 && $1<=113'` (env.sh 를 source 한 셸에서)
Expected: 6대 모두 나열. 다른 사람이 쓰고 있지 않은지(2026-10-02 확인: 접속자 없음) 사용자에게 한 번 더 알린다.

- [ ] **Step 5: 실행**

Run: `hack/ci/vm-reshape.sh 2>&1 | tee /tmp/vm-reshape.log`
Expected: 마지막 줄 `vm-reshape: done (VMs stopped)`. 오류가 나면 그 VM 에서 멈춘다. `qm rollback` 이 "VM is running" 이면 `qm stop --timeout` 이 안 먹은 것이므로 `qm stop <vmid> --skiplock` 후 재실행.

- [ ] **Step 6: 통과 확인**

Run: `hack/ci/check-vm-shape.sh`
Expected: `check-vm-shape: OK`

- [ ] **Step 7: 커밋**

```bash
git add hack/ci/vm-reshape.sh hack/ci/check-vm-shape.sh
git commit -m "ci(vm): 슬롯 3·4 VM 을 init 으로 되돌리고 zvol·자원·GPU 를 설계대로 맞춘다"
```

---

### Task 3: 노드 준비 `hack/ci/node-prep.sh`

**Files:**
- Create: `hack/ci/node-prep.sh` (각 노드 안에서 실행되는 스크립트)
- Create: `hack/ci/prep-all.sh` (6대 기동 → 키 배포 → node-prep 실행)
- Create: `hack/ci/check-node.sh`

**Interfaces:**
- Consumes: `env.sh`, 환경변수 `CI_SSH_PASS`(최초 1회 키 배포에만), `~/.ssh/id_rsa.pub`.
- Produces: 6대가 기동 상태, root 키 로그인, 호스트명·IP·커널 설정·CRI-O·kubeadm 설치 완료. 함수 없이 `node-prep.sh <role>`(`role` = `worker|cp|runner`).

- [ ] **Step 1: 검사 스크립트**

```bash
cat > hack/ci/check-node.sh <<'EOF'
#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
# 노드 하나가 설계 3절 "호스트 설정" 을 만족하는지. 사용: check-node.sh <vmid>
set -euo pipefail
source "$(dirname "$0")/env.sh"
v=$1; ip=${VM_IP[$v]}; fail=0
t() { local desc=$1; shift; if node_ssh "$ip" "$@" >/dev/null 2>&1; then :; else echo "VM $v: FAIL $desc"; fail=1; fi; }
t hostname         "[ \$(hostname) = ${VM_NAME[$v]} ]"
t ens19            "ip -4 addr show ens19 | grep -q ${VM_IP2[$v]}/24"
t swap             "[ \$(swapon --noheadings | wc -l) = 0 ]"
t selinux          "[ \$(getenforce) = Permissive ]"
t firewalld        "! systemctl is-active firewalld"
t thp              "grep -q '\[never\]' /sys/kernel/mm/transparent_hugepage/enabled"
t crio             "systemctl is-active crio && crictl version"
t kubeadm          "kubeadm version -o short | grep -q v1.31.14"
t cgroupfs         "grep -q 'cgroup_manager = \"cgroupfs\"' /etc/crio/crio.conf.d/10-cgroupfs.conf"
t crun             "command -v crun"
if [[ " ${WORKER_VMIDS[*]} " == *" $v "* ]]; then
    t hugepages    "[ \$(awk '/HugePages_Total/{print \$2}' /proc/meminfo) -ge 4096 ]"
    t uio          "lsmod | grep -q uio_pci_generic"
    t nvme2        "[ \$(ls /dev/nvme?n1 | wc -l) = 2 ]"
    t kdev2        "[ -b /dev/sdb ] && [ -b /dev/sdc ]"
    t ib0          "ip -4 addr show ib0 | grep -q ${VM_IBIP[$v]}/24"
fi
[ $fail = 0 ] && echo "check-node $v: OK"
exit $fail
EOF
chmod +x hack/ci/check-node.sh
```

- [ ] **Step 2: 실패 확인(기동 후 키 없음 상태)**

Run: `hack/ci/check-node.sh 109; echo "exit=$?"`
Expected: VM 이 정지 상태라 모든 항목 FAIL, `exit=1`.

- [ ] **Step 3: node-prep.sh 작성**

```bash
cat > hack/ci/node-prep.sh <<'EOF'
#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# 노드 안에서 실행. 사용: node-prep.sh <worker|cp|runner> <hostname> <ens19-ip> <ib0-ip>
# 설계 3절 + testbed-k8s-exaci4-2.md 의 EL8 특이사항(conntrack, cgroupfs, crun).
set -euo pipefail
role=$1; name=$2; ip2=$3; ibip=$4
K8S_VER=1.31.14

hostnamectl set-hostname "$name"
nmcli -t -f NAME con show | grep -qx ens19 || nmcli con add type ethernet ifname ens19 con-name ens19 ipv4.method manual ipv4.addresses "$ip2/24" autoconnect yes
nmcli con up ens19 >/dev/null

swapoff -a; sed -i '/\sswap\s/d' /etc/fstab
setenforce 0 || true; sed -i 's/^SELINUX=enforcing/SELINUX=permissive/' /etc/selinux/config
systemctl disable --now firewalld || true

# THP never (즉시 + 영구)
echo never > /sys/kernel/mm/transparent_hugepage/enabled
grubby --update-kernel=ALL --args="transparent_hugepage=never"

cat > /etc/modules-load.d/k8s.conf <<M
overlay
br_netfilter
M
modprobe overlay; modprobe br_netfilter
cat > /etc/sysctl.d/99-k8s.conf <<S
net.bridge.bridge-nf-call-iptables  = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward                 = 1
S
sysctl --system >/dev/null

if [ "$role" = worker ]; then
    echo uio_pci_generic > /etc/modules-load.d/uio.conf; modprobe uio_pci_generic
    echo "vm.nr_hugepages = 4096" > /etc/sysctl.d/90-hugepages.conf
    sysctl -w vm.nr_hugepages=4096 >/dev/null
    # IPoIB (verbs 프로파일용). VF 가 없는 VM 이면 건너뛴다.
    if ip link show ib0 >/dev/null 2>&1; then
        nmcli -t -f NAME con show | grep -qx ib0 || nmcli con add type infiniband ifname ib0 con-name ib0 ipv4.method manual ipv4.addresses "$ibip/24" autoconnect yes
        nmcli con up ib0 >/dev/null || true
    fi
fi

# 저장소: pkgs.k8s.io (kubeadm 1.31.x), CRI-O 1.31
cat > /etc/yum.repos.d/kubernetes.repo <<R
[kubernetes]
name=Kubernetes
baseurl=https://pkgs.k8s.io/core:/stable:/v1.31/rpm/
enabled=1
gpgcheck=1
gpgkey=https://pkgs.k8s.io/core:/stable:/v1.31/rpm/repodata/repomd.xml.key
exclude=kubelet kubeadm kubectl cri-tools kubernetes-cni
R
cat > /etc/yum.repos.d/cri-o.repo <<R
[cri-o]
name=CRI-O
baseurl=https://pkgs.k8s.io/addons:/cri-o:/stable:/v1.31/rpm/
enabled=1
gpgcheck=1
gpgkey=https://pkgs.k8s.io/addons:/cri-o:/stable:/v1.31/rpm/repodata/repomd.xml.key
R
dnf install -y -q conntrack-tools iproute-tc socat crun chrony
dnf install -y -q --disableexcludes=kubernetes "kubelet-$K8S_VER" "kubeadm-$K8S_VER" "kubectl-$K8S_VER" cri-o
cat > /etc/crio/crio.conf.d/10-cgroupfs.conf <<C
[crio.runtime]
cgroup_manager = "cgroupfs"
conmon_cgroup = "pod"
default_runtime = "crun"
C
systemctl enable --now crio chronyd
systemctl enable kubelet
echo "node-prep $role $name: done"
EOF
chmod +x hack/ci/node-prep.sh
```

- [ ] **Step 4: prep-all.sh 작성**

```bash
cat > hack/ci/prep-all.sh <<'EOF'
#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# 6대 기동 → root 공개키 배포(CI_SSH_PASS 로 1회) → node-prep.sh 를 각 노드에서 실행.
set -euo pipefail
source "$(dirname "$0")/env.sh"
: "${CI_SSH_PASS:?set CI_SSH_PASS (jenkins_lib t/CI/env/ExaCI5-3.src SSHPASS)}"
export SSHPASS=$CI_SSH_PASS
PUB=${PUB:-$HOME/.ssh/id_rsa.pub}
ALL=("$RUNNER_VMID" "${CLUSTER_VMIDS[@]}")

for v in "${ALL[@]}"; do qm start "$v"; done
for v in "${ALL[@]}"; do
    ip=${VM_IP[$v]}
    until sshpass -e ssh -o StrictHostKeyChecking=no -o ConnectTimeout=3 -o PreferredAuthentications=password "root@$ip" true 2>/dev/null; do sleep 5; done
    sshpass -e ssh-copy-id -o StrictHostKeyChecking=no -i "$PUB" "root@$ip" >/dev/null 2>&1
done

role_of() { case $1 in "$RUNNER_VMID") echo runner;; "$CP_VMID") echo cp;; *) echo worker;; esac; }
for v in "${ALL[@]}"; do
    ip=${VM_IP[$v]}
    scp "${SSH_OPTS[@]}" "$(dirname "$0")/node-prep.sh" "root@$ip:/root/node-prep.sh"
    node_ssh "$ip" "bash /root/node-prep.sh $(role_of "$v") ${VM_NAME[$v]} ${VM_IP2[$v]} ${VM_IBIP[$v]}" &
done
wait
echo "prep-all: done"
EOF
chmod +x hack/ci/prep-all.sh
```

- [ ] **Step 5: 실행**

Run: `CI_SSH_PASS="$(awk -F'"' '/^SSHPASS=/{print $2}' ~/src/Flexa/jenkins_lib/t/CI/env/ExaCI5-3.src)" hack/ci/prep-all.sh 2>&1 | tee /tmp/prep-all.log`
Expected: `node-prep worker exaci5-3a: done` 등 6줄과 `prep-all: done`. 5~10분. `cri-o` 패키지 버전은 로그에서 `cri-o-1.31.x` 로 확인한다(계획 기준 1.31.5, 더 높은 1.31.x 도 허용).

- [ ] **Step 6: 통과 확인**

Run: `for v in 108 111 109 110 112 113; do hack/ci/check-node.sh $v; done`
Expected: 6줄 모두 `check-node <vmid>: OK`. `ib0` 항목이 FAIL 이면 VF 가 서브넷 매니저에 안 붙은 것이다. vdi5 에서 `ibstat ibp75s0f0 | grep State` 가 `Active` 인지 보고, 게스트 `ibstat` 상태를 기록한 뒤 **Task 는 통과로 간주하고** 설계 문서 12절 위험에 추가한다(verbs 는 Tier 2 항목).

- [ ] **Step 7: 커밋**

```bash
git add hack/ci/node-prep.sh hack/ci/prep-all.sh hack/ci/check-node.sh
git commit -m "ci(node): 6대 노드 준비 스크립트 — EL8 kubeadm/CRI-O, THP, hugepages, uio, IPoIB"
```

---

### Task 4: 클러스터 초기화 `hack/ci/cluster-init.sh`

**Files:**
- Create: `hack/ci/cluster-init.sh`
- Create: `hack/ci/kubeadm-config.yaml`
- Create: `hack/ci/check-cluster.sh`
- Create: `hack/ci/images.txt`

**Interfaces:**
- Consumes: `env.sh`, Task 3 결과, `REG_USER`/`REG_TOKEN`(GitLab 레지스트리 read_registry).
- Produces: 4J 컨트롤 플레인, 워커 4대 join, 라벨, `daos-system` 네임스페이스와 `gitlab-registry` 풀 시크릿, 노드별 이미지 사전 pull. `KUBECONFIG` 파일이 개발 PC `~/.kube/daos-ci.conf` 에 복사됨.

- [ ] **Step 1: 검사 스크립트**

```bash
cat > hack/ci/check-cluster.sh <<'EOF'
#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
# 클러스터가 설계 1·3절 모양인지. KUBECONFIG 필요.
set -euo pipefail
source "$(dirname "$0")/env.sh"
: "${KUBECONFIG:?}"
fail=0
ready=$(kubectl get nodes --no-headers | awk '$2=="Ready"' | wc -l)
[ "$ready" = 5 ] || { echo "ready nodes: $ready (want 5)"; fail=1; }
for n in exaci5-3a exaci5-3b exaci5-4b; do
    kubectl get node "$n" -o jsonpath='{.metadata.labels.daos\.gluesys\.com/role}' | grep -qx storage || { echo "$n: role=storage label missing"; fail=1; }
done
kubectl get node exaci5-4a -o jsonpath='{.metadata.labels.daos\.gluesys\.com/client}' | grep -qx true || { echo "exaci5-4a: client=true missing"; fail=1; }
kubectl get node exaci5-4a -o jsonpath='{.metadata.labels.daos\.gluesys\.com/gpu}' | grep -qx true || { echo "exaci5-4a: gpu=true missing"; fail=1; }
kubectl -n kube-flannel get ds kube-flannel-ds -o jsonpath='{.status.numberReady}' | grep -qx 5 || { echo "flannel not ready on 5"; fail=1; }
kubectl -n daos-system get secret gitlab-registry >/dev/null || { echo "pull secret missing"; fail=1; }
# 사전 pull: 워커마다 images.txt 의 모든 이미지가 있어야 한다
while read -r img; do
    [ -z "$img" ] && continue
    for v in "${WORKER_VMIDS[@]}"; do
        node_ssh "${VM_IP[$v]}" "crictl images -q $img | grep -q ." || { echo "${VM_NAME[$v]}: missing $img"; fail=1; }
    done
done < "$(dirname "$0")/images.txt"
[ $fail = 0 ] && echo "check-cluster: OK"
exit $fail
EOF
chmod +x hack/ci/check-cluster.sh
```

- [ ] **Step 2: 실패 확인**

Run: `KUBECONFIG=/nonexistent hack/ci/check-cluster.sh; echo "exit=$?"`
Expected: kubectl 연결 오류, `exit=1`.

- [ ] **Step 3: kubeadm 설정과 이미지 목록**

```bash
cat > hack/ci/kubeadm-config.yaml <<'EOF'
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
localAPIEndpoint:
  advertiseAddress: 192.168.35.40
nodeRegistration:
  name: exaci5-4j
  criSocket: unix:///var/run/crio/crio.sock
---
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
kubernetesVersion: v1.31.14
networking:
  podSubnet: 10.244.0.0/16
---
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
cgroupDriver: cgroupfs
EOF
cat > hack/ci/images.txt <<'EOF'
registry.gitlab.gluesys.com/exastor/daos-operator/daos-operator:latest
registry.gitlab.gluesys.com/exastor/daos-operator/daos-hostprep:latest
registry.gitlab.gluesys.com/exastor/daos-csi/daos-csi:latest
registry.gitlab.gluesys.com/exastor/daos-images/daos-server:2.8.0-20260923
registry.gitlab.gluesys.com/exastor/daos-images/daos-agent:2.8.0-20260923
registry.gitlab.gluesys.com/exastor/daos-images/daos-admin:2.8.0-20260923
registry.gitlab.gluesys.com/exastor/daos-images/daos-client:2.8.0-20260923
registry.gitlab.gluesys.com/exastor/daos-images/versitygw-daos:2.8.0-20260923
EOF
```
`images.txt` 의 태그가 레지스트리에 실제로 있는지 먼저 확인한다:
Run: `for i in $(cat hack/ci/images.txt); do podman manifest inspect --authfile ~/.docker/config.json docker://$i >/dev/null 2>&1 && echo "ok $i" || echo "MISSING $i"; done`
Expected: 8줄 모두 `ok`. `MISSING` 이면 해당 저장소 레지스트리에서 최신 태그로 바꾼다(`daos-images` 는 `2.8.0-YYYYMMDD` 중 최신).

- [ ] **Step 4: cluster-init.sh 작성**

```bash
cat > hack/ci/cluster-init.sh <<'EOF'
#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# kubeadm init(4J) → flannel(ens18) → 워커 join → 라벨 → 네임스페이스/풀 시크릿 → 이미지 사전 pull.
# 설계 3절. 한 번 실행하고 snapshot.sh 로 k8s-ready 를 찍는다.
set -euo pipefail
source "$(dirname "$0")/env.sh"
: "${REG_USER:?}" "${REG_TOKEN:?}"
here=$(dirname "$0")
cp=${VM_IP[$CP_VMID]}
FLANNEL=https://github.com/flannel-io/flannel/releases/latest/download/kube-flannel.yml

scp "${SSH_OPTS[@]}" "$here/kubeadm-config.yaml" "root@$cp:/root/kubeadm-config.yaml"
node_ssh "$cp" "kubeadm init --config /root/kubeadm-config.yaml --upload-certs | tail -20"
node_ssh "$cp" "mkdir -p /root/.kube && cp /etc/kubernetes/admin.conf /root/.kube/config"
# flannel 은 ens18 로 고정한다(testbed-k8s-exaci4-2). CNI 를 CRI-O 가 늦게 집으면 crio 재시작.
node_ssh "$cp" "curl -fsSL $FLANNEL | sed 's#- --kube-subnet-mgr#- --kube-subnet-mgr\n        - --iface=ens18#' | kubectl apply -f -"
node_ssh "$cp" "sleep 20; systemctl restart crio"

join=$(node_ssh "$cp" "kubeadm token create --print-join-command")
for v in "${WORKER_VMIDS[@]}"; do
    node_ssh "${VM_IP[$v]}" "$join --node-name ${VM_NAME[$v]} --cri-socket unix:///var/run/crio/crio.sock" &
done
wait
mkdir -p ~/.kube
scp "${SSH_OPTS[@]}" "root@$cp:/etc/kubernetes/admin.conf" ~/.kube/daos-ci.conf
export KUBECONFIG=~/.kube/daos-ci.conf
for v in "${WORKER_VMIDS[@]}"; do kubectl wait --for=condition=Ready "node/${VM_NAME[$v]}" --timeout=300s; done

kubectl label node exaci5-3a exaci5-3b exaci5-4b daos.gluesys.com/role=storage --overwrite
kubectl label node exaci5-4a daos.gluesys.com/client=true daos.gluesys.com/gpu=true --overwrite
kubectl create namespace daos-system --dry-run=client -o yaml | kubectl apply -f -
kubectl -n daos-system create secret docker-registry gitlab-registry \
    --docker-server=registry.gitlab.gluesys.com --docker-username="$REG_USER" --docker-password="$REG_TOKEN" \
    --dry-run=client -o yaml | kubectl apply -f -
kubectl -n daos-system patch sa default -p '{"imagePullSecrets":[{"name":"gitlab-registry"}]}'

# 사전 pull: 스냅샷에 들어가므로 매 실행의 pull 시간이 사라진다.
for v in "${WORKER_VMIDS[@]}"; do
    node_ssh "${VM_IP[$v]}" "while read -r img; do [ -n \"\$img\" ] && crictl pull --creds '$REG_USER:$REG_TOKEN' \"\$img\" >/dev/null && echo \"${VM_NAME[$v]} pulled \$img\"; done" < "$here/images.txt" &
done
wait
echo "cluster-init: done (KUBECONFIG=~/.kube/daos-ci.conf)"
EOF
chmod +x hack/ci/cluster-init.sh
```

- [ ] **Step 5: 실행**

Run: `REG_USER=<gitlab user> REG_TOKEN=<read_registry token> hack/ci/cluster-init.sh 2>&1 | tee /tmp/cluster-init.log`
Expected: `cluster-init: done`. 5~10분. `kubeadm init` 이 preflight 에서 멈추면 그 메시지대로 패키지(`conntrack`, `tc`)를 보완한다. 컨트롤 플레인이 `NotReady` 로 오래 머물면 `systemctl restart crio` 를 한 번 더 한다.

- [ ] **Step 6: 통과 확인**

Run: `KUBECONFIG=~/.kube/daos-ci.conf hack/ci/check-cluster.sh`
Expected: `check-cluster: OK`

- [ ] **Step 7: 커밋**

```bash
git add hack/ci/cluster-init.sh hack/ci/kubeadm-config.yaml hack/ci/images.txt hack/ci/check-cluster.sh
git commit -m "ci(cluster): kubeadm 1.31 + flannel + 워커 join + 라벨 + 이미지 사전 pull"
```

---

### Task 5: 스냅샷과 롤백 `hack/ci/snapshot.sh`, `hack/ci/rollback.sh`

**Files:**
- Create: `hack/ci/snapshot.sh`
- Create: `hack/ci/rollback.sh`
- Create: `hack/ci/check-rollback.sh`

**Interfaces:**
- Consumes: `env.sh`(`SNAP`, `CLUSTER_VMIDS`, `qm`), Task 4 의 클러스터.
- Produces: 스냅샷 `k8s-ready`(5대, 정지 상태 스냅샷). `rollback.sh` 는 인자 없이 실행하면 5대를 롤백·기동하고 노드 5 Ready 까지 기다린 뒤 걸린 초를 출력한다. 종료 코드 0 = 성공. 이후 모든 파이프라인의 첫 단계.

정지 상태 스냅샷을 쓰는 이유: 실행 중 스냅샷은 RAM 을 안 담으면 디스크가 크래시 상태가 되고, 담으면 32 GB ×4 를 저장해 느리다. 깨끗한 부팅이 3분 목표 안에 든다.

- [ ] **Step 1: 검사 스크립트**

```bash
cat > hack/ci/check-rollback.sh <<'EOF'
#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
# 롤백이 "깨끗한 클러스터를 3분 안에" 주는지. 더럽힌 뒤 rollback.sh 를 돌리고 흔적이 없어야 한다.
set -euo pipefail
source "$(dirname "$0")/env.sh"
export KUBECONFIG=${KUBECONFIG:-$HOME/.kube/daos-ci.conf}
here=$(dirname "$0")
for v in "${CLUSTER_VMIDS[@]}"; do qm listsnapshot "$v" | grep -q " $SNAP " || { echo "VM $v: snapshot $SNAP missing"; exit 1; }; done
kubectl create namespace rollback-probe --dry-run=client -o yaml | kubectl apply -f - >/dev/null
node_ssh "${VM_IP[109]}" "dd if=/dev/urandom of=/dev/nvme0n1 bs=1M count=1 oflag=direct status=none; touch /root/dirty"
start=$(date +%s)
"$here/rollback.sh"
took=$(( $(date +%s) - start ))
kubectl get namespace rollback-probe >/dev/null 2>&1 && { echo "namespace survived rollback"; exit 1; }
node_ssh "${VM_IP[109]}" "[ ! -e /root/dirty ]" || { echo "root fs survived rollback"; exit 1; }
node_ssh "${VM_IP[109]}" "cmp -n 1048576 /dev/nvme0n1 /dev/zero" || { echo "nvme0n1 not clean after rollback"; exit 1; }
[ "$took" -le 180 ] || { echo "rollback took ${took}s (> 180)"; exit 1; }
echo "check-rollback: OK (${took}s)"
EOF
chmod +x hack/ci/check-rollback.sh
```

- [ ] **Step 2: 실패 확인**

Run: `hack/ci/check-rollback.sh; echo "exit=$?"`
Expected: `VM 111: snapshot k8s-ready missing`, `exit=1`.

- [ ] **Step 3: snapshot.sh 와 rollback.sh 작성**

```bash
cat > hack/ci/snapshot.sh <<'EOF'
#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# 클러스터 5대를 깨끗이 내리고 정지 상태 스냅샷 $SNAP 을 찍은 뒤 다시 올린다.
# 같은 이름이 있으면 지우고 다시 찍는다(이미지 갱신 뒤 재스냅샷용).
set -euo pipefail
source "$(dirname "$0")/env.sh"
for v in "${CLUSTER_VMIDS[@]}"; do qm shutdown "$v" --timeout 120 || qm stop "$v"; done
for v in "${CLUSTER_VMIDS[@]}"; do
    qm listsnapshot "$v" | grep -q " $SNAP " && qm delsnapshot "$v" "$SNAP"
    qm snapshot "$v" "$SNAP" --description "DAOS K8s CI base $(date +%F)"
done
for v in "${CLUSTER_VMIDS[@]}"; do qm start "$v"; done
echo "snapshot: $SNAP taken on ${CLUSTER_VMIDS[*]}"
EOF
cat > hack/ci/rollback.sh <<'EOF'
#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# 매 파이프라인의 첫 단계. 클러스터 5대를 $SNAP 으로 되돌리고 5 노드 Ready 까지 기다린다.
# 러너 VM(3J)은 건드리지 않는다. 실패하면 0 이 아닌 코드로 끝나 파이프라인을 멈춘다.
set -euo pipefail
source "$(dirname "$0")/env.sh"
export KUBECONFIG=${KUBECONFIG:-$HOME/.kube/daos-ci.conf}
start=$(date +%s)
for v in "${CLUSTER_VMIDS[@]}"; do
    ( qm stop "$v" --timeout 60 >/dev/null 2>&1 || true
      qm rollback "$v" "$SNAP" >/dev/null
      qm start "$v" >/dev/null ) &
done
wait
wait_ssh "${VM_IP[$CP_VMID]}" 240
until [ "$(kubectl get nodes --no-headers 2>/dev/null | awk '$2=="Ready"' | wc -l)" = 5 ]; do
    [ $(( $(date +%s) - start )) -ge 300 ] && { echo "rollback: nodes not Ready in 300s"; kubectl get nodes || true; exit 1; }
    sleep 5
done
kubectl -n kube-system wait --for=condition=Ready pod --all --timeout=120s >/dev/null
echo "rollback: cluster ready in $(( $(date +%s) - start ))s"
EOF
chmod +x hack/ci/snapshot.sh hack/ci/rollback.sh
```

- [ ] **Step 4: 스냅샷 생성**

Run: `hack/ci/snapshot.sh`
Expected: `snapshot: k8s-ready taken on 111 109 110 112 113`. 이어서 `KUBECONFIG=~/.kube/daos-ci.conf hack/ci/check-cluster.sh` 가 다시 `OK`(재기동 후 클러스터 복귀 확인).

- [ ] **Step 5: 통과 확인**

Run: `hack/ci/check-rollback.sh`
Expected: `check-rollback: OK (<n>s)`, n ≤ 180. 180 을 넘으면 `qm start` 이후 어느 단계가 느린지 `rollback.sh` 에 타임스탬프를 넣어 재고, 설계 4절 목표(3분)를 실측치로 고친다.

- [ ] **Step 6: 커밋**

```bash
git add hack/ci/snapshot.sh hack/ci/rollback.sh hack/ci/check-rollback.sh
git commit -m "ci(reset): k8s-ready 정지 스냅샷과 5대 병렬 롤백 — 매 실행의 리셋 단계"
```

---

### Task 6: A-3 — SPDK `nvme` 클래스 검증 `hack/ci/verify-nvme.sh`

**Files:**
- Create: `test/e2e/profiles/nvme-1rank.yaml`
- Create: `hack/ci/verify-nvme.sh`
- Modify: `doc/ci-design-2026-10-02.md` (5절 결과 기록, 10절 A-3 행)

**Interfaces:**
- Consumes: Task 5 롤백, 차트 `charts/daos-operator`(values 키는 `system.spec.*` = `DaosSystemSpec` 필드).
- Produces: 판정 `NVME_CLASS_OK` 또는 `NVME_CLASS_FAIL` 과 근거 로그. 성공이면 Tier 1 프로파일이 `nvme-1rank` 로 확정된다. 실패이면 Plan C 는 `kdev-1rank` 로 쓴다.

- [ ] **Step 1: 프로파일 values**

```bash
mkdir -p test/e2e/profiles
cat > test/e2e/profiles/nvme-1rank.yaml <<'EOF'
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# Tier 1 프로파일: 워커 1대(exaci5-3a), SPDK nvme 클래스, QEMU NVMe + uio_pci_generic(vIOMMU 없음).
# 설계 doc/ci-design-2026-10-02.md 5절.
image:
  repository: registry.gitlab.gluesys.com/exastor/daos-operator/daos-operator
  tag: latest
imagePullSecrets:
  - name: gitlab-registry
hostprep:
  defaultImage: registry.gitlab.gluesys.com/exastor/daos-operator/daos-hostprep:latest
system:
  create: true
  name: daos
  spec:
    version: "2.8.0"
    namespace: daos-system
    images:
      server: registry.gitlab.gluesys.com/exastor/daos-images/daos-server:2.8.0-20260923
      agent:  registry.gitlab.gluesys.com/exastor/daos-images/daos-agent:2.8.0-20260923
      admin:  registry.gitlab.gluesys.com/exastor/daos-images/daos-admin:2.8.0-20260923
      client: registry.gitlab.gluesys.com/exastor/daos-images/daos-client:2.8.0-20260923
    placement: dedicated
    nodeSelector:
      kubernetes.io/hostname: exaci5-3a
    msReplicas: 1
    provider: "ofi+tcp"
    nrHugepages: 2048
    allowInsecure: true
    disableVFIO: true
    systemRamReservedGiB: 8
    engines:
      - targets: 4
        helpers: 0
        fabricIface: ens19
        bdevClass: nvme
        bdevList: []            # hostprep 이 uio_pci_generic 으로 묶은 QEMU NVMe 를 발견한다
        scmSizeGiB: 4
    hostPrep:
      enabled: true
      bindNvme: true
EOF
```
키 이름이 CRD 와 맞는지 렌더로 확인한다:
Run: `helm template daos-operator charts/daos-operator -n daos-system -f test/e2e/profiles/nvme-1rank.yaml | kubectl apply --dry-run=server -f - 2>&1 | tail -3` (KUBECONFIG 설정 상태, CRD 는 차트가 설치)
Expected: 오류 없이 `… created (server dry run)` 줄들. `unknown field` 가 나오면 그 키를 `api/v1alpha1/daossystem_types.go` 의 json 태그로 고친다.

- [ ] **Step 2: verify-nvme.sh 작성**

```bash
cat > hack/ci/verify-nvme.sh <<'EOF'
#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# 설계 A-3: QEMU NVMe 위에서 SPDK nvme 클래스 DaosSystem 이 뜨고 풀이 Ready 가 되는가.
# 롤백 → helm install(nvme-1rank) → DaosSystem Ready·ranksJoined=1 → DaosPool Ready.
# 출력 마지막 줄이 NVME_CLASS_OK 또는 NVME_CLASS_FAIL. 실패 시 서버 파드 로그 꼬리를 남긴다.
set -uo pipefail
source "$(dirname "$0")/env.sh"
export KUBECONFIG=${KUBECONFIG:-$HOME/.kube/daos-ci.conf}
repo=$(cd "$(dirname "$0")/../.." && pwd)
log=${LOG:-/tmp/verify-nvme.log}; : > "$log"

"$(dirname "$0")/rollback.sh" | tee -a "$log"
helm upgrade --install daos-operator "$repo/charts/daos-operator" -n daos-system --create-namespace \
    -f "$repo/test/e2e/profiles/nvme-1rank.yaml" --wait --timeout 5m | tee -a "$log"

# hostprep 이 NVMe 를 보고 묶었는가
sleep 60
kubectl get node exaci5-3a -o jsonpath='{.metadata.annotations}' | tr ',' '\n' | grep daos.gluesys.com | tee -a "$log"
node_ssh "${VM_IP[109]}" "lspci -k | grep -A2 Non-Volatile" | tee -a "$log"

if kubectl -n daos-system wait daossystem/daos --for=condition=Ready --timeout=600s 2>&1 | tee -a "$log"; then
    joined=$(kubectl -n daos-system get daossystem daos -o jsonpath='{.status.ranksJoined}')
    echo "ranksJoined=$joined" | tee -a "$log"
    kubectl -n daos-system apply -f - <<P
apiVersion: daos.gluesys.com/v1alpha1
kind: DaosPool
metadata: {name: nvmepool, namespace: daos-system}
spec: {systemRef: daos, size: 8Gi, redundancyFactor: 0}
P
    if kubectl -n daos-system wait daospool/nvmepool --for=condition=Ready --timeout=300s 2>&1 | tee -a "$log" && [ "$joined" = 1 ]; then
        kubectl -n daos-system get daossystem,daospool -o wide | tee -a "$log"
        echo NVME_CLASS_OK | tee -a "$log"; exit 0
    fi
fi
kubectl -n daos-system get daossystem daos -o jsonpath='{.status.conditions}' | tee -a "$log"; echo
kubectl -n daos-system logs -l app.kubernetes.io/component=server --tail=80 --all-containers 2>/dev/null | tee -a "$log"
echo NVME_CLASS_FAIL | tee -a "$log"; exit 1
EOF
chmod +x hack/ci/verify-nvme.sh
```

- [ ] **Step 3: 실행**

Run: `hack/ci/verify-nvme.sh; echo "exit=$?"`
Expected(성공): `ranksJoined=1`, `daospool … Ready`, 마지막 줄 `NVME_CLASS_OK`, `exit=0`.
실패 분기(설계 12절):
- annotation 에 `nvme-candidates` 가 비어 있다 → hostprep 이 QEMU NVMe 를 후보에서 뺀 것. `kubectl -n daos-system logs ds/daos-hostprep` 에서 이유(파티션·holder·DSN 없음)를 적는다.
- `lspci -k` 가 `nvme` 드라이버 그대로 → `bindNvme` 경로가 안 돈 것. hostprep 로그 확인.
- 서버 로그에 `vfio`/`IOMMU` 오류 → `disableVFIO` 가 렌더에 안 들어간 것. `kubectl -n daos-system get cm daos-server -o yaml | grep disable_vfio` 로 확인.
- 서버 로그 `DER_NOMEM` → hugepages. `nrHugepages`/노드 `vm.nr_hugepages` 확인.
위 어느 것도 1일 안에 못 풀면 **FAIL 로 판정**하고 Step 4 로 간다. 디버깅을 하루 넘기지 않는다.

- [ ] **Step 4: 설계 문서에 결과 기록**

`doc/ci-design-2026-10-02.md` 5절 둘째 불릿("SPDK `nvme` 클래스로 서버를 띄운 선례는 아직 없다.") 바로 뒤에 한 줄을 넣는다. 성공 시:
```
  **2026-10-0X 검증 결과: 성공.** exaci5-3a 에서 QEMU NVMe 2개를 uio_pci_generic 으로 묶어 rank 0 join, 8 GiB 풀 Ready(`hack/ci/verify-nvme.sh`, 로그 요지: …).
```
실패 시:
```
  **2026-10-0X 검증 결과: 실패.** 원인: … . Tier 1 프로파일은 `kdev-1rank` 로 바꾸고 nvme 클래스는 da1~4 야간 잡에서 본다.
```
10절 표의 A-3 행 공수 뒤에 `(완료, 결과 5절)` 을 붙인다. 실패 시 5절 표의 `nvme-1rank` 행 Tier 를 `da1~4` 로, 7절 Tier 1 머리말을 `kdev-1rank` 로 고친다.

- [ ] **Step 5: 커밋**

```bash
git add test/e2e/profiles/nvme-1rank.yaml hack/ci/verify-nvme.sh doc/ci-design-2026-10-02.md
git commit -m "ci(a3): QEMU NVMe 위 SPDK nvme 클래스 검증 스크립트와 결과를 기록한다"
```

---

### Task 7: 3J 러너와 빈 파이프라인

**Files:**
- Create: `hack/ci/runner-setup.sh` (3J 안에서 실행)
- Modify: `.gitlab-ci.yml` (`e2e` 스테이지, `env-reset` 잡)

**Interfaces:**
- Consumes: Task 5 `rollback.sh`, GitLab 그룹 `exastor` 의 러너 등록 토큰(사용자가 GitLab UI *exastor → Settings → CI/CD → Runners → New group runner* 에서 태그 `daos-k8s`, "Run untagged: off" 로 만들어 `glrt-…` 토큰을 준다), CI 변수.
- Produces: 러너 `exaci5-3j-daos-k8s`(태그 `daos-k8s`, shell), MR 파이프라인에서 수동 실행 가능한 `env-reset` 잡이 롤백 후 `kubectl get nodes` 를 찍는다.

- [ ] **Step 1: 러너 설치 스크립트**

```bash
cat > hack/ci/runner-setup.sh <<'EOF'
#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# 3J(exaci5-3j) 안에서 root 로 실행. 사용: RUNNER_TOKEN=glrt-… runner-setup.sh
# shell executor 는 gitlab-runner 사용자로 잡을 돌린다. 그 사용자에게 kubectl·helm·vdi5 접근 키를 준다.
set -euo pipefail
: "${RUNNER_TOKEN:?group runner authentication token (glrt-…)}"
curl -fsSL https://packages.gitlab.com/install/repositories/runner/gitlab-runner/script.rpm.sh | bash
dnf install -y -q gitlab-runner git jq
curl -fsSL https://dl.k8s.io/release/v1.31.14/bin/linux/amd64/kubectl -o /usr/local/bin/kubectl && chmod +x /usr/local/bin/kubectl
curl -fsSL https://get.helm.sh/helm-v3.19.0-linux-amd64.tar.gz | tar xz -C /tmp && install /tmp/linux-amd64/helm /usr/local/bin/helm
gitlab-runner register --non-interactive --url https://gitlab.gluesys.com/ --token "$RUNNER_TOKEN" \
    --executor shell --description exaci5-3j-daos-k8s
sed -i 's/^concurrent = .*/concurrent = 1/' /etc/gitlab-runner/config.toml   # 클러스터는 하나다
systemctl enable --now gitlab-runner
# 잡은 gitlab-runner 사용자로 돈다. ssh 키는 CI 변수(file)로 들어오므로 여기서는 디렉터리만.
install -d -o gitlab-runner -g gitlab-runner -m 700 /home/gitlab-runner/.ssh /home/gitlab-runner/.kube
gitlab-runner verify
echo "runner-setup: done"
EOF
chmod +x hack/ci/runner-setup.sh
```

- [ ] **Step 2: 3J 에서 실행**

Run: `scp hack/ci/runner-setup.sh root@192.168.35.30:/root/ && ssh root@192.168.35.30 "RUNNER_TOKEN=glrt-… bash /root/runner-setup.sh"`
Expected: `Verifying runner... is valid` 와 `runner-setup: done`. GitLab *exastor → Settings → CI/CD → Runners* 에 `exaci5-3j-daos-k8s` 가 online.

- [ ] **Step 3: CI 변수 등록(사용자가 GitLab UI 에서, 그룹 `exastor` 범위, 모두 masked + protected 해제·file 타입)**

| 변수 | 내용 |
|---|---|
| `DAOS_CI_KUBECONFIG` | `~/.kube/daos-ci.conf` 전문 |
| `DAOS_CI_SSH_KEY` | 노드 root 와 vdi5 `jenkins-ci` 에 등록된 개인키 (`ssh-keygen -t ed25519 -f ~/.ssh/daos-ci -N ''` 로 새로 만들고 공개키를 6대 `/root/.ssh/authorized_keys` 와 vdi5 `/home/jenkins-ci/.ssh/authorized_keys` 에 추가) |

공개키 배포:
Run: `for ip in 192.168.35.30 .40 .31 .32 .41 .42; do ssh root@192.168.35.${ip##*.} "cat >> /root/.ssh/authorized_keys" < ~/.ssh/daos-ci.pub; done; ssh vdi5 "cat >> /home/jenkins-ci/.ssh/authorized_keys" < ~/.ssh/daos-ci.pub`
Expected: 오류 없음. `ssh -i ~/.ssh/daos-ci -p9349 jenkins-ci@192.168.2.186 sudo qm list | head -2` 가 표 머리글을 찍는다.

- [ ] **Step 4: `.gitlab-ci.yml` 에 잡 추가**

`stages: [check, image, release]` 를 `stages: [check, image, e2e, release]` 로 바꾸고, `release-cli:` 블록 앞에 추가:

```yaml
# e2e: ExaCI5 슬롯 3·4 VM 클러스터(doc/ci-design-2026-10-02.md). 러너 exaci5-3j, 태그 daos-k8s.
# 한 번에 한 파이프라인만 클러스터를 쓴다. Phase A 는 롤백만 검증한다(수동).
.daos-k8s: &daos-k8s
  stage: e2e
  tags: [daos-k8s]
  resource_group: daos-k8s-ci
  timeout: 45m
  before_script:
    - install -m 600 "$DAOS_CI_SSH_KEY" ~/.ssh/id_ed25519
    - install -m 600 "$DAOS_CI_KUBECONFIG" ~/.kube/daos-ci.conf
    - export KUBECONFIG=~/.kube/daos-ci.conf
    - export VDI_SSH="ssh -i ~/.ssh/id_ed25519 -o BatchMode=yes -o StrictHostKeyChecking=no -p 9349 jenkins-ci@192.168.2.186"

env-reset:
  <<: *daos-k8s
  script:
    - hack/ci/rollback.sh
    - kubectl get nodes -o wide
  rules:
    - if: $CI_PIPELINE_SOURCE == "merge_request_event"
      when: manual
      allow_failure: true
```

- [ ] **Step 5: 문법 확인**

Run: `python3 -c "import yaml,sys; yaml.safe_load(open('.gitlab-ci.yml')); print('yaml ok')"` 그리고 `curl -s --header "PRIVATE-TOKEN: $(tr -d '\n' < ~/src/Flexa/tokens/PAT.gitlab_jenkins_connection.token)" --header "Content-Type: application/json" --data "$(jq -Rs '{content: .}' < .gitlab-ci.yml)" "https://gitlab.gluesys.com/api/v4/projects/820/ci/lint" | jq '.valid, .errors'`
Expected: `yaml ok`, `true`, `[]`.

- [ ] **Step 6: 커밋**

```bash
git add hack/ci/runner-setup.sh .gitlab-ci.yml
git commit -m "ci(runner): 3J shell 러너와 수동 env-reset 잡 — 롤백이 파이프라인에서 돈다"
```

- [ ] **Step 7: end-to-end 확인(푸시는 사용자 요청 후)**

사용자 요청으로 브랜치를 푸시하고 MR 을 만든 뒤 `env-reset` 을 수동 실행한다.
Expected: 잡 로그에 `rollback: cluster ready in <n>s` 와 노드 5줄(`Ready`). 이 결과(잡 URL, 소요 초)를 설계 문서 4절 마지막에 한 줄로 적는다.

---

### Task 8: 마무리

**Files:**
- Modify: `doc/ci-design-2026-10-02.md` (4절 실측, 10절 A 행 완료 표시)
- Modify: `README.md` (Documentation 에 `hack/ci/` 한 줄)

- [ ] **Step 1: README 한 줄**

`README.md` Documentation 목록의 `doc/ci-design-2026-10-02.md` 항목 끝에 이어서:
```
- [`hack/ci/`](hack/ci/) — the scripts that build and reset that CI cluster
  (`vm-reshape.sh` once, `prep-all.sh`/`cluster-init.sh`/`snapshot.sh` to rebuild the
  base, `rollback.sh` before every run) *(Korean comments)*
```

- [ ] **Step 2: 전체 스크립트 문법**

Run: `for f in hack/ci/*.sh; do bash -n "$f" && echo "ok $f"; done; head -3 hack/ci/*.sh test/e2e/profiles/*.yaml | grep -c SPDX`
Expected: 모든 파일 `ok`, SPDX 개수 = 파일 수(스크립트 12 + yaml 1 = 13).

- [ ] **Step 3: 커밋**

```bash
git add README.md doc/ci-design-2026-10-02.md
git commit -m "docs(ci): Phase A 완료 — 롤백 실측과 hack/ci 안내를 기록한다"
```

- [ ] **Step 4: 다음 플랜**

A-3 결과(`NVME_CLASS_OK`/`FAIL`)를 가지고 Plan B(Tier 0)와 Plan C(Tier 1 Ginkgo 스펙)를 쓴다. Plan C 의 기본 프로파일은 이 결과로 정해진다.
