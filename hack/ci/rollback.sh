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
