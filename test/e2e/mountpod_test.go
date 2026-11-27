//go:build e2e

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Gluesys Co., Ltd.

package e2e

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	mpPluginSel = "app.kubernetes.io/name=daos-csi,app.kubernetes.io/component=node"
	mpMountSel  = "csi.daos.gluesys.com/mount-pod=true"
	// The writer ticks every 0.2 s. A node plugin restart takes several seconds, so a mount that
	// depended on the plugin would stall the writes for longer than this.
	mpMaxWriteGap = 3 * time.Second
)

var okLineRe = regexp.MustCompile(`(?m)^(\S+) OK (\d+)$`)

// maxWriteGap reads `kubectl logs --timestamps` of the writer and returns the longest time
// between consecutive OK lines whose numbers are within [from, to], and the last OK number.
func maxWriteGap(logs string, from, to int) (time.Duration, int) {
	var gap time.Duration
	var prev time.Time
	last := 0
	for _, m := range okLineRe.FindAllStringSubmatch(logs, -1) {
		at, err := time.Parse(time.RFC3339Nano, m[1])
		if err != nil {
			continue
		}
		n, _ := strconv.Atoi(m[2])
		last = n
		if n < from || n > to {
			prev = time.Time{}
			if n < from {
				prev = at
			}
			continue
		}
		if !prev.IsZero() && at.Sub(prev) > gap {
			gap = at.Sub(prev)
		}
		prev = at
	}
	return gap, last
}

