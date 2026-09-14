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

package controller

import (
	"context"
	"strings"
	"sync"

	"gitlab.gluesys.com/exastor/daos-operator/internal/dmg"
)

// fakeDmg scripts dmg output per argument list ("system query -v", "storage format").
type fakeDmg struct {
	mu     sync.Mutex
	script map[string]*dmg.Result
	calls  []string
	specs  []dmg.RunSpec
}

func (f *fakeDmg) Run(_ context.Context, s dmg.RunSpec) (*dmg.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// system Jobs are scripted by args ("system query -v"); pool/container Jobs by
	// name suffix ("-dmg-create", "-daos-query")
	key := strings.Join(s.Args, " ")
	if i := strings.LastIndex(s.Name, "-dmg-"); i >= 0 && strings.HasPrefix(s.Name, "pool-") {
		key = s.Name[i:]
	}
	if i := strings.LastIndex(s.Name, "-daos-"); i >= 0 && strings.HasPrefix(s.Name, "cont-") {
		key = s.Name[i:]
	}
	f.calls = append(f.calls, key)
	f.specs = append(f.specs, s)
	if r, ok := f.script[key]; ok {
		return r, nil
	}
	return &dmg.Result{}, nil // still running
}

// last returns the most recent RunSpec whose Job name ends with suffix.
func (f *fakeDmg) last(suffix string) *dmg.RunSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.specs) - 1; i >= 0; i-- {
		if strings.HasSuffix(f.specs[i].Name, suffix) {
			s := f.specs[i]
			return &s
		}
	}
	return nil
}

func (f *fakeDmg) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == key {
			n++
		}
	}
	return n
}

func (f *fakeDmg) set(key string, r *dmg.Result) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script[key] = r
}

const (
	dmgUninitialized = `{"response": null, "error": "system is uninitialized (storage format required?)", "status": -1017}`
	dmgUnreachable   = `{"response": null, "error": "unable to contact the DAOS Management Service", "status": -1009}`
	dmgFormatOK      = `{"response": {"host_errors": {}, "host_storage_map": {}}, "error": null, "status": 0}`
	dmgFormatHostErr = `{"response": {"host_errors": {"storage format failed: instance 0: nvme format: DER_IO(-1005)": "10.0.0.2"}, "host_storage_map": {}}, "error": null, "status": 0}`
	dmgMembers       = `{"response": {"members": [
	  {"addr": "10.0.0.1:10001", "state": "joined", "rank": 0, "uuid": "u0", "fault_domain": "/n1"},
	  {"addr": "10.0.0.2:10001", "state": "joined", "rank": 1, "uuid": "u1", "fault_domain": "/n2"}]}, "error": null, "status": 0}`
	dmgMembersAwait = `{"response": {"members": [
	  {"addr": "10.0.0.1:10001", "state": "joined", "rank": 0, "uuid": "u0", "fault_domain": "/n1"},
	  {"addr": "10.0.0.2:10001", "state": "awaitformat", "rank": 1, "uuid": "u1", "fault_domain": "/n2"}]}, "error": null, "status": 0}`
)

// captured on daos_ci (2026-09-14), trimmed
const (
	dmgPoolNotFound = `{"response": null, "error": "unable to find pool service with label \"p1\"", "status": -1025}`
	dmgPoolQuery    = `{"response": {"state": "Ready", "uuid": "8a9ca36d-495a-4d50-a0d2-f111b80d5d9d", "total_targets": 2, "active_targets": 2, "disabled_targets": 0,
	  "rebuild": {"status": 0, "state": "idle"}, "tier_stats": [{"total": 486539264, "free": 443035176, "media_type": "scm"}, {"total": 7520000000, "free": 7456817152, "media_type": "nvme"}],
	  "enabled_ranks": "[0-1]", "disabled_ranks": []}, "error": null, "status": 0}`
	dmgPoolCreate    = `{"response": {"uuid": "8a9ca36d-495a-4d50-a0d2-f111b80d5d9d", "svc_ldr": 0, "svc_reps": [0], "tgt_ranks": [0, 1], "tier_bytes": [644245094, 10093475840]}, "error": null, "status": 0}`
	dmgPoolTooSmall  = `{"response": null, "error": "pool create failed: server: code = 605 description = \"requested NVMe capacity too small (min 1.0 GiB per target)\"", "status": -1025}`
	dmgOK            = `{"response": null, "error": null, "status": 0}`
	daosContNotFound = `{"response": null, "error": "failed to open container: DER_NONEXIST(-1005): The specified entity does not exist", "status": -1005}`
	daosContQuery    = `{"response": {"pool_uuid": "8a9ca36d-495a-4d50-a0d2-f111b80d5d9d", "container_uuid": "e4d6c5b3-efd6-4891-9526-3a263925212d", "container_label": "c1",
	  "redundancy_factor": 1, "container_type": "POSIX", "health": "HEALTHY", "chunk_size": 4194304, "dir_object_class": "RP_2G1", "file_object_class": "RP_2GX"}, "error": null, "status": 0}`
)
