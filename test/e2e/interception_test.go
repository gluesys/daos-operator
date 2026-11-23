//go:build e2e

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Gluesys Co., Ltd.

package e2e

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// ilNS is the run's own namespace, outside the release namespace (which the
// interception webhook never sees); interceptionCase sets it.
var ilNS string

// ilClientImage has the /opt/daos-il bundle (daos-images !11); the profiles' client image predates it.
var ilClientImage = envOr("E2E_IL_CLIENT_IMAGE", "registry.gitlab.gluesys.com/exastor/daos-images/daos-client:2.8.0-20261119-g995d6836")

func applyIn(namespace, manifest string) {
	cmd := exec.Command("kubectl", "-n", namespace, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	b, err := cmd.CombinedOutput()
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "kubectl apply\n%s\n%s", manifest, b)
}

// applySecret applies a Secret without putting its manifest or kubectl's output
// (which can echo it) into the failure message: the pull secret holds
// registry credentials.
func applySecret(kubectlBin, namespace, name string, manifest []byte) error {
	cmd := exec.Command(kubectlBin, "-n", namespace, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(string(manifest))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl apply secret %s/%s: %v", namespace, name, err)
	}
	return nil
}

func ilField(pod, path string) string {
	out, _ := kubectlE("-n", ilNS, "get", "pod", pod, "-o", "jsonpath={"+path+"}")
	return strings.TrimSpace(out)
}

