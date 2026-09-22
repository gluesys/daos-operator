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
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	daosv1alpha1 "gitlab.gluesys.com/exastor/daos-operator/api/v1alpha1"
	"gitlab.gluesys.com/exastor/daos-operator/internal/render"
)

// reconcileExternal handles spec.externalMsReplicas: a DAOS system somebody
// else runs (bare metal, another cluster) that this Kubernetes cluster only
// consumes. The operator renders the agent and control configuration from the
// given management-service addresses, mirrors membership, and leaves pools,
// containers and rank operations working exactly as they do for a system it
// runs itself. It creates no server workloads, prepares no hosts, and never
// tries to upgrade or format someone else's system.
func (r *DaosSystemReconciler) reconcileExternal(ctx context.Context, sys *daosv1alpha1.DaosSystem, ns string, status daosv1alpha1.DaosSystemStatus) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	addrs := sys.Spec.ExternalMsReplicas

	// things that only make sense for a system we run
	if serverEnabled(sys) {
		setCond(&status, daosv1alpha1.ConditionServersReady, metav1.ConditionFalse, "External",
			"spec.externalMsReplicas is set: this system is run elsewhere, so no server pods are created (spec.server is ignored)")
	} else {
		setCond(&status, daosv1alpha1.ConditionServersReady, metav1.ConditionFalse, "External", "external system: no server pods")
	}
	setCond(&status, daosv1alpha1.ConditionDriveConflict, metav1.ConditionFalse, "NotApplicable", "external system: drives are not managed here")
	setCond(&status, daosv1alpha1.ConditionTelemetry, metav1.ConditionFalse, "External", "external system: scrape its servers directly, there are no pods here")
	setCond(&status, daosv1alpha1.ConditionUpgrading, metav1.ConditionFalse, "External", "external system: upgrades are done where the engines run")
	setCond(&status, daosv1alpha1.ConditionCertificates, metav1.ConditionFalse, "External",
		"external system: mount its certificates yourself; the operator does not generate them for a system it does not run")
	status.MsReplicaNodes = nil

	// client and admin configuration, from the addresses we were given
	if err := r.upsertConfigMap(ctx, sys, ns, sys.Name+"-agent", "", map[string]string{
		"daos_agent.yml": render.Agent(systemName(sys), addrs, controlPortOf(sys), sys.Spec.AllowInsecure, clientIfaces(sys))}); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.upsertConfigMap(ctx, sys, ns, sys.Name+"-control", "", map[string]string{
		"daos_control.yml": render.Control(systemName(sys), addrs, controlPortOf(sys), sys.Spec.AllowInsecure)}); err != nil {
		return ctrl.Result{}, err
	}
	setCond(&status, daosv1alpha1.ConditionConfigRendered, metav1.ConditionTrue, "ExternalClientConfig",
		fmt.Sprintf("agent and control config for management service %s", strings.Join(addrs, ",")))

	// membership, pools and rank operations work the same way: they are all dmg
	requeue, err := r.reconcileFormatExternal(ctx, sys, ns, &status)
	if err != nil {
		return ctrl.Result{}, err
	}
	rankRequeue, err := r.reconcileRankOp(ctx, sys, ns, &status)
	if err != nil {
		return ctrl.Result{}, err
	}
	requeue = minRequeue(requeue, rankRequeue)

	switch {
	case !status.Formatted:
		setCond(&status, daosv1alpha1.ConditionReady, metav1.ConditionFalse, "SystemUnavailable",
			"cannot read the external system yet; see condition Formatted")
	case status.RanksTotal == 0 || status.RanksJoined < status.RanksTotal:
		setCond(&status, daosv1alpha1.ConditionReady, metav1.ConditionFalse, "RanksNotJoined", fmt.Sprintf("%d/%d ranks joined", status.RanksJoined, status.RanksTotal))
	default:
		setCond(&status, daosv1alpha1.ConditionReady, metav1.ConditionTrue, "Ready", fmt.Sprintf("external system: %d ranks joined", status.RanksJoined))
	}
	log.Info("external system", "system", sys.Name, "ms", addrs, "ranks", status.RanksJoined)
	return r.updateStatus(ctx, sys, status, requeue)
}

// reconcileFormatExternal mirrors membership without the format gate: formatting
// a system the operator does not run is not its decision to make.
func (r *DaosSystemReconciler) reconcileFormatExternal(ctx context.Context, sys *daosv1alpha1.DaosSystem, ns string, status *daosv1alpha1.DaosSystemStatus) (time.Duration, error) {
	in := formatInput{ns: ns, nodeByAddr: map[string]string{}, rendered: 1, serversAllReady: true}
	for _, n := range status.SelectedNodes {
		in.nodeByAddr[n] = n
	}
	return r.reconcileFormat(ctx, sys, in, status)
}
