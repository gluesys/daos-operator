#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
# 클러스터가 설계 1·3절 모양인지. KUBECONFIG 필요.
set -euo pipefail
source "$(dirname "$0")/env.sh"
: "${KUBECONFIG:?}"
fail=0
ready=$(kubectl get nodes --no-headers | awk '$2=="Ready"' | wc -l)
want=${#CLUSTER_VMIDS[@]}
[ "$ready" = "$want" ] || { echo "ready nodes: $ready (want $want)"; fail=1; }
for n in "${STORAGE_NODES[@]}"; do
    kubectl get node "$n" -o jsonpath='{.metadata.labels.daos\.gluesys\.com/role}' | grep -qx storage || { echo "$n: role=storage label missing"; fail=1; }
done
kubectl get node "$CLIENT_NODE" -o jsonpath='{.metadata.labels.daos\.gluesys\.com/client}' | grep -qx true || { echo "$CLIENT_NODE: client=true missing"; fail=1; }
if [ -n "$GPU_VMID" ]; then
    kubectl get node "${VM_NAME[$GPU_VMID]}" -o jsonpath='{.metadata.labels.daos\.gluesys\.com/gpu}' | grep -qx true || { echo "${VM_NAME[$GPU_VMID]}: gpu=true missing"; fail=1; }
fi
kubectl -n kube-flannel get ds kube-flannel-ds -o jsonpath='{.status.numberReady}' | grep -qx "$want" || { echo "flannel not ready on $want"; fail=1; }
kubectl -n daos-system get secret gitlab-registry >/dev/null || { echo "pull secret missing"; fail=1; }
# 사전 pull: 워커마다 images.txt 의 모든 이미지가 있어야 한다
while read -r img; do
    [ -z "$img" ] && continue
    for v in "${WORKER_VMIDS[@]}"; do
        # </dev/null: ssh would otherwise read the rest of images.txt and end the loop after one image
        node_ssh "${VM_IP[$v]}" "crictl images -q $img | grep -q ." </dev/null || { echo "${VM_NAME[$v]}: missing $img"; fail=1; }
    done
done < "$(dirname "$0")/images.txt"
[ $fail = 0 ] && echo "check-cluster: OK"
exit $fail
