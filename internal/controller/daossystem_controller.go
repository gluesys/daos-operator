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

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	daosv1alpha1 "gitlab.gluesys.com/exastor/daos-operator/api/v1alpha1"
	"gitlab.gluesys.com/exastor/daos-operator/internal/discovery"
	"gitlab.gluesys.com/exastor/daos-operator/internal/render"
)

// DaosSystemReconciler reconciles a DaosSystem object.
//
// Phase 2 step 1 (#7): select nodes, read per-node facts from Node annotations,
// refuse nodes that share a physical NVMe (dual-port chassis), pick the
// management-service replicas, and render one daos_server.yml ConfigMap per
// node plus the agent and control configs. Pods (#9), host preparation (#8) and
// the format approval gate (#10) build on what this step leaves in status.
//
// Rules carried from the ADRs: status mirrors what we observe, it is never a
// second source of truth; nothing here can destroy data.
type DaosSystemReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const (
	controlPort  = int32(10001)
	requeueSlow  = 30 * time.Second
	keyServerYML = "daos_server.yml"
)

// +kubebuilder:rbac:groups=daos.gluesys.com,resources=daossystems,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=daos.gluesys.com,resources=daossystems/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=daos.gluesys.com,resources=daossystems/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile renders the per-node configuration for a DaosSystem.
func (r *DaosSystemReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	sys := &daosv1alpha1.DaosSystem{}
	if err := r.Get(ctx, req.NamespacedName, sys); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !sys.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // owned ConfigMaps are garbage-collected
	}
	ns := sys.Spec.Namespace
	if ns == "" {
		ns = "daos-system"
	}

	// 1. select nodes
	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes, client.MatchingLabels(sys.Spec.NodeSelector)); err != nil {
		return ctrl.Result{}, err
	}
	sort.Slice(nodes.Items, func(i, j int) bool { return nodes.Items[i].Name < nodes.Items[j].Name })
	msWant := int(sys.Spec.MsReplicas)
	if msWant == 0 {
		msWant = 1
	}
	status := daosv1alpha1.DaosSystemStatus{Conditions: sys.Status.Conditions}
	for _, n := range nodes.Items {
		status.SelectedNodes = append(status.SelectedNodes, n.Name)
	}
	if len(nodes.Items) < msWant {
		setCond(&status, daosv1alpha1.ConditionNodesSelected, metav1.ConditionFalse, "InsufficientNodes",
			fmt.Sprintf("%d node(s) match nodeSelector %v, need at least msReplicas=%d", len(nodes.Items), sys.Spec.NodeSelector, msWant))
		setCond(&status, daosv1alpha1.ConditionConfigRendered, metav1.ConditionFalse, "Waiting", "no configuration rendered")
		setCond(&status, daosv1alpha1.ConditionReady, metav1.ConditionFalse, "NotReady", "waiting for nodes")
		return r.updateStatus(ctx, sys, status, requeueSlow)
	}
	setCond(&status, daosv1alpha1.ConditionNodesSelected, metav1.ConditionTrue, "Selected", fmt.Sprintf("%d node(s)", len(nodes.Items)))

	// 2. facts and drive-conflict check
	facts := make([]discovery.NodeFacts, 0, len(nodes.Items))
	for i := range nodes.Items {
		facts = append(facts, discovery.FromNode(&nodes.Items[i]))
	}
	conflicts := discovery.DriveConflicts(facts)
	conflicted := map[string]string{}
	if len(conflicts) > 0 {
		var msgs []string
		for dsn, ns := range conflicts {
			msgs = append(msgs, fmt.Sprintf("DSN %s on %s", dsn, strings.Join(ns, "+")))
			for _, n := range ns {
				conflicted[n] = dsn
			}
		}
		sort.Strings(msgs)
		setCond(&status, daosv1alpha1.ConditionDriveConflict, metav1.ConditionTrue, "SharedPhysicalDrive",
			"nodes expose the same NVMe (dual-port chassis); they are excluded until fixed: "+strings.Join(msgs, "; "))
	} else {
		setCond(&status, daosv1alpha1.ConditionDriveConflict, metav1.ConditionFalse, "None", "no shared drives detected")
	}

	// 3. management-service replicas: first msWant healthy nodes by name (stable across reconciles)
	var msAddrs []string
	for _, f := range facts {
		if _, bad := conflicted[f.Name]; bad {
			continue
		}
		if len(msAddrs) < msWant {
			status.MsReplicaNodes = append(status.MsReplicaNodes, f.Name)
			msAddrs = append(msAddrs, f.ControlAddr)
		}
	}
	if len(msAddrs) < msWant {
		setCond(&status, daosv1alpha1.ConditionConfigRendered, metav1.ConditionFalse, "InsufficientHealthyNodes",
			fmt.Sprintf("only %d conflict-free node(s) for msReplicas=%d", len(msAddrs), msWant))
		setCond(&status, daosv1alpha1.ConditionReady, metav1.ConditionFalse, "NotReady", "waiting for conflict-free nodes")
		return r.updateStatus(ctx, sys, status, requeueSlow)
	}

	// 4. namespace
	if err := r.ensureNamespace(ctx, ns); err != nil {
		return ctrl.Result{}, err
	}

	// 5. per-node server config
	rendered, allReady := 0, true
	var hostlist []string
	for _, f := range facts {
		ncs := daosv1alpha1.NodeConfigStatus{Node: f.Name, ControlAddr: f.ControlAddr}
		if dsn, bad := conflicted[f.Name]; bad {
			ncs.Message = "excluded: shares physical drive " + dsn + " with another node"
			allReady = false
			status.NodeConfigs = append(status.NodeConfigs, ncs)
			continue
		}
		cfg := render.ServerConfig{Label: sys.Name, SystemName: systemName(sys), MsReplicas: msAddrs, Port: controlPort,
			Provider: sys.Spec.Provider, NrHugepages: sys.Spec.NrHugepages, AllowInsecure: sys.Spec.AllowInsecure}
		var missing []string
		for i, e := range sys.Spec.Engines {
			fabric, bdevs, numa, miss := discovery.Resolve(e, f)
			missing = append(missing, miss...)
			port := e.FabricPort
			if port == 0 {
				port = 31316
			}
			cfg.Engines = append(cfg.Engines, render.Engine{Index: i, Targets: e.Targets, Helpers: e.Helpers,
				FabricIface: fabric, FabricPort: port + int32(i)*100, PinnedNuma: numa, ScmSizeGiB: e.ScmSizeGiB, Bdevs: bdevs})
			ncs.FabricIface, ncs.BdevCount = fabric, int32(len(bdevs))
		}
		if len(missing) > 0 {
			ncs.Message = "waiting for node facts: " + strings.Join(uniq(missing), ", ")
			allReady = false
			status.NodeConfigs = append(status.NodeConfigs, ncs)
			continue
		}
		yml, err := render.Server(cfg)
		if err != nil {
			ncs.Message = err.Error()
			allReady = false
			status.NodeConfigs = append(status.NodeConfigs, ncs)
			continue
		}
		cmName := fmt.Sprintf("%s-server-%s", sys.Name, f.Name)
		if err := r.upsertConfigMap(ctx, sys, ns, cmName, f.Name, map[string]string{keyServerYML: yml}); err != nil {
			return ctrl.Result{}, err
		}
		ncs.ConfigMap, ncs.Ready = cmName, true
		rendered++
		hostlist = append(hostlist, f.ControlAddr)
		status.NodeConfigs = append(status.NodeConfigs, ncs)
	}

	// 6. agent + control configs
	if err := r.upsertConfigMap(ctx, sys, ns, sys.Name+"-agent", "", map[string]string{
		"daos_agent.yml": render.Agent(systemName(sys), msAddrs, controlPort, sys.Spec.AllowInsecure)}); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.upsertConfigMap(ctx, sys, ns, sys.Name+"-control", "", map[string]string{
		"daos_control.yml": render.Control(systemName(sys), hostlist, controlPort, sys.Spec.AllowInsecure)}); err != nil {
		return ctrl.Result{}, err
	}

	if allReady {
		setCond(&status, daosv1alpha1.ConditionConfigRendered, metav1.ConditionTrue, "Rendered", fmt.Sprintf("%d server config(s) + agent + control", rendered))
	} else {
		setCond(&status, daosv1alpha1.ConditionConfigRendered, metav1.ConditionFalse, "Partial",
			fmt.Sprintf("%d of %d node(s) rendered; see nodeConfigs", rendered, len(facts)))
	}
	// Engines are not managed yet (#9): Ready stays False with an explicit reason.
	setCond(&status, daosv1alpha1.ConditionReady, metav1.ConditionFalse, "PodsNotManaged", "configuration rendered; server pods are not managed by this version")
	log.Info("rendered", "system", sys.Name, "nodes", len(facts), "configmaps", rendered, "conflicts", len(conflicts))
	return r.updateStatus(ctx, sys, status, 0)
}

