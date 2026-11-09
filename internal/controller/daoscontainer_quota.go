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
	"fmt"
	"regexp"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	daosv1alpha1 "github.com/gluesys/daos-operator/api/v1alpha1"
	"github.com/gluesys/daos-operator/internal/dmg"
)

// Container quota (ADR-005, extended edition). Upstream DAOS 2.8 has no
// per-container limit; the exastor extended edition has the space_limit
// property and reports space usage in `daos cont query`. The operator never
// assumes an edition: containers are created without space_limit (an upstream
// `daos cont create` would refuse the property), and the limit is set with
// `daos cont set-prop` once a query showed the extended edition.

const (
	editionExtended = "extended"
	editionUpstream = "upstream"
)

var ecOclassRE = regexp.MustCompile(`^EC_(\d+)P(\d+)`)

// spaceAmp is the per-mille storage amplification of an EC file object class
// ((k+p)/k), which the extended edition needs to account a logical limit; 0 for
// anything else (the engine derives (rd_fac+1) itself, exact for replication).
// `daos cont create` fills it on its own only when space_limit is given at create.
func spaceAmp(fileOclass string) int {
	m := ecOclassRE.FindStringSubmatch(fileOclass)
	if m == nil {
		return 0
	}
	k, _ := strconv.Atoi(m[1])
	p, _ := strconv.Atoi(m[2])
	if k <= 0 {
		return 0
	}
	return (k + p) * 1000 / k
}

// wantSpaceLimit is the space_limit the spec asks for: its bytes, 0 when the
// field was removed after a limit had been set (back to unlimited), nil when
// there is nothing to do.
func wantSpaceLimit(c *daosv1alpha1.DaosContainer, applied *int64) *int64 {
	if c.Spec.SpaceLimit != nil {
		v := c.Spec.SpaceLimit.Value()
		return &v
	}
	if applied != nil && *applied != 0 {
		zero := int64(0)
		return &zero
	}
	return nil
}

func quotaProps(c *daosv1alpha1.DaosContainer) string {
	want := int64(0)
	if c.Spec.SpaceLimit != nil {
		want = c.Spec.SpaceLimit.Value()
	}
	props := "space_limit:" + strconv.FormatInt(want, 10)
	if amp := spaceAmp(c.Spec.FileOclass); amp > 0 && want > 0 {
		props += ",space_amp:" + strconv.Itoa(amp)
	}
	return props
}

// syncSpaceLimit records the edition and usage of a queried container and
// decides whether space_limit must be (re)set. It returns true when the caller
// should start opQuota.
func (r *DaosContainerReconciler) syncSpaceLimit(c *daosv1alpha1.DaosContainer, status *daosv1alpha1.DaosContainerStatus, info *dmg.ContainerInfo) bool {
	status.Edition, status.SpaceUsedBytes = editionUpstream, nil
	if info.SpaceUsedBytes != nil {
		status.Edition = editionExtended
		used := int64(*info.SpaceUsedBytes)
		status.SpaceUsedBytes = &used
	}
	want := wantSpaceLimit(c, status.AppliedSpaceLimit)
	if want == nil {
		meta.RemoveStatusCondition(&status.Conditions, daosv1alpha1.ConditionSpaceLimit)
		return false
	}
	setQ := func(v metav1.ConditionStatus, reason, msg string) {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: daosv1alpha1.ConditionSpaceLimit, Status: v, Reason: reason, Message: msg, ObservedGeneration: c.Generation})
	}
	prev := meta.FindStatusCondition(status.Conditions, daosv1alpha1.ConditionSpaceLimit)
	if status.Edition != editionExtended {
		if prev == nil || prev.Reason != "Unsupported" {
			r.event(c, corev1.EventTypeWarning, "SpaceLimitUnsupported", "spec.spaceLimit is ignored: this DAOS has no container quota (upstream edition); capacity is enforced per pool only")
		}
		setQ(metav1.ConditionFalse, "Unsupported", "upstream DAOS has no container quota; spec.spaceLimit is not enforced (capacity is per pool)")
		return false
	}
	if status.AppliedSpaceLimit != nil && *status.AppliedSpaceLimit == *want {
		msg := "unlimited"
		if *want > 0 {
			msg = fmt.Sprintf("space_limit %d bytes", *want)
			if status.SpaceUsedBytes != nil {
				msg += fmt.Sprintf(", %d used", *status.SpaceUsedBytes)
			}
		}
		setQ(metav1.ConditionTrue, "Applied", msg)
		return false
	}
	if prev != nil && prev.Reason == "ApplyFailed" && prev.ObservedGeneration == c.Generation {
		return false // retried when the spec changes
	}
	return true
}
