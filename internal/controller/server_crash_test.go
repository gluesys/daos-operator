// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Gluesys Co., Ltd.

package controller

import (
	"gitlab.gluesys.com/exastor/daos-operator/internal/dmg"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A server that exits at start (THP enabled, too little RAM, no space for a file bdev) used to
// show up only as "CrashLoopBackOff"; the reason was in the pod log (CI cluster, 2026-10-04,
// design 13절). The condition must carry the cause.
func TestPodNotReadyMessageCarriesTheCrashCause(t *testing.T) {
	thp := "DAOS Server config loaded from /etc/daos/daos_server.yml\n" +
		"ERROR: failed to create syslogger with priority 24 (severity=NOTICE, facility=DAEMON): Unix syslog delivery error\n" +
		"ERROR: server: code = 623 description = \"transparent hugepage (THP) enabled on storage server, DAOS requires THP to be disabled\"\n" +
		"ERROR: server: code = 623 resolution = \"disable THP by adding 'transparent_hugepage=never'\"\n"
	p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "daos-server-n1-0"}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: thp}},
	}}}}
	msg := podNotReadyMessage(p)
	for _, want := range []string{"CrashLoopBackOff", "code = 623", "transparent hugepage"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
	if strings.Contains(msg, "resolution") {
		t.Errorf("only the first error line is wanted, got %q", msg)
	}

	// no termination message: unchanged wording
	p.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{}
	if got := podNotReadyMessage(p); got != "server pod daos-server-n1-0: CrashLoopBackOff" {
		t.Errorf("without a message: %q", got)
	}
}

func TestCrashCause(t *testing.T) {
	cases := map[string]string{
		"":                                   "",
		"just a line\n":                      "just a line",
		"a\nERROR: first\nERROR: second\n":   "ERROR: first",
		"ERROR: " + strings.Repeat("x", 400): "ERROR: " + strings.Repeat("x", 233) + "…",
	}
	for in, want := range cases {
		if got := crashCause(in); got != want {
			t.Errorf("crashCause(%q) = %q, want %q", in, got, want)
		}
	}
}

// dmg reports a failed format as a top-level "1 host had errors" plus the per-host errors; only
// the count reached the condition, and the fallocate/no-space cause was lost (CI, 2026-10-04).
func TestFormatFailureKeepsHostErrors(t *testing.T) {
	out := `{"response":{"host_errors":{"bdev: code = 302 description = \"NVMe format failed on \\\"/var/daos/bdev0\\\": fallocate \\\"/var/daos/bdev0\\\": no space left on device\"":"192.168.35.31"}},"error":"1 host had errors","status":-1}`
	msg := formatFailure(&dmg.Result{Done: true, Output: out})
	for _, want := range []string{"1 host had errors", "no space left on device", "192.168.35.31"} {
		if !strings.Contains(msg, want) {
			t.Errorf("formatFailure: %q lacks %q", msg, want)
		}
	}
}
