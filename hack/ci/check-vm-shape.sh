#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
# 6대의 qm config 가 설계 2절과 일치하는지 본다. 어긋나면 그 VM 과 항목을 찍고 1 로 끝난다.
set -euo pipefail
source "$(dirname "$0")/env.sh"
fail=0
chk() { local v=$1 key=$2 want=$3 got; got=$(qm config "$v" | awk -F': ' -v k="$key" '$1==k{print $2}'); [ "$got" = "$want" ] || { echo "VM $v $key: got '$got' want '$want'"; fail=1; }; }
for v in "${WORKER_VMIDS[@]}"; do
    chk "$v" cores 12; chk "$v" memory 32768; chk "$v" hugepages 2
    chk "$v" numa0 "cpus=0-11,memory=32768,hostnodes=1,policy=bind"
    for d in nvme0 nvme1 scsi1 scsi2; do
        qm config "$v" | grep -qE "^$d: local-SSD:vm-$v-disk-[0-9]+,.*size=100G" || { echo "VM $v $d: not a 100G local-SSD zvol"; fail=1; }
    done
    qm config "$v" | grep -q 'remote-NVMe' && { echo "VM $v: remote-NVMe disk still attached"; fail=1; }
done
chk 112 hostpci2 0000:b1:00.0
chk 111 cores 8;  chk 111 memory 8192; chk 111 numa0 "cpus=0-7,memory=8192,hostnodes=1,policy=bind"
chk 108 cores 4;  chk 108 memory 8192
[ $fail = 0 ] && echo "check-vm-shape: OK"
exit $fail
