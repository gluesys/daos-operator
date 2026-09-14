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
	InUse     bool     // has partitions, mounts or holders -> never a DAOS candidate
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
			if len(parts) > 0 || len(holders) > 0 || mounted["/dev/"+name] {
				n.InUse = true
			}
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PCI < out[j].PCI })
	return out, nil
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
