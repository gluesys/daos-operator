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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	daosv1alpha1 "github.com/gluesys/daos-operator/api/v1alpha1"
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

	s := Sys{Root: root, Mounts: mounts, Swaps: filepath.Join(root, "noswaps")}
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

// A whole-disk ext4 left on an unmounted NVMe: SPDK setup.sh skipped it and hostprep
// reported bound=0 with nothing to say why (moon, 2026-11-13).
func TestSignatureMakesADeviceInUse(t *testing.T) {
	root := t.TempDir()
	fakeNVMe(t, root, "0000:04:00.0", "nvme", 0, 0, "nvme1n1", false)
	fakeNVMe(t, root, "0000:05:00.0", "nvme", 0, 0, "nvme2n1", false)
	os.MkdirAll(filepath.Join(root, "dev"), 0o755)
	img := make([]byte, 0x10048)
	img[0x438], img[0x439] = 0x53, 0xef
	os.WriteFile(filepath.Join(root, "dev/nvme1n1"), img, 0o644)
	os.WriteFile(filepath.Join(root, "dev/nvme2n1"), make([]byte, 0x10048), 0o644) // blank: still a candidate
	mounts := filepath.Join(root, "mounts")
	os.WriteFile(mounts, nil, 0o644)

	devs, err := Sys{Root: root, Mounts: mounts, Swaps: filepath.Join(root, "noswaps")}.NVMeDevices()
	if err != nil {
		t.Fatal(err)
	}
	_, cand, skipped := Classify(devs)
	if len(cand) != 1 || cand[0].PCI != "0000:05:00.0" {
		t.Fatalf("candidates %+v", cand)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0].InUseWhy, "ext2/3/4 signature") || !strings.Contains(skipped[0].InUseWhy, "wipefs -a /dev/nvme1n1") {
		t.Fatalf("skipped %+v", skipped)
	}
	st := Annotations(Facts{Skipped: skipped}, time.Now())[AnnotationHostPrepStatus]
	if !strings.Contains(st, "inUseReasons") || !strings.Contains(st, "0000:04:00.0") {
		t.Errorf("status %s", st)
	}
}

func TestBlockSignature(t *testing.T) {
	dir := t.TempDir()
	put := func(off int, magic string) string {
		b := make([]byte, 0x10048)
		copy(b[off:], magic)
		p := filepath.Join(dir, fmt.Sprint(off, magic))
		os.WriteFile(p, b, 0o644)
		return p
	}
	gpt4k := put(4096, "EFI PART")
	if b, err := os.ReadFile(gpt4k); err == nil { // protective MBR too: GPT must win
		b[510], b[511] = 0x55, 0xaa
		os.WriteFile(gpt4k, b, 0o644)
	}
	cases := map[string]string{
		gpt4k: "GPT", put(0x438, "\x53\xef"): "ext2/3/4", put(4086, "SWAP-SPACE"): "swap", put(0, "LABELONE"): "LVM2",
		put(0, "XFSB"): "xfs", put(0x10040, "_BHRfS_M"): "btrfs", put(4086, "SWAPSPACE2"): "swap",
		put(512, "LABELONE"): "LVM2", put(512, "EFI PART"): "GPT", put(510, "\x55\xaa"): "MBR", put(0, ""): "",
	}
	for p, want := range cases {
		if got := blockSignature(p); got != want {
			t.Errorf("%s: got %q want %q", filepath.Base(p), got, want)
		}
	}
	if got := blockSignature(filepath.Join(dir, "missing")); got != "" {
		t.Errorf("missing device: %q", got)
	}
}

func TestTHP(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "sys/kernel/mm/transparent_hugepage")
	os.MkdirAll(p, 0o755)
	os.WriteFile(filepath.Join(p, "enabled"), []byte("always [madvise] never\n"), 0o644)
	s := Sys{Root: root}
	if got := s.THP(); got != "madvise" {
		t.Fatalf("THP = %q", got)
	}
	if THPWarning("madvise") == "" || THPWarning("never") != "" || THPWarning("") != "" {
		t.Error("THPWarning")
	}
	if err := s.DisableTHP(); err != nil {
		t.Fatal(err)
	}
	if got := s.THP(); got != "never" { // a plain file keeps what was written
		t.Fatalf("after DisableTHP: %q", got)
	}
}

func TestBindAndKubeletWarnings(t *testing.T) {
	after := []NVMe{{PCI: "a", Driver: "vfio-pci"}, {PCI: "b", Driver: "nvme"}}
	w := BindWarnings([]string{"a", "b"}, after)
	if len(w) != 1 || !strings.Contains(w[0], "b on driver \"nvme\"") {
		t.Errorf("BindWarnings %v", w)
	}
	if KubeletHugepagesWarning(6144, 0) == "" {
		t.Error("kubelet saw none of 6144 pages")
	}
	if KubeletHugepagesWarning(6144, 6144*2<<20) != "" || KubeletHugepagesWarning(0, 0) != "" {
		t.Error("no warning expected")
	}
}

