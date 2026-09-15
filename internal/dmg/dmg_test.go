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
		"system is uninitialized (storage format required?)":                                         ErrUnformatted, // system.ErrUninitialized
		"raft service unavailable (not started yet?)":                                                ErrUnformatted, // system.ErrRaftUnavail
		"unable to contact the DAOS Management Service":                                              ErrUnreachable,
		"dial tcp 10.0.0.1:10001: connect: connection refused":                                       ErrUnreachable,
		"unable to find pool service with label \"optest\"":                                          ErrNotFound,
		"failed to connect to pool: DER_NONEXIST(-1005): The specified entity does not exist":        ErrNotFound,
		"pool create failed: server: code = 605 description = \"requested NVMe capacity too small\"": ErrOther,
	}
	for msg, want := range cases {
		if got := Classify(msg); got != want {
			t.Errorf("%q: got %d want %d", msg, got, want)
		}
	}
}

// captured on kind (2026-09-15): pod logs interleave the client log, the JSON and the trailing ERROR line
const daosUnreachLog = `2026/09/14 15:27:49.533565 daos-dev-control-plane DAOS[28/28/0] mgmt ERR  src/mgmt/cli_mgmt.c:374 get_attach_info() GetAttachInfo((null)) failed: DER_UNREACH(-1006): 'Unreachable node'
{
  "response": null,
  "error": "failed to initialize DAOS API: DER_UNREACH(-1006): Unreachable node",
  "status": -1006
}
ERROR: daos: failed to initialize DAOS API: DER_UNREACH(-1006): Unreachable node
`

func TestParseTrailingErrorLine(t *testing.T) {
	e, err := Parse(daosUnreachLog)
	if err != nil {
		t.Fatal(err)
	}
	if e.Error == nil || Classify(*e.Error) != ErrUnreachable || e.Status != -1006 {
		t.Fatalf("%+v", e)
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

// captured on daos_ci (2026-09-14): dmg -j pool query nvme_pool (trimmed)
const poolQueryOK = `{"response": {"query_mask": "disabled_engines,rebuild,space", "state": "Ready",
  "uuid": "8a9ca36d-495a-4d50-a0d2-f111b80d5d9d", "total_targets": 1, "active_targets": 1, "total_engines": 1, "disabled_targets": 0,
  "rebuild": {"status": 0, "state": "idle", "derived_state": "idle"},
  "tier_stats": [{"total": 486539264, "free": 443035176, "media_type": "scm"}, {"total": 7520000000, "free": 7456817152, "media_type": "nvme"}],
  "disabled_ranks": [], "enabled_ranks": "[0-1,3]"}, "error": null, "status": 0}`

// captured on daos_ci (2026-09-14): daos -j cont query nvme_pool nixltest
const contQueryOK = `{"response": {"pool_uuid": "8a9ca36d-495a-4d50-a0d2-f111b80d5d9d", "container_uuid": "e4d6c5b3-efd6-4891-9526-3a263925212d",
  "container_label": "nixltest", "redundancy_factor": 0, "num_handles": 1, "container_type": "POSIX", "health": "HEALTHY",
  "chunk_size": 1048576, "dir_object_class": "S1", "file_object_class": "S1"}, "error": null, "status": 0}`

func TestPoolAndContainerParsers(t *testing.T) {
	e, err := Parse(poolQueryOK)
	if err != nil {
		t.Fatal(err)
	}
	p, err := PoolQuery(e)
	if err != nil || p.State != "Ready" || p.UUID == "" || p.Rebuild.State != "idle" {
		t.Fatalf("%+v %v", p, err)
	}
	if tot, free := p.Totals(); tot != 486539264+7520000000 || free != 443035176+7456817152 {
		t.Errorf("totals %d %d", tot, free)
	}
	if r := Ranks(p.EnabledRanks); len(r) != 3 || r[0] != 0 || r[1] != 1 || r[2] != 3 {
		t.Errorf("ranks from range string: %v", r)
	}
	if r := Ranks(p.DisabledRanks); r != nil {
		t.Errorf("empty array -> nil, got %v", r)
	}
	if r := Ranks([]byte(`[2,0]`)); len(r) != 2 || r[0] != 0 {
		t.Errorf("array: %v", r)
	}
	ce, _ := Parse(`{"response": {"uuid": "11111111-2222-3333-4444-555555555555", "svc_ldr": 0, "svc_reps": [0], "tgt_ranks": [0, 1]}, "error": null, "status": 0}`)
	u, ranks, err := PoolCreate(ce)
	if err != nil || u != "11111111-2222-3333-4444-555555555555" || len(ranks) != 2 {
		t.Fatalf("%s %v %v", u, ranks, err)
	}
	e, _ = Parse(contQueryOK)
	c, err := ContainerQuery(e)
	if err != nil || c.UUID == "" || c.Type != "POSIX" || c.Health != "HEALTHY" || c.ChunkSize != 1048576 {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestWithACLFile(t *testing.T) {
	cmd := []string{"dmg", "-o", "/etc/daos/daos_control.yml", "-j", "pool", "create", "-a", "/tmp/acl", "p1"}
	if got := WithACLFile(nil, "/tmp/acl", cmd); len(got) != len(cmd) {
		t.Fatal("no entries must not wrap")
	}
	got := WithACLFile([]string{"A::OWNER@:rw", "A:G:GROUP@:rw"}, "/tmp/acl", cmd)
	if got[0] != "bash" || got[1] != "-c" {
		t.Fatal(got)
	}
	want := `printf '%s\n' 'A::OWNER@:rw' 'A:G:GROUP@:rw' > '/tmp/acl' && exec 'dmg' '-o' '/etc/daos/daos_control.yml' '-j' 'pool' 'create' '-a' '/tmp/acl' 'p1'`
	if got[2] != want {
		t.Fatalf("\n got %s\nwant %s", got[2], want)
	}
}

func TestRankOpBothShapes(t *testing.T) {
	// exclude/reintegrate: member results (no json tags on most fields in DAOS 2.8)
	e, err := Parse(`{"response": {"Results": [{"Addr": "10.0.0.2:10001", "Rank": 1, "Action": "exclude", "Errored": false, "Msg": "", "state": "adminexcluded"}]}, "error": null, "status": 0}`)
	if err != nil {
		t.Fatal(err)
	}
	r, err := RankOp(e)
	if err != nil || len(r) != 1 || r[0].Rank != 1 || r[0].State != "adminexcluded" || r[0].Errored {
		t.Fatalf("%+v %v", r, err)
	}
	// drain: per-pool results
	e, _ = Parse(`{"response": {"responses": [{"id": "kv", "results": [{"rank": 2, "errored": true, "msg": "pool busy"}, {"rank": 1, "errored": false, "msg": ""}]}]}, "error": null, "status": 0}`)
	r, err = RankOp(e)
	if err != nil || len(r) != 2 || r[0].Rank != 1 || r[1].Rank != 2 || !r[1].Errored || r[1].Pool != "kv" {
		t.Fatalf("%+v %v", r, err)
	}
	e, _ = Parse(`{"response": null, "error": null, "status": 0}`)
	if r, err := RankOp(e); err != nil || r != nil {
		t.Fatalf("empty response: %+v %v", r, err)
	}
}
