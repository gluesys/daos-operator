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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	daosv1alpha1 "gitlab.gluesys.com/exastor/daos-operator/api/v1alpha1"
	"gitlab.gluesys.com/exastor/daos-operator/internal/dmg"
)

// S3ServiceReconciler runs versitygw-daos in front of a DaosPool (ADR-004).
// The gateway speaks libdfs, so this is an ordinary Deployment plus the
// daos_agent sidecar every DAOS client needs -- no PV, no dfuse.
type S3ServiceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

const (
	s3Container   = "versitygw"
	s3IAMDir      = "/var/lib/versitygw/iam"
	s3TLSDir      = "/etc/versitygw/tls"
	s3RequeueWait = 30 * time.Second
)

// +kubebuilder:rbac:groups=daos.gluesys.com,resources=s3services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=daos.gluesys.com,resources=s3services/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile keeps one Deployment and one Service in step with the CR.
func (r *S3ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	svc := &daosv1alpha1.S3Service{}
	if err := r.Get(ctx, req.NamespacedName, svc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	status := *svc.Status.DeepCopy()
	status.ObservedGeneration = svc.Generation
	setC := func(t string, s metav1.ConditionStatus, reason, msg string) {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: t, Status: s, Reason: reason,
			Message: msg, ObservedGeneration: svc.Generation})
	}
	ready := func(s metav1.ConditionStatus, reason, msg string) {
		setC(daosv1alpha1.ConditionReady, s, reason, msg)
	}

	pool := &daosv1alpha1.DaosPool{}
	if err := r.Get(ctx, types.NamespacedName{Name: svc.Spec.PoolRef}, pool); err != nil {
		if apierrors.IsNotFound(err) {
			setC("PoolReady", metav1.ConditionFalse, "PoolNotFound", "DaosPool "+svc.Spec.PoolRef+" does not exist")
			ready(metav1.ConditionFalse, "PoolNotFound", "no pool to serve")
			return r.updateStatus(ctx, svc, status, s3RequeueWait)
		}
		return ctrl.Result{}, err
	}
	sys, _, err := systemFor(ctx, r.Client, pool.Spec.SystemRef)
	if err != nil {
		if apierrors.IsNotFound(err) {
			setC("PoolReady", metav1.ConditionFalse, "SystemNotFound", "DaosSystem "+pool.Spec.SystemRef+" does not exist")
			ready(metav1.ConditionFalse, "SystemNotFound", "no system to serve")
			return r.updateStatus(ctx, svc, status, s3RequeueWait)
		}
		return ctrl.Result{}, err
	}
	status.PoolUUID, status.SystemName = pool.Status.UUID, systemName(sys)
	if c := meta.FindStatusCondition(pool.Status.Conditions, daosv1alpha1.ConditionReady); c == nil || c.Status != metav1.ConditionTrue {
		setC("PoolReady", metav1.ConditionFalse, "PoolNotReady", "DaosPool "+pool.Name+" is not Ready yet")
		ready(metav1.ConditionFalse, "PoolNotReady", "waiting for the pool")
		return r.updateStatus(ctx, svc, status, s3RequeueWait)
	}
	setC("PoolReady", metav1.ConditionTrue, "Ready", fmt.Sprintf("pool %s (uuid %s) in system %s", pool.Name, pool.Status.UUID, status.SystemName))

	// Identity lives outside DAOS: say plainly whether it survives the pod, and
	// refuse a replica count that would split it (ADR-004).
	durable, why := iamDurability(svc)
	if !durable {
		setC("IAMDurable", metav1.ConditionFalse, "Ephemeral", why)
	} else {
		setC("IAMDurable", metav1.ConditionTrue, "Durable", why)
	}
	if err := validateReplicas(svc); err != nil {
		setC("Deployed", metav1.ConditionFalse, "ReplicasUnsafe", err.Error())
		ready(metav1.ConditionFalse, "ReplicasUnsafe", err.Error())
		r.event(svc, corev1.EventTypeWarning, "ReplicasUnsafe", err.Error())
		return r.updateStatus(ctx, svc, status, 0) // a spec change brings us back
	}

	dep, err := r.applyDeployment(ctx, svc, pool, sys)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.applyService(ctx, svc); err != nil {
		return ctrl.Result{}, err
	}
	setC("Deployed", metav1.ConditionTrue, "Applied", fmt.Sprintf("Deployment %s and Service %s applied", dep.Name, svc.Name))
	status.ReadyReplicas = dep.Status.ReadyReplicas
	status.Endpoint = endpointOf(svc)

	switch {
	case svc.Spec.Replicas == 0:
		ready(metav1.ConditionFalse, "ScaledToZero", "spec.replicas is 0")
	case dep.Status.ReadyReplicas == 0:
		ready(metav1.ConditionFalse, "NoReadyReplicas", "no gateway pod is ready yet")
	default:
		ready(metav1.ConditionTrue, "Ready", fmt.Sprintf("%d/%d replicas ready at %s",
			dep.Status.ReadyReplicas, svc.Spec.Replicas, status.Endpoint))
	}
	return r.updateStatus(ctx, svc, status, s3RequeueWait)
}

