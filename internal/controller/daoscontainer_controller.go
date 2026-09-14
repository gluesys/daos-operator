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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	daosv1alpha1 "gitlab.gluesys.com/exastor/daos-operator/api/v1alpha1"
	"gitlab.gluesys.com/exastor/daos-operator/internal/dmg"
)

// DaosContainerReconciler drives `daos cont ...` through Jobs (#11).
//
// The `daos` client needs daos_agent, so each Job runs the agent image as a
// native sidecar (initContainer with restartPolicy Always, Kubernetes >= 1.29)
// sharing /var/run/daos_agent with the client container. The client talks to
// the engines over the fabric, hence hostNetwork, /dev and privileged (Phase 0;
// an RDMA device plugin replaces that later). Jobs run in the DaosSystem's
// namespace and are owned by the DaosPool (cluster-scoped, so cross-namespace
// ownership is legal); a label maps them back to the DaosContainer.
//
// Same rules as pools: status mirrors `daos cont query`, a missing container
// is created once, the ACL follows spec.acl, deletion destroys the DAOS
// container only with daos.gluesys.com/destroy-approved=true.
type DaosContainerReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	Dmg              dmg.Runner
	Recorder         record.EventRecorder
	DisableProbeHold bool
}

const (
	agentSocketDir = "/var/run/daos_agent"
	// waitForAgent blocks the client command until the sidecar's socket exists.
	waitForAgent = "for i in $(seq 1 60); do [ -S " + agentSocketDir + "/daos_agent.sock ] && break; sleep 1; done; "
)

// +kubebuilder:rbac:groups=daos.gluesys.com,resources=daoscontainers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=daos.gluesys.com,resources=daoscontainers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=daos.gluesys.com,resources=daoscontainers/finalizers,verbs=update

func contLabel(c *daosv1alpha1.DaosContainer) string {
	if c.Spec.Label != "" {
		return c.Spec.Label
	}
	return c.Name
}

func contKey(c *daosv1alpha1.DaosContainer) string { return "cont/" + c.Namespace + "/" + c.Name }

// contJobLabel identifies the owning DaosContainer on a Job (label values cannot hold "/").
func contJobLabel(c *daosv1alpha1.DaosContainer) string { return c.Namespace + "." + c.Name }

func (r *DaosContainerReconciler) event(obj runtime.Object, typ, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(obj, typ, reason, msg)
	}
}

// clientCommand wraps a `daos` invocation: wait for the agent, optionally write
// the ACL file, then exec.
func clientCommand(sysName string, acl []string, args ...string) []string {
	var b strings.Builder
	b.WriteString(waitForAgent)
	if len(acl) > 0 {
		b.WriteString("printf '%s\\n'")
		for _, e := range acl {
			b.WriteString(" " + dmg.ShellQuote(e))
		}
		b.WriteString(" > " + dmg.ShellQuote(aclFilePath) + " && ")
	}
	b.WriteString("exec daos -j")
	if sysName != "" {
		b.WriteString(" -G " + dmg.ShellQuote(sysName))
	}
	for _, a := range args {
		b.WriteString(" " + dmg.ShellQuote(a))
	}
	return []string{"bash", "-c", b.String()}
}

