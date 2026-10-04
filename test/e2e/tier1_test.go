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
		// Provisioning time is reported, not just bounded: it was 5-7 min while the operator's Job
		// pods were BestEffort and starved next to the engine (fixed by daos-operator !5: 20 s).
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
		// The rank state is the signal: Ready alone also dips briefly on its own (design 13절 #3),
		// so a False Ready right after the kill proves nothing.
		Eventually(func() string { return jsonpath("daossystem", sysName, ".status.ranks") }).WithTimeout(3*time.Minute).
			ShouldNot(ContainSubstring(`"joined"`), "the dead rank must stop being reported as joined")
		Expect(condStatus("Ready")).To(Equal("False"), "Ready must be False while the rank is not joined")
		AddReportEntry("ServersReady while engine dead", condStatus("ServersReady"))
		By("restarting the server pod and waiting for the system to come back")
		kubectl("delete", "pod", pod, "--wait=true", "--timeout=3m")
		waitReadyStable(10 * time.Minute)
	})

	// Case 3. The S3 gateway in front of a pool: a real put/list/get through versitygw.
	It("serves S3 put, list and get from a DaosPool (case 3)", func() {
		apply(`apiVersion: v1
kind: Secret
metadata: {name: e2e-s3root}
stringData: {accessKey: e2eaccess, secretKey: e2esecret0123456789}`)
		apply(`apiVersion: daos.gluesys.com/v1alpha1
kind: DaosPool
metadata: {name: e2es3}
spec: {systemRef: daos, size: 8Gi, redundancyFactor: 0} # >= 1 GiB NVMe per target (4 targets)`)
		Eventually(func() string { return jsonpath("daospool", "e2es3", `.status.conditions[?(@.type=="Ready")].status`) }).
			WithTimeout(5 * time.Minute).Should(Equal("True"))
		// -fix28: the gateway works around #28, an object write pattern that SIGSEGVs the DAOS 2.8
		// engine in vos_fetch_begin. The plain 2.8.0-20260923 image reproduced that crash here
		// twice (2026-10-03), so it must not be used; this case catches a regression of the workaround.
		image := envOr("E2E_S3_IMAGE", "registry.gitlab.gluesys.com/exastor/daos-images/versitygw-daos:2.8.0-20260923-fix28")
		apply(fmt.Sprintf(`apiVersion: daos.gluesys.com/v1alpha1
kind: S3Service
metadata: {name: e2es3}
spec: {poolRef: e2es3, image: %q, replicas: 1, port: 7070, region: kr-1, serviceType: ClusterIP, rootCredentialsSecret: e2e-s3root}`, image))
		Eventually(func() string { return jsonpath("s3service", "e2es3", `.status.conditions[?(@.type=="Ready")].status`) }).
			WithTimeout(5 * time.Minute).Should(Equal("True"))
		svc := jsonpath("s3service", "e2es3", ".status.endpoint")
		Expect(svc).NotTo(BeEmpty())
		By("put / list / get through " + svc)
		aws := envOr("E2E_AWSCLI_IMAGE", "docker.io/amazon/aws-cli:2.17.0")
		script := `set -e; export AWS_ACCESS_KEY_ID=e2eaccess AWS_SECRET_ACCESS_KEY=e2esecret0123456789 AWS_DEFAULT_REGION=kr-1
ep=http://e2es3.` + ns + `.svc:7070; s3="aws --endpoint-url $ep s3"
head -c 8388608 /dev/urandom > /tmp/obj; sha=$(sha256sum /tmp/obj | cut -d' ' -f1)
$s3 mb s3://e2e; $s3 cp /tmp/obj s3://e2e/obj; $s3 ls s3://e2e | grep -q obj && echo LISTED
$s3 cp s3://e2e/obj /tmp/back; [ "$(sha256sum /tmp/back | cut -d' ' -f1)" = "$sha" ] && echo S3_OK
$s3 rm s3://e2e/obj; $s3 rb s3://e2e`
		apply(fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: e2e-s3client}
spec:
  restartPolicy: Never
  containers: [{name: c, image: %s, command: [sh, -c, %q]}]`, aws, script))
		Eventually(func() string { return jsonpath("pod", "e2e-s3client", ".status.phase") }).WithTimeout(5 * time.Minute).
			Should(BeElementOf("Succeeded", "Failed"))
		logs := kubectl("logs", "e2e-s3client")
		Expect(logs).To(ContainSubstring("LISTED"), logs)
		Expect(logs).To(ContainSubstring("S3_OK"), logs)

		kubectl("delete", "pod", "e2e-s3client", "--wait=false")
		kubectl("delete", "s3service", "e2es3", "--wait=true", "--timeout=3m")
		kubectl("annotate", "daospool", "e2es3", "daos.gluesys.com/destroy-approved=true", "--overwrite")
		kubectl("delete", "daospool", "e2es3", "--wait=false")
		waitGone("daospool", "e2es3", 5*time.Minute)
		kubectl("delete", "secret", "e2e-s3root")
	})

	// Case 4. Every kubectl-daos subcommand against a live system: the ones that change something
	// must have their effect, the ones that do not apply must refuse with their message, and a
	// no-op upgrade approval must not restart the engine. Rank exclude -> reintegrate is the
	// recovery path that once could not finish (0b9f544).
	It("runs every kubectl-daos subcommand (case 4)", func() {
		bin := envOr("KUBECTL_DAOS", "kubectl-daos")
		daos := func(args ...string) (string, error) { return runE(bin, append(args, "-n", ns)...) }
		mustDaos := func(args ...string) string {
			out, err := daos(args...)
			ExpectWithOffset(1, err).NotTo(HaveOccurred(), "kubectl-daos %v\n%s", args, out)
			return out
		}

		By("system status")
		Expect(mustDaos("system", "status", sysName)).To(And(ContainSubstring("Ready"), ContainSubstring("rank")))

		By("system format / certs refuse when they do not apply")
		out, err := daos("system", "format", sysName, "--yes")
		Expect(err).To(HaveOccurred())
		Expect(out).To(ContainSubstring("already formatted"))
		out, err = daos("system", "certs", sysName, "--yes")
		Expect(err).To(HaveOccurred())
		Expect(out).To(ContainSubstring("allowInsecure"))

		By("system upgrade without a new image does not restart the engine")
		pod := kubectl("get", "pod", "-l", "app.kubernetes.io/name=daos-server", "-o", "jsonpath={.items[0].metadata.name}")
		uid := jsonpath("pod", pod, ".metadata.uid")
		mustDaos("system", "upgrade", sysName, "--yes")
		Consistently(func() string { return jsonpath("pod", pod, ".metadata.uid") }).WithTimeout(time.Minute).Should(Equal(uid))
		waitReadyStable(3 * time.Minute)

		// a pool on the rank, so reintegrate has pool-ranks to work on (with no pool DAOS answers
		// "no pool-ranks found to operate on" and the operator records a failure)
		By("creating a pool that lives on rank 0")
		apply(`apiVersion: daos.gluesys.com/v1alpha1
kind: DaosPool
metadata: {name: e2ekd}
spec: {systemRef: daos, size: 8Gi, redundancyFactor: 0} # >= 1 GiB NVMe per target (4 targets)`)
		Eventually(func() string { return jsonpath("daospool", "e2ekd", `.status.conditions[?(@.type=="Ready")].status`) }).
			WithTimeout(5 * time.Minute).Should(Equal("True"))
		// The documented undo of an exclude (kubectl daos rank exclude --help): the excluded rank's
		// engine stops, so clear-exclude, restart the server pod, then reintegrate.
		// Rank operations are one-shot and kubectl-daos returns as soon as it has set the
		// annotation, so each step waits for status.lastRankOp. A step sent while the server is
		// cycling fails with "unable to contact the DAOS Management Service" and is not retried by
		// the operator (CI cluster, 2026-10-03); like a careful admin, the test re-sends only that.
		rankOp := func(op string) {
			for attempt := 1; ; attempt++ {
				mustDaos("rank", op, sysName, "--ranks=0", "--yes")
				var msg string
				Eventually(func() string {
					if jsonpath("daossystem", sysName, ".status.lastRankOp.op") != op {
						return ""
					}
					msg = jsonpath("daossystem", sysName, ".status.lastRankOp.message")
					return jsonpath("daossystem", sysName, ".status.lastRankOp.finishedAt")
				}).WithTimeout(8*time.Minute).ShouldNot(BeEmpty(), "rank %s never reported a result (last message: %s)", op, msg)
				if jsonpath("daossystem", sysName, ".status.lastRankOp.succeeded") == "true" {
					return
				}
				Expect(msg).To(ContainSubstring("unable to contact"), "rank %s failed: %s", op, msg)
				Expect(attempt).To(BeNumerically("<", 4), "rank %s: management service unreachable after %d tries: %s", op, attempt, msg)
				AddReportEntry("rank "+op+" re-sent", msg)
				time.Sleep(20 * time.Second)
			}
		}
		By("rank exclude -> clear-exclude -> pod restart -> reintegrate")
		rankOp("exclude")
		Eventually(func() string { return jsonpath("daossystem", sysName, ".status.ranks") }).WithTimeout(8 * time.Minute).
			Should(ContainSubstring("excluded"))
		rankOp("clear-exclude")
		kubectl("delete", "pod", pod, "--wait=true", "--timeout=3m")
		Eventually(func() string { return condStatus("ServersReady") }).WithTimeout(5 * time.Minute).Should(Equal("True"))
		// After clear-exclude and the restart the rank was seen to rejoin on its own, and a
		// reintegrate sent then was dropped without a lastRankOp record (2026-10-03, design 13절).
		// Reintegrate only if it has not.
		time.Sleep(90 * time.Second)
		if !strings.Contains(jsonpath("daossystem", sysName, ".status.ranks"), `"joined"`) {
			rankOp("reintegrate")
		} else {
			AddReportEntry("rank rejoined without reintegrate", jsonpath("daossystem", sysName, ".status.ranks"))
		}
		waitReadyStable(10 * time.Minute)

		By("cont destroy and pool destroy")
		apply(`apiVersion: daos.gluesys.com/v1alpha1
kind: DaosContainer
metadata: {name: e2ekd}
spec: {poolRef: e2ekd, type: POSIX, fileOclass: SX, dirOclass: S1, redundancyFactor: 0}`)
		Eventually(func() string {
			return jsonpath("daoscontainer", "e2ekd", `.status.conditions[?(@.type=="Ready")].status`)
		}).
			WithTimeout(5 * time.Minute).Should(Equal("True"))
		mustDaos("cont", "destroy", "e2ekd", "--yes")
		waitGone("daoscontainer", "e2ekd", 5*time.Minute)
		mustDaos("pool", "destroy", "e2ekd", "--yes")
		waitGone("daospool", "e2ekd", 5*time.Minute)
	})

	// Case 8. Two systems on one node that together want more hugepages than it has: the second
	// cannot be placed, and its condition must say why instead of "Pending, not ready" (#37
	// family). The first system must not notice. daos2 has hostprep off (it would raise the
	// node's hugepages and rebind the NVMe), its own ports and a file engine it never reaches.
	It("reports a second system that does not fit the node's hugepages (case 8)", func() {
		node := jsonpath("daossystem", sysName, `.spec.nodeSelector.kubernetes\.io/hostname`)
		Expect(node).NotTo(BeEmpty())
		all := jsonpath("node", node, `.status.allocatable.hugepages-2Mi`)
		Expect(all).NotTo(BeEmpty(), "node %s has no hugepages-2Mi", node)
		server := jsonpath("daossystem", sysName, ".spec.images.server")
		DeferCleanup(func() {
			_, _ = kubectlE("delete", "daossystem", "daos2", "--wait=true", "--timeout=3m")
			_, _ = kubectlE("delete", "namespace", "daos2-system", "--ignore-not-found", "--wait=false")
		})
		apply(fmt.Sprintf(`apiVersion: daos.gluesys.com/v1alpha1
kind: DaosSystem
metadata: {name: daos2}
spec:
  version: "2.8.0"
  namespace: daos2-system
  images: {server: %q, agent: %q, admin: %q, client: %q}
  nodeSelector: {kubernetes.io/hostname: %s}
  msReplicas: 1
  provider: "ofi+tcp"
  controlPort: 10101
  nrHugepages: 1024
  allowInsecure: true
  systemRamReservedGiB: 8
  hostPrep: {enabled: false}
  server: {hugepagesRequest: %s}   # all of the node's: the first system already holds some
  engines:
    - {targets: 1, helpers: 0, fabricIface: ens19, fabricPort: 31916, bdevClass: file, bdevList: [/var/daos/daos2-bdev0], bdevSizeGiB: 1, scmSizeGiB: 1}
`, server, jsonpath("daossystem", sysName, ".spec.images.agent"), jsonpath("daossystem", sysName, ".spec.images.admin"),
			jsonpath("daossystem", sysName, ".spec.images.client"), node, all))
		cond := func(t, f string) string {
			return jsonpath("daossystem", "daos2", fmt.Sprintf(`.status.conditions[?(@.type=="%s")].%s`, t, f))
		}
		Eventually(func() string { return cond("ServersReady", "message") }).WithTimeout(5 * time.Minute).
			Should(ContainSubstring("Insufficient hugepages-2Mi"))
		Expect(cond("ServersReady", "status")).To(Equal("False"))
		AddReportEntry("daos2 ServersReady", cond("ServersReady", "message"))
		Consistently(systemHealthy).WithTimeout(time.Minute).Should(Equal(healthy), "the first system must not notice")
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
