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
