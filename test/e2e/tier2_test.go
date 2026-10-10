//go:build e2e

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Gluesys Co., Ltd.

package e2e

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Tier 2 (doc/ci-design-2026-10-02.md 7절): what multi-rank systems do when parts of them fail.
// Run against kdev-3rank: E2E_PROFILE=kdev-3rank E2E_RANKS=3 -ginkgo.label-filter='tier2 && resilience'.
// Recovery times go to $E2E_TIMINGS (recordDuration) so their trend is visible night to night.
var _ = Describe("Tier 2 resilience", Label("tier2", "resilience"), Ordered, func() {
	// rankOn returns the rank number of node's first engine, from status.ranks.
	rankOn := func(node string) int {
		out := jsonpath("daossystem", sysName, `.status.ranks[?(@.node=="`+node+`")].rank`)
		f := strings.Fields(out)
		Expect(f).NotTo(BeEmpty(), "no rank on %s", node)
		n, err := strconv.Atoi(f[0])
		Expect(err).NotTo(HaveOccurred())
		return n
	}
	rankState := func(rank int) string {
		return jsonpath("daossystem", sysName, fmt.Sprintf(`.status.ranks[?(@.rank==%d)].state`, rank))
	}
	// signalServer sends sig (STOP/CONT/KILL) to daos_server from the node itself. Inside the
	// container daos_server is PID 1, and the kernel drops SIGSTOP/SIGKILL sent to a namespace's
	// init from inside it: the first NoQuorum run froze nothing (2026-10-05).
	signalServer := func(node, sig string) {
		ip := kubectl("get", "node", node, "-o", `jsonpath={.status.addresses[?(@.type=="InternalIP")].address}`)
		out := run("ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "root@"+ip,
			"pkill -"+sig+" -x daos_server && echo signalled")
		Expect(out).To(ContainSubstring("signalled"), "no daos_server on %s", node)
	}
	// poolLacksRank: the pool still has the rank's targets excluded. A rank that rejoins the system
	// on its own (DAOS 2.8 does after a node reboot) stays excluded in the pools it was dropped from:
	// the 2026-10-07 nightly saw the system Ready 3/3 and e2epool TargetsExcluded.
	poolLacksRank := func(rank int) bool {
		if d := jsonpath("daospool", "e2epool", ".status.disabledTargets"); d != "" && d != "0" {
			return true
		}
		enabled := jsonpath("daospool", "e2epool", ".status.enabledRanks")
		return !strings.Contains(" "+strings.Trim(strings.ReplaceAll(enabled, ",", " "), "[]")+" ", fmt.Sprintf(" %d ", rank))
	}
	// poolQueriedAfter: the operator has queried e2epool after t, so the status read
	// next describes the pool as it is now. The idle refresh is 60 s: the 2026-10-09 nightly read a
	// status queried just before DAOS excluded the rebooted rank's targets, skipped the reintegrate,
	// saw Ready=True, and failed a minute later on TargetsExcluded.
	poolQueriedAfter := func(t time.Time) bool {
		q, err := time.Parse(time.RFC3339, jsonpath("daospool", "e2epool", ".status.lastQueryTime"))
		return err == nil && q.After(t)
	}
	waitPoolQueriedAfter := func(t time.Time) {
		Eventually(func() bool { return poolQueriedAfter(t) }).WithTimeout(5*time.Minute).WithPolling(5*time.Second).
			Should(BeTrue(), "e2epool queried after %s", t.Format(time.RFC3339))
	}
	// containersReady: every DaosContainer reports Ready. With rd_fac 0 a rank exclusion leaves
	// the container at "failures exceed RF" until the operator clears it (incremental pools only),
	// and until then dfuse returns EIO on the whole mount (2026-10-10).
	containersReady := func() bool {
		out := kubectl("get", "daoscontainer", "-n", ns, "-o", `jsonpath={range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{" "}{end}`)
		f := strings.Fields(out)
		for _, s := range f {
			if s != "True" {
				return false
			}
		}
		return len(f) > 0
	}
	poolReady := func() string {
		return jsonpath("daospool", "e2epool", `.status.conditions[?(@.type=="Ready")].status`)
	}
	// reintegrateIfNeeded is the documented recovery once the engine is back: reintegrate the rank
	// unless it is back in the system and in the pool on its own (kubectl daos waits for the
	// result, daos-operator !14), then wait for the pool to finish rebuilding.
	reintegrateIfNeeded := func(rank int) {
		Eventually(func() string { return condStatus("ServersReady") }).WithTimeout(10 * time.Minute).Should(Equal("True"))
		time.Sleep(90 * time.Second)
		decided := time.Now()
		waitPoolQueriedAfter(decided)
		if rankState(rank) != "joined" || poolLacksRank(rank) {
			out := run(envOr("KUBECTL_DAOS", "kubectl-daos"), "rank", "reintegrate", sysName,
				fmt.Sprintf("--ranks=%d", rank), "--yes", "--wait=8m", "-n", ns)
			Expect(out).To(ContainSubstring("done: "))
			decided = time.Now()
		}
		// Ready only counts from a query made after the decision (or the reintegrate).
		Eventually(func() string {
			if !poolQueriedAfter(decided) {
				return "stale"
			}
			return poolReady()
		}).WithTimeout(10*time.Minute).WithPolling(10*time.Second).Should(Equal("True"), "e2epool back to Ready after rank %d", rank)
	}
	nodes := strings.Split(envOr("E2E_STORAGE_NODES", "exaci5-3a,exaci5-3b,exaci5-4b"), ",")
	victim := nodes[1] // not the client node, not necessarily the MS leader

	BeforeAll(func() {
		By("a pool and a volume with a checksummed file, kept for the whole container")
		apply(`apiVersion: daos.gluesys.com/v1alpha1
kind: DaosPool
metadata: {name: e2epool}
spec: {systemRef: daos, size: ` + poolSize() + `, redundancyFactor: 0}`)
		Eventually(func() string { return jsonpath("daospool", "e2epool", `.status.conditions[?(@.type=="Ready")].status`) }).
			WithTimeout(5 * time.Minute).Should(Equal("True"))
		apply(`apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: e2e-t2}
spec: {accessModes: [ReadWriteMany], storageClassName: daos-e2e, resources: {requests: {storage: 1Gi}}}`)
		apply(fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: e2e-t2-app}
spec:
  nodeSelector: {daos.gluesys.com/client: "true"}
  containers:
    - name: c
      image: %s
      command: [sh, -c, "head -c 16777216 /dev/urandom > /data/blob && sha256sum /data/blob > /data/blob.sha256 && sleep infinity"]
      volumeMounts: [{name: d, mountPath: /data}]
  volumes: [{name: d, persistentVolumeClaim: {claimName: e2e-t2}}]`, jsonpath("daossystem", sysName, ".spec.images.client")))
		Eventually(func() string { return jsonpath("pod", "e2e-t2-app", ".status.phase") }).WithTimeout(15 * time.Minute).Should(Equal("Running"))
		Eventually(func() error {
			_, err := kubectlE("exec", "e2e-t2-app", "--", "test", "-s", "/data/blob.sha256")
			return err
		}).WithTimeout(2 * time.Minute).Should(Succeed())
	})
	// checksumOK waits for the DaosContainers first: every case that excludes a rank leaves an
	// rd_fac 0 container at "failures exceed RF" until the operator clears it, and the drain case
	// read EIO before that (2026-10-10). 8 min: the operator waits up to 3 min for a pool status
	// newer than its DER_RF query, then queries again (rfPendingMax), ~4m40s in the worst case.
	checksumOK := func() {
		Eventually(containersReady).WithTimeout(8*time.Minute).WithPolling(10*time.Second).Should(BeTrue(),
			"DaosContainers Ready before the checksum (the operator clears failures-exceed-RF on incremental pools)")
		out, err := kubectlE("exec", "e2e-t2-app", "--", "sha256sum", "-c", "/data/blob.sha256")
		Expect(err).NotTo(HaveOccurred(), out)
		Expect(out).To(ContainSubstring("OK"))
	}
	It("brings a rank back after its server pod is deleted", func() {
		rank := rankOn(victim)
		start := time.Now()
		kubectl("delete", "pod", serverPod(victim), "--wait=false")
		// The StatefulSet restarts the pod in seconds; on the CI VMs the engine rejoined before
		// the membership probe saw it leave, and the rank read "joined" throughout (2026-10-05).
		// What must hold is the recovery, not the dip: record the dip when there is one.
		dipped := false
		for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(5 * time.Second) {
			if rankState(rank) != "joined" {
				dipped = true
				break
			}
		}
		if dipped {
			recordDuration("rank down detected", time.Since(start))
		} else {
			AddReportEntry("rank stayed joined", "the pod restarted inside the membership probe interval")
		}
		reintegrateIfNeeded(rank)
		waitReadyStable(10 * time.Minute)
		recordDuration("rank back to healthy", time.Since(start))
		checksumOK()
	})
	It("reports NoQuorum while two of three MS replicas are frozen (#34)", func() {
		frozen := []string{nodes[1], nodes[2]}
		DeferCleanup(func() {
			for _, n := range frozen {
				ip := kubectl("get", "node", n, "-o", `jsonpath={.status.addresses[?(@.type=="InternalIP")].address}`)
				_, _ = runE("ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "root@"+ip, "pkill -CONT -x daos_server")
			}
		})
		start := time.Now()
		for _, n := range frozen {
			signalServer(n, "STOP")
		}
		Eventually(func() string { return condReason("ManagementService") }).WithTimeout(5 * time.Minute).Should(Equal("NoQuorum"))
		recordDuration("NoQuorum reported", time.Since(start))
		for _, n := range frozen {
			signalServer(n, "CONT")
		}
		start = time.Now()
		waitReadyStable(10 * time.Minute)
		recordDuration("quorum back to healthy", time.Since(start))
	})
	It("elects a new MS leader after the leader's daos_server is killed", func() {
		leaderRe := regexp.MustCompile(`leader ([0-9.]+):`)
		leaderIP := func() string {
			m := leaderRe.FindStringSubmatch(jsonpath("daossystem", sysName, `.status.conditions[?(@.type=="ManagementService")].message`))
			if m == nil {
				return ""
			}
			return m[1]
		}
		old := leaderIP()
		Expect(old).NotTo(BeEmpty())
		node := jsonpath("daossystem", sysName, `.status.nodeConfigs[?(@.controlAddr=="`+old+`")].node`)
		Expect(node).NotTo(BeEmpty(), "no node with control address %s", old)
		start := time.Now()
		signalServer(node, "KILL")
		Eventually(func() string { return leaderIP() }).WithTimeout(2*time.Minute).
			Should(And(Not(BeEmpty()), Not(Equal(old))), "a new leader must be reported")
		recordDuration("new MS leader reported", time.Since(start))
		// The design expected the data path to stall ~2 min after a leader change. A re-read of the
		// file came back in under 10 s (2026-10-05), but that can be dfuse's cache. Write and fsync
		// new data instead and record how long it takes: a stall shows as time, bounded at 5 min.
		w := time.Now()
		out, err := kubectlE("exec", "e2e-t2-app", "--", "sh", "-c",
			"timeout 300 sh -c 'head -c 4194304 /dev/urandom > /data/after-leader && sync /data/after-leader' && echo WROTE")
		Expect(err).NotTo(HaveOccurred(), out)
		Expect(out).To(ContainSubstring("WROTE"))
		recordDuration("write+fsync after leader change", time.Since(w))
		reintegrateIfNeeded(rankOn(node))
		waitReadyStable(10 * time.Minute)
		recordDuration("leader kill back to healthy", time.Since(start))
		checksumOK()
	})
	It("keeps the pool and its data across a storage node reboot", func() {
		rank := rankOn(victim)
		start := time.Now()
		run(filepath.Join(repoRoot, "hack", "ci", "node-power.sh"), "reboot", victim)
		Eventually(func() string {
			return kubectl("get", "node", victim, "-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`)
		}).WithTimeout(10 * time.Minute).Should(Equal("True"))
		recordDuration("node Ready after reboot", time.Since(start))
		reintegrateIfNeeded(rank)
		waitReadyStable(15 * time.Minute)
		recordDuration("reboot back to healthy", time.Since(start))
		settled := time.Now()
		waitPoolQueriedAfter(settled)
		Expect(poolReady()).To(Equal("True"), "e2epool after the system held Ready")
		checksumOK()
		recordDuration("reboot to container Ready", time.Since(start))
	})
	It("recovers after a storage node is drained and uncordoned", func() {
		rank := rankOn(victim)
		start := time.Now()
		DeferCleanup(func() { _, _ = kubectlE("uncordon", victim) })
		kubectl("drain", victim, "--ignore-daemonsets", "--delete-emptydir-data", "--force", "--timeout=5m")
		Eventually(func() string { return rankState(rank) }).WithTimeout(5 * time.Minute).ShouldNot(Equal("joined"))
		kubectl("uncordon", victim)
		reintegrateIfNeeded(rank)
		waitReadyStable(10 * time.Minute)
		recordDuration("drain/uncordon back to healthy", time.Since(start))
		checksumOK()
	})
	It("formats and joins a fourth rank when a node is added (scale-out)", func() {
		node := envOr("E2E_SCALEOUT_NODE", "exaci5-4a")
		start := time.Now()
		kubectl("label", "node", node, "daos.gluesys.com/role=storage", "--overwrite")
		Eventually(func() string {
			return jsonpath("daossystem", sysName, `.status.conditions[?(@.type=="Formatted")].message`)
		}).WithTimeout(10 * time.Minute).Should(ContainSubstring("not formatted yet"))
		kubectl("annotate", "daossystem", sysName, "daos.gluesys.com/format-approved=true", "--overwrite")
		Eventually(systemHealthy).WithTimeout(15 * time.Minute).Should(Equal("Ready=True ManagementService=True ranksJoined=4"))
		recordDuration("scale-out to 4 ranks", time.Since(start))
		checksumOK()
	})

	It("keeps a mounted volume readable across a CSI node plugin restart (known daos-csi bug)", func() {
		node := jsonpath("pod", "e2e-t2-app", ".spec.nodeName")
		plugin := kubectl("get", "pod", "-n", ns, "-l", "app.kubernetes.io/name=daos-csi,app.kubernetes.io/component=node",
			"--field-selector", "spec.nodeName="+node, "-o", "jsonpath={.items[0].metadata.name}")
		Expect(plugin).NotTo(BeEmpty())
		kubectl("delete", "pod", "-n", ns, plugin, "--wait=true", "--timeout=3m")
		Eventually(func() string {
			return kubectl("get", "pod", "-n", ns, "-l", "app.kubernetes.io/name=daos-csi,app.kubernetes.io/component=node",
				"--field-selector", "spec.nodeName="+node, "-o", `jsonpath={.items[0].status.conditions[?(@.type=="Ready")].status}`)
		}).WithTimeout(3 * time.Minute).Should(Equal("True"))
		// daos-csi runs dfuse inside the node plugin: the restart kills it, the new plugin mounts a
		// fresh dfuse in its own mount namespace, and the pod's bind still points at the dead FUSE
		// connection ("Transport endpoint is not connected", 2026-10-05). Last in the container
		// because it leaves the volume broken.
		expectKnownBug("daos-csi: a node plugin restart breaks every mounted volume on that node", func() {
			checksumOK()
		})
	})
})