// mountPodCase is Tier 1 case 16 (daos-csi ADR-csi-002): with csi.mountMode pod, dfuse runs in a
// mount pod per volume, so a node plugin restart must not touch a volume a pod is writing to.
// In process mode the same restart breaks the mount (tier2 "known daos-csi bug"). It switches
// the release to pod mode and leaves it there: run it after every other CSI case, before case 7.
func mountPodCase() {
	sel := envOr("E2E_CLIENT_SELECTOR", "daos.gluesys.com/client=true")
	k, v, _ := strings.Cut(sel, "=")

	By("switching the release to mount pods (needs the node agent)")
	agent, _ := json.Marshal(map[string]any{"enabled": true, "nodeSelector": map[string]string{k: v}})
	run("helm", "upgrade", release, filepath.Join(repoRoot, "charts", "daos-operator"), "-n", ns, "--reuse-values",
		"--set", "csi.mountMode=pod", "--set-json", "system.spec.clientAgent="+string(agent))
	Eventually(func() string { return condStatus("ClientAgent") }).WithTimeout(5 * time.Minute).Should(Equal("True"))
	ds := kubectl("get", "daemonset", "-l", mpPluginSel, "-o", "jsonpath={.items[0].metadata.name}")
	Expect(ds).NotTo(BeEmpty(), "no CSI node plugin DaemonSet")
	kubectl("rollout", "status", "daemonset/"+ds, "--timeout=5m")
	Expect(jsonpath("daemonset", ds, ".spec.template.spec.containers[*].args")).To(ContainSubstring("--mount-mode=pod"))

	By("creating the pool and a volume")
	apply(`apiVersion: daos.gluesys.com/v1alpha1
kind: DaosPool
metadata: {name: e2epool}
spec: {systemRef: daos, size: ` + poolSize() + `, redundancyFactor: 0}`)
	Eventually(func() string { return jsonpath("daospool", "e2epool", `.status.conditions[?(@.type=="Ready")].status`) }).
		WithTimeout(5 * time.Minute).Should(Equal("True"))
	DeferCleanup(func() {
		_, _ = kubectlE("delete", "pod", "e2e-mp-writer", "e2e-mp-reader", "--wait=true", "--timeout=3m")
		_, _ = kubectlE("delete", "pvc", "e2e-mp", "--wait=true", "--timeout=3m")
		Eventually(func() string { out, _ := kubectlE("get", "daoscontainer", "-o", "name"); return strings.TrimSpace(out) }).
			WithTimeout(12*time.Minute).Should(BeEmpty(), "DaosContainer left behind")
		kubectl("annotate", "daospool", "e2epool", "daos.gluesys.com/destroy-approved=true", "--overwrite")
		kubectl("delete", "daospool", "e2epool", "--wait=false")
		waitGone("daospool", "e2epool", 12*time.Minute)
	})
	apply(`apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: e2e-mp}
spec:
  accessModes: [ReadWriteMany]
  storageClassName: daos-e2e
  resources: {requests: {storage: 1Gi}}`)

	// One file held open for the whole case (a restart that swaps the FUSE connection under it
	// fails the next write with ENOTCONN) and a fresh create each tick, five ticks a second.
	client := jsonpath("daossystem", sysName, ".spec.images.client")
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
  volumes: [{name: d, persistentVolumeClaim: {claimName: e2e-mp}}]`, name, k, v, client, script)
	}
	By("writing without a pause")
	apply(pod("e2e-mp-writer", `exec 3>>/data/long.dat; i=0
while [ ! -e /tmp/stop ]; do i=$((i+1))
  if head -c 65536 /dev/urandom >&3 && head -c 4096 /dev/urandom > /data/f.$((i % 50)); then echo "OK $i"; else echo "ERR $i"; fi
  sleep 0.2
done
exec 3>&-; sha256sum /data/long.dat; echo STOPPED`))
	Eventually(func() string { return jsonpath("pod", "e2e-mp-writer", ".status.phase") }).WithTimeout(15 * time.Minute).Should(Equal("Running"))
	node := jsonpath("pod", "e2e-mp-writer", ".spec.nodeName")
	mountPod := func() string {
		return kubectl("get", "pod", "-l", mpMountSel, "--field-selector", "spec.nodeName="+node,
			"-o", "jsonpath={range .items[*]}{.metadata.uid} {.status.containerStatuses[0].restartCount}{end}")
	}
	Eventually(mountPod).WithTimeout(time.Minute).ShouldNot(BeEmpty(), "no mount pod on %s", node)
	before := mountPod()
	Expect(strings.Fields(before)).To(HaveLen(2), "one mount pod for one volume on %s", node)
	Expect(kubectl("get", "pod", "-l", mpMountSel, "--field-selector", "spec.nodeName="+node,
		"-o", "jsonpath={.items[0].metadata.ownerReferences[0].kind}/{.items[0].metadata.ownerReferences[0].name}")).
		To(Equal("Node/"+node), "the mount pod belongs to its node (drain evicts it, node removal collects it)")

	// the last OK the writer logged; the gaps between OKs come from kubelet's log timestamps
	// (one clock, the node's)
	lastOK := func() int {
		_, last := maxWriteGap(kubectl("logs", "--timestamps", "e2e-mp-writer"), 0, 1<<30)
		return last
	}
	Eventually(lastOK).WithTimeout(time.Minute).Should(BeNumerically(">", 20))

	pluginReady := func() string {
		return kubectl("get", "pod", "-l", mpPluginSel, "--field-selector", "spec.nodeName="+node,
			"-o", `jsonpath={range .items[*]}{.metadata.name} {.status.conditions[?(@.type=="Ready")].status} {end}`)
	}
	restarted := func(old string) func() bool {
		return func() bool {
			f := strings.Fields(pluginReady())
			return len(f) == 2 && f[0] != old && f[1] == "True"
		}
	}
	for _, how := range []string{"delete", "rollout restart"} {
		By("restarting the node plugin under the writer: " + how)
		old := strings.Fields(pluginReady())[0]
		from := lastOK()
		if how == "delete" {
			kubectl("delete", "pod", old, "--wait=true", "--timeout=3m")
		} else {
			kubectl("rollout", "restart", "daemonset/"+ds)
		}
		Eventually(restarted(old)).WithTimeout(5*time.Minute).Should(BeTrue(), "node plugin on %s did not come back", node)
		to := lastOK() + 20
		Eventually(lastOK).WithTimeout(time.Minute).Should(BeNumerically(">=", to), "the writer stalled after the plugin restart")
		// Not only "it went on afterwards": a pause while the plugin was away would pass that.
		gap, _ := maxWriteGap(kubectl("logs", "--timestamps", "e2e-mp-writer"), from, to)
		Expect(gap).To(BeNumerically("<", mpMaxWriteGap), "the writes paused %s while the plugin restarted (%s)", gap, how)
		AddReportEntry("longest write gap across a plugin "+how, gap.String())
		Expect(mountPod()).To(Equal(before), "the mount pod must outlive the plugin (same UID, no restart)")
		Expect(kubectl("logs", "e2e-mp-writer")).NotTo(ContainSubstring("ERR"))
	}

	By("stopping the writer and reading the open file back from a new pod")
	kubectl("exec", "e2e-mp-writer", "--", "touch", "/tmp/stop")
	Eventually(func() string { return jsonpath("pod", "e2e-mp-writer", ".status.phase") }).WithTimeout(2 * time.Minute).Should(Equal("Succeeded"))
	logs := kubectl("logs", "e2e-mp-writer")
	Expect(logs).NotTo(ContainSubstring("ERR"))
	sum := regexp.MustCompile(`(?m)^([0-9a-f]{64})  /data/long\.dat$`).FindStringSubmatch(logs)
	Expect(sum).To(HaveLen(2), "writer printed no checksum:\n%s", tail(logs, 5))
	apply(pod("e2e-mp-reader", "sha256sum /data/long.dat"))
	Eventually(func() string { return jsonpath("pod", "e2e-mp-reader", ".status.phase") }).WithTimeout(5 * time.Minute).Should(Equal("Succeeded"))
	Expect(kubectl("logs", "e2e-mp-reader")).To(ContainSubstring(sum[1]))

	By("unstaging: the last pod gone, the mount pod goes too")
	kubectl("delete", "pod", "e2e-mp-writer", "e2e-mp-reader", "--wait=true", "--timeout=3m")
	Eventually(func() string { return kubectl("get", "pod", "-l", mpMountSel, "-o", "name") }).
		WithTimeout(3*time.Minute).Should(BeEmpty(), "mount pod left after its volume was unstaged")
}