func (r *DaosContainerReconciler) opSpec(c *daosv1alpha1.DaosContainer, pool *daosv1alpha1.DaosPool, sys *daosv1alpha1.DaosSystem, ns, op string) dmg.RunSpec {
	pl, cl := poolLabel(pool), contLabel(c)
	hostDir := corev1.HostPathDirectory
	spec := dmg.RunSpec{
		Owner: pool, Namespace: ns, Name: "cont-" + c.Namespace + "-" + c.Name + "-daos-" + op,
		Image: sys.Spec.Images.Client, ControlConfigMap: sys.Name + "-control",
		NodeSelector: sys.Spec.NodeSelector, Tolerations: sys.Spec.Tolerations,
		Labels:                map[string]string{daosv1alpha1.LabelSystem: sys.Name, daosv1alpha1.LabelPool: pool.Name, daosv1alpha1.LabelContainer: contJobLabel(c)},
		Env:                   []corev1.EnvVar{{Name: "DAOS_AGENT_DRPC_DIR", Value: agentSocketDir}},
		CertsSecret:           adminCertsSecret(sys),
		CertsFiles:            adminCertFiles(), // daos CLI is an admin-style client of the MS via the agent; agent gets its own set
		SidecarCertsFiles:     agentCertFiles(),
		Privileged:            true,
		ShareProcessNamespace: true,
		Sidecar: &corev1.Container{
			Name: "agent", Image: sys.Spec.Images.Agent, ImagePullPolicy: corev1.PullIfNotPresent,
			VolumeMounts: []corev1.VolumeMount{
				{Name: "agentcfg", MountPath: "/etc/daos/daos_agent.yml", SubPath: "daos_agent.yml", ReadOnly: true},
				{Name: "agentsock", MountPath: agentSocketDir},
			},
		},
		Volumes: []corev1.Volume{
			{Name: "agentcfg", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: sys.Name + "-agent"}}}},
			{Name: "agentsock", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "dev", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev", Type: &hostDir}}},
		},
		Mounts: []corev1.VolumeMount{{Name: "agentsock", MountPath: agentSocketDir}, {Name: "dev", MountPath: "/dev"}},
	}
	switch op {
	case "query":
		spec.Command = clientCommand(systemName(sys), nil, "cont", "query", pl, cl)
	case opCreate:
		props := []string{}
		if c.Spec.RedundancyFactor != nil {
			props = append(props, fmt.Sprintf("rd_fac:%d", *c.Spec.RedundancyFactor))
		}
		if c.Spec.Checksum != "" {
			props = append(props, "cksum:"+c.Spec.Checksum)
		}
		keys := make([]string, 0, len(c.Spec.Properties))
		for k := range c.Spec.Properties {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			props = append(props, k+":"+c.Spec.Properties[k])
		}
		args := []string{"cont", "create", pl, cl}
		if c.Spec.Type != "" {
			args = append(args, "--type", string(c.Spec.Type))
		}
		if c.Spec.FileOclass != "" {
			args = append(args, "--file-oclass", c.Spec.FileOclass)
		}
		if c.Spec.DirOclass != "" {
			args = append(args, "--dir-oclass", c.Spec.DirOclass)
		}
		if c.Spec.ChunkSize > 0 {
			args = append(args, "--chunk-size", fmt.Sprint(c.Spec.ChunkSize))
		}
		if len(props) > 0 {
			args = append(args, "--properties", strings.Join(props, ","))
		}
		if len(c.Spec.ACL) > 0 {
			args = append(args, "--acl-file", aclFilePath)
		}
		spec.Command = clientCommand(systemName(sys), c.Spec.ACL, args...)
	case opACL:
		spec.Command = clientCommand(systemName(sys), c.Spec.ACL, "cont", "overwrite-acl", pl, cl, "--acl-file", aclFilePath)
	case opDestroy:
		spec.Command = clientCommand(systemName(sys), nil, "cont", "destroy", pl, cl)
	}
	return spec
}

