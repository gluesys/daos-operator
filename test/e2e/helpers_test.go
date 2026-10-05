//go:build e2e

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Gluesys Co., Ltd.

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// run executes a command and fails the spec with its output when it does not succeed.
func run(name string, args ...string) string {
	out, err := runE(name, args...)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "%s %s\n%s", name, strings.Join(args, " "), out)
	return out
}

func runE(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = repoRoot
	b, err := cmd.CombinedOutput()
	return string(b), err
}

// kubectl runs kubectl in the suite namespace and fails on error.
func kubectl(args ...string) string {
	out, err := kubectlE(args...)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "kubectl %s\n%s", strings.Join(args, " "), out)
	return strings.TrimSpace(out)
}

func kubectlE(args ...string) (string, error) {
	return runE("kubectl", append([]string{"-n", ns}, args...)...)
}

// jsonpath returns a jsonpath field of an object, "" when absent or unreadable.
func jsonpath(kind, name, path string) string {
	out, err := kubectlE("get", kind, name, "-o", "jsonpath={"+path+"}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func condStatus(t string) string {
	return jsonpath("daossystem", sysName, fmt.Sprintf(`.status.conditions[?(@.type=="%s")].status`, t))
}

func condReason(t string) string {
	return jsonpath("daossystem", sysName, fmt.Sprintf(`.status.conditions[?(@.type=="%s")].reason`, t))
}

// systemHealthy is the composite the suite calls "ready": Ready, a leader, and every
// rank joined. Returned as a string so a failing Eventually/Consistently shows why.
func systemHealthy() string {
	return fmt.Sprintf("Ready=%s ManagementService=%s ranksJoined=%s",
		condStatus("Ready"), condStatus("ManagementService"), jsonpath("daossystem", sysName, ".status.ranksJoined"))
}

// healthyWant is systemHealthy's value for a healthy system of E2E_RANKS ranks (default 1).
func healthyWant() string {
	return "Ready=True ManagementService=True ranksJoined=" + envOr("E2E_RANKS", "1")
}

// poolSize is 8 GiB per rank. DAOS wants at least 1 GiB per target (code 605 "requested NVMe
// capacity too small"), and a fixed 8 GiB fell short on 3 ranks x 4 targets (kdev-3rank, 2026-10-05).
func poolSize() string {
	n, err := strconv.Atoi(envOr("E2E_RANKS", "1"))
	if err != nil || n < 1 {
		n = 1
	}
	return fmt.Sprintf("%dGi", 8*n)
}

// waitReadyStable waits for systemHealthy and then requires it to hold for a minute.
func waitReadyStable(timeout time.Duration) {
	EventuallyWithOffset(1, systemHealthy).WithTimeout(timeout).Should(Equal(healthyWant()))
	ConsistentlyWithOffset(1, systemHealthy).WithTimeout(time.Minute).Should(Equal(healthyWant()))
}

// recordDuration reports a recovery time and appends "spec,name,seconds" to $E2E_TIMINGS (Tier 2
// trends recovery times, not performance: doc/ci-design-2026-10-02.md 7절).
func recordDuration(name string, d time.Duration) {
	AddReportEntry(name, d.Round(time.Second).String())
	path := os.Getenv("E2E_TIMINGS")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	Expect(err).NotTo(HaveOccurred())
	defer f.Close()
	_, err = fmt.Fprintf(f, "%q,%q,%.0f\n", CurrentSpecReport().LeafNodeText, name, d.Seconds())
	Expect(err).NotTo(HaveOccurred())
}

// expectKnownBug runs body, which asserts the correct behaviour. While the issue is open the
// assertion is expected to fail: the failure is reported, not hidden. Once it passes the spec
// fails so the marker gets removed together with the fix.
func expectKnownBug(issue string, body func()) {
	failures := InterceptGomegaFailures(body)
	if len(failures) == 0 {
		Fail(fmt.Sprintf("%s looks fixed: the assertion now passes. Remove expectKnownBug.", issue))
	}
	AddReportEntry("known bug "+issue, strings.Join(failures, "\n"))
	fmt.Fprintf(GinkgoWriter, "KNOWN BUG %s (expected failure):\n%s\n", issue, strings.Join(failures, "\n"))
}

// serverPod is the pod of the single-node server StatefulSet on the given node.
func serverPod(node string) string { return fmt.Sprintf("%s-server-%s-0", "daos", node) }

func dumpDiagnostics() {
	for _, args := range [][]string{
		{"get", "daossystem,daospool,daoscontainer,pods,jobs,pvc", "-o", "wide"},
		{"get", "daossystem", sysName, "-o", "jsonpath={range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{\"\\n\"}{end}"},
		{"logs", "deploy/" + release, "--tail=60"},
		{"get", "events", "--sort-by=.lastTimestamp"},
	} {
		out, _ := kubectlE(args...)
		fmt.Fprintf(GinkgoWriter, "\n==== kubectl %s\n%s\n", strings.Join(args, " "), tail(out, 80))
	}
	pods, _ := kubectlE("get", "pods", "-l", "app.kubernetes.io/name=daos-server", "-o", "name")
	for _, p := range strings.Fields(pods) {
		out, _ := kubectlE("logs", p, "--all-containers", "--tail=60")
		fmt.Fprintf(GinkgoWriter, "\n==== %s\n%s\n", p, out)
	}
}

func tail(s string, n int) string {
	l := strings.Split(s, "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}

func waitGone(kind, name string, timeout time.Duration) {
	EventuallyWithOffset(1, func() error {
		_, err := kubectlE("get", kind, name)
		return err
	}).WithTimeout(timeout).Should(HaveOccurred(), "%s/%s still present", kind, name)
}

func apply(manifest string) {
	cmd := exec.Command("kubectl", "-n", ns, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	b, err := cmd.CombinedOutput()
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "kubectl apply\n%s\n%s", manifest, b)
}
