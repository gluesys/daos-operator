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
		Provider: "ofi+verbs;ofi_rxm", NrHugepages: 8192, AllowInsecure: true,
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
	if err := yaml.Unmarshal([]byte(Agent("daos_server", []string{"10.0.0.1"}, 10001, false)), &a); err != nil {
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
}
