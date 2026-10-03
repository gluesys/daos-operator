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

package dmg

import (
	"context"
	"k8s.io/apimachinery/pkg/api/resource"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestJobRunnerLifecycle(t *testing.T) {
	ctx := context.Background()
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "ns", UID: "u1"}}
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(owner).Build()
	r := &JobRunner{Client: c, Scheme: scheme.Scheme} // Kube nil: logs are skipped
	spec := RunSpec{Owner: owner, Namespace: "ns", Name: "sys-dmg-query", Image: "admin", ControlConfigMap: "sys-control",
		Args: []string{"system", "query", "-v"}, NodeSelector: map[string]string{"role": "storage"}}

	res, err := r.Run(ctx, spec)
	if err != nil || res.Done {
		t.Fatalf("first run should create the job and not be done: %+v %v", res, err)
	}
	job := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "sys-dmg-query"}, job); err != nil {
		t.Fatal(err)
	}
	pod := job.Spec.Template.Spec
	if !pod.HostNetwork || pod.RestartPolicy != corev1.RestartPolicyNever || pod.NodeSelector["role"] != "storage" {
		t.Errorf("pod spec: %+v", pod)
	}
	cmd := pod.Containers[0].Command
	if len(cmd) != 5 || cmd[0] != "dmg" || cmd[1] != "-j" || cmd[2] != "system" {
		t.Errorf("command: %v", cmd)
	}
	if pod.Containers[0].VolumeMounts[0].MountPath != "/etc/daos/daos_control.yml" || pod.Volumes[0].ConfigMap.Name != "sys-control" {
		t.Errorf("control config mount: %+v", pod)
	}
	if len(job.OwnerReferences) != 1 || *job.Spec.BackoffLimit != 0 {
		t.Errorf("owner/backoff: %+v", job)
	}

	res, err = r.Run(ctx, spec)
	if err != nil || res.Done {
		t.Fatalf("running job must report not done: %+v %v", res, err)
	}

	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "DeadlineExceeded", Message: "Job was active longer than specified deadline"}}
	if err := c.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	res, err = r.Run(ctx, spec)
	if err != nil || !res.Done || res.ExitCode != -1 || res.Failure == "" {
		t.Fatalf("finished job: %+v %v", res, err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "sys-dmg-query"}, job); !apierrors.IsNotFound(err) {
		t.Fatalf("job must be deleted after its result is read, got %v", err)
	}
	// and the cycle restarts
	res, err = r.Run(ctx, spec)
	if err != nil || res.Done {
		t.Fatalf("recreate: %+v %v", res, err)
	}
}

func TestSecretVolume(t *testing.T) {
	v := secretVolume("certs", "s1", map[string]string{"daosCA.crt": "daosCA.crt", "clients/agent.crt": "agent.crt", "admin.key": "admin.key"})
	if v.Secret.SecretName != "s1" || len(v.Secret.Items) != 3 {
		t.Fatalf("%+v", v)
	}
	for _, it := range v.Secret.Items {
		if it.Key == "admin.key" && *it.Mode != 0o400 {
			t.Errorf("key file mode %o", *it.Mode)
		}
		if it.Path == "clients/agent.crt" && it.Key != "agent.crt" {
			t.Errorf("clients mapping: %+v", it)
		}
	}
	if v.Secret.Items[0].Path != "admin.key" {
		t.Errorf("items must be sorted by path for a stable spec: %v", v.Secret.Items)
	}
}

func TestJobFinished(t *testing.T) {
	j := &batchv1.Job{}
	if ok, _ := jobFinished(j); ok {
		t.Fatal("empty job is not finished")
	}
	j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if ok, f := jobFinished(j); !ok || f != "" {
		t.Fatalf("%v %q", ok, f)
	}
}

// Job pods run on the storage nodes next to a busy-polling engine. Without a CPU request they
// are BestEffort, get the minimum CPU share, and every daos/dmg call took 8-80 s instead of
// <0.5 s (CI cluster, 2026-10-03): PVCs took 5-7 min and an agent that missed the client's
// wait window failed creates with DER_AGENT_COMM. Both containers must request CPU.
func TestJobPodsRequestCPU(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	r := &JobRunner{Client: c, Scheme: scheme.Scheme}
	spec := RunSpec{Namespace: "ns", Name: "cont-x-daos-create", Image: "client", ControlConfigMap: "sys-control",
		Command: []string{"daos", "cont", "create"}, Sidecar: &corev1.Container{Name: "agent", Image: "agent"}}
	if _, err := r.Run(ctx, spec); err != nil {
		t.Fatal(err)
	}
	job := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "cont-x-daos-create"}, job); err != nil {
		t.Fatal(err)
	}
	pod := job.Spec.Template.Spec
	for _, ct := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
		cpu := ct.Resources.Requests[corev1.ResourceCPU]
		if cpu.IsZero() {
			t.Errorf("container %q has no CPU request (BestEffort starves next to the engine)", ct.Name)
		}
	}
	// a request the caller set on the sidecar is kept
	spec2 := spec
	spec2.Name = "cont-y-daos-create"
	spec2.Sidecar = &corev1.Container{Name: "agent", Image: "agent", Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}}}
	if _, err := r.Run(ctx, spec2); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "cont-y-daos-create"}, job); err != nil {
		t.Fatal(err)
	}
	if got := job.Spec.Template.Spec.InitContainers[0].Resources.Requests[corev1.ResourceCPU]; got.String() != "1" {
		t.Errorf("caller's sidecar CPU request overwritten: %s", got.String())
	}
}