// A disk the host mounted whole, or swaps on, is in use -- not a "signature to wipe" -- even
// though hostprep's own /proc/mounts does not show it (review of !92).
func TestHostMountsAndSwapsWin(t *testing.T) {
	root := t.TempDir()
	fakeNVMe(t, root, "0000:04:00.0", "nvme", 0, 0, "nvme1n1", false)
	fakeNVMe(t, root, "0000:05:00.0", "nvme", 0, 0, "nvme2n1", false)
	os.MkdirAll(filepath.Join(root, "dev"), 0o755)
	img := make([]byte, 0x10048)
	img[0x438], img[0x439] = 0x53, 0xef
	os.WriteFile(filepath.Join(root, "dev/nvme1n1"), img, 0o644)
	sw := make([]byte, 0x10048)
	copy(sw[4086:], "SWAPSPACE2")
	os.WriteFile(filepath.Join(root, "dev/nvme2n1"), sw, 0o644)
	mounts := filepath.Join(root, "mounts")
	os.WriteFile(mounts, []byte("/dev/nvme1n1 /data ext4 rw 0 0\n"), 0o644)
	swaps := filepath.Join(root, "swaps")
	os.WriteFile(swaps, []byte("Filename\tType\tSize\tUsed\tPriority\n/dev/nvme2n1 partition 1000 0 -2\n"), 0o644)
	devs, err := Sys{Root: root, Mounts: mounts, Swaps: swaps}.NVMeDevices()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range devs {
		if !d.InUse || strings.Contains(d.InUseWhy, "wipefs") || !strings.Contains(d.InUseWhy, "mounted") {
			t.Errorf("%s: InUse=%v why=%q", d.PCI, d.InUse, d.InUseWhy)
		}
	}
}

// Two namespaces: the reason of the first one that made the device in use stays.
func TestFirstInUseReasonStays(t *testing.T) {
	root := t.TempDir()
	fakeNVMe(t, root, "0000:04:00.0", "nvme", 0, 0, "nvme1n1", true) // n1 partitioned
	b := filepath.Join(root, "sys/bus/pci/devices/0000:04:00.0/nvme/nvme0/nvme1n2")
	os.MkdirAll(filepath.Join(b, "holders"), 0o755)
	os.MkdirAll(filepath.Join(root, "dev"), 0o755)
	img := make([]byte, 0x10048)
	copy(img, "XFSB")
	os.WriteFile(filepath.Join(root, "dev/nvme1n2"), img, 0o644) // n2 only a signature
	devs, err := Sys{Root: root, Mounts: filepath.Join(root, "none"), Swaps: filepath.Join(root, "none")}.NVMeDevices()
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 1 || devs[0].InUseWhy != "nvme1n1 has partitions" {
		t.Fatalf("%+v", devs)
	}
}

func TestHugepages2M(t *testing.T) {
	root := t.TempDir()
	s := Sys{Root: root}
	if s.Hugepages2M() != -1 {
		t.Error("missing file should be -1")
	}
	d := filepath.Join(root, "sys/kernel/mm/hugepages/hugepages-2048kB")
	os.MkdirAll(d, 0o755)
	os.WriteFile(filepath.Join(d, "nr_hugepages"), []byte("6144\n"), 0o644)
	if s.Hugepages2M() != 6144 {
		t.Error("Hugepages2M")
	}
	if KubeletHugepagesWarning(-1, 0) != "" {
		t.Error("unknown 2M count must not warn")
	}
}

