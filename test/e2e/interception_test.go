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

// ilNS is outside the release namespace, which the interception webhook never sees.
const ilNS = "e2e-il"

// ilClientImage has the /opt/daos-il bundle (daos-images !11); the profiles' client image predates it.
var ilClientImage = envOr("E2E_IL_CLIENT_IMAGE", "registry.gitlab.gluesys.com/exastor/daos-images/daos-client:2.8.0-20261119-g995d6836")

func applyIn(namespace, manifest string) {
	cmd := exec.Command("kubectl", "-n", namespace, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	b, err := cmd.CombinedOutput()
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "kubectl apply\n%s\n%s", manifest, b)
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

	By("creating the pool, a namespace with the pull secret, and a volume in it")
	apply(`apiVersion: daos.gluesys.com/v1alpha1
kind: DaosPool
metadata: {name: e2epool}
spec: {systemRef: daos, size: ` + poolSize() + `, redundancyFactor: 0}`)
	Eventually(func() string { return jsonpath("daospool", "e2epool", `.status.conditions[?(@.type=="Ready")].status`) }).
		WithTimeout(5 * time.Minute).Should(Equal("True"))
	if out, err := runE("kubectl", "create", "namespace", ilNS); err != nil && !strings.Contains(out, "AlreadyExists") {
		Fail("create namespace " + ilNS + ": " + out)
	}
	DeferCleanup(func() {
		_, _ = runE("kubectl", "delete", "namespace", ilNS, "--wait=true", "--timeout=5m")
		Eventually(func() string { out, _ := kubectlE("get", "daoscontainer", "-o", "name"); return strings.TrimSpace(out) }).
			WithTimeout(12*time.Minute).Should(BeEmpty(), "DaosContainer left behind")
		kubectl("annotate", "daospool", "e2epool", "daos.gluesys.com/destroy-approved=true", "--overwrite")
		kubectl("delete", "daospool", "e2epool", "--wait=false")
		waitGone("daospool", "e2epool", 12*time.Minute)
	})
	secret := kubectl("get", "secret", "gitlab-registry", "-o", "json")
	var s map[string]any
	Expect(json.Unmarshal([]byte(secret), &s)).To(Succeed())
	s["metadata"] = map[string]any{"name": "gitlab-registry", "namespace": ilNS}
	b, _ := json.Marshal(s)
	applyIn(ilNS, string(b))
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

	pod := func(name, mode, image string, hostNetwork bool, script string) string {
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
  nodeSelector: {%q: %q}
  containers:
    - name: app
      image: %s
      command: [sh, -c, %q]
      volumeMounts: [{name: d, mountPath: /data}]
  volumes: [{name: d, persistentVolumeClaim: {claimName: il-data}}]`, name, mode, hostNetwork, k, v, image, script)
	}
	write := func(dir string) string {
		return "set -e; mkdir -p /data/" + dir + "; dd if=/dev/zero of=/data/" + dir + "/blob bs=1M count=16 status=none; " +
			"for i in 1 2 3; do echo $i > /data/" + dir + "/f$i; done; cat /data/" + dir + "/f3; echo IL-DONE"
	}

	By("starting a compatible pod (pil4dfs-compat), an Alpine pod (ioil) and a pod-network pod (ioil)")
	applyIn(ilNS, pod("il-el9", "pil4dfs-compat", ilClientImage, true, write("el9")))
	applyIn(ilNS, pod("il-alpine", "ioil", envOr("E2E_ALPINE_IMAGE", "docker.io/library/alpine:3.20"), true, write("alpine")))
	applyIn(ilNS, pod("il-podnet", "ioil", ilClientImage, false, write("podnet")))

	for _, p := range []string{"il-el9", "il-alpine", "il-podnet"} {
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
}
