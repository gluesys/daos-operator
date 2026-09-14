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
	key := strings.Join(s.Args, " ")
	f.calls = append(f.calls, key)
	f.specs = append(f.specs, s)
	if r, ok := f.script[key]; ok {
		return r, nil
	}
	return &dmg.Result{}, nil // still running
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
