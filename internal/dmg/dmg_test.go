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

package dmg

import "testing"

// captured on daos_ci (DAOS 2.8.0, 2026-09-14): dmg -j system query -v
const queryOK = `{
  "response": {
    "members": [
      {"addr": "127.0.0.100:10001", "state": "joined", "fault_domain": "/flexa_3423_1-a", "rank": 1, "uuid": "62210c5f-2409-470c-8ab1-53357b7d2215", "info": ""},
      {"addr": "127.0.0.100:10001", "state": "joined", "fault_domain": "/flexa_3423_1-a", "rank": 0, "uuid": "8649295d-c542-4174-892a-bbd5c7d3b166", "info": ""}
    ],
    "providers": ["ofi+verbs;ofi_rxm"]
  },
  "error": null,
  "status": 0
}`

func TestSystemQuery(t *testing.T) {
	e, err := Parse("DEBUG some log line\n" + queryOK)
	if err != nil {
		t.Fatal(err)
	}
	if e.Error != nil || e.Status != 0 {
		t.Fatalf("envelope: %+v", e)
	}
	m, err := SystemQuery(e)
	if err != nil || len(m) != 2 || m[0].Rank != 0 || m[1].State != "joined" || Host(m[0].Addr) != "127.0.0.100" {
		t.Fatalf("members: %+v err=%v", m, err)
	}
}

func TestClassify(t *testing.T) {
	cases := map[string]ErrorKind{
		"": ErrNone,
		"system is uninitialized (storage format required?)":   ErrUnformatted, // system.ErrUninitialized
		"raft service unavailable (not started yet?)":          ErrUnformatted, // system.ErrRaftUnavail
		"unable to contact the DAOS Management Service":        ErrUnreachable,
		"dial tcp 10.0.0.1:10001: connect: connection refused": ErrUnreachable,
		"pool not found": ErrOther,
	}
	for msg, want := range cases {
		if got := Classify(msg); got != want {
			t.Errorf("%q: got %d want %d", msg, got, want)
		}
	}
}

func TestErrorEnvelopeAndFormat(t *testing.T) {
	e, err := Parse(`{"response": null, "error": "system is uninitialized (storage format required?)", "status": -1017}`)
	if err != nil || e.Error == nil || Classify(*e.Error) != ErrUnformatted {
		t.Fatalf("%+v %v", e, err)
	}
	if _, err := Parse("ERROR: dmg: no config"); err == nil {
		t.Fatal("expected error for non-JSON output")
	}
	f, err := Parse(`{"response": {"host_errors": {"storage format failed: instance 0: already formatted": "10.0.0.[1-2]"}, "host_storage_map": {}}, "error": null, "status": 0}`)
	if err != nil {
		t.Fatal(err)
	}
	he, err := StorageFormat(f)
	if err != nil || len(he) != 1 || he["storage format failed: instance 0: already formatted"] != "10.0.0.[1-2]" {
		t.Fatalf("%v %v", he, err)
	}
}
