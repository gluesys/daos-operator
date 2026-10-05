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
ALL=("${ALL_VMIDS[@]}")

for v in "${ALL[@]}"; do qm status "$v" | grep -q running || qm start "$v"; done
for v in "${ALL[@]}"; do
    ip=${VM_IP[$v]}
    until sshpass -e ssh -o StrictHostKeyChecking=no -o ConnectTimeout=3 -o PreferredAuthentications=password "root@$ip" true 2>/dev/null; do sleep 5; done
    sshpass -e ssh-copy-id -o StrictHostKeyChecking=no -i "$PUB" "root@$ip" >/dev/null 2>&1
done

role_of() { case $1 in "$CP_VMID") echo cp;; "${RUNNER_VMID:-none}") echo runner;; *) echo worker;; esac; }
for v in "${ALL[@]}"; do
    ip=${VM_IP[$v]}
    scp "${SSH_OPTS[@]}" "$(dirname "$0")/node-prep.sh" "root@$ip:/root/node-prep.sh"
    node_ssh "$ip" "bash /root/node-prep.sh $(role_of "$v") ${VM_NAME[$v]} ${VM_IP2[$v]} ${VM_IBIP[$v]}" > "/tmp/node-prep-${VM_NAME[$v]}.log" 2>&1 &
done
wait
tail -n1 /tmp/node-prep-exaci5-*.log
echo "prep-all: done"
