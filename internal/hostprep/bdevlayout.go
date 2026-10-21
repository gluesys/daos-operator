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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Bdev layout check (#38).
//
// With control_metadata set, daos_server writes each engine's SPDK config (daos_nvme.conf)
// into control_metadata at format time only. A restart that finds the metadata formatted
// skips the format and keeps that file as long as it parses (DAOS 2.8 UpgradeBdevConfig);
// it never compares it with the bdev_list of the current config. A reinstall with another
// storage layout over the same node directory therefore starts every engine on the old
// devices, the engines die with "No available SPDK bdevs", and the management service still
// lists the ranks joined from the earlier install (lane A, 2026-10-14 and 2026-10-21).

// BdevLayout is the SPDK side of an engine's bdevs: "aio" (classes kdev and file) or
// "nvme", and the devices (file paths or PCI addresses).
type BdevLayout struct {
	Kind    string
	Devices []string
}

func (l BdevLayout) String() string {
	return l.Kind + " " + strings.Join(l.Devices, ",")
}

func (l BdevLayout) equal(o BdevLayout) bool {
	return l.String() == o.String()
}

// spdkConf is the part of daos_nvme.conf the check reads.
type spdkConf struct {
	Subsystems []struct {
		Config []struct {
			Method string `json:"method"`
			Params struct {
				Filename string `json:"filename"`
				Traddr   string `json:"traddr"`
			} `json:"params"`
		} `json:"config"`
	} `json:"subsystems"`
}

// ReadBdevLayout returns the layout a daos_nvme.conf describes. ok is false for a file the
// check does not understand (VMD, both kinds, no device); such a file is left to DAOS.
func ReadBdevLayout(conf []byte) (l BdevLayout, ok bool) {
	var c spdkConf
	if err := json.Unmarshal(conf, &c); err != nil {
		return l, false
	}
	kinds := map[string]bool{}
	for _, s := range c.Subsystems {
		for _, m := range s.Config {
			switch {
			case strings.Contains(m.Method, "vmd"):
				return l, false
			case m.Method == "bdev_aio_create":
				kinds["aio"] = true
				l.Devices = append(l.Devices, m.Params.Filename)
			case m.Method == "bdev_nvme_attach_controller":
				kinds["nvme"] = true
				l.Devices = append(l.Devices, m.Params.Traddr)
			}
		}
	}
	if len(kinds) != 1 || len(l.Devices) == 0 {
		return BdevLayout{}, false
	}
	for k := range kinds {
		l.Kind = k
	}
	sort.Strings(l.Devices)
	return l, true
}

// ParseBdevLayouts reads the flag form "0=aio:/dev/sdb;1=nvme:0000:00:03.0,0000:00:04.0".
func ParseBdevLayouts(s string) (map[int]BdevLayout, error) {
	out := map[int]BdevLayout{}
	for _, part := range strings.Split(s, ";") {
		if part == "" {
			continue
		}
		idx, rest, ok1 := strings.Cut(part, "=")
		kind, devs, ok2 := strings.Cut(rest, ":")
		i, err := strconv.Atoi(idx)
		if !ok1 || !ok2 || err != nil || devs == "" {
			return nil, fmt.Errorf("bad engine layout %q (want <index>=<aio|nvme>:<dev>[,<dev>...])", part)
		}
		l := BdevLayout{Kind: kind, Devices: strings.Split(devs, ",")}
		sort.Strings(l.Devices)
		out[i] = l
	}
	return out, nil
}

// FormatBdevLayouts is the inverse of ParseBdevLayouts, ordered by engine index.
func FormatBdevLayouts(m map[int]BdevLayout) string {
	idx := make([]int, 0, len(m))
	for i := range m {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	var parts []string
	for _, i := range idx {
		l := m[i]
		parts = append(parts, fmt.Sprintf("%d=%s:%s", i, l.Kind, strings.Join(l.Devices, ",")))
	}
	return strings.Join(parts, ";")
}

// CheckBdevLayouts compares, per engine, the layout the config asks for with the one the
// formatted control_metadata holds (<metaDir>/daos_control/engine<i>/daos_nvme.conf). An
// engine without that file (not formatted yet) or with a file the check does not understand
// passes. It returns one line per engine whose layout changed.
func CheckBdevLayouts(metaDir string, want map[int]BdevLayout) ([]string, error) {
	idx := make([]int, 0, len(want))
	for i := range want {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	var bad []string
	for _, i := range idx {
		b, err := os.ReadFile(filepath.Join(metaDir, "daos_control", fmt.Sprintf("engine%d", i), "daos_nvme.conf"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		have, ok := ReadBdevLayout(b)
		if !ok || have.equal(want[i]) {
			continue
		}
		bad = append(bad, fmt.Sprintf("engine %d: control_metadata was formatted for %s, the config asks for %s", i, have, want[i]))
	}
	return bad, nil
}
