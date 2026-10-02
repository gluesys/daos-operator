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
export PATH=$HOME/.local/bin:$PATH    # kubectl/helm 을 사용자 영역에 둔다(러너·개발 PC 공통)
SSH_OPTS=(-o BatchMode=yes -o StrictHostKeyChecking=no -o ConnectTimeout=10)

qm() { $VDI_SSH "sudo env LC_ALL=C qm $*"; }   # env LC_ALL: sudo 가 ko_KR LC_* 를 넘겨 perl 이 경고를 쏟는다
node_ssh() { local ip=$1; shift; ssh "${SSH_OPTS[@]}" "root@$ip" "$@"; }
wait_ssh() {
    local ip=$1 limit=${2:-300} t=0
    until ssh "${SSH_OPTS[@]}" -o ConnectTimeout=3 "root@$ip" true 2>/dev/null; do
        sleep 5; t=$((t+5)); [ $t -ge $limit ] && { echo "wait_ssh $ip: timeout" >&2; return 1; }
    done
}
