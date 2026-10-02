#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# 노드 안에서 실행. 사용: node-prep.sh <worker|cp|runner> <hostname> <ens19-ip> <ib-ip>
# 설계 3절 + testbed-k8s-exaci4-2.md 의 EL8 특이사항(conntrack, cgroupfs, crun).
set -euo pipefail
role=$1; name=$2; ip2=$3; ibip=$4
K8S_VER=1.31.14

hostnamectl set-hostname "$name"
nmcli -t -f NAME con show | grep -qx ens19 || nmcli con add type ethernet ifname ens19 con-name ens19 ipv4.method manual ipv4.addresses "$ip2/24" autoconnect yes
nmcli con up ens19 >/dev/null

swapoff -a; sed -i '/\sswap\s/d' /etc/fstab
setenforce 0 || true; sed -i 's/^SELINUX=enforcing/SELINUX=permissive/' /etc/selinux/config
systemctl disable --now firewalld || true

# THP never (즉시 + 영구)
echo never > /sys/kernel/mm/transparent_hugepage/enabled
grubby --update-kernel=ALL --args="transparent_hugepage=never"

cat > /etc/modules-load.d/k8s.conf <<M
overlay
br_netfilter
M
modprobe overlay; modprobe br_netfilter
cat > /etc/sysctl.d/99-k8s.conf <<S
net.bridge.bridge-nf-call-iptables  = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward                 = 1
S
sysctl --system >/dev/null

if [ "$role" = worker ]; then
    echo uio_pci_generic > /etc/modules-load.d/uio.conf; modprobe uio_pci_generic
    echo "vm.nr_hugepages = 4096" > /etc/sysctl.d/90-hugepages.conf
    sysctl -w vm.nr_hugepages=4096 >/dev/null
    # IPoIB (verbs 프로파일용). VF 인터페이스 이름은 보장되지 않으므로 첫 ib* 를 쓴다. 없으면 건너뛴다.
    ibif=$(ls /sys/class/net | grep -m1 '^ib' || true)
    if [ -n "$ibif" ]; then
        nmcli -t -f NAME con show | grep -qx ibci || nmcli con add type infiniband ifname "$ibif" con-name ibci ipv4.method manual ipv4.addresses "$ibip/24" autoconnect yes
        nmcli con up ibci >/dev/null || true
    fi
fi

# 저장소: pkgs.k8s.io (kubeadm 1.31.x), CRI-O 1.31
cat > /etc/yum.repos.d/kubernetes.repo <<R
[kubernetes]
name=Kubernetes
baseurl=https://pkgs.k8s.io/core:/stable:/v1.31/rpm/
enabled=1
gpgcheck=1
gpgkey=https://pkgs.k8s.io/core:/stable:/v1.31/rpm/repodata/repomd.xml.key
exclude=kubelet kubeadm kubectl cri-tools kubernetes-cni
R
cat > /etc/yum.repos.d/cri-o.repo <<R
[cri-o]
name=CRI-O
baseurl=https://pkgs.k8s.io/addons:/cri-o:/stable:/v1.31/rpm/
enabled=1
gpgcheck=1
gpgkey=https://pkgs.k8s.io/addons:/cri-o:/stable:/v1.31/rpm/repodata/repomd.xml.key
R
dnf install -y -q conntrack-tools iproute-tc socat crun chrony
dnf install -y -q --disableexcludes=kubernetes "kubelet-$K8S_VER" "kubeadm-$K8S_VER" "kubectl-$K8S_VER" cri-o
cat > /etc/crio/crio.conf.d/10-cgroupfs.conf <<C
[crio.runtime]
cgroup_manager = "cgroupfs"
conmon_cgroup = "pod"
default_runtime = "crun"
C
systemctl enable --now crio chronyd
systemctl enable kubelet
echo "node-prep $role $name: done ($(rpm -q cri-o))"
