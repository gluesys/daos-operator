/*
SPDX-License-Identifier: Apache-2.0
Copyright 2026 Gluesys Co., Ltd.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// kubectl-daos is the imperative side of the operator (#17): the few actions
// that must stay human decisions -- formatting, upgrading, destroying -- are
// approvals written onto the CRs, and this plugin writes them after showing
// what they will do and asking. Put the binary on PATH as kubectl-daos and
// call it as `kubectl daos ...`.
//
//	kubectl daos system status  <sys>
//	kubectl daos system format  <sys>            # daos.gluesys.com/format-approved=true
//	kubectl daos system upgrade <sys> [--image I] [--version V]   # spec.upgrade.approved=true
//	kubectl daos pool destroy   <pool>           # destroy-approved=true + delete
//	kubectl daos cont destroy   -n <ns> <cont>   # destroy-approved=true + delete
//
// --yes skips the prompt (scripts, CI).
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	daosv1alpha1 "gitlab.gluesys.com/exastor/daos-operator/api/v1alpha1"
)

// app holds the dependencies so the command logic is testable without a cluster.
type app struct {
	c      client.Client
	in     io.Reader
	out    io.Writer
	yes    bool
	ns     string
	kubecf string
	ctx    string
}

func main() {
	a := &app{in: os.Stdin, out: os.Stdout}
	fs := flag.NewFlagSet("kubectl-daos", flag.ContinueOnError)
	fs.BoolVar(&a.yes, "yes", false, "do not ask for confirmation")
	fs.StringVar(&a.ns, "n", "default", "namespace (DaosContainer only)")
	fs.StringVar(&a.ns, "namespace", "default", "namespace (DaosContainer only)")
	fs.StringVar(&a.kubecf, "kubeconfig", os.Getenv("KUBECONFIG"), "kubeconfig path")
	fs.StringVar(&a.ctx, "context", "", "kubeconfig context")
	// flags may appear anywhere: collect positionals, parse the rest
	var args, flagArgs []string
	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, "-") {
			flagArgs = append(flagArgs, arg)
		} else if len(flagArgs) > 0 && !strings.Contains(flagArgs[len(flagArgs)-1], "=") && needsValue(flagArgs[len(flagArgs)-1]) {
			flagArgs = append(flagArgs, arg)
		} else {
			args = append(args, arg)
		}
	}
	if err := fs.Parse(flagArgs); err != nil {
		os.Exit(2)
	}
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	c, err := a.newClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubectl daos:", err)
		os.Exit(1)
	}
	a.c = c
	if err := a.run(context.Background(), args); err != nil {
		fmt.Fprintln(os.Stderr, "kubectl daos:", err)
		os.Exit(1)
	}
}

func needsValue(f string) bool {
	switch strings.TrimLeft(f, "-") {
	case "n", "namespace", "kubeconfig", "context", "image", "version":
		return true
	}
	return false
}

const usage = `usage: kubectl daos <command> <subcommand> <name> [--yes] [-n ns]

  system status  <sys>                         conditions, ranks, pending decisions
  system format  <sys>                         approve the one-shot storage format
  system upgrade <sys> [--image I] [--version V]  set the new server image and approve the full-stop upgrade
  pool   destroy <pool>                        approve destruction and delete the DaosPool
  cont   destroy -n <ns> <cont>                approve destruction and delete the DaosContainer`

func (a *app) newClient() (client.Client, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if a.kubecf != "" {
		rules.ExplicitPath = a.kubecf
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: a.ctx}).ClientConfig()
	if err != nil {
		return nil, err
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := daosv1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return client.New(cfg, client.Options{Scheme: scheme})
}

func (a *app) run(ctx context.Context, args []string) error {
	cmd := args[0] + " " + args[1]
	rest := args[2:]
	switch cmd {
	case "system status":
		return a.systemStatus(ctx, need(rest, "system name"))
	case "system format":
		return a.systemFormat(ctx, need(rest, "system name"))
	case "system upgrade":
		return a.systemUpgrade(ctx, rest)
	case "pool destroy":
		return a.poolDestroy(ctx, need(rest, "pool name"))
	case "cont destroy", "container destroy":
		return a.contDestroy(ctx, need(rest, "container name"))
	}
	return fmt.Errorf("unknown command %q\n%s", cmd, usage)
}

func need(rest []string, what string) string {
	for _, r := range rest {
		if !strings.HasPrefix(r, "-") {
			return r
		}
	}
	fmt.Fprintf(os.Stderr, "missing %s\n%s\n", what, usage)
	os.Exit(2)
	return ""
}

// confirm prints what is about to happen and asks unless --yes.
func (a *app) confirm(what string) bool {
	fmt.Fprintln(a.out, what)
	if a.yes {
		return true
	}
	fmt.Fprint(a.out, "Type 'yes' to continue: ")
	line, _ := bufio.NewReader(a.in).ReadString('\n')
	return strings.TrimSpace(line) == "yes"
}

func condLine(conds []metav1.Condition, t string) string {
	for _, c := range conds {
		if c.Type == t {
			return fmt.Sprintf("%-14s %-7s %-22s %s", c.Type, c.Status, c.Reason, c.Message)
		}
	}
	return fmt.Sprintf("%-14s -", t)
}

func (a *app) systemStatus(ctx context.Context, name string) error {
	sys := &daosv1alpha1.DaosSystem{}
	if err := a.c.Get(ctx, types.NamespacedName{Name: name}, sys); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "DaosSystem %s  version %s  image %s\n", sys.Name, sys.Spec.Version, sys.Spec.Images.Server)
	for _, t := range []string{daosv1alpha1.ConditionNodesSelected, daosv1alpha1.ConditionDriveConflict, daosv1alpha1.ConditionConfigRendered,
		daosv1alpha1.ConditionServersReady, daosv1alpha1.ConditionCertificates, daosv1alpha1.ConditionFormatted, daosv1alpha1.ConditionTelemetry,
		daosv1alpha1.ConditionUpgrading, daosv1alpha1.ConditionReady} {
		fmt.Fprintln(a.out, "  "+condLine(sys.Status.Conditions, t))
	}
	fmt.Fprintf(a.out, "nodes: %d selected, ms replicas %v\n", len(sys.Status.SelectedNodes), sys.Status.MsReplicaNodes)
	for _, nc := range sys.Status.NodeConfigs {
		state := "not rendered"
		if nc.Ready {
			state = "rendered"
			if nc.ServerReady {
				state += ", server ready"
			}
		}
		fmt.Fprintf(a.out, "  %-24s %-8s fabric=%-10s bdevs=%d  %s %s\n", nc.Node, nc.ControlAddr, nc.FabricIface, nc.BdevCount, state, nc.Message)
	}
	if sys.Status.RanksTotal > 0 {
		fmt.Fprintf(a.out, "ranks: %d/%d joined\n", sys.Status.RanksJoined, sys.Status.RanksTotal)
		for _, r := range sys.Status.Ranks {
			fmt.Fprintf(a.out, "  rank %-3d %-24s %s\n", r.Rank, r.Node, r.State)
		}
	}
	if sys.Status.PendingFormat && !sys.Status.Formatted {
		fmt.Fprintf(a.out, "\nDECISION PENDING: storage is not formatted. Review the drives above, then: kubectl daos system format %s\n", name)
	}
	if up := sys.Status.Upgrade; up != nil && up.Phase != "" {
		fmt.Fprintf(a.out, "upgrade: %s %s -> %s  %s\n", up.Phase, up.FromImage, up.ToImage, up.Message)
		if up.Phase == "Pending" {
			fmt.Fprintf(a.out, "DECISION PENDING: drain clients, then: kubectl daos system upgrade %s\n", name)
		}
	}
	return nil
}

func (a *app) systemFormat(ctx context.Context, name string) error {
	sys := &daosv1alpha1.DaosSystem{}
	if err := a.c.Get(ctx, types.NamespacedName{Name: name}, sys); err != nil {
		return err
	}
	if sys.Status.Formatted && !sys.Status.PendingFormat {
		return fmt.Errorf("%s is already formatted; nothing to approve", name)
	}
	if !sys.Status.PendingFormat {
		return fmt.Errorf("%s does not report pendingFormat yet (Formatted: %s); wait for the operator's probe", name, condLine(sys.Status.Conditions, daosv1alpha1.ConditionFormatted))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "About to approve `dmg storage format` for DaosSystem %s. This ERASES every listed NVMe device:\n", name)
	for _, nc := range sys.Status.NodeConfigs {
		if nc.Ready {
			fmt.Fprintf(&b, "  %-24s fabric=%-10s %d device(s)\n", nc.Node, nc.FabricIface, nc.BdevCount)
		}
	}
	b.WriteString("The operator runs it once, only when every server pod is Ready, and drops the approval afterwards.")
	if !a.confirm(b.String()) {
		return fmt.Errorf("aborted")
	}
	patch := client.MergeFrom(sys.DeepCopy())
	if sys.Annotations == nil {
		sys.Annotations = map[string]string{}
	}
	sys.Annotations[daosv1alpha1.AnnotationFormatApproved] = "true"
	if err := a.c.Patch(ctx, sys, patch); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "approved: %s=true on DaosSystem %s (watch: kubectl get daossys %s -w)\n", daosv1alpha1.AnnotationFormatApproved, name, name)
	return nil
}

func (a *app) systemUpgrade(ctx context.Context, rest []string) error {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	image := fs.String("image", "", "new spec.images.server")
	version := fs.String("version", "", "new spec.version")
	var pos []string
	for i := 0; i < len(rest); i++ {
		if strings.HasPrefix(rest[i], "-") {
			if err := fs.Parse(rest[i:]); err != nil {
				return err
			}
			pos = append(pos, fs.Args()...)
			break
		}
		pos = append(pos, rest[i])
	}
	if len(pos) == 0 {
		return fmt.Errorf("missing system name")
	}
	name := pos[0]
	sys := &daosv1alpha1.DaosSystem{}
	if err := a.c.Get(ctx, types.NamespacedName{Name: name}, sys); err != nil {
		return err
	}
	patch := client.MergeFrom(sys.DeepCopy())
	if *image != "" {
		sys.Spec.Images.Server = *image
	}
	if *version != "" {
		sys.Spec.Version = *version
	}
	from := "(current pods)"
	if sys.Status.Upgrade != nil && sys.Status.Upgrade.FromImage != "" {
		from = sys.Status.Upgrade.FromImage
	}
	msg := fmt.Sprintf("About to approve a FULL-STOP upgrade of DaosSystem %s: %s -> %s\n"+
		"All engines stop (dmg system stop), every server pod is replaced, then dmg system start. There is no rolling upgrade before DAOS 3.0.\n"+
		"By approving you confirm that clients are drained; the operator cannot see client handles.", name, from, sys.Spec.Images.Server)
	if !a.confirm(msg) {
		return fmt.Errorf("aborted")
	}
	sys.Spec.Upgrade.Approved = true
	if err := a.c.Patch(ctx, sys, patch); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "approved: spec.upgrade.approved=true (image %s, version %s); follow with kubectl get daossys %s -w\n", sys.Spec.Images.Server, sys.Spec.Version, name)
	return nil
}

func (a *app) poolDestroy(ctx context.Context, name string) error {
	pool := &daosv1alpha1.DaosPool{}
	if err := a.c.Get(ctx, types.NamespacedName{Name: name}, pool); err != nil {
		return err
	}
	msg := fmt.Sprintf("About to DESTROY DAOS pool %s (uuid %s, %s used of %s) with all its containers, and delete the DaosPool.\n"+
		"Without this approval `kubectl delete daospool` only forgets the pool.", name, pool.Status.UUID,
		humanBytes(pool.Status.TotalBytes-pool.Status.FreeBytes), humanBytes(pool.Status.TotalBytes))
	if !a.confirm(msg) {
		return fmt.Errorf("aborted")
	}
	patch := client.MergeFrom(pool.DeepCopy())
	if pool.Annotations == nil {
		pool.Annotations = map[string]string{}
	}
	pool.Annotations[daosv1alpha1.AnnotationDestroyApproved] = "true"
	if err := a.c.Patch(ctx, pool, patch); err != nil {
		return err
	}
	if err := a.c.Delete(ctx, pool); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "DaosPool %s marked for destruction and deleted; the operator runs dmg pool destroy --recursive and then removes the finalizer\n", name)
	return nil
}

func (a *app) contDestroy(ctx context.Context, name string) error {
	c := &daosv1alpha1.DaosContainer{}
	if err := a.c.Get(ctx, types.NamespacedName{Namespace: a.ns, Name: name}, c); err != nil {
		return err
	}
	msg := fmt.Sprintf("About to DESTROY DAOS container %s (uuid %s) in pool %s, and delete DaosContainer %s/%s.\n"+
		"Without this approval `kubectl delete daoscontainer` only forgets the container.", c.Spec.Label, c.Status.UUID, c.Spec.PoolRef, a.ns, name)
	if !a.confirm(msg) {
		return fmt.Errorf("aborted")
	}
	patch := client.MergeFrom(c.DeepCopy())
	if c.Annotations == nil {
		c.Annotations = map[string]string{}
	}
	c.Annotations[daosv1alpha1.AnnotationDestroyApproved] = "true"
	if err := a.c.Patch(ctx, c, patch); err != nil {
		return err
	}
	if err := a.c.Delete(ctx, c); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "DaosContainer %s/%s marked for destruction and deleted\n", a.ns, name)
	return nil
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

var _ = time.Second
