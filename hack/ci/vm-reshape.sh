#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# 슬롯 3·4 VM 6대를 설계 2절 모양으로 만든다(정지 상태로 끝난다).
#   1) 정지 → `init` 롤백 → 다시 정지(init 이 실행 중 스냅샷인 VM 이 있다)
#   2) remote-NVMe 디스크 전부 분리   3) 워커에 zvol 4개
#   4) 코어·메모리   5) 4A 에 A2 GPU
# 되돌릴 수 없다. 한 번 실행한다. 이후의 리셋은 rollback.sh(k8s-ready) 가 맡는다.
set -euo pipefail
source "$(dirname "$0")/env.sh"

ALL=("$RUNNER_VMID" "${CLUSTER_VMIDS[@]}")

echo "== stop + rollback init + stop"
for v in "${ALL[@]}"; do
    qm stop "$v" --timeout 90 || true
    qm rollback "$v" init
    qm stop "$v" --timeout 90 || true
done

echo "== detach every disk that is not the root qcow2"
for v in "${ALL[@]}"; do
    for key in $(qm config "$v" | awk -F: '/^(scsi|nvme|virtio|sata|ide)[0-9]+:/ && !/qcow2/ && !/cdrom/ {print $1}'); do
        qm set "$v" --delete "$key"
    done
    # `qm set --delete unusedN` 은 볼륨 파일을 지운다(B 노드와 공유하는 NFS raw). 참조 줄만 지운다.
    $VDI_SSH "sudo sed -i '/^unused[0-9]*:/d' /etc/pve/qemu-server/$v.conf"
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