// Reconcile drives one DaosContainer.
func (r *DaosContainerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	c := &daosv1alpha1.DaosContainer{}
	if err := r.Get(ctx, req.NamespacedName, c); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	status := *c.Status.DeepCopy()
	setC := func(v metav1.ConditionStatus, reason, msg string) {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: daosv1alpha1.ConditionReady, Status: v, Reason: reason, Message: msg, ObservedGeneration: c.Generation})
		status.Ready = v == metav1.ConditionTrue
	}
	if r.Dmg == nil {
		setC(metav1.ConditionUnknown, "NoRunner", "operator started without a dmg runner")
		return r.updateStatus(ctx, c, status, 0)
	}
	pool := &daosv1alpha1.DaosPool{}
	if err := r.Get(ctx, types.NamespacedName{Name: c.Spec.PoolRef}, pool); err != nil {
		if apierrors.IsNotFound(err) {
			setC(metav1.ConditionFalse, "PoolNotFound", "DaosPool "+c.Spec.PoolRef+" does not exist")
			return r.updateStatus(ctx, c, status, poolRequeueWait)
		}
		return ctrl.Result{}, err
	}
	sys, ns, err := systemFor(ctx, r.Client, pool.Spec.SystemRef)
	if err != nil {
		if apierrors.IsNotFound(err) {
			setC(metav1.ConditionFalse, "SystemNotFound", "DaosSystem "+pool.Spec.SystemRef+" does not exist")
			return r.updateStatus(ctx, c, status, poolRequeueWait)
		}
		return ctrl.Result{}, err
	}

	if c.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(c, daosv1alpha1.FinalizerContainer) {
		controllerutil.AddFinalizer(c, daosv1alpha1.FinalizerContainer)
		return ctrl.Result{Requeue: true}, r.Update(ctx, c)
	}
	if !c.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, c, pool, sys, ns, status)
	}
	if pool.Status.UUID == "" {
		setC(metav1.ConditionFalse, "PoolNotReady", "DaosPool "+pool.Name+" has no pool yet")
		return r.updateStatus(ctx, c, status, poolRequeueWait)
	}
	if status.Operation != "" {
		return r.pollOp(ctx, c, pool, sys, ns, status)
	}

	if !r.DisableProbeHold {
		if wait := holdFor(contKey(c)); wait > 0 {
			return ctrl.Result{RequeueAfter: wait}, nil
		}
	}
	res, err := r.Dmg.Run(ctx, r.opSpec(c, pool, sys, ns, "query"))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("cont query job: %w", err)
	}
	if !res.Done {
		if meta.FindStatusCondition(status.Conditions, daosv1alpha1.ConditionReady) == nil {
			setC(metav1.ConditionUnknown, "Probing", "daos cont query is running")
		}
		return r.updateStatus(ctx, c, status, poolRequeueOp)
	}
	if !r.DisableProbeHold {
		setHold(contKey(c), poolRequeueWait)
	}
	env, perr := parseResult(res)
	if perr != nil {
		setC(metav1.ConditionUnknown, "ProbeFailed", perr.Error())
		return r.updateStatus(ctx, c, status, poolRequeueWait)
	}
	if env.Error != nil {
		switch dmg.Classify(*env.Error) {
		case dmg.ErrNotFound:
			if status.UUID != "" {
				setC(metav1.ConditionFalse, "ContainerMissing", fmt.Sprintf("container %s (uuid %s) no longer exists in pool %s; delete and re-create the DaosContainer to make a new, empty one", contLabel(c), status.UUID, poolLabel(pool)))
				return r.updateStatus(ctx, c, status, poolRequeueIdle)
			}
			if cond := meta.FindStatusCondition(status.Conditions, daosv1alpha1.ConditionReady); cond != nil && cond.Reason == "CreateFailed" && status.ObservedGeneration == c.Generation {
				return r.updateStatus(ctx, c, status, poolRequeueFailed)
			}
			return r.startOp(ctx, c, pool, sys, ns, status, opCreate)
		case dmg.ErrUnreachable:
			setC(metav1.ConditionUnknown, "ManagementUnreachable", *env.Error)
		default:
			setC(metav1.ConditionUnknown, "QueryError", *env.Error)
		}
		return r.updateStatus(ctx, c, status, poolRequeueWait)
	}
	info, err := dmg.ContainerQuery(env)
	if err != nil {
		setC(metav1.ConditionUnknown, "ProbeFailed", err.Error())
		return r.updateStatus(ctx, c, status, poolRequeueWait)
	}
	now := metav1.Now()
	status.UUID, status.PoolUUID, status.Health, status.Type, status.LastQueryTime = info.UUID, info.PoolUUID, info.Health, info.Type, &now
	if h := aclHash(c.Spec.ACL); h != "" && h != status.AppliedACLHash {
		if cond := meta.FindStatusCondition(status.Conditions, daosv1alpha1.ConditionReady); !(cond != nil && cond.Reason == "AclFailed" && status.ObservedGeneration == c.Generation) {
			return r.startOp(ctx, c, pool, sys, ns, status, opACL)
		}
	}
	if info.Health == "HEALTHY" || info.Health == "" {
		setC(metav1.ConditionTrue, "Ready", fmt.Sprintf("%s container %s, health %s", info.Type, info.UUID, info.Health))
	} else {
		setC(metav1.ConditionFalse, "Health"+info.Health, "daos cont query reports health "+info.Health)
	}
	log.Info("container", "name", c.Name, "namespace", c.Namespace, "health", info.Health)
	return r.updateStatus(ctx, c, status, poolRequeueIdle)
}

