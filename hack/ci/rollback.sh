#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# 매 파이프라인의 첫 단계. 클러스터 5대를 $SNAP 으로 되돌리고 5 노드 Ready 까지 기다린다.
# 러너 VM(3J)은 건드리지 않는다. 실패하면 0 이 아닌 코드로 끝나 파이프라인을 멈춘다.
set -euo pipefail
source "$(dirname "$0")/env.sh"
export KUBECONFIG=${KUBECONFIG:-$KCFG}
start=$(date +%s)
for v in "${CLUSTER_VMIDS[@]}"; do
    ( qm stop "$v" --timeout 60 >/dev/null 2>&1 || true
      qm rollback "$v" "$SNAP" >/dev/null
      qm start "$v" >/dev/null ) &
done
wait
wait_ssh "${VM_IP[$CP_VMID]}" 240
# 스냅샷 안의 etcd 는 롤백 직전의 Ready 를 그대로 들고 있다. 롤백 이후 kubelet 이 보낸
# 하트비트(lastHeartbeatTime >= 시작 시각)로 Ready 인 노드만 센다.
since=$(date -u -d "@$start" +%Y-%m-%dT%H:%M:%SZ)
fresh_ready() {
    kubectl get nodes -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status} {.status.conditions[?(@.type=="Ready")].lastHeartbeatTime}{"\n"}{end}' 2>/dev/null \
        | awk -v s="$since" '$1=="True" && $2>=s' | wc -l
}
until [ "$(fresh_ready)" = "${#CLUSTER_VMIDS[@]}" ]; do
    [ $(( $(date +%s) - start )) -ge 300 ] && { echo "rollback: nodes not Ready in 300s"; kubectl get nodes || true; exit 1; }
    sleep 5
done
kubectl -n kube-system wait --for=condition=Ready pod --all --timeout=120s >/dev/null
echo "rollback: cluster ready in $(( $(date +%s) - start ))s"
