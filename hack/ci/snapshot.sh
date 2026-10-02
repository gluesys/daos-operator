#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# 클러스터 5대를 깨끗이 내리고 정지 상태 스냅샷 $SNAP 을 찍은 뒤 다시 올린다.
# 같은 이름이 있으면 지우고 다시 찍는다(이미지 갱신 뒤 재스냅샷용).
set -euo pipefail
source "$(dirname "$0")/env.sh"
for v in "${CLUSTER_VMIDS[@]}"; do qm shutdown "$v" --timeout 120 || qm stop "$v"; done
for v in "${CLUSTER_VMIDS[@]}"; do
    qm listsnapshot "$v" | grep -q " $SNAP " && qm delsnapshot "$v" "$SNAP"
    qm snapshot "$v" "$SNAP" --description "DAOS-K8s-CI-base-$(date +%F)"   # qm() 가 ssh 로 넘기므로 공백 금지
done
for v in "${CLUSTER_VMIDS[@]}"; do qm start "$v"; done
echo "snapshot: $SNAP taken on ${CLUSTER_VMIDS[*]}"