func (r *DaosContainerReconciler) startOp(ctx context.Context, c *daosv1alpha1.DaosContainer, pool *daosv1alpha1.DaosPool, sys *daosv1alpha1.DaosSystem, ns string, status daosv1alpha1.DaosContainerStatus, op string) (ctrl.Result, error) {
	if _, err := r.Dmg.Run(ctx, r.opSpec(c, pool, sys, ns, op)); err != nil {
		return ctrl.Result{}, fmt.Errorf("start %s: %w", op, err)
	}
	status.Operation = op
	status.Ready = false
	title := strings.ToUpper(op[:1]) + op[1:]
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: daosv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: title + "InProgress", Message: "daos cont " + op + " is running", ObservedGeneration: c.Generation})
	r.event(c, corev1.EventTypeNormal, "Container"+title, "daos cont "+op+" started")
	return r.updateStatus(ctx, c, status, poolRequeueOp)
}

func (r *DaosContainerReconciler) pollOp(ctx context.Context, c *daosv1alpha1.DaosContainer, pool *daosv1alpha1.DaosPool, sys *daosv1alpha1.DaosSystem, ns string, status daosv1alpha1.DaosContainerStatus) (ctrl.Result, error) {
	op := status.Operation
	res, err := r.Dmg.Run(ctx, r.opSpec(c, pool, sys, ns, op))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("poll %s: %w", op, err)
	}
	if !res.Done {
		return ctrl.Result{RequeueAfter: poolRequeueOp}, nil
	}
	status.Operation = ""
	status.ObservedGeneration = c.Generation
	title := strings.ToUpper(op[:1]) + op[1:]
	env, perr := parseResult(res)
	failed := func(msg string) (ctrl.Result, error) {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: daosv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: title + "Failed", Message: msg, ObservedGeneration: c.Generation})
		status.Ready = false
		r.event(c, corev1.EventTypeWarning, title+"Failed", msg)
		return r.updateStatus(ctx, c, status, poolRequeueFailed)
	}
	if perr != nil {
		return failed("daos cont " + op + ": " + perr.Error())
	}
	if env.Error != nil {
		return failed("daos cont " + op + ": " + *env.Error)
	}
	switch op {
	case opCreate:
		if info, err := dmg.ContainerQuery(env); err == nil {
			status.UUID, status.PoolUUID, status.Type = info.UUID, info.PoolUUID, info.Type
		}
		status.AppliedACLHash = aclHash(c.Spec.ACL)
		r.event(c, corev1.EventTypeNormal, "ContainerCreated", "container "+contLabel(c)+" created in pool "+poolLabel(pool)+", uuid "+status.UUID)
	case opACL:
		status.AppliedACLHash = aclHash(c.Spec.ACL)
		r.event(c, corev1.EventTypeNormal, "ContainerACLApplied", "ACL overwritten from spec.acl")
	}
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: daosv1alpha1.ConditionReady, Status: metav1.ConditionUnknown, Reason: "Querying", Message: "daos cont " + op + " succeeded; refreshing", ObservedGeneration: c.Generation})
	clearHold(contKey(c))
	return r.updateStatus(ctx, c, status, time.Second)
}

