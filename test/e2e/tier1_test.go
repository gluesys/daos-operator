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
spec: {systemRef: daos, size: ` + poolSize() + `, redundancyFactor: 0}`)
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
	// pods and the MS leader, which are indeed fine. EnginesReady (#37) is the condition that
	// names the node whose engine is gone and where its log is.
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
		node := jsonpath("pod", pod, ".spec.nodeName")
		Eventually(func() string { return condReason("EnginesReady") }).WithTimeout(3 * time.Minute).Should(Equal("EnginesDown"))
		msg := jsonpath("daossystem", sysName, `.status.conditions[?(@.type=="EnginesReady")].message`)
		Expect(msg).To(And(HavePrefix(node+": 0/1 engine(s) joined"), ContainSubstring("/var/log/daos/daos_engine")))
		Expect(condStatus("ServersReady")).To(Equal("True"), "the pod is Ready: only the engine died")
		AddReportEntry("EnginesReady while engine dead", msg)
		By("restarting the server pod and waiting for the system to come back")
		kubectl("delete", "pod", pod, "--wait=true", "--timeout=3m")
		waitReadyStable(10 * time.Minute)
		Expect(condStatus("EnginesReady")).To(Equal("True"))
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
spec: {systemRef: daos, size: ` + poolSize() + `, redundancyFactor: 0}`)
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
		// the approval is one-shot even with nothing to upgrade (#11): a latched approval would let
		// the next image change restart the system without a fresh one
		Expect(jsonpath("daossystem", sysName, ".spec.upgrade.approved")).NotTo(Equal("true"), "no-op upgrade approval latched")
		waitReadyStable(3 * time.Minute)

		// a pool on the rank, so reintegrate has pool-ranks to work on (with no pool DAOS answers
		// "no pool-ranks found to operate on", which the operator records as nothing to do, #14)
		By("creating a pool that lives on rank 0")
		apply(`apiVersion: daos.gluesys.com/v1alpha1
kind: DaosPool
metadata: {name: e2ekd}
spec: {systemRef: daos, size: ` + poolSize() + `, redundancyFactor: 0}`)
		Eventually(func() string { return jsonpath("daospool", "e2ekd", `.status.conditions[?(@.type=="Ready")].status`) }).
			WithTimeout(5 * time.Minute).Should(Equal("True"))
		// The documented undo of an exclude (kubectl daos rank exclude --help): the excluded rank's
		// engine stops, so clear-exclude, restart the server pod, then reintegrate.
		// kubectl-daos waits for the operator's result and fails when the operation failed, and
		// the operator re-sends a request that never reached the management service (sent while
		// the server is cycling). Both were missing once (#9, CI cluster, 2026-10-03): the test
		// had to poll status.lastRankOp and re-send by hand. Now one call must be enough.
		rankOp := func(op string) {
			out := mustDaos("rank", op, sysName, "--ranks=0", "--yes", "--wait=8m")
			ExpectWithOffset(1, out).To(ContainSubstring("done: "), "rank %s reported no result within 8m:\n%s", op, out)
			AddReportEntry("rank "+op, out)
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
		Consistently(systemHealthy).WithTimeout(time.Minute).Should(Equal(healthyWant()), "the first system must not notice")
	})

	// Case 14. Snapshots (DaosContainerSnapshot). DAOS 2.8 has no rollback, so a snapshot is
	// only useful if it really holds the old data and goes away when asked. The read-only claim
	// (spec.volume) is mounted by daos-csi at the snapshot epoch. The source volume also grows
	// once (csi.expansion): on upstream DAOS that moves spec.spaceLimit and the PVC size only.
	It("grows a volume, snapshots it, keeps the old data in it, restores a writable copy, destroys it on approval, prunes by policy and serves CSI VolumeSnapshots (case 14)", func() {
		client := jsonpath("daossystem", sysName, ".spec.images.client")
		sel := envOr("E2E_CLIENT_SELECTOR", "daos.gluesys.com/client=true")
		k, v, _ := strings.Cut(sel, "=")
		pod := func(name, claim string, readOnly bool, script string) string {
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
      volumeMounts: [{name: d, mountPath: /data, readOnly: %t}]
  volumes: [{name: d, persistentVolumeClaim: {claimName: %s, readOnly: %t}}]`, name, k, v, client, script, readOnly, claim, readOnly)
		}
		runPod := func(manifest, name, want string) {
			apply(manifest)
			Eventually(func() string { return jsonpath("pod", name, ".status.phase") }).WithTimeout(5*time.Minute).Should(Equal("Succeeded"), "pod %s", name)
			Expect(kubectl("logs", name)).To(ContainSubstring(want))
		}
		DeferCleanup(func() {
			_, _ = kubectlE("delete", "pod", "e2e-snap-w1", "e2e-snap-w2", "e2e-snap-r", "e2e-snap-rw", "e2e-snap-w3", "e2e-vs-r", "e2e-vs-rw", "--ignore-not-found", "--wait=true", "--timeout=3m")
		})

		By("a volume holding v1")
		apply(`apiVersion: daos.gluesys.com/v1alpha1
kind: DaosPool
metadata: {name: e2epool}
spec: {systemRef: daos, size: ` + poolSize() + `, redundancyFactor: 0}`)
		Eventually(func() string { return jsonpath("daospool", "e2epool", `.status.conditions[?(@.type=="Ready")].status`) }).
			WithTimeout(5 * time.Minute).Should(Equal("True"))
		apply(`apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: e2e-snap-src}
spec:
  accessModes: [ReadWriteMany]
  storageClassName: daos-e2e
  resources: {requests: {storage: 1Gi}}`)
		Eventually(func() string { return jsonpath("pvc", "e2e-snap-src", ".status.phase") }).WithTimeout(15 * time.Minute).Should(Equal("Bound"))
		runPod(pod("e2e-snap-w1", "e2e-snap-src", false, "set -e; echo v1 > /data/v; sync; echo WROTE"), "e2e-snap-w1", "WROTE")
		pv := kubectl("get", "pvc", "e2e-snap-src", "-o", "jsonpath={.spec.volumeName}")
		cont := jsonpath("pv", pv, ".spec.csi.volumeAttributes.crName")
		Expect(cont).NotTo(BeEmpty(), "daos-csi names the DaosContainer in the volume context")
		Expect(jsonpath("daoscontainer", cont, ".spec.spaceLimit")).To(Equal("1073741824"), "daos-csi caps the container at the PVC size")

		By("growing the volume online: spec.spaceLimit follows, upstream DAOS says Unsupported and the PVC still grows")
		kubectl("patch", "pvc", "e2e-snap-src", "--type=merge", "-p", `{"spec":{"resources":{"requests":{"storage":"2Gi"}}}}`)
		start := time.Now()
		Eventually(func() string { return jsonpath("pvc", "e2e-snap-src", ".status.capacity.storage") }).WithTimeout(3 * time.Minute).Should(Equal("2Gi"))
		recordDuration("volume expand", time.Since(start))
		Expect(jsonpath("daoscontainer", cont, ".spec.spaceLimit")).To(Equal("2147483648"))
		Expect(jsonpath("daoscontainer", cont, `.status.conditions[?(@.type=="SpaceLimit")].reason`)).To(Equal("Unsupported"))
		Expect(jsonpath("pv", pv, ".spec.capacity.storage")).To(Equal("2Gi"))

		By("taking the snapshot")
		apply(`apiVersion: daos.gluesys.com/v1alpha1
kind: DaosContainerSnapshot
metadata: {name: e2e-snap}
spec:
  containerRef: ` + cont + `
  volume: {claimName: e2e-snap-ro}`)
		start = time.Now()
		Eventually(func() string { return jsonpath("daoscontainersnapshot", "e2e-snap", ".status.ready") }).WithTimeout(5 * time.Minute).Should(Equal("true"))
		recordDuration("snapshot ready", time.Since(start))
		epoch := jsonpath("daoscontainersnapshot", "e2e-snap", ".status.epoch")
		Expect(epoch).To(MatchRegexp(`^[1-9][0-9]+$`))
		Expect(jsonpath("daoscontainersnapshot", "e2e-snap", ".status.creationTime")).To(HavePrefix(time.Now().UTC().Format("2006-")), "creation time from the HLC")
		Expect(jsonpath("daoscontainersnapshot", "e2e-snap", ".status.containerUUID")).To(Equal(jsonpath("daoscontainer", cont, ".status.uuid")))

		By("the live volume moves on to v2")
		runPod(pod("e2e-snap-w2", "e2e-snap-src", false, "set -e; echo v2 > /data/v; sync; cat /data/v"), "e2e-snap-w2", "v2")

		By("the read-only claim still shows v1 and refuses writes")
		Eventually(func() string { return jsonpath("pvc", "e2e-snap-ro", ".status.phase") }).WithTimeout(3 * time.Minute).Should(Equal("Bound"))
		runPod(pod("e2e-snap-r", "e2e-snap-ro", true, "cat /data/v; if echo x > /data/w 2>/dev/null; then echo WRITABLE; else echo READONLY; fi"), "e2e-snap-r", "READONLY")
		Expect(kubectl("logs", "e2e-snap-r")).To(SatisfyAll(ContainSubstring("v1"), Not(ContainSubstring("v2")), Not(ContainSubstring("WRITABLE"))))
		kubectl("delete", "pod", "e2e-snap-r", "--wait=true", "--timeout=3m")

		By("a writable restore copies the snapshot into a new container (v1, and writes work)")
		apply(`apiVersion: daos.gluesys.com/v1alpha1
kind: DaosContainer
metadata: {name: e2e-restored}
spec: {poolRef: e2epool, type: POSIX, fileOclass: SX, dirOclass: S1, redundancyFactor: 0, restoreFrom: {snapshotRef: e2e-snap}, spaceLimit: 1Gi}`)
		start = time.Now()
		Eventually(func() string {
			return jsonpath("daoscontainer", "e2e-restored", `.status.conditions[?(@.type=="Ready")].status`)
		}).
			WithTimeout(10 * time.Minute).Should(Equal("True"))
		recordDuration("writable restore", time.Since(start))
		Expect(jsonpath("daoscontainer", "e2e-restored", ".status.restoredFrom")).To(Equal("e2e-snap@" + epoch))
		// the lane runs upstream DAOS: no container quota, said so, served anyway (ADR-005)
		Expect(jsonpath("daoscontainer", "e2e-restored", ".status.edition")).To(Equal("upstream"))
		Expect(jsonpath("daoscontainer", "e2e-restored", `.status.conditions[?(@.type=="SpaceLimit")].reason`)).To(Equal("Unsupported"))
		// a static volume for it: any daos-csi stages pool/container attributes
		apply(`apiVersion: v1
kind: PersistentVolume
metadata: {name: e2e-restored}
spec:
  capacity: {storage: 1Gi}
  accessModes: [ReadWriteMany]
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ""
  csi:
    driver: ` + jsonpath("pv", pv, ".spec.csi.driver") + `
    volumeHandle: e2e-restored
    volumeAttributes: {pool: e2epool, container: e2e-restored, destroyOnDelete: "false"}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: e2e-restored}
spec:
  accessModes: [ReadWriteMany]
  storageClassName: ""
  volumeName: e2e-restored
  resources: {requests: {storage: 1Gi}}`)
		Eventually(func() string { return jsonpath("pvc", "e2e-restored", ".status.phase") }).WithTimeout(3 * time.Minute).Should(Equal("Bound"))
		runPod(pod("e2e-snap-rw", "e2e-restored", false, "cat /data/v; echo w > /data/w && cat /data/w && echo WRITABLE"), "e2e-snap-rw", "WRITABLE")
		Expect(kubectl("logs", "e2e-snap-rw")).To(SatisfyAll(ContainSubstring("v1"), Not(ContainSubstring("v2"))))
		kubectl("delete", "pod", "e2e-snap-rw", "--wait=true", "--timeout=3m")
		kubectl("delete", "pvc", "e2e-restored", "--wait=true", "--timeout=3m")
		kubectl("delete", "pv", "e2e-restored", "--wait=true", "--timeout=3m")
		kubectl("annotate", "daoscontainer", "e2e-restored", "daos.gluesys.com/destroy-approved=true", "--overwrite")
		kubectl("delete", "daoscontainer", "e2e-restored", "--wait=false")
		waitGone("daoscontainer", "e2e-restored", 5*time.Minute)

		By("an approved delete destroys it in DAOS (the finalizer is only released after destroy-snap succeeds)")
		kubectl("annotate", "daoscontainersnapshot", "e2e-snap", "daos.gluesys.com/destroy-approved=true", "--overwrite")
		kubectl("delete", "daoscontainersnapshot", "e2e-snap", "--wait=false")
		waitGone("daoscontainersnapshot", "e2e-snap", 5*time.Minute)
		Expect(kubectl("get", "events", "--field-selector", "involvedObject.name=e2e-snap", "-o", "jsonpath={.items[*].reason}")).To(ContainSubstring("SnapshotDestroyed"))
		waitGone("pvc", "e2e-snap-ro", 3*time.Minute)
		apply(`apiVersion: daos.gluesys.com/v1alpha1
kind: DaosContainerSnapshot
metadata: {name: e2e-snap}
spec: {containerRef: ` + cont + `}`)
		Eventually(func() string { return jsonpath("daoscontainersnapshot", "e2e-snap", ".status.ready") }).WithTimeout(5 * time.Minute).Should(Equal("true"))
		Expect(jsonpath("daoscontainersnapshot", "e2e-snap", ".status.epoch")).NotTo(Equal(epoch), "a new object takes a new snapshot")

		By("an unapproved delete keeps it and says so")
		kubectl("delete", "daoscontainersnapshot", "e2e-snap", "--wait=true", "--timeout=3m")
		Expect(kubectl("get", "events", "--field-selector", "involvedObject.name=e2e-snap", "-o", "jsonpath={.items[*].reason}")).To(ContainSubstring("SnapshotOrphaned"))

		By("a DaosSnapshotPolicy takes its own snapshot and destroys the oldest beyond keep")
		apply(`apiVersion: daos.gluesys.com/v1alpha1
kind: DaosContainerSnapshot
metadata: {name: e2e-pol-seed, labels: {daos.gluesys.com/snapshot-policy: e2e-pol}}
spec: {containerRef: ` + cont + `}`)
		Eventually(func() string { return jsonpath("daoscontainersnapshot", "e2e-pol-seed", ".status.ready") }).WithTimeout(5 * time.Minute).Should(Equal("true"))
		apply(`apiVersion: daos.gluesys.com/v1alpha1
kind: DaosSnapshotPolicy
metadata: {name: e2e-pol}
spec: {containerRef: ` + cont + `, interval: 5m, keep: 1}`)
		start = time.Now()
		waitGone("daoscontainersnapshot", "e2e-pol-seed", 4*time.Minute) // no interval wait: the seed is older than the policy
		recordDuration("policy snapshot + prune", time.Since(start))
		Expect(kubectl("get", "events", "--field-selector", "involvedObject.name=e2e-pol-seed", "-o", "jsonpath={.items[*].reason}")).To(ContainSubstring("SnapshotDestroyed"),
			"pruning goes through destroy-snap, not just the object")
		Expect(kubectl("get", "events", "--field-selector", "involvedObject.name=e2e-pol", "-o", "jsonpath={.items[*].reason}")).To(SatisfyAll(ContainSubstring("SnapshotScheduled"), ContainSubstring("SnapshotPruned")))
		Eventually(func() string { return jsonpath("daossnapshotpolicy", "e2e-pol", ".status.snapshots") }).WithTimeout(2 * time.Minute).Should(Equal("1"))
		taken := jsonpath("daossnapshotpolicy", "e2e-pol", ".status.lastSnapshot")
		Expect(taken).To(HavePrefix("e2e-pol-2"))
		Expect(jsonpath("daoscontainersnapshot", taken, ".status.ready")).To(Equal("true"))

		By("deleting the policy leaves its snapshot")
		kubectl("delete", "daossnapshotpolicy", "e2e-pol", "--wait=true", "--timeout=1m")
		Consistently(func() string { return jsonpath("daoscontainersnapshot", taken, ".status.ready") }).WithTimeout(20 * time.Second).Should(Equal("true"))
		kubectl("annotate", "daoscontainersnapshot", taken, "daos.gluesys.com/destroy-approved=true", "--overwrite")
		kubectl("delete", "daoscontainersnapshot", taken, "--wait=false")
		waitGone("daoscontainersnapshot", taken, 5*time.Minute)

		By("a CSI VolumeSnapshot is a DaosContainerSnapshot of the volume")
		apply(`apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata: {name: e2e-vs}
spec:
  volumeSnapshotClassName: daos-snapshot
  source: {persistentVolumeClaimName: e2e-snap-src}`)
		start = time.Now()
		Eventually(func() string { return jsonpath("volumesnapshot", "e2e-vs", ".status.readyToUse") }).WithTimeout(5 * time.Minute).Should(Equal("true"))
		recordDuration("VolumeSnapshot ready", time.Since(start))
		handle := jsonpath("volumesnapshotcontent", jsonpath("volumesnapshot", "e2e-vs", ".status.boundVolumeSnapshotContentName"), ".status.snapshotHandle")
		Expect(handle).NotTo(BeEmpty())
		Expect(jsonpath("daoscontainersnapshot", handle, ".spec.containerRef")).To(Equal(cont))
		Expect(jsonpath("daoscontainersnapshot", handle, ".status.ready")).To(Equal("true"))
		runPod(pod("e2e-snap-w3", "e2e-snap-src", false, "set -e; echo v3 > /data/v; sync; cat /data/v"), "e2e-snap-w3", "v3")

		By("restoring the VolumeSnapshot: ReadOnlyMany mounts the snapshot, ReadWriteMany gets a copy (both v2)")
		claim := func(name, mode string) string {
			return fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: %s}
spec:
  accessModes: [%s]
  storageClassName: daos-e2e
  resources: {requests: {storage: 2Gi}}
  dataSource: {apiGroup: snapshot.storage.k8s.io, kind: VolumeSnapshot, name: e2e-vs}`, name, mode)
		}
		apply(claim("e2e-vs-ro", "ReadOnlyMany"))
		apply(claim("e2e-vs-rw", "ReadWriteMany"))
		Eventually(func() string { return jsonpath("pvc", "e2e-vs-ro", ".status.phase") }).WithTimeout(5 * time.Minute).Should(Equal("Bound"))
		runPod(pod("e2e-vs-r", "e2e-vs-ro", true, "cat /data/v; if echo x > /data/w 2>/dev/null; then echo WRITABLE; else echo READONLY; fi"), "e2e-vs-r", "READONLY")
		Expect(kubectl("logs", "e2e-vs-r")).To(SatisfyAll(ContainSubstring("v2"), Not(ContainSubstring("v3")), Not(ContainSubstring("WRITABLE"))))
		start = time.Now()
		Eventually(func() string { return jsonpath("pvc", "e2e-vs-rw", ".status.phase") }).WithTimeout(10 * time.Minute).Should(Equal("Bound"))
		recordDuration("VolumeSnapshot writable restore", time.Since(start))
		runPod(pod("e2e-vs-rw", "e2e-vs-rw", false, "cat /data/v; echo w > /data/w && cat /data/w && echo WRITABLE"), "e2e-vs-rw", "WRITABLE")
		Expect(kubectl("logs", "e2e-vs-rw")).To(SatisfyAll(ContainSubstring("v2"), Not(ContainSubstring("v3"))))
		kubectl("delete", "pod", "e2e-vs-r", "e2e-vs-rw", "--wait=true", "--timeout=3m")
		kubectl("delete", "pvc", "e2e-vs-rw", "--wait=true", "--timeout=3m")

		By("deleting the VolumeSnapshot waits for the read-only restore, then destroys the DAOS snapshot")
		kubectl("delete", "volumesnapshot", "e2e-vs", "--wait=false")
		Consistently(func() string { return jsonpath("daoscontainersnapshot", handle, ".status.ready") }).WithTimeout(30*time.Second).Should(Equal("true"),
			"a volume restored from it still mounts the epoch")
		kubectl("delete", "pvc", "e2e-vs-ro", "--wait=true", "--timeout=3m")
		waitGone("volumesnapshot", "e2e-vs", 5*time.Minute)
		waitGone("daoscontainersnapshot", handle, 5*time.Minute)
		Expect(kubectl("get", "events", "--field-selector", "involvedObject.name="+handle, "-o", "jsonpath={.items[*].reason}")).To(ContainSubstring("SnapshotDestroyed"))

		By("cleaning up: destroying the container takes its snapshots with it")
		kubectl("delete", "pod", "e2e-snap-w1", "e2e-snap-w2", "e2e-snap-w3", "--wait=true", "--timeout=3m")
		kubectl("delete", "pvc", "e2e-snap-src", "--wait=true", "--timeout=3m")
		waitGone("daoscontainer", cont, 12*time.Minute)
		kubectl("annotate", "daospool", "e2epool", "daos.gluesys.com/destroy-approved=true", "--overwrite")
		kubectl("delete", "daospool", "e2epool", "--wait=false")
		waitGone("daospool", "e2epool", 12*time.Minute)
	})

	// Case 15: interception injection (interception_test.go).
	It("injects the interception library into pods that ask for it and leaves the others on dfuse (case 15)", interceptionCase)

	// Case 7. The chart keeps the DaosSystem on uninstall (helm.sh/resource-policy: keep:
	// "uninstall must never stop engines"), and pools and containers carry finalizers only the
	// operator releases. So a clean removal is: tear the DAOS objects down while the operator
	// still runs, then uninstall. Afterwards nothing may be left behind.
	It("tears down in order and uninstalls cleanly (case 7)", func() {
		By("destroying containers, pools and the system while the operator runs")
		for _, kind := range []string{"daossnapshotpolicy", "daoscontainersnapshot", "daoscontainer", "daospool"} {
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
