//go:build e2e

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Gluesys Co., Ltd.

// Package e2e is the Tier 1 suite (doc/ci-design-2026-10-02.md 7절). It runs against
// whatever cluster KUBECONFIG points at, with the chart values in test/e2e/profiles/
// <E2E_PROFILE>.yaml, so the same specs serve the CI VM cluster and real hardware.
//
//	E2E_PROFILE   profile name (default nvme-1rank)
//	E2E_RESET=1   run hack/ci/rollback.sh first (CI cluster only)
//	E2E_SET       extra helm --set values, comma separated (CI image tags)
//	E2E_UPGRADE_FROM  git tag of the release to upgrade from; run with -ginkgo.label-filter=upgrade
//	E2E_NO_INSTALL=1  skip the suite install; run with -ginkgo.label-filter=regression
package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	ns      = "daos-system"
	release = "daos-operator"
	sysName = "daos"
)

var (
	repoRoot string
	profile  string
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	SetDefaultEventuallyPollingInterval(5 * time.Second)
	SetDefaultConsistentlyPollingInterval(5 * time.Second)
	RunSpecs(t, "daos-operator Tier 1 e2e")
}

var _ = BeforeSuite(func() {
	// ReportAfterEach does not see a BeforeSuite failure; without this a system that never came
	// up left nothing to read (kdev-3rank, one rank missing, 2026-10-05)
	setupDone := false
	DeferCleanup(func() {
		if !setupDone {
			dumpDiagnostics()
		}
	})
	defer func() { setupDone = !CurrentSpecReport().Failed() }()
	wd, err := os.Getwd()
	Expect(err).NotTo(HaveOccurred())
	repoRoot = filepath.Clean(filepath.Join(wd, "..", ".."))
	Expect(os.Getenv("KUBECONFIG")).NotTo(BeEmpty(), "KUBECONFIG must point at the target cluster")
	name := envOr("E2E_PROFILE", "nvme-1rank")
	profile = filepath.Join(repoRoot, "test", "e2e", "profiles", name+".yaml")
	Expect(profile).To(BeAnExistingFile())
	fmt.Fprintf(GinkgoWriter, "profile %s, cluster %s\n", name, os.Getenv("KUBECONFIG"))

	if os.Getenv("E2E_RESET") == "1" {
		By("resetting the cluster to the k8s-ready snapshot")
		run(filepath.Join(repoRoot, "hack", "ci", "rollback.sh"))
	}
	if os.Getenv("E2E_UPGRADE_FROM") != "" || os.Getenv("E2E_NO_INSTALL") == "1" {
		return // the upgrade and regression cases install their own systems
	}
	installChart()
	approveFormatAndWaitReady()
})

// installChart installs the operator, the DaosSystem and CSI from the profile.
func installChart() {
	By("helm install " + filepath.Base(profile))
	args := []string{"upgrade", "--install", release, filepath.Join(repoRoot, "charts", "daos-operator"),
		"-n", ns, "--create-namespace", "-f", profile}
	if s := os.Getenv("E2E_SET"); s != "" {
		args = append(args, "--set", s)
	}
	// No --wait: helm 4 also waits for custom resources, and the DaosSystem cannot become
	// ready before the suite approves the format below.
	run("helm", args...)
	run("kubectl", "-n", ns, "rollout", "status", "deploy/"+release, "--timeout=5m")
}

// approveFormatAndWaitReady approves the storage format once the servers are up (the
// operator waits for a human; on the CI cluster the suite is that human) and waits until
// Ready has held for a full minute: right after pool creation the status was seen to drop
// to 0/0 ranks for ~30 s and come back (2026-10-03), so a single Ready sample is not enough.
func approveFormatAndWaitReady() {
	By("waiting for the server pods")
	Eventually(func() string { return condStatus("ServersReady") }).WithTimeout(10 * time.Minute).Should(Equal("True"))
	By("approving the format")
	Eventually(func() error {
		_, err := kubectlE("annotate", "daossystem", sysName, "daos.gluesys.com/format-approved=true", "--overwrite")
		return err
	}).WithTimeout(time.Minute).Should(Succeed())
	By("waiting for Ready to hold")
	waitReadyStable(10 * time.Minute)
}

var _ = ReportAfterEach(func(r SpecReport) {
	if r.Failed() {
		dumpDiagnostics()
	}
})
