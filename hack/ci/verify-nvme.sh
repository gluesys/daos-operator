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
    -f "$repo/test/e2e/profiles/nvme-1rank.yaml" --wait --timeout 5m 2>&1 | tee -a "$log"

# 포맷은 사람 승인을 기다린다(operator 설계). CI 클러스터에서는 스크립트가 승인한다.
# 실패한 포맷은 operator 가 승인을 지우므로(2026-10-02) 서버가 Ready 가 된 뒤에 단다.
kubectl -n daos-system wait daossystem/daos --for=condition=ServersReady --timeout=600s 2>&1 | tee -a "$log"
kubectl -n daos-system annotate daossystem daos daos.gluesys.com/format-approved=true --overwrite | tee -a "$log"

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
