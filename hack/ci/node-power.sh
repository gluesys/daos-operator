#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# 워커 VM 전원 제어(Tier 2 노드 재부팅 케이스). env.sh 의 qm() 은 3J 에서는 jenkins-ci@vdi5, 개발 PC 에서는
# `ssh vdi5` 로 간다. 사용: node-power.sh reboot exaci5-3b
set -euo pipefail
op=${1:-}; node=${2:-}
case $op in
  reboot) ;;
  *) echo "node-power: unknown op ${op:-<none>} (want: reboot <node>)" >&2; exit 2 ;;
esac
. "$(dirname "$0")/env.sh"
vmid=""
for v in "${!VM_NAME[@]}"; do [ "${VM_NAME[$v]}" = "$node" ] && vmid=$v; done
[ -n "$vmid" ] || { echo "node-power: unknown node ${node:-<none>}" >&2; exit 2; }
qm stop "$vmid" --timeout 120
qm start "$vmid"
wait_ssh "${VM_IP[$vmid]}" 300
echo "node-power: $node ($vmid) back"
