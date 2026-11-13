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

// Package hostprep discovers what a storage node offers to DAOS (NVMe devices
// and their PCI serials, the RDMA fabric NIC, NUMA) and prepares the host
// (hugepages, optional SPDK binding). It publishes the result as Node
// annotations that the DaosSystem controller consumes (api/v1alpha1/constants.go).
//
// Everything reads sysfs/procfs under a root path so tests can use a fake tree.
package hostprep

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// NVMe is one PCI NVMe controller.
type NVMe struct {
	PCI       string
	Driver    string   // nvme, vfio-pci, uio_pci_generic, "" (unbound)
	DSN       string   // PCI Device Serial Number, 16 hex chars, "" if the device has none
	Numa      int      // -1 when unknown
	BlockDevs []string // nvme0n1 ... while bound to the kernel driver
	InUse     bool     // has partitions, mounts, holders or an on-disk signature -> never a DAOS candidate
	InUseWhy  string   // why InUse, for the node's hostprep-status
}

// BoundForSPDK reports whether the device is already detached from the kernel
// NVMe driver in a way SPDK can use.
func (n NVMe) BoundForSPDK() bool { return n.Driver == "vfio-pci" || n.Driver == "uio_pci_generic" }

// Sys wraps the filesystem roots.
type Sys struct {
	Root   string // "" -> "/"
	Mounts string // "" -> /proc/mounts
}

func (s Sys) path(p string) string {
	if s.Root == "" {
		return p
	}
	return filepath.Join(s.Root, p)
}

const pciClassNVMe = "0x010802"

// NVMeDevices lists NVMe controllers from /sys/bus/pci/devices.
func (s Sys) NVMeDevices() ([]NVMe, error) {
	entries, err := os.ReadDir(s.path("/sys/bus/pci/devices"))
	if err != nil {
		return nil, err
	}
	mounted := s.mountedSources()
	var out []NVMe
	for _, e := range entries {
		d := s.path(filepath.Join("/sys/bus/pci/devices", e.Name()))
		cls, err := os.ReadFile(filepath.Join(d, "class"))
		if err != nil || strings.TrimSpace(string(cls)) != pciClassNVMe {
			continue
		}
		n := NVMe{PCI: e.Name(), Numa: -1}
		if t, err := os.Readlink(filepath.Join(d, "driver")); err == nil {
			n.Driver = filepath.Base(t)
		}
		if b, err := os.ReadFile(filepath.Join(d, "numa_node")); err == nil {
			if v, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				n.Numa = v
			}
		}
		if cfg, err := os.ReadFile(filepath.Join(d, "config")); err == nil {
			n.DSN = parseDSN(cfg)
		}
		blks, _ := filepath.Glob(filepath.Join(d, "nvme", "nvme*", "nvme*n*"))
		for _, b := range blks {
			name := filepath.Base(b)
			n.BlockDevs = append(n.BlockDevs, name)
			parts, _ := filepath.Glob(filepath.Join(b, name+"p*"))
			holders, _ := os.ReadDir(filepath.Join(b, "holders"))
			switch {
			case len(parts) > 0:
				n.InUse, n.InUseWhy = true, name+" has partitions"
			case len(holders) > 0:
				n.InUse, n.InUseWhy = true, name+" has holders (LVM, md, dm)"
			case mounted["/dev/"+name]:
				n.InUse, n.InUseWhy = true, name+" is mounted"
			default:
				// SPDK setup.sh (daos_server nvme prepare) skips a device that still carries
				// a filesystem or partition-table signature and reports success: hostprep then
				// published bound=0 with no error (moon, ext4 left on an unmounted disk, 2026-11-13).
				if sig := blockSignature(s.path("/dev/" + name)); sig != "" {
					n.InUse, n.InUseWhy = true, fmt.Sprintf("%s signature on %s: SPDK will not take it; wipefs -a /dev/%s to give it to DAOS", sig, name, name)
				}
			}
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PCI < out[j].PCI })
	return out, nil
}