func systemName(sys *daosv1alpha1.DaosSystem) string {
	// DAOS 2.8 does not support changing the system name from the default yet
	// (packaged daos_server.yml: "It must not be changed from the default").
	return "daos_server"
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func setCond(st *daosv1alpha1.DaosSystemStatus, t string, v metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: t, Status: v, Reason: reason, Message: msg})
}

func (r *DaosSystemReconciler) updateStatus(ctx context.Context, sys *daosv1alpha1.DaosSystem, st daosv1alpha1.DaosSystemStatus, requeue time.Duration) (ctrl.Result, error) {
	// keep fields this step does not own
	st.Formatted, st.PendingFormat, st.ObservedVersion = sys.Status.Formatted, sys.Status.PendingFormat, sys.Status.ObservedVersion
	st.Ranks, st.RanksJoined, st.RanksTotal = sys.Status.Ranks, sys.Status.RanksJoined, sys.Status.RanksTotal
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &daosv1alpha1.DaosSystem{}
		if err := r.Get(ctx, types.NamespacedName{Name: sys.Name}, latest); err != nil {
			return err
		}
		latest.Status = st
		return r.Status().Update(ctx, latest)
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	if requeue > 0 {
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
	return ctrl.Result{}, nil
}

func (r *DaosSystemReconciler) ensureNamespace(ctx context.Context, name string) error {
	ns := &corev1.Namespace{}
	err := r.Get(ctx, types.NamespacedName{Name: name}, ns)
	if apierrors.IsNotFound(err) {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"app.kubernetes.io/managed-by": "daos-operator"}}}
		if err := r.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		return nil
	}
	return err
}