// iamDurability explains what happens to S3 users and keys when the pod dies.
func iamDurability(svc *daosv1alpha1.S3Service) (bool, string) {
	if svc.Spec.IAM.Type == daosv1alpha1.S3IAMExternal {
		return true, "identity is external (configured through spec.extraEnv)"
	}
	if svc.Spec.IAM.ClaimName != "" {
		return true, "internal IAM on PersistentVolumeClaim " + svc.Spec.IAM.ClaimName
	}
	return false, "internal IAM on an emptyDir: S3 users and keys are lost when the pod restarts " +
		"(objects in DAOS are not affected). Set spec.iam.claimName to keep them."
}

// validateReplicas refuses the one combination that silently diverges: several
// pods each keeping their own file-based user list.
func validateReplicas(svc *daosv1alpha1.S3Service) error {
	if svc.Spec.Replicas <= 1 {
		return nil
	}
	if svc.Spec.IAM.Type == daosv1alpha1.S3IAMExternal || svc.Spec.IAM.ClaimName != "" {
		return nil
	}
	return fmt.Errorf("spec.replicas is %d but the internal IAM store is a per-pod emptyDir; "+
		"set spec.iam.claimName to a ReadWriteMany claim or spec.iam.type=external", svc.Spec.Replicas)
}

func endpointOf(svc *daosv1alpha1.S3Service) string {
	scheme := "http"
	if svc.Spec.TLSSecretName != "" {
		scheme = "https"
	}
	ep := fmt.Sprintf("%s://%s.%s.svc:%d", scheme, svc.Name, svc.Namespace, portOf(svc))
	if svc.Spec.ServiceType == corev1.ServiceTypeNodePort && svc.Spec.NodePort > 0 {
		ep += fmt.Sprintf(" (nodePort %d)", svc.Spec.NodePort)
	}
	return ep
}

func portOf(svc *daosv1alpha1.S3Service) int32 {
	if svc.Spec.Port > 0 {
		return svc.Spec.Port
	}
	return 7070
}

func s3Labels(svc *daosv1alpha1.S3Service, sys *daosv1alpha1.DaosSystem) map[string]string {
	l := map[string]string{
		"app.kubernetes.io/name":     "versitygw",
		"app.kubernetes.io/instance": svc.Name,
		daosv1alpha1.LabelRole:       "s3",
	}
	if sys != nil {
		l[daosv1alpha1.LabelSystem] = sys.Name
	}
	return l
}

func selectorLabels(svc *daosv1alpha1.S3Service) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "versitygw",
		"app.kubernetes.io/instance": svc.Name,
	}
}