func (r *DaosContainerReconciler) finalize(ctx context.Context, c *daosv1alpha1.DaosContainer, pool *daosv1alpha1.DaosPool, sys *daosv1alpha1.DaosSystem, ns string, status daosv1alpha1.DaosContainerStatus) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(c, daosv1alpha1.FinalizerContainer) {
		return ctrl.Result{}, nil
	}
	approved := c.Annotations[daosv1alpha1.AnnotationDestroyApproved] == "true"
	if !approved || status.UUID == "" || pool.Status.UUID == "" {
		if status.UUID != "" {
			r.event(c, corev1.EventTypeWarning, "ContainerOrphaned", fmt.Sprintf("DaosContainer deleted without %s=true: DAOS container %s (uuid %s) in pool %s is kept; remove it by hand with `daos cont destroy %s %s` if that is intended",
				daosv1alpha1.AnnotationDestroyApproved, contLabel(c), status.UUID, poolLabel(pool), poolLabel(pool), contLabel(c)))
		}
		controllerutil.RemoveFinalizer(c, daosv1alpha1.FinalizerContainer)
		return ctrl.Result{}, r.Update(ctx, c)
	}
	res, err := r.Dmg.Run(ctx, r.opSpec(c, pool, sys, ns, opDestroy))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("destroy job: %w", err)
	}
	if !res.Done {
		if status.Operation != opDestroy {
			status.Operation = opDestroy
			meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: daosv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "Destroying", Message: "daos cont destroy is running", ObservedGeneration: c.Generation})
			return r.updateStatus(ctx, c, status, poolRequeueOp)
		}
		return ctrl.Result{RequeueAfter: poolRequeueOp}, nil
	}
	env, perr := parseResult(res)
	if perr == nil && env.Error != nil && dmg.Classify(*env.Error) != dmg.ErrNotFound {
		perr = fmt.Errorf("%s", *env.Error)
	}
	if perr != nil {
		status.Operation = ""
		msg := "daos cont destroy: " + perr.Error() + "; the DaosContainer stays until destroy succeeds or the annotation is removed (then the container is kept)"
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: daosv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "DestroyFailed", Message: msg, ObservedGeneration: c.Generation})
		r.event(c, corev1.EventTypeWarning, "DestroyFailed", msg)
		return r.updateStatus(ctx, c, status, poolRequeueIdle)
	}
	r.event(c, corev1.EventTypeNormal, "ContainerDestroyed", "DAOS container "+contLabel(c)+" destroyed on request")
	controllerutil.RemoveFinalizer(c, daosv1alpha1.FinalizerContainer)
	return ctrl.Result{}, r.Update(ctx, c)
}

func (r *DaosContainerReconciler) updateStatus(ctx context.Context, c *daosv1alpha1.DaosContainer, st daosv1alpha1.DaosContainerStatus, requeue time.Duration) (ctrl.Result, error) {
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &daosv1alpha1.DaosContainer{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: c.Namespace, Name: c.Name}, latest); err != nil {
			return err
		}
		latest.Status = st
		return r.Status().Update(ctx, latest)
	})
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if requeue > 0 {
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager. Jobs are owned by
// the DaosPool, so they are mapped back to the container through the label.
func (r *DaosContainerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	jobToContainer := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
		v := o.GetLabels()[daosv1alpha1.LabelContainer]
		i := strings.IndexByte(v, '.')
		if i <= 0 {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: v[:i], Name: v[i+1:]}}}
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&daosv1alpha1.DaosContainer{}).
		Watches(&batchv1.Job{}, jobToContainer).
		Named("daoscontainer").
		Complete(r)
}