// blockSignature names the filesystem, volume or partition-table signature at the start of a
// block device, or "" when there is none (or the device cannot be read). It covers what
// SPDK setup.sh's in-use check sees on a disk that was used before.
func blockSignature(dev string) string {
	f, err := os.Open(dev)
	if err != nil {
		return ""
	}
	defer f.Close()
	b := make([]byte, 0x10048)
	n, _ := f.ReadAt(b, 0)
	b = b[:n]
	has := func(off int, magic string) bool {
		return len(b) >= off+len(magic) && string(b[off:off+len(magic)]) == magic
	}
	switch {
	case has(0x438, "\x53\xef"):
		return "ext2/3/4"
	case has(0, "XFSB"):
		return "xfs"
	case has(0x10040, "_BHRfS_M"):
		return "btrfs"
	case has(4086, "SWAPSPACE2"), has(4086, "SWAP-SPACE"):
		return "swap"
	case has(512, "LABELONE"), has(0, "LABELONE"):
		return "LVM2"
	case has(512, "EFI PART"):
		return "GPT"
	case has(510, "\x55\xaa"):
		return "MBR"
	}
	return ""
}

// THP returns the selected value of /sys/kernel/mm/transparent_hugepage/enabled
// ("always", "madvise" or "never"); daos_server refuses to start unless it is never
// (code 623). "" when the file cannot be read.
func (s Sys) THP() string {
	b, err := os.ReadFile(s.path("/sys/kernel/mm/transparent_hugepage/enabled"))
	if err != nil {
		return ""
	}
	v := string(b)
	if i := strings.Index(v, "["); i >= 0 {
		if j := strings.Index(v[i:], "]"); j > 0 {
			return v[i+1 : i+j]
		}
	}
	return strings.TrimSpace(v)
}

// DisableTHP writes never to /sys/kernel/mm/transparent_hugepage/enabled. It is
// host-wide and lasts until reboot; hostprep re-applies it on every run.
func (s Sys) DisableTHP() error {
	return os.WriteFile(s.path("/sys/kernel/mm/transparent_hugepage/enabled"), []byte("never"), 0o644)
}

// parseDSN walks the PCIe extended capability list (from 0x100) for the Device
// Serial Number capability (ID 3) and returns "%08X%08X" of upper,lower dwords.
// Devices with a 256-byte config space (no extended caps, e.g. emulated NVMe) return "".
func parseDSN(cfg []byte) string {
	off := 0x100
	for i := 0; i < 64 && off != 0 && off+12 <= len(cfg); i++ {
		hdr := binary.LittleEndian.Uint32(cfg[off:])
		if hdr&0xffff == 3 {
			lo := binary.LittleEndian.Uint32(cfg[off+4:])
			hi := binary.LittleEndian.Uint32(cfg[off+8:])
			if lo == 0 && hi == 0 {
				return ""
			}
			return fmt.Sprintf("%08X%08X", hi, lo)
		}
		off = int(hdr >> 20)
	}
	return ""
}

func (s Sys) mountedSources() map[string]bool {
	p := s.Mounts
	if p == "" {
		p = "/proc/mounts"
	}
	out := map[string]bool{}
	f, err := os.Open(p)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if fields := strings.Fields(sc.Text()); len(fields) > 0 {
			out[fields[0]] = true
			// /dev/nvme0n1p1 mounted -> nvme0n1 in use
			if i := strings.LastIndex(fields[0], "p"); i > 0 {
				out[fields[0][:i]] = true
			}
		}
	}
	return out
}

// Iface is a network interface with RDMA capability info.
type Iface struct {
	Name string
	RDMA bool
	Numa int
	IPv4 string
}

// RDMAIfaces lists /sys/class/net entries that have an RDMA device behind them.
func (s Sys) RDMAIfaces(addrOf func(name string) string) ([]Iface, error) {
	entries, err := os.ReadDir(s.path("/sys/class/net"))
	if err != nil {
		return nil, err
	}
	var out []Iface
	for _, e := range entries {
		d := s.path(filepath.Join("/sys/class/net", e.Name(), "device"))
		if _, err := os.Stat(filepath.Join(d, "infiniband")); err != nil {
			continue
		}
		it := Iface{Name: e.Name(), RDMA: true, Numa: -1}
		if b, err := os.ReadFile(filepath.Join(d, "numa_node")); err == nil {
			if v, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				it.Numa = v
			}
		}
		if addrOf != nil {
			it.IPv4 = addrOf(e.Name())
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// SystemIPv4 returns the first IPv4 address of an interface using the OS.
func SystemIPv4(name string) string {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return ""
	}
	addrs, _ := ifc.Addrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
			return ipn.IP.String()
		}
	}
	return ""
}

