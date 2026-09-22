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
	"strings"
	"testing"
)

func TestJobNameFitsLabels(t *testing.T) {
	short := jobName("cont", "default/c1", "daos-query")
	if short != "cont-default-c1-daos-query" {
		t.Fatalf("short names must stay readable: %s", short)
	}
	// a PV name in a namespace is what broke this in the field (2026-09-22)
	long := jobName("cont", "daos-system/pvc-64a8aefd-33e9-42b3-968c-5c4e83a3a56c", "daos-query")
	if len(long) > 63 {
		t.Fatalf("%d chars: %s", len(long), long)
	}
	if !strings.HasPrefix(long, "cont-daos-system-pvc-") || !strings.HasSuffix(long, "-daos-query") {
		t.Errorf("keep it recognisable: %s", long)
	}
	other := jobName("cont", "daos-system/pvc-64a8aefd-33e9-42b3-968c-5c4e83a3a56d", "daos-query")
	if other == long {
		t.Error("different owners must get different job names")
	}
	if strings.Contains(long, "--") {
		t.Errorf("no dangling separator: %s", long)
	}
}
