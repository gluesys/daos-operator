#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# DAOS K8s CI 환경 상수. 설계: doc/ci-design-2026-10-02.md 1절.
# 개발 PC 에서는 `ssh vdi5`(root) 가, 3J 러너에서는 jenkins-ci 계정이 Proxmox 에 닿는다.
# VDI_SSH 를 바꾸면 두 경우를 모두 덮는다.
VDI_SSH=${VDI_SSH:-"ssh -o BatchMode=yes -o ConnectTimeout=10 vdi5"}

# LANE=a (기본): 슬롯 3·4, 워커 4대 — Tier 1 이외(upgrade·regression·Tier 2).
# LANE=b: 슬롯 5-2(Jenkins 라벨 DAOS-K8S-2), CP + 워커 2대 — Tier 1 전용(설계 8절 레인 B). 러너는 3J 공용.
LANE=${LANE:-a}
case $LANE in
a)
    CP_VMID=111
    RUNNER_VMID=108
    CLUSTER_VMIDS=(111 109 110 112 113)
    WORKER_VMIDS=(109 110 112 113)
    STORAGE_NODES=(exaci5-3a exaci5-3b exaci5-4b)
    CLIENT_NODE=exaci5-4a
    GPU_VMID=112
    KCFG=$HOME/.kube/daos-ci.conf
    declare -A VM_NAME=([108]=exaci5-3j [111]=exaci5-4j [109]=exaci5-3a [110]=exaci5-3b [112]=exaci5-4a [113]=exaci5-4b)
    declare -A VM_IP=([108]=192.168.35.30 [111]=192.168.35.40 [109]=192.168.35.31 [110]=192.168.35.32 [112]=192.168.35.41 [113]=192.168.35.42)
    ;;
b)
    CP_VMID=105
    RUNNER_VMID=""
    CLUSTER_VMIDS=(105 106 107)
    WORKER_VMIDS=(106 107)
    STORAGE_NODES=(exaci5-2a)
    CLIENT_NODE=exaci5-2b
    GPU_VMID=""
    KCFG=$HOME/.kube/daos-ci-b.conf
    declare -A VM_NAME=([105]=exaci5-2j [106]=exaci5-2a [107]=exaci5-2b)
    declare -A VM_IP=([105]=192.168.35.20 [106]=192.168.35.21 [107]=192.168.35.22)
    ;;
*) echo "env.sh: unknown LANE=$LANE" >&2; exit 2 ;;
esac
ALL_VMIDS=(${RUNNER_VMID:+"$RUNNER_VMID"} "${CLUSTER_VMIDS[@]}")
declare -A VM_IP2 VM_IBIP
for v in "${!VM_IP[@]}"; do
    last=${VM_IP[$v]##*.}
    VM_IP2[$v]=10.10.35.$last
    VM_IBIP[$v]=10.10.36.$last
done

SNAP=${SNAP:-k8s-ready}
export PATH=$HOME/.local/bin:$PATH    # kubectl/helm 을 사용자 영역에 둔다(러너·개발 PC 공통)
SSH_OPTS=(-o BatchMode=yes -o StrictHostKeyChecking=no -o ConnectTimeout=10)

qm() { $VDI_SSH "LC_ALL=C sudo qm $*" 2> >(grep -v -E '^perl: warning|^[[:space:]]+(LANG|LC_|LANGUAGE)|are supported|Please check|Setting locale|Falling back' >&2); }   # jenkins-ci sudoers 는 /usr/sbin/qm 만 허용한다(env 래핑 불가)
node_ssh() { local ip=$1; shift; ssh "${SSH_OPTS[@]}" "root@$ip" "$@"; }
wait_ssh() {
    local ip=$1 limit=${2:-300} t=0
    until ssh "${SSH_OPTS[@]}" -o ConnectTimeout=3 "root@$ip" true 2>/dev/null; do
        sleep 5; t=$((t+5)); [ $t -ge $limit ] && { echo "wait_ssh $ip: timeout" >&2; return 1; }
    done
    # the loop's status is its body's last command ([ … ] && …, false while waiting): without this a
    # node that answered on the second try returned 1 and set -e ended node-power.sh (2026-10-05)
    return 0
}
