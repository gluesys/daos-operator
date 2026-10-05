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
# 재실행 가능: 이미 init 된 컨트롤 플레인과 join 된 워커는 건너뛴다.
node_ssh "$cp" "[ -f /etc/kubernetes/admin.conf ] || kubeadm init --config /root/kubeadm-config.yaml --upload-certs | tail -20"
node_ssh "$cp" "mkdir -p /root/.kube && cp /etc/kubernetes/admin.conf /root/.kube/config"
# flannel 은 ens18 로 고정한다(testbed-k8s-exaci4-2). CNI 를 CRI-O 가 늦게 집으면 crio 재시작.
node_ssh "$cp" "curl -fsSL $FLANNEL | sed 's#- --kube-subnet-mgr#- --kube-subnet-mgr\n        - --iface=ens18#' | kubectl apply -f -"
node_ssh "$cp" "sleep 20; systemctl restart crio"

join=$(node_ssh "$cp" "kubeadm token create --print-join-command")
for v in "${WORKER_VMIDS[@]}"; do
    node_ssh "${VM_IP[$v]}" "[ -f /etc/kubernetes/kubelet.conf ] || $join --node-name ${VM_NAME[$v]} --cri-socket unix:///var/run/crio/crio.sock" &
done
wait
mkdir -p ~/.kube
scp "${SSH_OPTS[@]}" "root@$cp:/etc/kubernetes/admin.conf" "$KCFG"
export KUBECONFIG=$KCFG PATH=$HOME/.local/bin:$PATH
for v in "${WORKER_VMIDS[@]}"; do kubectl wait --for=condition=Ready "node/${VM_NAME[$v]}" --timeout=300s; done

kubectl label node "${STORAGE_NODES[@]}" daos.gluesys.com/role=storage --overwrite
kubectl label node "$CLIENT_NODE" daos.gluesys.com/client=true --overwrite
[ -n "$GPU_VMID" ] && kubectl label node "${VM_NAME[$GPU_VMID]}" daos.gluesys.com/gpu=true --overwrite
kubectl create namespace daos-system --dry-run=client -o yaml | kubectl apply -f -
kubectl -n daos-system create secret docker-registry gitlab-registry \
    --docker-server=registry.gitlab.gluesys.com --docker-username="$REG_USER" --docker-password="$REG_TOKEN" \
    --dry-run=client -o yaml | kubectl apply -f -
for i in $(seq 30); do kubectl -n daos-system get sa default >/dev/null 2>&1 && break; sleep 1; done   # SA 는 비동기로 생긴다
kubectl -n daos-system patch sa default -p '{"imagePullSecrets":[{"name":"gitlab-registry"}]}'

# 사전 pull: 스냅샷에 들어가므로 매 실행의 pull 시간이 사라진다.
for v in "${WORKER_VMIDS[@]}"; do
    node_ssh "${VM_IP[$v]}" "while read -r img; do [ -n \"\$img\" ] && crictl pull --creds '$REG_USER:$REG_TOKEN' \"\$img\" >/dev/null && echo \"${VM_NAME[$v]} pulled \$img\"; done" < "$here/images.txt" &
done
wait
echo "cluster-init: done (KUBECONFIG=~/.kube/daos-ci.conf)"
