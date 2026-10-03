//go:build e2e

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Gluesys Co., Ltd.

package e2e

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The Tier 1 cases of doc/ci-design-2026-10-02.md 7절, slice 1. Ordered: the uninstall
// case removes what the others use, so it runs last.
var _ = Describe("Tier 1", Ordered, func() {

	// Case 1. Not a single green sample: the system must stay healthy for a minute.
	It("brings the system up and keeps it healthy (case 1)", func() {
		waitReadyStable(5 * time.Minute)
		Expect(condReason("ManagementService")).To(Equal("Leader"))
	})

	// Case 2. The data path, end to end: the #34/#36/#37 family is "green but not working",
	// so the suite writes through dfuse and reads it back from a second pod.
	It("writes and reads back through a PVC, then leaves nothing behind (case 2)", func() {
		client := jsonpath("daossystem", sysName, ".spec.images.client")
		Expect(client).NotTo(BeEmpty())
		sel := envOr("E2E_CLIENT_SELECTOR", "daos.gluesys.com/client=true")
		k, v, _ := strings.Cut(sel, "=")

		By("creating the pool behind the StorageClass")
		apply(`apiVersion: daos.gluesys.com/v1alpha1
kind: DaosPool
metadata: {name: e2epool}
spec: {systemRef: daos, size: 8Gi, redundancyFactor: 0}`)
		Eventually(func() string { return jsonpath("daospool", "e2epool", `.status.conditions[?(@.type=="Ready")].status`) }).
			WithTimeout(5 * time.Minute).Should(Equal("True"))

		By("claiming a volume")
		apply(`apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: e2e-data}
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
  nodeSelector: {%q: %q}
  containers:
    - name: c
      image: %s
      command: [sh, -c, %q]
      volumeMounts: [{name: d, mountPath: /data}]
  volumes: [{name: d, persistentVolumeClaim: {claimName: e2e-data}}]`, name, k, v, client, script)
		}
		// Provisioning is slow today: every DaosContainer operation is a Job with a fresh
		// daos_agent sidecar whose first client call takes ~2 min (2026-10-03, CI cluster), and a
		// container needs a create and a query Job. Measure it and report it rather than hide it.
		By("waiting for the volume to be provisioned")
		start := time.Now()
		Eventually(func() string { return jsonpath("pvc", "e2e-data", ".status.phase") }).WithTimeout(15 * time.Minute).Should(Equal("Bound"))
		AddReportEntry("PVC provisioning time", time.Since(start).Round(time.Second).String())
		fmt.Fprintf(GinkgoWriter, "PVC bound after %s\n", time.Since(start).Round(time.Second))

		By("writing 64 MiB and its checksum")
		apply(pod("e2e-writer", "set -e; head -c 67108864 /dev/urandom > /data/blob; sha256sum /data/blob > /data/blob.sha256; sync; df -h /data | tail -1; echo WROTE"))
		Eventually(func() string { return jsonpath("pod", "e2e-writer", ".status.phase") }).WithTimeout(5 * time.Minute).Should(Equal("Succeeded"))
		Expect(kubectl("logs", "e2e-writer")).To(ContainSubstring("WROTE"))
		Expect(kubectl("logs", "e2e-writer")).To(ContainSubstring("dfuse"), "the volume must be DAOS (dfuse), not a host directory (daos-csi 6203907)")

		By("reading it back from a new pod")
		apply(pod("e2e-reader", "set -e; sha256sum -c /data/blob.sha256 && echo CHECKSUM_OK"))
		Eventually(func() string { return jsonpath("pod", "e2e-reader", ".status.phase") }).WithTimeout(5 * time.Minute).Should(Equal("Succeeded"))
		Expect(kubectl("logs", "e2e-reader")).To(ContainSubstring("CHECKSUM_OK"))

		By("deleting everything")
		kubectl("delete", "pod", "e2e-writer", "e2e-reader", "--wait=true", "--timeout=3m")
		pv := kubectl("get", "pvc", "e2e-data", "-o", "jsonpath={.spec.volumeName}")
		kubectl("delete", "pvc", "e2e-data", "--wait=true", "--timeout=3m")
		Eventually(func() error { _, err := runE("kubectl", "get", "pv", pv); return err }).WithTimeout(3 * time.Minute).Should(HaveOccurred())
		Eventually(func() string { out, _ := kubectlE("get", "daoscontainer", "-o", "name"); return strings.TrimSpace(out) }).
			WithTimeout(12*time.Minute).Should(BeEmpty(), "DaosContainer left behind") // each destroy Job pays the slow-agent cost (see provisioning note)
		kubectl("annotate", "daospool", "e2epool", "daos.gluesys.com/destroy-approved=true", "--overwrite")
		kubectl("delete", "daospool", "e2epool", "--wait=false")
		waitGone("daospool", "e2epool", 12*time.Minute)
		waitReadyStable(3 * time.Minute)
	})

	// Case 5. The operator dies in the middle of things; when it comes back it must not
	// rewrite what it already made.
	It("survives an operator restart without changing the workloads (case 5)", func() {
		// the StatefulSet carries only managed-by/system/node labels; reach it through its pod
		pod := kubectl("get", "pod", "-l", "app.kubernetes.io/name=daos-server", "-o", "jsonpath={.items[0].metadata.name}")
		sts := strings.TrimSuffix(pod, "-0")
		cm := sts // the server ConfigMap is named like its StatefulSet: <system>-server-<node>
		genBefore := jsonpath("sts", sts, ".metadata.generation")
		cmBefore := jsonpath("configmap", cm, ".metadata.resourceVersion")
		Expect(genBefore).NotTo(BeEmpty())

		By("deleting the operator pod")
		old := kubectl("get", "pod", "-l", "app.kubernetes.io/name=daos-operator", "-o", "jsonpath={.items[0].metadata.name}")
		kubectl("delete", "pod", old, "--wait=false")
		Eventually(func() string {
			return jsonpath("deploy", release, ".status.readyReplicas")
		}).WithTimeout(3 * time.Minute).Should(Equal("1"))
		Eventually(func() string {
			return kubectl("get", "pod", "-l", "app.kubernetes.io/name=daos-operator", "-o", "jsonpath={.items[*].metadata.name}")
		}).WithTimeout(3 * time.Minute).ShouldNot(ContainSubstring(old))

		By("letting the new operator reconcile for a minute")
		waitReadyStable(3 * time.Minute)
		Expect(jsonpath("sts", sts, ".metadata.generation")).To(Equal(genBefore), "server StatefulSet spec was rewritten")
		if cmBefore != "" {
			Expect(jsonpath("configmap", cm, ".metadata.resourceVersion")).To(Equal(cmBefore), "server ConfigMap was rewritten")
		}
	})

	// Case 13 (#37). Only the engine dies; the conditions must say so. Seen on the CI cluster
	// (2026-10-03): within about a minute the rank turns "errored" and Ready goes False
	// (RanksNotJoined), while ServersReady and ManagementService stay True -- they are about
	// pods and the MS leader, which are indeed fine. #37 asks for more than that; this case
	// pins the part that holds today so a regression shows up.
	It("reports an engine that died (case 13, #37)", func() {
		pod := kubectl("get", "pod", "-l", "app.kubernetes.io/name=daos-server", "-o", "jsonpath={.items[0].metadata.name}")
		By("killing daos_engine inside " + pod)
		out := kubectl("exec", pod, "-c", "daos-server", "--", "sh", "-c",
			`for p in /proc/[0-9]*; do [ "$(cat $p/comm 2>/dev/null)" = daos_engine ] && kill -9 ${p#/proc/} && echo killed ${p#/proc/}; done; true`)
		Expect(out).To(ContainSubstring("killed"), "no daos_engine process found")
		Eventually(func() string { return condStatus("Ready") }).WithTimeout(3*time.Minute).Should(Equal("False"),
			"Ready must turn False while the engine is dead")
		Expect(jsonpath("daossystem", sysName, ".status.ranks")).NotTo(ContainSubstring(`"joined"`),
			"the dead rank must not be reported as joined")
		AddReportEntry("ServersReady while engine dead", condStatus("ServersReady"))
		By("restarting the server pod and waiting for the system to come back")
		kubectl("delete", "pod", pod, "--wait=true", "--timeout=3m")
		waitReadyStable(10 * time.Minute)
	})

	// Case 7. The chart keeps the DaosSystem on uninstall (helm.sh/resource-policy: keep:
	// "uninstall must never stop engines"), and pools and containers carry finalizers only the
	// operator releases. So a clean removal is: tear the DAOS objects down while the operator
	// still runs, then uninstall. Afterwards nothing may be left behind.
	It("tears down in order and uninstalls cleanly (case 7)", func() {
		By("destroying containers, pools and the system while the operator runs")
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

		By("uninstalling the operator")
		run("helm", "uninstall", release, "-n", ns, "--wait", "--timeout", "5m")
		Eventually(func() string {
			out, _ := kubectlE("get", "pods,sts,ds,deploy", "-o", "name")
			return strings.TrimSpace(out)
		}).
			WithTimeout(5*time.Minute).Should(BeEmpty(), "workloads left after uninstall")
	})
})
