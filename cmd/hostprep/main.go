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

// hostprep runs on every storage node as a privileged DaemonSet pod. It
// discovers NVMe (with PCI serials), the RDMA fabric NIC and NUMA, raises
// hugepages, optionally hands unused NVMe to SPDK, and publishes the result as
// Node annotations for the DaosSystem controller. It never formats, wipes or
// touches a device that has partitions, mounts or holders.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"gitlab.gluesys.com/exastor/daos-operator/internal/hostprep"
)

func main() {
	var (
		nodeName   = flag.String("node", os.Getenv("NODE_NAME"), "node to annotate (env NODE_NAME)")
		bind       = flag.Bool("bind-nvme", os.Getenv("DAOS_HOSTPREP_BIND_NVME") == "true", "unbind unused NVMe from the kernel for SPDK (daos_server nvme prepare)")
		cidr       = flag.String("fabric-cidr", os.Getenv("DAOS_HOSTPREP_FABRIC_CIDR"), "pick the RDMA NIC with an address in this IPv4 CIDR")
		hugepages  = flag.Int("hugepages", envInt("DAOS_HOSTPREP_HUGEPAGES", 0), "raise vm.nr_hugepages to at least this (0 = leave)")
		interval   = flag.Duration("interval", envDur("DAOS_HOSTPREP_INTERVAL", 5*time.Minute), "rediscovery interval (0 = run once)")
		sysRoot    = flag.String("sys-root", os.Getenv("DAOS_HOSTPREP_SYS_ROOT"), "prefix for /sys and /proc (host mounts); empty = /")
		daosServer = flag.String("daos-server", "daos_server", "daos_server binary used for nvme prepare")
		kubeconfig = flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "kubeconfig (empty = in-cluster)")
	)
	flag.Parse()
	if *nodeName == "" {
		fmt.Fprintln(os.Stderr, "NODE_NAME is required")
		os.Exit(2)
	}
	cs, err := newClient(*kubeconfig)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kube client:", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	sys := hostprep.Sys{Root: *sysRoot}
	for {
		if err := runOnce(ctx, cs, sys, *nodeName, *bind, *cidr, *hugepages, *daosServer); err != nil {
			fmt.Fprintln(os.Stderr, "hostprep:", err)
		}
		if *interval <= 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(*interval):
		}
	}
}

func runOnce(ctx context.Context, cs kubernetes.Interface, sys hostprep.Sys, node string, bind bool, cidr string, hugepages int, daosServer string) error {
	f := hostprep.Facts{}
	hp, err := sys.Hugepages(hugepages)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hugepages:", err)
		f.HugepagesErr = err.Error()
	}
	f.Hugepages = hp
	devs, err := sys.NVMeDevices()
	if err != nil {
		return fmt.Errorf("nvme scan: %w", err)
	}
	f.Bound, f.Candidates, f.Skipped = hostprep.Classify(devs)
	if bind && len(f.Candidates) > 0 {
		var pcis []string
		for _, d := range f.Candidates {
			pcis = append(pcis, d.PCI)
		}
		// Only the explicit allow-list is touched; in-use devices are never on it.
		args := []string{"nvme", "prepare", "--target-user", "root"}
		if hugepages > 0 {
			args = append(args, "-p", fmt.Sprint(hugepages))
		}
		args = append(args, strings.Join(pcis, ","))
		fmt.Fprintln(os.Stderr, "binding for SPDK:", strings.Join(pcis, ","))
		if out, err := exec.CommandContext(ctx, daosServer, args...).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "nvme prepare failed: %v\n%s\n", err, out)
		} else if devs, err = sys.NVMeDevices(); err == nil {
			f.Bound, f.Candidates, f.Skipped = hostprep.Classify(devs)
		}
	}
	ifs, err := sys.RDMAIfaces(hostprep.SystemIPv4)
	if err != nil {
		f.FabricErr = err.Error()
	} else if f.Fabric, err = hostprep.PickFabric(ifs, cidr); err != nil {
		f.FabricErr = err.Error()
	}
	ann := hostprep.Annotations(f, time.Now())
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": ann}})
	if _, err := cs.CoreV1().Nodes().Patch(ctx, node, types.MergePatchType, patch, metav1PatchOptions()); err != nil {
		return fmt.Errorf("annotate node %s: %w", node, err)
	}
	fmt.Fprintf(os.Stderr, "annotated %s: fabric=%q bound=%d candidates=%d inUse=%d hugepages=%d\n",
		node, f.Fabric.Name, len(f.Bound), len(f.Candidates), len(f.Skipped), f.Hugepages)
	return nil
}

func newClient(kubeconfig string) (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		if kubeconfig == "" {
			return nil, err
		}
		if cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig); err != nil {
			return nil, err
		}
	}
	return kubernetes.NewForConfig(cfg)
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func metav1PatchOptions() metav1.PatchOptions {
	return metav1.PatchOptions{FieldManager: "daos-hostprep"}
}
