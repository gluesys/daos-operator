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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	daosv1alpha1 "gitlab.gluesys.com/exastor/daos-operator/api/v1alpha1"
)

var _ = Describe("S3Service Controller", func() {
	ctx := context.Background()
	const sysName, poolName, s3Name = "s3sys", "s3pool", "gw"
	nn := types.NamespacedName{Namespace: "default", Name: s3Name}

	reconcileS3 := func() {
		r := &S3ServiceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
	}
	get := func() *daosv1alpha1.S3Service {
		s := &daosv1alpha1.S3Service{}
		Expect(k8sClient.Get(ctx, nn, s)).To(Succeed())
		return s
	}
	cond := func(t string) *metav1.Condition {
		return meta.FindStatusCondition(get().Status.Conditions, t)
	}

	BeforeEach(func() {
		sys := &daosv1alpha1.DaosSystem{ObjectMeta: metav1.ObjectMeta{Name: sysName},
			Spec: daosv1alpha1.DaosSystemSpec{Version: "2.8.0", Namespace: "daos-test", SystemName: "daos_k8s",
				AllowInsecure: true,
				Images:        daosv1alpha1.ImagesSpec{Server: "s", Agent: "agent:1", Admin: "d", Client: "c"},
				Engines:       []daosv1alpha1.EngineSpec{{Targets: 8}}}}
		Expect(k8sClient.Create(ctx, sys)).To(Succeed())
		pool := &daosv1alpha1.DaosPool{ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec: daosv1alpha1.DaosPoolSpec{SystemRef: sysName, Size: resource.MustParse("10Gi")}}
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{Type: daosv1alpha1.ConditionReady,
			Status: metav1.ConditionTrue, Reason: "Ready", Message: "ready"})
		pool.Status.UUID = "pool-uuid"
		Expect(k8sClient.Status().Update(ctx, pool)).To(Succeed())

		s3 := &daosv1alpha1.S3Service{ObjectMeta: metav1.ObjectMeta{Name: s3Name, Namespace: "default"},
			Spec: daosv1alpha1.S3ServiceSpec{PoolRef: poolName, Image: "versitygw-daos:1", Replicas: 1,
				Port: 7070, Region: "kr-1", RootCredentialsSecret: "s3root", ServiceType: corev1.ServiceTypeClusterIP}}
		Expect(k8sClient.Create(ctx, s3)).To(Succeed())
	})
	AfterEach(func() {
		// envtest runs no garbage collector, so owned objects outlive their owner
		dep := &appsv1.Deployment{}
		if k8sClient.Get(ctx, nn, dep) == nil {
			Expect(k8sClient.Delete(ctx, dep)).To(Succeed())
		}
		ksvc := &corev1.Service{}
		if k8sClient.Get(ctx, nn, ksvc) == nil {
			Expect(k8sClient.Delete(ctx, ksvc)).To(Succeed())
		}
		s3 := &daosv1alpha1.S3Service{}
		if k8sClient.Get(ctx, nn, s3) == nil {
			Expect(k8sClient.Delete(ctx, s3)).To(Succeed())
		}
		pool := &daosv1alpha1.DaosPool{}
		if k8sClient.Get(ctx, types.NamespacedName{Name: poolName}, pool) == nil {
			pool.Finalizers = nil
			Expect(k8sClient.Update(ctx, pool)).To(Succeed())
			Expect(k8sClient.Delete(ctx, pool)).To(Succeed())
		}
		sys := &daosv1alpha1.DaosSystem{}
		if k8sClient.Get(ctx, types.NamespacedName{Name: sysName}, sys) == nil {
			Expect(k8sClient.Delete(ctx, sys)).To(Succeed())
		}
	})

	It("deploys the gateway with the agent sidecar and the pool's identity", func() {
		reconcileS3()
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, nn, dep)).To(Succeed())

		Expect(dep.Spec.Template.Spec.InitContainers).To(HaveLen(1))
		agent := dep.Spec.Template.Spec.InitContainers[0]
		Expect(agent.Image).To(Equal("agent:1"))
		Expect(*agent.RestartPolicy).To(Equal(corev1.ContainerRestartPolicyAlways), "the agent must be a native sidecar")

		gw := dep.Spec.Template.Spec.Containers[0]
		cmd := strings.Join(gw.Command, " ")
		Expect(cmd).To(ContainSubstring("daos --pool '" + poolName + "'"))
		Expect(cmd).To(ContainSubstring("--system 'daos_k8s'"))
		Expect(cmd).To(ContainSubstring("--iam-dir " + s3IAMDir))
		Expect(cmd).To(ContainSubstring("--region 'kr-1'"))
		// credentials come from the Secret, never from the CR
		names := map[string]string{}
		for _, e := range gw.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				names[e.Name] = e.ValueFrom.SecretKeyRef.Name + "/" + e.ValueFrom.SecretKeyRef.Key
			}
		}
		Expect(names).To(HaveKeyWithValue("ROOT_ACCESS_KEY_ID", "s3root/accessKey"))
		Expect(names).To(HaveKeyWithValue("ROOT_SECRET_ACCESS_KEY", "s3root/secretKey"))

		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, nn, svc)).To(Succeed())
		Expect(svc.Spec.Ports[0].Port).To(Equal(int32(7070)))

		Expect(cond("PoolReady").Status).To(Equal(metav1.ConditionTrue))
		// no PVC: say so rather than pretend the users are safe
		Expect(cond("IAMDurable").Status).To(Equal(metav1.ConditionFalse))
		Expect(cond("IAMDurable").Message).To(ContainSubstring("lost when the pod restarts"))
		Expect(get().Status.Endpoint).To(Equal("http://gw.default.svc:7070"))
	})

	It("refuses several replicas that would each keep their own user list", func() {
		s3 := get()
		s3.Spec.Replicas = 3
		Expect(k8sClient.Update(ctx, s3)).To(Succeed())
		reconcileS3()

		Expect(cond("Ready").Reason).To(Equal("ReplicasUnsafe"))
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, nn, dep)).NotTo(Succeed(), "nothing may be deployed while the spec is unsafe")

		// a shared claim makes it legitimate
		s3 = get()
		s3.Spec.IAM.ClaimName = "iam-rwx"
		Expect(k8sClient.Update(ctx, s3)).To(Succeed())
		reconcileS3()
		Expect(k8sClient.Get(ctx, nn, dep)).To(Succeed())
		Expect(*dep.Spec.Replicas).To(Equal(int32(3)))
		Expect(cond("IAMDurable").Status).To(Equal(metav1.ConditionTrue))
		var claim string
		for _, v := range dep.Spec.Template.Spec.Volumes {
			if v.Name == "iam" && v.PersistentVolumeClaim != nil {
				claim = v.PersistentVolumeClaim.ClaimName
			}
		}
		Expect(claim).To(Equal("iam-rwx"))
	})

	It("waits for the pool instead of serving an endpoint that cannot work", func() {
		pool := &daosv1alpha1.DaosPool{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: poolName}, pool)).To(Succeed())
		meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{Type: daosv1alpha1.ConditionReady,
			Status: metav1.ConditionFalse, Reason: "CreateInProgress", Message: "creating"})
		Expect(k8sClient.Status().Update(ctx, pool)).To(Succeed())
		reconcileS3()
		Expect(cond("Ready").Reason).To(Equal("PoolNotReady"))
		Expect(k8sClient.Get(ctx, nn, &appsv1.Deployment{})).NotTo(Succeed())
	})

	It("mounts the TLS secret and reports an https endpoint", func() {
		s3 := get()
		s3.Spec.TLSSecretName = "gw-tls"
		s3.Spec.ServiceType = corev1.ServiceTypeNodePort
		s3.Spec.NodePort = 30070
		Expect(k8sClient.Update(ctx, s3)).To(Succeed())
		reconcileS3()
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, nn, dep)).To(Succeed())
		Expect(strings.Join(dep.Spec.Template.Spec.Containers[0].Command, " ")).To(ContainSubstring("--cert " + s3TLSDir + "/tls.crt"))
		Expect(get().Status.Endpoint).To(ContainSubstring("https://"))
		Expect(get().Status.Endpoint).To(ContainSubstring("nodePort 30070"))
		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, nn, svc)).To(Succeed())
		Expect(svc.Spec.Ports[0].NodePort).To(Equal(int32(30070)))
	})
})
