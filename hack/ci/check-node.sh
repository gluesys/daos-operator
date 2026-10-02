#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
# 노드 하나가 설계 3절 "호스트 설정" 을 만족하는지. 사용: check-node.sh <vmid>
set -euo pipefail
source "$(dirname "$0")/env.sh"
v=$1; ip=${VM_IP[$v]}; fail=0
t() { local desc=$1; shift; if node_ssh "$ip" "$@" >/dev/null 2>&1; then :; else echo "VM $v: FAIL $desc"; fail=1; fi; }
t hostname         "[ \$(hostname) = ${VM_NAME[$v]} ]"
t ens19            "ip -4 addr show ens19 | grep -q ${VM_IP2[$v]}/24"
t swap             "[ \$(swapon --noheadings | wc -l) = 0 ]"
t selinux          "[ \$(getenforce) = Permissive ]"
t firewalld        "! systemctl is-active firewalld"
t thp              "grep -q '\[never\]' /sys/kernel/mm/transparent_hugepage/enabled"
t crio             "systemctl is-active crio && crictl version"
t kubeadm          "kubeadm version -o short | grep -q v1.31.14"
t cgroupfs         "grep -q 'cgroup_manager = \"cgroupfs\"' /etc/crio/crio.conf.d/10-cgroupfs.conf"
t crun             "command -v crun"
if [[ " ${WORKER_VMIDS[*]} " == *" $v "* ]]; then
    t hugepages    "[ \$(awk '/HugePages_Total/{print \$2}' /proc/meminfo) -ge 4096 ]"
    t uio          "lsmod | grep -q uio_pci_generic"
    t nvme2        "[ \$(ls /dev/nvme?n1 | wc -l) = 2 ]"
    t kdev2        "[ -b /dev/sdb ] && [ -b /dev/sdc ]"
    t ib           "ip -4 addr show | grep -q ${VM_IBIP[$v]}/24"
fi
[ $fail = 0 ] && echo "check-node $v: OK"
exit $fail