// Native NVMe multipath (Rocky 10, kernel 6.12): the controller lists the path node nvmeXcYnZ,
// while partitions, holders, the mount and the /dev node are on the head nvmeXnZ in /sys/block.
// Found on io500-5/6 (2026-11-19): the LVM root disk was offered for binding.
func TestMultipathPathNodeUsesHead(t *testing.T) {
	root := t.TempDir()
	// OS disk as on io500-5: path node under the controller (the PCI address comes from the
	// controller, the head's device/address is empty), head with p1..p3 and LVM on p3's
	// holders, the head's own holders empty
	fakeNVMe(t, root, "0000:6f:00.0", "nvme", 0, 0, "nvme0c0n1", false)
	head := filepath.Join(root, "sys/block/nvme0n1")
	os.MkdirAll(filepath.Join(head, "holders"), 0o755)
	os.MkdirAll(filepath.Join(head, "device"), 0o755)
	os.WriteFile(filepath.Join(head, "device/address"), nil, 0o644)
	for _, p := range []string{"p1", "p2", "p3"} {
		os.MkdirAll(filepath.Join(head, "nvme0n1"+p, "holders"), 0o755)
	}
	os.MkdirAll(filepath.Join(head, "nvme0n1p3/holders/dm-0"), 0o755)
	// a namespace shared by two controllers: in use through either
	fakeNVMe(t, root, "0000:70:00.0", "nvme", 0, 0, "nvme1c1n1", false)
	fakeNVMe(t, root, "0000:71:00.0", "nvme", 0, 0, "nvme1c2n1", false)
	os.MkdirAll(filepath.Join(root, "sys/block/nvme1n1/holders/md0"), 0o755)
	// an unused multipath disk and a plain (non-multipath) one stay candidates
	fakeNVMe(t, root, "0000:72:00.0", "nvme", 0, 0, "nvme2c3n1", false)
	os.MkdirAll(filepath.Join(root, "sys/block/nvme2n1/holders"), 0o755)
	fakeNVMe(t, root, "0000:73:00.0", "nvme", 0, 0, "nvme3n1", false)
	// a mount of the head is the head's, too
	fakeNVMe(t, root, "0000:74:00.0", "nvme", 0, 0, "nvme4c4n1", false)
	os.MkdirAll(filepath.Join(root, "sys/block/nvme4n1/holders"), 0o755)
	mounts := filepath.Join(root, "mounts")
	os.WriteFile(mounts, []byte("/dev/nvme4n1 /scratch xfs rw 0 0\n"), 0o644)

	devs, err := Sys{Root: root, Mounts: mounts, Swaps: filepath.Join(root, "noswaps")}.NVMeDevices()
	if err != nil {
		t.Fatal(err)
	}
	_, cand, skipped := Classify(devs)
	var got []string
	for _, c := range cand {
		got = append(got, c.PCI)
	}
	if strings.Join(got, ",") != "0000:72:00.0,0000:73:00.0" {
		t.Fatalf("candidates %v, skipped %+v", got, skipped)
	}
	why := map[string]string{}
	for _, s := range skipped {
		why[s.PCI] = s.InUseWhy
	}
	for pci, want := range map[string]string{
		"0000:6f:00.0": "nvme0n1 has partitions",
		"0000:70:00.0": "nvme1n1 has holders",
		"0000:71:00.0": "nvme1n1 has holders",
		"0000:74:00.0": "nvme4n1 is mounted",
	} {
		if !strings.Contains(why[pci], want) {
			t.Errorf("%s: %q, want %q", pci, why[pci], want)
		}
	}
	for _, d := range devs {
		if d.PCI == "0000:6f:00.0" && strings.Join(d.BlockDevs, ",") != "nvme0n1" {
			t.Errorf("block devices %v, want the head", d.BlockDevs)
		}
	}
}

func TestMultipathHead(t *testing.T) {
	for in, want := range map[string]string{"nvme4c4n1": "nvme4n1", "nvme10c12n3": "nvme10n3", "nvme0n1": "", "nvme0n1p1": "", "nvme1c1n1p2": ""} {
		if got := multipathHead(in); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}

// A head whose paths are all down queues the read; the scan must still return, and the
// device must not look blank (bot review of !113).
func TestSignatureReadThatDoesNotFinish(t *testing.T) {
	root := t.TempDir()
	fakeNVMe(t, root, "0000:6f:00.0", "nvme", 0, 0, "nvme0c0n1", false) // stuck head
	os.MkdirAll(filepath.Join(root, "sys/block/nvme0n1/holders"), 0o755)
	fakeNVMe(t, root, "0000:73:00.0", "nvme", 0, 0, "nvme3n1", false) // answers, blank
	mounts := filepath.Join(root, "mounts")
	os.WriteFile(mounts, nil, 0o644)
	block := make(chan struct{})
	defer close(block)
	probe := func(dev string) string {
		if strings.HasSuffix(dev, "/nvme0n1") {
			<-block
		}
		return ""
	}
	start := time.Now()
	devs, err := Sys{Root: root, Mounts: mounts, Swaps: filepath.Join(root, "noswaps"),
		Signature: probe, SignatureTimeout: 100 * time.Millisecond}.NVMeDevices()
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("scan took %s", d)
	}
	_, cand, skipped := Classify(devs)
	if len(cand) != 1 || cand[0].PCI != "0000:73:00.0" {
		t.Fatalf("candidates %+v", cand)
	}
	if len(skipped) != 1 || skipped[0].PCI != "0000:6f:00.0" || !strings.Contains(skipped[0].InUseWhy, "signature check of nvme0n1 did not finish") {
		t.Fatalf("skipped %+v", skipped)
	}
}
