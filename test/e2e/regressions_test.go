//go:build e2e

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Gluesys Co., Ltd.

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Tier 1 cases 9-12: failures that really happened on the test beds (doc/ci-handoff-2026-10-02.md
// 1절). Each one breaks a fresh system on purpose and asserts that the DaosSystem itself says why --
// "green but not working" or "red but only the pod log knows" is what these cases exist to catch.
// Run on their own: E2E_NO_INSTALL=1 -ginkgo.label-filter=regression (each case resets the cluster).
var _ = Describe("Regressions", Label("regression"), Ordered, func() {
	profiles := func(name string) string { return filepath.Join(repoRoot, "test", "e2e", "profiles", name+".yaml") }
	base := envOr("E2E_REGRESSION_BASE", "kdev-1rank")
	serverNode := envOr("E2E_SERVER_NODE", "exaci5-3a")

	// fresh resets the cluster, runs prepare (node changes that the reset would otherwise undo)
	// and installs the base profile with an overlay.
	var onNode func(cmd string) string
	fresh := func(overlay string, prepare ...func()) {
		By("resetting the cluster")
		run(filepath.Join(repoRoot, "hack", "ci", "rollback.sh"))
		for _, p := range prepare {
			p()
		}
		By("installing " + base + " + " + overlay)
		args := []string{"upgrade", "--install", release, filepath.Join(repoRoot, "charts", "daos-operator"),
			"-n", ns, "--create-namespace", "-f", profiles(base)}
		if overlay != "" {
			args = append(args, "-f", profiles(overlay))
		}
		if s := os.Getenv("E2E_SET"); s != "" {
			args = append(args, "--set", s)
		}
		run("helm", args...)
		run("kubectl", "-n", ns, "rollout", "status", "deploy/"+release, "--timeout=5m")
	}
	onNode = func(cmd string) string {
		ip := run("kubectl", "get", "node", serverNode, "-o", `jsonpath={.status.addresses[?(@.type=="InternalIP")].address}`)
		return run("ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "root@"+strings.TrimSpace(ip), cmd)
	}
	condMsg := func(t string) func() string {
		return func() string {
			return jsonpath("daossystem", sysName, `.status.conditions[?(@.type=="`+t+`")].message`)
		}
	}

	// Case 9. THP on and hostprep off: daos_server refuses to start with code 623. hostprep does not
	// manage THP either (design 13절), so the cause must reach ServersReady.
	It("names THP when the server refuses to start (case 9)", func() {
		fresh("neg-thp", func() { onNode("echo always > /sys/kernel/mm/transparent_hugepage/enabled") })
		Eventually(condMsg("ServersReady")).WithTimeout(8*time.Minute).Should(ContainSubstring("code = 623"),
			"ServersReady must say the server stopped because THP is enabled")
	})

	// Case 10. DAOS's default system_ram_reserved (64 GiB) on a 32 GiB node.
	It("names the RAM shortage when the server refuses to start (case 10)", func() {
		fresh("neg-ram")
		Eventually(condMsg("ServersReady")).WithTimeout(8 * time.Minute).Should(ContainSubstring("insufficient ram"))
	})

	// Case 11. A file bdev bigger than the root filesystem: the server starts, the format fails.
	It("names the space shortage when the format fails (case 11)", func() {
		fresh("neg-file-root")
		Eventually(func() string { return condStatus("ServersReady") }).WithTimeout(8 * time.Minute).Should(Equal("True"))
		kubectl("annotate", "daossystem", sysName, "daos.gluesys.com/format-approved=true", "--overwrite")
		Eventually(condMsg("Formatted")).WithTimeout(8 * time.Minute).Should(ContainSubstring("no space left on device"))
	})

	// Case 12 (#36). A pool whose rank is down cannot be destroyed; the operator kept recreating the
	// destroy Job for four days without a word. It must give up visibly (DestroyStalled).
	It("stops retrying a pool destroy that cannot succeed (case 12, #36)", func() {
		fresh("")
		approveFormatAndWaitReady()
		apply(`apiVersion: daos.gluesys.com/v1alpha1
kind: DaosPool
metadata: {name: e2estuck}
spec: {systemRef: daos, size: ` + poolSize() + `, redundancyFactor: 0}`)
		Eventually(func() string {
			return jsonpath("daospool", "e2estuck", `.status.conditions[?(@.type=="Ready")].status`)
		}).
			WithTimeout(5 * time.Minute).Should(Equal("True"))
		kubectl("annotate", "daossystem", sysName, "daos.gluesys.com/rank-op=exclude:0", "--overwrite")
		Eventually(func() string { return jsonpath("daossystem", sysName, ".status.ranks") }).WithTimeout(8 * time.Minute).
			Should(ContainSubstring("excluded"))
		kubectl("annotate", "daospool", "e2estuck", "daos.gluesys.com/destroy-approved=true", "--overwrite")
		kubectl("delete", "daospool", "e2estuck", "--wait=false")
		// three destroy runs, each cut at 240 s, with a minute between them
		Eventually(func() string {
			return jsonpath("daospool", "e2estuck", `.status.conditions[?(@.type=="Ready")].reason`)
		}).WithTimeout(20 * time.Minute).WithPolling(15 * time.Second).Should(Equal("DestroyStalled"))
		Expect(jsonpath("daospool", "e2estuck", `.status.conditions[?(@.type=="Ready")].message`)).
			To(ContainSubstring("daos.gluesys.com/destroy-approved"), "the stall message says how to get out")

		By("removing the approval releases the DaosPool and keeps the DAOS pool")
		kubectl("annotate", "daospool", "e2estuck", "daos.gluesys.com/destroy-approved-")
		waitGone("daospool", "e2estuck", 2*time.Minute)
	})
})