// gatewayCommand builds the versitygw invocation. Global flags come before the
// `daos` subcommand (urfave/cli), and the agent socket is waited for because a
// native sidecar is started, not necessarily listening.
func gatewayCommand(svc *daosv1alpha1.S3Service, pool *daosv1alpha1.DaosPool, sys *daosv1alpha1.DaosSystem) []string {
	var b strings.Builder
	b.WriteString(waitForAgent)
	b.WriteString("exec versitygw")
	fmt.Fprintf(&b, " --port :%d", portOf(svc))
	if svc.Spec.Region != "" {
		b.WriteString(" --region " + dmg.ShellQuote(svc.Spec.Region))
	}
	if svc.Spec.TLSSecretName != "" {
		b.WriteString(" --cert " + s3TLSDir + "/tls.crt --key " + s3TLSDir + "/tls.key")
	}
	if svc.Spec.IAM.Type != daosv1alpha1.S3IAMExternal {
		b.WriteString(" --iam-dir " + s3IAMDir)
	}
	b.WriteString(" daos --pool " + dmg.ShellQuote(poolLabel(pool)))
	if n := systemName(sys); n != "" {
		b.WriteString(" --system " + dmg.ShellQuote(n))
	}
	if svc.Spec.ContainerCacheSize > 0 {
		fmt.Fprintf(&b, " --container-cache %d", svc.Spec.ContainerCacheSize)
	}
	return []string{"bash", "-c", b.String()}
}

func (r *S3ServiceReconciler) applyDeployment(ctx context.Context, svc *daosv1alpha1.S3Service,
	pool *daosv1alpha1.DaosPool, sys *daosv1alpha1.DaosSystem) (*appsv1.Deployment, error) {

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: svc.Name, Namespace: svc.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = s3Labels(svc, sys)
		dep.Spec.Replicas = ptr.To(svc.Spec.Replicas)
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: selectorLabels(svc)}
		dep.Spec.Template.ObjectMeta.Labels = s3Labels(svc, sys)
		p := &dep.Spec.Template.Spec
		p.NodeSelector = svc.Spec.NodeSelector
		p.Tolerations = svc.Spec.Tolerations
		// the client library resolves its own fabric address, like every other
		// DAOS client the operator runs
		p.HostNetwork = true
		p.DNSPolicy = corev1.DNSClusterFirstWithHostNet
		p.ShareProcessNamespace = ptr.To(true)
		p.Volumes = s3Volumes(svc, sys)
		p.InitContainers = []corev1.Container{agentSidecar(svc, sys)}
		p.Containers = []corev1.Container{{
			Name:            s3Container,
			Image:           svc.Spec.Image,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         gatewayCommand(svc, pool, sys),
			Env:             s3Env(svc),
			Ports:           []corev1.ContainerPort{{Name: "s3", ContainerPort: portOf(svc)}},
			VolumeMounts:    s3Mounts(svc, sys),
			Resources:       svc.Spec.Resources,
			// RDMA devices for the client library, same reason as the dmg/daos Jobs
			SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler:        corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(portOf(svc))}},
				InitialDelaySeconds: 5, PeriodSeconds: 10,
			},
		}}
		return controllerutil.SetControllerReference(svc, dep, r.Scheme)
	})
	return dep, err
}

func s3Env(svc *daosv1alpha1.S3Service) []corev1.EnvVar {
	sec := svc.Spec.RootCredentialsSecret
	env := []corev1.EnvVar{
		{Name: "DAOS_AGENT_DRPC_DIR", Value: agentSocketDir},
		{Name: "ROOT_ACCESS_KEY_ID", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: sec}, Key: "accessKey"}}},
		{Name: "ROOT_SECRET_ACCESS_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: sec}, Key: "secretKey"}}},
	}
	return append(env, svc.Spec.ExtraEnv...)
}

