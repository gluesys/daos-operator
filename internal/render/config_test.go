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

package render

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestServerRendersParsableYAML(t *testing.T) {
	numa := int32(1)
	out, err := Server(ServerConfig{
		SystemName: "daos_server", MsReplicas: []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}, Port: 10001,
		Provider: "ofi+verbs;ofi_rxm", NrHugepages: 8192, AllowInsecure: true, TelemetryPort: 9191,
		Engines: []Engine{{Index: 0, Targets: 8, Helpers: 2, FabricIface: "ens2", FabricPort: 31316, PinnedNuma: &numa,
			ScmSizeGiB: 32, Bdevs: []string{"0000:03:00.0", "0000:04:00.0"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not YAML: %v\n%s", err, out)
	}
	for _, k := range []string{"name", "mgmt_svc_replicas", "provider", "engines", "control_metadata"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("missing key %s", k)
		}
	}
	if _, legacy := doc["access_points"]; legacy {
		t.Errorf("server config must use mgmt_svc_replicas, not access_points")
	}
	engines := doc["engines"].([]any)
	st := engines[0].(map[string]any)["storage"].([]any)
	if len(st) != 2 || st[1].(map[string]any)["class"] != "nvme" {
		t.Errorf("unexpected storage tiers: %v", st)
	}
	// test-bed shapes: no SPDK, lowered reservation, data tier on files
	tb, err := Server(ServerConfig{SystemName: "daos_k8s", MsReplicas: []string{"10.0.0.9"}, Port: 10101, Provider: "ofi+tcp",
		NrHugepages: 0, SystemRamReservedGiB: 2, AllowInsecure: true,
		Engines: []Engine{{Index: 0, Targets: 2, FabricIface: "ens18", FabricPort: 31516, ScmSizeGiB: 4,
			BdevClass: "file", BdevSizeGiB: 20, Bdevs: []string{"/var/daos/bdev0"}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"port: 10101", "nr_hugepages: 0", "system_ram_reserved: 2", "class: file", "bdev_size: 20", "fabric_iface_port: 31516"} {
		if !strings.Contains(tb, want) {
			t.Errorf("missing %q in:\n%s", want, tb)
		}
	}
	if strings.Contains(out, "system_ram_reserved") {
		t.Error("system_ram_reserved must be omitted when unset (DAOS default applies)")
	}
	if !strings.Contains(out, "class: nvme") {
		t.Error("nvme must stay the default class")
	}
	if !strings.Contains(out, "telemetry_port: 9191") {
		t.Errorf("telemetry_port missing:\n%s", out)
	}
	if off, _ := Server(ServerConfig{SystemName: "daos_server", MsReplicas: []string{"a"}, Engines: []Engine{{FabricIface: "e", Bdevs: []string{"b"}}}}); strings.Contains(off, "telemetry_port") {
		t.Errorf("telemetry_port must be absent when 0")
	}
	if !strings.Contains(out, "pinned_numa_node: 1") {
		t.Errorf("pinned numa missing")
	}
}

func TestServerRejectsIncompleteEngine(t *testing.T) {
	_, err := Server(ServerConfig{SystemName: "s", MsReplicas: []string{"a"}, Engines: []Engine{{Targets: 8}}})
	if err == nil {
		t.Fatal("expected error for engine without fabric_iface/bdev_list")
	}
}

func TestAgentAndControl(t *testing.T) {
	var a, c map[string]any
	if err := yaml.Unmarshal([]byte(Agent("daos_server", []string{"10.0.0.1"}, 10001, false, []string{"ens18"})), &a); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(Control("daos_server", []string{"n1", "n2"}, 10001, false)), &c); err != nil {
		t.Fatal(err)
	}
	if _, ok := a["access_points"]; !ok {
		t.Error("agent needs access_points")
	}
	if len(c["hostlist"].([]any)) != 2 {
		t.Error("control hostlist wrong")
	}
	// without this the agent's scan on a Kubernetes node also offers cni0/flannel.1
	if got, ok := a["include_fabric_ifaces"].([]any); !ok || len(got) != 1 || got[0] != "ens18" {
		t.Errorf("include_fabric_ifaces = %v, want [ens18]", a["include_fabric_ifaces"])
	}
	var noIfaces map[string]any
	if err := yaml.Unmarshal([]byte(Agent("s", []string{"10.0.0.1"}, 10001, false, nil)), &noIfaces); err != nil {
		t.Fatal(err)
	}
	if _, ok := noIfaces["include_fabric_ifaces"]; ok {
		t.Error("empty iface list must not emit include_fabric_ifaces")
	}
}
