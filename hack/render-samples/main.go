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

// render-samples writes the configurations the operator would render for a few
// representative systems, so they can be checked against the real DAOS 2.8
// binaries (exastor/daos-images scripts/validate-config.sh). It uses the same
// internal/render package the controller uses -- no duplicated templates.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/utils/ptr"

	"gitlab.gluesys.com/exastor/daos-operator/internal/render"
)

func main() {
	out := flag.String("out", "dist/render", "directory to write the sample configurations to")
	flag.Parse()
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fatal(err)
	}

	servers := map[string]render.ServerConfig{
		// what a Phase 0 two-host test bed gets: one engine, ram scm, MD-on-SSD roles
		"server-single-engine.yml": {
			Label: "daos-dev", SystemName: "daos_server", MsReplicas: []string{"10.0.0.1"}, Port: 10001,
			Provider: "ofi+verbs;ofi_rxm", NrHugepages: 8192, AllowInsecure: true, TelemetryPort: 9191,
			Engines: []render.Engine{{Index: 0, Targets: 8, Helpers: 2, FabricIface: "ib0", FabricPort: 31316,
				ScmSizeGiB: 32, Bdevs: []string{"0000:00:03.0", "0000:00:04.0"}}},
		},
		// two engines pinned to sockets, TLS on, three management replicas
		"server-two-engines-tls.yml": {
			Label: "daos-prod", SystemName: "daos_server", MsReplicas: []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}, Port: 10001,
			Provider: "ofi+verbs;ofi_rxm", NrHugepages: 16384, AllowInsecure: false, TelemetryPort: 9191,
			Engines: []render.Engine{
				{Index: 0, Targets: 16, Helpers: 4, FabricIface: "ib0", FabricPort: 31316, PinnedNuma: ptr.To(int32(0)),
					ScmSizeGiB: 64, Bdevs: []string{"0000:81:00.0", "0000:82:00.0", "0000:83:00.0", "0000:84:00.0"}},
				{Index: 1, Targets: 16, Helpers: 4, FabricIface: "ib1", FabricPort: 31416, PinnedNuma: ptr.To(int32(1)),
					ScmSizeGiB: 64, Bdevs: []string{"0000:c1:00.0", "0000:c2:00.0", "0000:c3:00.0", "0000:c4:00.0"}},
			},
		},
		// a system that is not called daos_server (spec.systemName). Addresses are
		// TEST-NET-1 (RFC 5737): a sample must never point at a live system.
		"server-custom-system-name.yml": {
			Label: "daos-flexa", SystemName: "daos_flexa", MsReplicas: []string{"192.0.2.10"}, Port: 10001,
			Provider: "ofi+verbs;ofi_rxm", NrHugepages: 2048, AllowInsecure: true,
			Engines: []render.Engine{{Index: 0, Targets: 1, Helpers: 0, FabricIface: "ib0", FabricPort: 31316,
				ScmSizeGiB: 4, Bdevs: []string{"0000:00:03.0"}}},
		},
	}
	// test-bed shapes without SPDK (#23): no hugepages, no VFIO, lowered system
	// reservation, data tier on kernel devices or files
	servers["server-kdev-testbed.yml"] = render.ServerConfig{
		Label: "daos-k8s", SystemName: "daos_k8s", MsReplicas: []string{"192.0.2.22"}, Port: 10101,
		Provider: "ofi+tcp", NrHugepages: 0, AllowInsecure: true, SystemRamReservedGiB: 2,
		Engines: []render.Engine{{Index: 0, Targets: 2, Helpers: 0, FabricIface: "ens18", FabricPort: 31516,
			ScmSizeGiB: 4, BdevClass: "kdev", Bdevs: []string{"/dev/sdb", "/dev/sdc"}}},
	}
	servers["server-file-testbed.yml"] = render.ServerConfig{
		Label: "daos-k8s", SystemName: "daos_k8s", MsReplicas: []string{"192.0.2.22"}, Port: 10101,
		Provider: "ofi+tcp", NrHugepages: 0, AllowInsecure: true, SystemRamReservedGiB: 2,
		Engines: []render.Engine{{Index: 0, Targets: 2, Helpers: 0, FabricIface: "ens18", FabricPort: 31516,
			ScmSizeGiB: 4, BdevClass: "file", BdevSizeGiB: 20, Bdevs: []string{"/var/daos/bdev0"}}},
	}
	for name, cfg := range servers {
		yml, err := render.Server(cfg)
		if err != nil {
			fatal(fmt.Errorf("%s: %w", name, err))
		}
		write(filepath.Join(*out, name), yml)
	}
	write(filepath.Join(*out, "agent.yml"), render.Agent("daos_server", []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}, 10001, false, nil))
	write(filepath.Join(*out, "agent-insecure.yml"), render.Agent("daos_flexa", []string{"192.0.2.10"}, 10001, true, []string{"ens18"}))
	write(filepath.Join(*out, "control.yml"), render.Control("daos_server", []string{"10.0.0.1", "10.0.0.2"}, 10001, false))
	write(filepath.Join(*out, "control-insecure.yml"), render.Control("daos_flexa", []string{"192.0.2.10"}, 10001, true))
	fmt.Printf("wrote %d configurations to %s\n", len(servers)+4, *out)
}

func write(path, content string) {
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "render-samples:", err)
	os.Exit(1)
}
