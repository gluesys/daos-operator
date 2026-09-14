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
	"strconv"
	"strings"
	"time"

	daosv1alpha1 "gitlab.gluesys.com/exastor/daos-operator/api/v1alpha1"
)

// AnnotationHostPrepStatus carries a JSON summary of the last hostprep run.
const AnnotationHostPrepStatus = "daos.gluesys.com/hostprep-status"

// Annotations turns facts into the Node annotations the controller reads.
// bdev-list = devices already bound for SPDK; candidates are published
// separately so an operator can see what BindNvme=true would take.
func Annotations(f Facts, now time.Time) map[string]string {
	pci := func(ds []NVMe) string {
		var s []string
		for _, d := range ds {
			s = append(s, d.PCI)
		}
		return strings.Join(s, ",")
	}
	var dsn []string
	numa := -1
	for _, d := range f.Bound {
		if d.DSN != "" {
			dsn = append(dsn, d.PCI+"="+d.DSN)
		}
		if numa < 0 && d.Numa >= 0 {
			numa = d.Numa
		}
	}
	if f.Fabric.Name != "" && f.Fabric.Numa >= 0 {
		numa = f.Fabric.Numa
	}
	a := map[string]string{
		daosv1alpha1.AnnotationFabricIface: f.Fabric.Name,
		daosv1alpha1.AnnotationBdevList:    pci(f.Bound),
		daosv1alpha1.AnnotationBdevDSN:     strings.Join(dsn, ","),
		"daos.gluesys.com/nvme-candidates": pci(f.Candidates),
		"daos.gluesys.com/nvme-in-use":     pci(f.Skipped),
	}
	if numa >= 0 {
		a[daosv1alpha1.AnnotationNumaNode] = strconv.Itoa(numa)
	} else {
		a[daosv1alpha1.AnnotationNumaNode] = ""
	}
	st := map[string]any{"time": now.UTC().Format(time.RFC3339), "hugepages": f.Hugepages,
		"bound": len(f.Bound), "candidates": len(f.Candidates), "inUse": len(f.Skipped), "fabricAddr": f.Fabric.IPv4}
	if f.FabricErr != "" {
		st["fabricError"] = f.FabricErr
	}
	if f.HugepagesErr != "" {
		st["hugepagesError"] = f.HugepagesErr
	}
	j, _ := json.Marshal(st)
	a[AnnotationHostPrepStatus] = string(j)
	return a
}
