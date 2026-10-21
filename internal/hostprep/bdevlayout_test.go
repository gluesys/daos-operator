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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// daos_nvme.conf as DAOS 2.8 wrote them on lane A (2026-10-19 kdev, 2026-10-21 tiers).
const aioConf = `{"daos_data":{"config":[{"params":{"begin":0,"end":2},"method":"hotplug_busid_range"}]},
"subsystems":[{"subsystem":"bdev","config":[
{"params":{"bdev_io_pool_size":65536,"bdev_io_cache_size":256},"method":"bdev_set_options"},
{"params":{"block_size":0,"name":"AIO_exaci5-3a_0_1_7","filename":"/dev/sdb"},"method":"bdev_aio_create"},
{"params":{"enable":true,"period_us":5000000},"method":"bdev_nvme_set_hotplug"}]}]}`

const nvmeConf = `{"subsystems":[{"subsystem":"bdev","config":[
{"params":{"trtype":"PCIe","name":"Nvme_exaci5-3a_0_2_1","traddr":"0000:00:04.0"},"method":"bdev_nvme_attach_controller"},
{"params":{"trtype":"PCIe","name":"Nvme_exaci5-3a_0_1_6","traddr":"0000:00:03.0"},"method":"bdev_nvme_attach_controller"}]}]}`

func TestReadBdevLayout(t *testing.T) {
	for _, c := range []struct {
		name, conf, want string
		ok               bool
	}{
		{"kdev", aioConf, "aio /dev/sdb", true},
		{"nvme tiers, sorted", nvmeConf, "nvme 0000:00:03.0,0000:00:04.0", true},
		{"vmd is left to DAOS", `{"subsystems":[{"config":[{"method":"vmd_enable"},{"method":"bdev_nvme_attach_controller","params":{"traddr":"5d0505:01:00.0"}}]}]}`, "", false},
		{"no device", `{"subsystems":[{"config":[{"method":"bdev_set_options"}]}]}`, "", false},
		{"not json", `{`, "", false},
	} {
		l, ok := ReadBdevLayout([]byte(c.conf))
		if ok != c.ok || (ok && l.String() != c.want) {
			t.Errorf("%s: got %q %v, want %q %v", c.name, l.String(), ok, c.want, c.ok)
		}
	}
}

func TestParseFormatBdevLayouts(t *testing.T) {
	s := "1=nvme:0000:00:04.0,0000:00:03.0;0=aio:/dev/sdb"
	m, err := ParseBdevLayouts(s)
	if err != nil {
		t.Fatal(err)
	}
	if got := FormatBdevLayouts(m); got != "0=aio:/dev/sdb;1=nvme:0000:00:03.0,0000:00:04.0" {
		t.Errorf("round trip: %q", got)
	}
	for _, bad := range []string{"x=aio:/dev/sdb", "0=aio", "0=aio:", "0"} {
		if _, err := ParseBdevLayouts(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestCheckBdevLayouts(t *testing.T) {
	meta := t.TempDir()
	put := func(engine, conf string) {
		d := filepath.Join(meta, "daos_control", "engine"+engine)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "daos_nvme.conf"), []byte(conf), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	kdev, _ := ParseBdevLayouts("0=aio:/dev/sdb")
	nvme, _ := ParseBdevLayouts("0=nvme:0000:00:03.0,0000:00:04.0")

	// not formatted yet: nothing to compare
	if bad, err := CheckBdevLayouts(meta, kdev); err != nil || len(bad) != 0 {
		t.Fatalf("empty metadata: %v %v", bad, err)
	}
	// the #38 case: tiers formatted, kdev-3rank installed over it
	put("0", nvmeConf)
	bad, err := CheckBdevLayouts(meta, kdev)
	if err != nil || len(bad) != 1 {
		t.Fatalf("changed layout: %v %v", bad, err)
	}
	want := "engine 0: control_metadata was formatted for nvme 0000:00:03.0,0000:00:04.0, the config asks for aio /dev/sdb"
	if bad[0] != want {
		t.Errorf("message:\n got %s\nwant %s", bad[0], want)
	}
	// same layout restarts as before
	if bad, _ := CheckBdevLayouts(meta, nvme); len(bad) != 0 {
		t.Errorf("same layout: %v", bad)
	}
	// a second engine that was never formatted is not reported
	two, _ := ParseBdevLayouts("0=nvme:0000:00:03.0,0000:00:04.0;1=nvme:0000:00:05.0")
	if bad, _ := CheckBdevLayouts(meta, two); len(bad) != 0 {
		t.Errorf("unformatted engine 1: %v", bad)
	}
	put("1", aioConf)
	if bad, _ := CheckBdevLayouts(meta, two); len(bad) != 1 || !strings.HasPrefix(bad[0], "engine 1:") {
		t.Errorf("engine 1 changed: %v", bad)
	}
}