func (r *DaosSystemReconciler) upsertConfigMap(ctx context.Context, sys *daosv1alpha1.DaosSystem, ns, name, node string, data map[string]string) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if cm.Labels == nil {
			cm.Labels = map[string]string{}
		}
		cm.Labels["app.kubernetes.io/name"] = "daos"
		cm.Labels["app.kubernetes.io/managed-by"] = "daos-operator"
		cm.Labels[daosv1alpha1.LabelSystem] = sys.Name
		if node != "" {
			cm.Labels[daosv1alpha1.LabelNode] = node
		}
		cm.Data = data
		// A namespaced object may be owned by a cluster-scoped owner.
		return controllerutil.SetControllerReference(sys, cm, r.Scheme)
	})
	return err
}

// SetupWithManager sets up the controller with the Manager.
func (r *DaosSystemReconciler) SetupWithManager(mgr ctrl.Manager) error {
	nodeToSystems := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		list := &daosv1alpha1.DaosSystemList{}
		if err := mgr.GetClient().List(ctx, list); err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(list.Items))
		for _, s := range list.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: s.Name}})
		}
		return reqs
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&daosv1alpha1.DaosSystem{}).
		Owns(&corev1.ConfigMap{}).
		Watches(&corev1.Node{}, nodeToSystems,
			// only label/annotation changes matter here; heartbeat status updates must not fan out
			// to every DaosSystem
			// nolint:staticcheck
			builderWithPredicates(predicate.Or(predicate.LabelChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		Named("daossystem").
		Complete(r)
}

func builderWithPredicates(p predicate.Predicate) builder.WatchesOption {
	return builder.WithPredicates(p)
}