// Case 15 (doc/plans/2026-11-19-client-interception.md S2-6, doc/interception.md). The webhook
// injects into a pod that can be a DAOS client and asks for it, the check in the application's
// image decides whether the library is used, and a pod that cannot use it runs on dfuse as before.
// The lane is ofi+tcp, so this covers the decision, the init containers, the agent socket and
// interception itself, not RDMA.
func interceptionCase() {
	Expect(jsonpath("deploy", release, ".spec.template.spec.containers[0].args")).To(ContainSubstring("--interception"),
		"the profile must set interception.enabled")
	sel := envOr("E2E_CLIENT_SELECTOR", "daos.gluesys.com/client=true")
	k, v, _ := strings.Cut(sel, "=")

	By("creating a namespace of this run's own")
	// a fixed name could be someone else's namespace, and the cleanup deletes it
	ilNS = fmt.Sprintf("e2e-il-%d", time.Now().UnixNano()%1_000_000_000)
	if out, err := runE("kubectl", "create", "namespace", ilNS); err != nil {
		Fail("create namespace " + ilNS + ": " + out)
	}
	DeferCleanup(func() { _, _ = runE("kubectl", "delete", "namespace", ilNS, "--wait=true", "--timeout=5m") })

	By("turning the client agent on and using a client image with the bundle")
	before := jsonpath("daossystem", sysName, ".spec.images.client")
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{
		"images":      map[string]any{"client": ilClientImage},
		"clientAgent": map[string]any{"enabled": true, "nodeSelector": map[string]string{k: v}},
	}})
	kubectl("patch", "daossystem", sysName, "--type", "merge", "-p", string(patch))
	DeferCleanup(func() {
		restore, _ := json.Marshal(map[string]any{"spec": map[string]any{
			"images": map[string]any{"client": before}, "clientAgent": nil}})
		kubectl("patch", "daossystem", sysName, "--type", "merge", "-p", string(restore))
	})
	Eventually(func() string { return condStatus("ClientAgent") }).WithTimeout(5 * time.Minute).Should(Equal("True"))

	By("creating the pool, the pull secret and a volume in the namespace")
	apply(`apiVersion: daos.gluesys.com/v1alpha1
kind: DaosPool
metadata: {name: e2epool}
spec: {systemRef: daos, size: ` + poolSize() + `, redundancyFactor: 0}`)
	Eventually(func() string { return jsonpath("daospool", "e2epool", `.status.conditions[?(@.type=="Ready")].status`) }).
		WithTimeout(5 * time.Minute).Should(Equal("True"))
	DeferCleanup(func() {
		// the volume's container goes with the namespace, before the pool
		_, _ = runE("kubectl", "delete", "namespace", ilNS, "--wait=true", "--timeout=5m")
		Eventually(func() string { out, _ := kubectlE("get", "daoscontainer", "-o", "name"); return strings.TrimSpace(out) }).
			WithTimeout(12*time.Minute).Should(BeEmpty(), "DaosContainer left behind")
		kubectl("annotate", "daospool", "e2epool", "daos.gluesys.com/destroy-approved=true", "--overwrite")
		kubectl("delete", "daospool", "e2epool", "--wait=false")
		waitGone("daospool", "e2epool", 12*time.Minute)
	})
	secret, err := kubectlE("get", "secret", "gitlab-registry", "-o", "json")
	Expect(err).NotTo(HaveOccurred(), "get secret gitlab-registry")
	var s map[string]any
	Expect(json.Unmarshal([]byte(secret), &s)).To(Succeed(), "secret gitlab-registry is not JSON")
	s["metadata"] = map[string]any{"name": "gitlab-registry", "namespace": ilNS}
	b, _ := json.Marshal(s)
	Expect(applySecret("kubectl", ilNS, "gitlab-registry", b)).To(Succeed())
	applyIn(ilNS, `apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: il-data}
spec:
  accessModes: [ReadWriteMany]
  storageClassName: daos-e2e
  resources: {requests: {storage: 1Gi}}`)
	Eventually(func() string {
		out, _ := kubectlE("-n", ilNS, "get", "pvc", "il-data", "-o", "jsonpath={.status.phase}")
		return out
	}).
		WithTimeout(15 * time.Minute).Should(Equal("Bound"))

	pinned := fmt.Sprintf("nodeSelector: {%q: %q}", k, v)
	// same nodes, but by affinity, which the webhook does not evaluate
	byAffinity := fmt.Sprintf(`affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms: [{matchExpressions: [{key: %q, operator: In, values: [%q]}]}]`, k, v)
	pod := func(name, mode, image string, hostNetwork bool, placement, script string) string {
		return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  annotations: {daos.gluesys.com/interception: %s, daos.gluesys.com/interception-report: "true"}
spec:
  restartPolicy: Never
  hostNetwork: %t
  dnsPolicy: ClusterFirstWithHostNet
  imagePullSecrets: [{name: gitlab-registry}]
  %s
  containers:
    - name: app
      image: %s
      command: [sh, -c, %q]
      volumeMounts: [{name: d, mountPath: /data}]
  volumes: [{name: d, persistentVolumeClaim: {claimName: il-data}}]`, name, mode, hostNetwork, placement, image, script)
	}
	write := func(dir string) string {
		return "set -e; mkdir -p /data/" + dir + "; dd if=/dev/zero of=/data/" + dir + "/blob bs=1M count=16 status=none; " +
			"for i in 1 2 3; do echo $i > /data/" + dir + "/f$i; done; cat /data/" + dir + "/f3; echo IL-DONE"
	}

	By("starting a compatible pod (pil4dfs-compat), an Alpine pod, a pod-network pod and a pod not pinned to the agent's nodes")
	applyIn(ilNS, pod("il-el9", "pil4dfs-compat", ilClientImage, true, pinned, write("el9")))
	applyIn(ilNS, pod("il-alpine", "ioil", envOr("E2E_ALPINE_IMAGE", "docker.io/library/alpine:3.20"), true, pinned, write("alpine")))
	applyIn(ilNS, pod("il-podnet", "ioil", ilClientImage, false, pinned, write("podnet")))
	applyIn(ilNS, pod("il-unpinned", "ioil", ilClientImage, true, byAffinity, write("unpinned")))

	for _, p := range []string{"il-el9", "il-alpine", "il-podnet", "il-unpinned"} {
		Eventually(func() string { return ilField(p, ".status.phase") }).WithTimeout(10*time.Minute).
			Should(Equal("Succeeded"), "pod %s", p)
		logs, _ := runE("kubectl", "-n", ilNS, "logs", p, "-c", "app")
		Expect(logs).To(ContainSubstring("IL-DONE"), "pod %s", p)
		fmt.Fprintf(GinkgoWriter, "%s: %s | %s\n", p, ilField(p, `.metadata.annotations.daos\.gluesys\.com/interception-result`),
			ilField(p, `.status.initContainerStatuses[?(@.name=="daos-il-check")].state.terminated.message`))
	}

	By("checking the decisions")
	result := func(p string) string {
		return ilField(p, `.metadata.annotations.daos\.gluesys\.com/interception-result`)
	}
	check := func(p string) string {
		return ilField(p, `.status.initContainerStatuses[?(@.name=="daos-il-check")].state.terminated.message`)
	}
	Expect(result("il-el9")).To(Equal("injected/libpil4dfs"))
	Expect(check("il-el9")).To(HavePrefix("injected/libpil4dfs"))
	logs, _ := runE("kubectl", "-n", ilNS, "logs", "il-el9", "-c", "app")
	Expect(logs).To(MatchRegexp(`\[write\s*\]\s*16\b`), "dd's 16 writes must go through libpil4dfs:\n%s", tail(logs, 40))

	Expect(result("il-alpine")).To(Equal("injected/libioil"))
	Expect(check("il-alpine")).To(HavePrefix("skipped/image-incompatible"), "musl cannot load the EL9 bundle")

	Expect(result("il-podnet")).To(HavePrefix("skipped/"))
	Expect(ilField("il-podnet", ".spec.initContainers")).To(BeEmpty(), "a skipped pod is left as it was")
	Expect(regexp.MustCompile(`LD_PRELOAD`).MatchString(ilField("il-podnet", ".spec.containers[0].env"))).To(BeFalse())

	Expect(result("il-unpinned")).To(Equal("skipped/agent-placement"), "the agent has a nodeSelector the pod does not carry")
	Expect(ilField("il-unpinned", ".spec.initContainers")).To(BeEmpty())
	Expect(ilField("il-unpinned", ".spec.volumes")).NotTo(ContainSubstring("hostPath"))

	accountUIDCase(pod, pinned)
}

// Part of case 15 (operator #40). A pod running as an account UID cannot take
// dfuse's pool handle (dfuse answers that ioctl only for its own UID), so
// libioil connects to the pool itself, and the agent has to build the
// credential from a UID its container does not know. Without
// spec.client.clientUserMap that fails and libioil quietly leaves the file on
// dfuse; with a map whose names the ACLs grant, the reads are intercepted; a
// map to a name the ACLs do not grant falls back again.
func accountUIDCase(pod func(name, mode, image string, hostNetwork bool, placement, script string) string, pinned string) {
	read := "set -e; id; ls -ln /data/uid; cat /data/uid/blob > /dev/null; echo IL-DONE"
	asUser := func(name string) string {
		return strings.Replace(pod(name, "ioil", ilClientImage, true, pinned, read),
			"  restartPolicy: Never\n", "  restartPolicy: Never\n  securityContext: {runAsUser: 1001, runAsGroup: 1001}\n", 1)
	}
	setMap := func(m any) {
		patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"client": map[string]any{"clientUserMap": m}}})
		kubectl("patch", "daossystem", sysName, "--type", "merge", "-p", string(patch))
		Eventually(func() string {
			out, _ := kubectlE("get", "configmap", sysName+"-agent", "-o", `jsonpath={.data.daos_agent\.yml}`)
			return out
		}).WithTimeout(2 * time.Minute).Should(func() OmegaMatcher {
			if m == nil {
				return Not(ContainSubstring("client_user_map"))
			}
			return ContainSubstring("client_user_map")
		}())
		// the node agents roll to a config version of their own
		Eventually(func() error {
			_, err := kubectlE("rollout", "status", "daemonset/"+sysName+"-agent", "--timeout=30s")
			return err
		}).WithTimeout(5 * time.Minute).Should(Succeed())
		Eventually(func() string { return condStatus("ClientAgent") }).WithTimeout(5 * time.Minute).Should(Equal("True"))
	}
	DeferCleanup(func() { setMap(nil) })
	runPod := func(name string) string {
		applyIn(ilNS, asUser(name))
		Eventually(func() string { return ilField(name, ".status.phase") }).WithTimeout(10*time.Minute).
			Should(BeElementOf("Succeeded", "Failed"), "pod %s", name)
		logs, _ := runE("kubectl", "-n", ilNS, "logs", name, "-c", "app")
		Expect(ilField(name, ".status.phase")).To(Equal("Succeeded"), "pod %s must read through dfuse at least:\n%s", name, tail(logs, 20))
		Expect(logs).To(ContainSubstring("uid=1001"), "pod %s", name)
		Expect(logs).To(ContainSubstring("IL-DONE"), "pod %s", name)
		Expect(ilField(name, `.metadata.annotations.daos\.gluesys\.com/interception-result`)).To(Equal("injected/libioil"))
		fmt.Fprintf(GinkgoWriter, "%s:\n%s\n", name, tail(logs, 20))
		return logs
	}
	By("a root pod leaves a file every UID may read")
	applyIn(ilNS, pod("il-uid-seed", "off", ilClientImage, true, pinned,
		"set -e; mkdir -p /data/uid; dd if=/dev/zero of=/data/uid/blob bs=1M count=16 status=none; "+
			"chmod 0755 /data/uid; chmod 0644 /data/uid/blob; ls -ln /data/uid; echo IL-DONE"))
	Eventually(func() string { return ilField("il-uid-seed", ".status.phase") }).WithTimeout(10 * time.Minute).Should(Equal("Succeeded"))

	By("an account UID without a map: no credential, libioil leaves the reads on dfuse")
	logs := runPod("il-uid-nomap")
	Expect(logs).NotTo(ContainSubstring("[libioil] Intercepting read"), "no credential, so no pool handle")

	By("mapping UID 1001 to the pool owner's principals: the reads are intercepted")
	setMap(map[string]any{"1001": map[string]any{"user": "root", "group": "root"}})
	logs = runPod("il-uid-map")
	Expect(logs).To(ContainSubstring("[libioil] Intercepting read"), "a mapped UID connects with the mapped names")

	By("mapping to names the ACLs do not grant: the pool refuses, libioil falls back again")
	setMap(map[string]any{"default": map[string]any{"user": "nobody", "group": "nobody"}})
	logs = runPod("il-uid-nobody")
	Expect(logs).NotTo(ContainSubstring("[libioil] Intercepting read"), "nobody@ is not in the default pool ACL")
}