// mixed-2engine only: `dmg storage query usage` panics daos_server 2.8 when a node mixes nvme and
// kdev bdev lists (2026-09-15). Pinned as a known bug; the system must still come back afterwards.
var _ = Describe("Tier 2 mixed", Label("tier2", "mixed"), func() {
	It("runs dmg storage query usage on mixed bdev lists (known panic)", func() {
		admin := jsonpath("daossystem", sysName, ".spec.images.admin")
		apply(fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: e2e-usage}
spec:
  restartPolicy: Never
  hostNetwork: true
  nodeSelector: {daos.gluesys.com/role: storage}
  containers:
    - name: c
      image: %s
      command: [dmg, -j, -o, /etc/daos/daos_control.yml, storage, query, usage]
      volumeMounts: [{name: ctl, mountPath: /etc/daos}]
  volumes: [{name: ctl, configMap: {name: %s-control}}]`, admin, sysName))
		DeferCleanup(func() { _, _ = kubectlE("delete", "pod", "e2e-usage", "--wait=false") })
		Eventually(func() string { return jsonpath("pod", "e2e-usage", ".status.phase") }).WithTimeout(5 * time.Minute).
			Should(Or(Equal("Succeeded"), Equal("Failed")))
		expectKnownBug("dmg storage query usage panics daos_server on mixed bdev lists (2026-09-15)", func() {
			Consistently(func() string { return condStatus("ServersReady") }).WithTimeout(2 * time.Minute).Should(Equal("True"))
		})
		waitReadyStable(15 * time.Minute)
	})
})