// PickFabric chooses the fabric NIC: inside cidr when given, otherwise the
// first RDMA interface that has an IPv4 address, otherwise the first RDMA one.
func PickFabric(ifaces []Iface, cidr string) (Iface, error) {
	if cidr != "" {
		_, netw, err := net.ParseCIDR(cidr)
		if err != nil {
			return Iface{}, fmt.Errorf("fabric cidr %q: %w", cidr, err)
		}
		for _, it := range ifaces {
			if ip := net.ParseIP(it.IPv4); ip != nil && netw.Contains(ip) {
				return it, nil
			}
		}
		return Iface{}, fmt.Errorf("no RDMA interface with an address in %s", cidr)
	}
	for _, it := range ifaces {
		if it.IPv4 != "" {
			return it, nil
		}
	}
	if len(ifaces) > 0 {
		return ifaces[0], nil
	}
	return Iface{}, fmt.Errorf("no RDMA-capable interface found")
}

// Hugepages reads /proc/sys/vm/nr_hugepages and raises it to want when lower.
// want <= 0 leaves the host untouched. Returns the value after the call.
func (s Sys) Hugepages(want int) (int, error) {
	p := s.path("/proc/sys/vm/nr_hugepages")
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, err
	}
	cur, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if want <= 0 || cur >= want {
		return cur, nil
	}
	if err := os.WriteFile(p, []byte(strconv.Itoa(want)), 0o644); err != nil {
		return cur, err
	}
	b, err = os.ReadFile(p)
	if err != nil {
		return cur, err
	}
	cur, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	if cur < want {
		// the kernel accepts the write but only allocates what it can find
		// as contiguous 2 MiB pages; low/fragmented memory ends here.
		return cur, fmt.Errorf("allocated %d of %d hugepages (fragmented or low memory)", cur, want)
	}
	return cur, nil
}

// Facts is the discovery result for one node.
type Facts struct {
	Fabric       Iface
	FabricErr    string
	Bound        []NVMe // usable by SPDK now
	Candidates   []NVMe // kernel-bound, unused: safe to hand to SPDK
	Skipped      []NVMe // kernel-bound but in use (OS disk etc.)
	Hugepages    int
	HugepagesErr string
	THP          string   // transparent_hugepage/enabled after this run, "" unknown
	Warnings     []string // what stops DAOS on this node that hostprep did not (or may not) fix
}

// Classify splits NVMe devices into bound / candidates / skipped.
func Classify(devs []NVMe) (bound, candidates, skipped []NVMe) {
	for _, d := range devs {
		switch {
		case d.BoundForSPDK():
			bound = append(bound, d)
		case d.InUse:
			skipped = append(skipped, d)
		default:
			candidates = append(candidates, d)
		}
	}
	return
}

// BindWarnings names the devices nvme prepare was asked to take but left on another
// driver. SPDK setup.sh skips what it considers in use and still exits 0, so without
// this the only trace was bound=0 in the status.
func BindWarnings(requested []string, after []NVMe) []string {
	drv := map[string]string{}
	for _, d := range after {
		if !d.BoundForSPDK() {
			drv[d.PCI] = d.Driver
		}
	}
	var w []string
	for _, pci := range requested {
		if d, left := drv[pci]; left {
			w = append(w, fmt.Sprintf("nvme prepare left %s on driver %q: SPDK skips a device it sees as in use (a signature hostprep does not know, or a process holding it); check it with wipefs/lsblk", pci, d))
		}
	}
	return w
}

// KubeletHugepagesWarning compares the host's 2 MiB pages with what the kubelet
// advertises. The kubelet counts hugepages only when it starts, so pages raised
// later (by hostprep or by hand) stay invisible to the scheduler and the server
// pods wait with "Insufficient hugepages-2Mi" (moon, 2026-11-13).
func KubeletHugepagesWarning(hostPages int, kubeletBytes int64) string {
	want := int64(hostPages) * 2 << 20
	if hostPages <= 0 || kubeletBytes >= want {
		return ""
	}
	return fmt.Sprintf("the kubelet advertises %d MiB of hugepages-2Mi but the host has %d pages (%d MiB): restart the kubelet on this node, it counts hugepages only when it starts",
		kubeletBytes>>20, hostPages, want>>20)
}

// THPWarning says what to do when THP is not never; "" when it is (or unknown).
func THPWarning(thp string) string {
	if thp == "" || thp == "never" {
		return ""
	}
	return "transparent_hugepage is " + thp + ": daos_server refuses to start (code 623); set spec.hostPrep.disableTHP: true, or add transparent_hugepage=never to the kernel command line"
}
