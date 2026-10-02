#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
# env.sh 가 정의하는 것들이 설계 문서 1절과 일치하는지 확인한다.
set -euo pipefail
source "$(dirname "$0")/env.sh"
[ "$CP_VMID" = 111 ] && [ "$RUNNER_VMID" = 108 ]
[ "${CLUSTER_VMIDS[*]}" = "111 109 110 112 113" ]
[ "${WORKER_VMIDS[*]}" = "109 110 112 113" ]
[ "${VM_IP[112]}" = 192.168.35.41 ] && [ "${VM_IP2[112]}" = 10.10.35.41 ] && [ "${VM_IBIP[112]}" = 10.10.36.41 ]
[ "${VM_NAME[113]}" = exaci5-4b ]
qm list >/dev/null          # vdi5 에 sudo qm 이 된다
echo "check-env: OK"
