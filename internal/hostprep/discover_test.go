/*
SPDX-License-Identifier: Apache-2.0
Copyright 2026 Gluesys Co., Ltd.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package hostprep

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	daosv1alpha1 "gitlab.gluesys.com/exastor/daos-operator/api/v1alpha1"
)

func fakeNVMe(t *testing.T, root, pci, driver string, dsnHi, dsnLo uint32, blk string, part bool) {
	d := filepath.Join(root, "sys/bus/pci/devices", pci)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(d, "class"), []byte("0x010802\n"), 0o644)
	os.WriteFile(filepath.Join(d, "numa_node"), []byte("1\n"), 0o644)
	if driver != "" {
		os.Symlink("../../../../bus/pci/drivers/"+driver, filepath.Join(d, "driver"))
	}
	cfg := make([]byte, 4096)
	if dsnHi != 0 || dsnLo != 0 {
		// one extended capability at 0x100: ID 3 (DSN), next = 0
		binary.LittleEndian.PutUint32(cfg[0x100:], 3)
		binary.LittleEndian.PutUint32(cfg[0x104:], dsnLo)
		binary.LittleEndian.PutUint32(cfg[0x108:], dsnHi)
	} else {
		cfg = cfg[:256]
	}
	os.WriteFile(filepath.Join(d, "config"), cfg, 0o644)
	if blk != "" {
		b := filepath.Join(d, "nvme", "nvme0", blk)
		os.MkdirAll(filepath.Join(b, "holders"), 0o755)
		if part {
			os.MkdirAll(filepath.Join(b, blk+"p1"), 0o755)
		}
	}
}

func TestDiscoverClassifyAnnotate(t *testing.T) {
	root := t.TempDir()
	fakeNVMe(t, root, "0000:03:00.0", "vfio-pci", 0x6479A701, 0xA8C0D000, "", false) // bound, real DSN
	fakeNVMe(t, root, "0000:04:00.0", "nvme", 0, 0, "nvme1n1", false)                // unused kernel NVMe -> candidate
	fakeNVMe(t, root, "0000:05:00.0", "nvme", 0, 0, "nvme2n1", true)                 // partitioned -> in use
	fakeNVMe(t, root, "0000:00:03.0", "uio_pci_generic", 0, 0, "", false)            // testbed VM style, 256B config
	os.MkdirAll(filepath.Join(root, "sys/class/net/ib0/device/infiniband"), 0o755)
	os.WriteFile(filepath.Join(root, "sys/class/net/ib0/device/numa_node"), []byte("0"), 0o644)
	os.MkdirAll(filepath.Join(root, "sys/class/net/eth0/device"), 0o755)
	os.MkdirAll(filepath.Join(root, "proc/sys/vm"), 0o755)
	os.WriteFile(filepath.Join(root, "proc/sys/vm/nr_hugepages"), []byte("1024\n"), 0o644)
	mounts := filepath.Join(root, "mounts")
	os.WriteFile(mounts, []byte("/dev/sda1 / ext4 rw 0 0\n"), 0o644)

	s := Sys{Root: root, Mounts: mounts}
	devs, err := s.NVMeDevices()
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 4 {
		t.Fatalf("want 4 devices, got %d", len(devs))
	}
	if devs[1].DSN != "6479A701A8C0D000" { // sorted by PCI: 00:03.0, 03:00.0, 04:00.0, 05:00.0
		t.Errorf("DSN parse: %q", devs[1].DSN)
	}
	if devs[0].DSN != "" || !devs[0].BoundForSPDK() {
		t.Errorf("uio device: dsn=%q bound=%v", devs[0].DSN, devs[0].BoundForSPDK())
	}
	bound, cand, skipped := Classify(devs)
	if len(bound) != 2 || len(cand) != 1 || len(skipped) != 1 || cand[0].PCI != "0000:04:00.0" {
		t.Fatalf("classify: bound=%d cand=%d skipped=%d", len(bound), len(cand), len(skipped))
	}
	ifs, err := s.RDMAIfaces(func(n string) string {
		if n == "ib0" {
			return "172.28.136.184"
		}
		return ""
	})
	if err != nil || len(ifs) != 1 || ifs[0].Name != "ib0" {
		t.Fatalf("rdma ifaces: %v %v", ifs, err)
	}
	if _, err := PickFabric(ifs, "10.0.0.0/8"); err == nil {
		t.Error("cidr without match must fail")
	}
	fab, err := PickFabric(ifs, "172.28.136.0/24")
	if err != nil || fab.Name != "ib0" {
		t.Fatalf("pick fabric: %v %v", fab, err)
	}
	hp, err := s.Hugepages(2048)
	if err != nil || hp != 2048 {
		t.Fatalf("hugepages raise: %d %v", hp, err)
	}
	if hp, _ = s.Hugepages(0); hp != 2048 {
		t.Fatalf("hugepages 0 must not touch: %d", hp)
	}
	ann := Annotations(Facts{Fabric: fab, Bound: bound, Candidates: cand, Skipped: skipped, Hugepages: hp}, time.Unix(0, 0))
	if ann[daosv1alpha1.AnnotationBdevList] != "0000:00:03.0,0000:03:00.0" {
		t.Errorf("bdev-list: %q", ann[daosv1alpha1.AnnotationBdevList])
	}
	if ann[daosv1alpha1.AnnotationBdevDSN] != "0000:03:00.0=6479A701A8C0D000" {
		t.Errorf("bdev-dsn: %q", ann[daosv1alpha1.AnnotationBdevDSN])
	}
	if ann[daosv1alpha1.AnnotationFabricIface] != "ib0" || ann[daosv1alpha1.AnnotationNumaNode] != "0" {
		t.Errorf("fabric/numa: %q %q", ann[daosv1alpha1.AnnotationFabricIface], ann[daosv1alpha1.AnnotationNumaNode])
	}
	if ann["daos.gluesys.com/nvme-in-use"] != "0000:05:00.0" {
		t.Errorf("in-use: %q", ann["daos.gluesys.com/nvme-in-use"])
	}
	// a node with nothing must not invent numa 0 from the zero-value Iface
	empty := Annotations(Facts{FabricErr: "none"}, time.Unix(0, 0))
	if empty[daosv1alpha1.AnnotationNumaNode] != "" || empty[daosv1alpha1.AnnotationFabricIface] != "" {
		t.Errorf("empty facts must yield empty numa/fabric, got %q/%q", empty[daosv1alpha1.AnnotationNumaNode], empty[daosv1alpha1.AnnotationFabricIface])
	}
}