func agentSidecar(svc *daosv1alpha1.S3Service, sys *daosv1alpha1.DaosSystem) corev1.Container {
	c := corev1.Container{
		Name:            "agent",
		Image:           sys.Spec.Images.Agent,
		ImagePullPolicy: corev1.PullIfNotPresent,
		RestartPolicy:   ptr.To(corev1.ContainerRestartPolicyAlways), // native sidecar
		SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "agentcfg", MountPath: "/etc/daos/daos_agent.yml", SubPath: "daos_agent.yml", ReadOnly: true},
			{Name: "agentsock", MountPath: agentSocketDir},
			{Name: "dev", MountPath: "/dev"},
		},
	}
	if adminCertsSecret(sys) != "" {
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "certs", MountPath: "/etc/daos/certs", ReadOnly: true})
	}
	return c
}

func s3Volumes(svc *daosv1alpha1.S3Service, sys *daosv1alpha1.DaosSystem) []corev1.Volume {
	hostDir := corev1.HostPathDirectory
	vols := []corev1.Volume{
		{Name: "agentcfg", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: sys.Name + "-agent"}}}},
		{Name: "agentsock", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "dev", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev", Type: &hostDir}}},
	}
	if svc.Spec.IAM.Type != daosv1alpha1.S3IAMExternal {
		iam := corev1.Volume{Name: "iam", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
		if svc.Spec.IAM.ClaimName != "" {
			iam.VolumeSource = corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: svc.Spec.IAM.ClaimName}}
		}
		vols = append(vols, iam)
	}
	if s := adminCertsSecret(sys); s != "" {
		vols = append(vols, corev1.Volume{Name: "certs", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: s, DefaultMode: ptr.To(int32(0o400))}}})
	}
	if svc.Spec.TLSSecretName != "" {
		vols = append(vols, corev1.Volume{Name: "tls", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: svc.Spec.TLSSecretName}}})
	}
	return vols
}

func s3Mounts(svc *daosv1alpha1.S3Service, sys *daosv1alpha1.DaosSystem) []corev1.VolumeMount {
	m := []corev1.VolumeMount{
		{Name: "agentsock", MountPath: agentSocketDir},
		{Name: "dev", MountPath: "/dev"},
	}
	if svc.Spec.IAM.Type != daosv1alpha1.S3IAMExternal {
		m = append(m, corev1.VolumeMount{Name: "iam", MountPath: s3IAMDir})
	}
	if adminCertsSecret(sys) != "" {
		m = append(m, corev1.VolumeMount{Name: "certs", MountPath: "/etc/daos/certs", ReadOnly: true})
	}
	if svc.Spec.TLSSecretName != "" {
		m = append(m, corev1.VolumeMount{Name: "tls", MountPath: s3TLSDir, ReadOnly: true})
	}
	return m
}

func (r *S3ServiceReconciler) applyService(ctx context.Context, s3 *daosv1alpha1.S3Service) error {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: s3.Name, Namespace: s3.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = s3Labels(s3, nil)
		svc.Spec.Selector = selectorLabels(s3)
		svc.Spec.Type = s3.Spec.ServiceType
		port := corev1.ServicePort{Name: "s3", Port: portOf(s3), TargetPort: intstr.FromInt32(portOf(s3))}
		if s3.Spec.ServiceType == corev1.ServiceTypeNodePort && s3.Spec.NodePort > 0 {
			port.NodePort = s3.Spec.NodePort
		}
		svc.Spec.Ports = []corev1.ServicePort{port}
		return controllerutil.SetControllerReference(s3, svc, r.Scheme)
	})
	return err
}

func (r *S3ServiceReconciler) updateStatus(ctx context.Context, svc *daosv1alpha1.S3Service,
	status daosv1alpha1.S3ServiceStatus, requeue time.Duration) (ctrl.Result, error) {

	svc.Status = status
	if err := r.Status().Update(ctx, svc); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

func (r *S3ServiceReconciler) event(obj runtime.Object, typ, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(obj, typ, reason, msg)
	}
}

func (r *S3ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&daosv1alpha1.S3Service{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Named("s3service").
		Complete(r)
}
