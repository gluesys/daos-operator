//go:build e2e

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Gluesys Co., Ltd.

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Case 6 (doc/ci-design-2026-10-02.md 7절): an installed release with data in use is upgraded to
// HEAD and nothing is lost. Its own run: E2E_UPGRADE_FROM=<git tag> -ginkgo.label-filter=upgrade.
// The DAOS images stay the same on both sides, so only the operator, CSI and chart change, and the
// engine must not be restarted (ADR-003: engines are only restarted by an approved full-stop upgrade).
var _ = Describe("Upgrade", Label("upgrade"), Ordered, func() {
	It("upgrades a release with data in use to HEAD and keeps the data (case 6)", func() {
		from := os.Getenv("E2E_UPGRADE_FROM")
		if from == "" {
			Fail("E2E_UPGRADE_FROM must name the release tag to upgrade from (e.g. v0.1.0-rc.4)")
		}
		fromValues := filepath.Join(repoRoot, "test", "e2e", "profiles", envOr("E2E_UPGRADE_FROM_VALUES", "upgrade-from-rc4")+".yaml")
		toValues := filepath.Join(repoRoot, "test", "e2e", "profiles", envOr("E2E_PROFILE", "kdev-1rank")+".yaml")

		By("installing " + from + " from its git tag")
		old := GinkgoT().TempDir()
		run("sh", "-c", fmt.Sprintf("git -C %q archive %q charts/daos-operator | tar -x -C %q", repoRoot, from, old))
		oldChart := filepath.Join(old, "charts", "daos-operator")
		run("kubectl", "apply", "--server-side", "--force-conflicts", "-f", filepath.Join(oldChart, "crds"))
		run("helm", "upgrade", "--install", release, oldChart, "-n", ns, "--create-namespace", "-f", fromValues)
		run("kubectl", "-n", ns, "rollout", "status", "deploy/"+release, "--timeout=5m")
		approveFormatAndWaitReady()

		By("writing data that must survive the upgrade")
		client := jsonpath("daossystem", sysName, ".spec.images.client")
		apply(`apiVersion: daos.gluesys.com/v1alpha1
kind: DaosPool
metadata: {name: e2epool} # the profile's StorageClass names this pool
spec: {systemRef: daos, size: ` + poolSize() + `, redundancyFactor: 0}`)
		Eventually(func() string { return jsonpath("daospool", "e2epool", `.status.conditions[?(@.type=="Ready")].status`) }).
			WithTimeout(5 * time.Minute).Should(Equal("True"))
		apply(`apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: e2e-up}
spec:
  accessModes: [ReadWriteMany]
  storageClassName: daos-e2e
  resources: {requests: {storage: 1Gi}}`)
		pod := func(name, script string) string {
			return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: %s}
spec:
  restartPolicy: Never
  nodeSelector: {daos.gluesys.com/client: "true"}
  containers:
    - name: c
      image: %s
      command: [sh, -c, %q]
      volumeMounts: [{name: d, mountPath: /data}]
  volumes: [{name: d, persistentVolumeClaim: {claimName: e2e-up}}]`, name, client, script)
		}
		// the old CSI is the one with the staging-bind leak (daos-csi !1), so this pod stays until
		// after the upgrade and is removed by the new driver
		apply(pod("e2e-up-writer", "set -e; head -c 33554432 /dev/urandom > /data/blob; sha256sum /data/blob > /data/blob.sha256; sync; echo WROTE"))
		Eventually(func() string { return jsonpath("pod", "e2e-up-writer", ".status.phase") }).WithTimeout(15 * time.Minute).Should(Equal("Succeeded"))
		server := kubectl("get", "pod", "-l", "app.kubernetes.io/name=daos-server", "-o", "jsonpath={.items[0].metadata.name}")
		serverUID := jsonpath("pod", server, ".metadata.uid")
		oldImage := jsonpath("deploy", release, ".spec.template.spec.containers[0].image")

		By("upgrading to HEAD: CRDs first (helm does not upgrade crds/), then the release")
		run("kubectl", "apply", "--server-side", "--force-conflicts", "-f", filepath.Join(repoRoot, "charts", "daos-operator", "crds"))
		args := []string{"upgrade", release, filepath.Join(repoRoot, "charts", "daos-operator"), "-n", ns, "-f", toValues}
		if s := os.Getenv("E2E_SET"); s != "" {
			args = append(args, "--set", s)
		}
		run("helm", args...)
		run("kubectl", "-n", ns, "rollout", "status", "deploy/"+release, "--timeout=5m")
		Expect(jsonpath("deploy", release, ".spec.template.spec.containers[0].image")).NotTo(Equal(oldImage))

		By("the engine kept running and the system stays healthy")
		waitReadyStable(10 * time.Minute)
		Expect(jsonpath("pod", server, ".metadata.uid")).To(Equal(serverUID), "the upgrade restarted the engine")

		By("reading the data back through the new CSI driver")
		apply(pod("e2e-up-reader", "set -e; sha256sum -c /data/blob.sha256 && echo CHECKSUM_OK"))
		Eventually(func() string { return jsonpath("pod", "e2e-up-reader", ".status.phase") }).WithTimeout(10 * time.Minute).
			Should(BeElementOf("Succeeded", "Failed"))
		Expect(kubectl("logs", "e2e-up-reader")).To(ContainSubstring("CHECKSUM_OK"))

		By("tearing down with the new operator")
		kubectl("delete", "pod", "e2e-up-writer", "e2e-up-reader", "--wait=true", "--timeout=5m")
		kubectl("delete", "pvc", "e2e-up", "--wait=true", "--timeout=5m")
		for _, kind := range []string{"daoscontainer", "daospool"} {
			for _, name := range strings.Fields(kubectl("get", kind, "-o", "jsonpath={.items[*].metadata.name}")) {
				kubectl("annotate", kind, name, "daos.gluesys.com/destroy-approved=true", "--overwrite")
				kubectl("delete", kind, name, "--wait=false")
			}
			Eventually(func() string { out, _ := kubectlE("get", kind, "-o", "name"); return strings.TrimSpace(out) }).
				WithTimeout(15*time.Minute).Should(BeEmpty(), "%s not destroyed", kind)
		}
		kubectl("delete", "daossystem", sysName, "--wait=false")
		waitGone("daossystem", sysName, 10*time.Minute)
		run("helm", "uninstall", release, "-n", ns, "--wait", "--timeout", "5m")
	})
})
