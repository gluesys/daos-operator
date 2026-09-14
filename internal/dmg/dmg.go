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

// Package dmg runs the DAOS admin tool as a Kubernetes Job (exastor/daos-images
// daos-admin image) and parses its JSON output. The operator never links DAOS
// libraries: everything it knows about the management service comes from
// `dmg -j`, so status is always what dmg said, never a second source of truth.
package dmg

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// RunSpec describes one dmg invocation.
type RunSpec struct {
	// Owner becomes the Job's controller reference (GC + watch).
	Owner client.Object
	// Namespace and Name of the Job. The name is deterministic per purpose
	// ("<sys>-dmg-query"); a finished Job is deleted after its output is read.
	Namespace, Name string
	Image           string
	// ControlConfigMap holds daos_control.yml (key daos_control.yml).
	ControlConfigMap string
	// Args follow `dmg -j`; the admin entrypoint adds `-o /etc/daos/daos_control.yml`.
	Args         []string
	NodeSelector map[string]string
	Tolerations  []corev1.Toleration
	Labels       map[string]string
	// DeadlineSeconds bounds the Job (default 600).
	DeadlineSeconds int64
}

// Result is what came back. Done=false means the Job is still running (or was
// just created); poll again.
type Result struct {
	Done bool
	// ExitCode of the dmg container; -1 when the pod never ran.
	ExitCode int32
	// Output is the combined stdout/stderr of the container (dmg -j writes JSON to stdout).
	Output string
	// Failure describes a Job-level failure (deadline, unschedulable) when Output is empty.
	Failure string
}

// Runner runs dmg. The controller depends on this interface so tests can script outputs.
type Runner interface {
	Run(ctx context.Context, spec RunSpec) (*Result, error)
}

// JobRunner is the real Runner: one Kubernetes Job per invocation.
type JobRunner struct {
	Client client.Client
	Kube   kubernetes.Interface
	Scheme *runtime.Scheme
}

const container = "dmg"

// Run creates the Job when absent, reports Done=false while it runs, and once it
// finished returns its output and deletes it so the next Run starts fresh.
func (j *JobRunner) Run(ctx context.Context, s RunSpec) (*Result, error) {
	job := &batchv1.Job{}
	err := j.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: s.Name}, job)
	if apierrors.IsNotFound(err) {
		return &Result{}, j.create(ctx, s)
	}
	if err != nil {
		return nil, err
	}
	if !job.DeletionTimestamp.IsZero() {
		return &Result{}, nil // previous run still being removed
	}
	finished, failure := jobFinished(job)
	if !finished {
		return &Result{}, nil
	}
	res := &Result{Done: true, ExitCode: -1, Failure: failure}
	pods := &corev1.PodList{}
	if err := j.Client.List(ctx, pods, client.InNamespace(s.Namespace), client.MatchingLabels{"job-name": s.Name}); err != nil {
		return nil, err
	}
	if len(pods.Items) > 0 {
		sort.Slice(pods.Items, func(a, b int) bool {
			return pods.Items[a].CreationTimestamp.After(pods.Items[b].CreationTimestamp.Time)
		})
		p := pods.Items[0]
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name == container && cs.State.Terminated != nil {
				res.ExitCode = cs.State.Terminated.ExitCode
			}
		}
		if j.Kube != nil {
			rc, err := j.Kube.CoreV1().Pods(s.Namespace).GetLogs(p.Name, &corev1.PodLogOptions{Container: container}).Stream(ctx)
			if err != nil {
				return nil, fmt.Errorf("logs of %s: %w", p.Name, err)
			}
			b, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				return nil, err
			}
			res.Output = string(b)
		}
	}
	// remove the Job (and its pod) so a later Run can create a new one
	if err := j.Client.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	return res, nil
}

func jobFinished(job *batchv1.Job) (bool, string) {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return true, ""
		case batchv1.JobFailed:
			return true, strings.TrimSpace(c.Reason + ": " + c.Message)
		}
	}
	return false, ""
}

func (j *JobRunner) create(ctx context.Context, s RunSpec) error {
	deadline := s.DeadlineSeconds
	if deadline <= 0 {
		deadline = 600
	}
	labels := map[string]string{"app.kubernetes.io/name": "daos-dmg", "app.kubernetes.io/managed-by": "daos-operator"}
	for k, v := range s.Labels {
		labels[k] = v
	}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: s.Name, Labels: labels}}
	job.Spec = batchv1.JobSpec{
		BackoffLimit:            ptr.To(int32(0)),
		ActiveDeadlineSeconds:   ptr.To(deadline),
		TTLSecondsAfterFinished: ptr.To(int32(3600)), // safety net if the operator is down
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				// same network as the servers: hostlist addresses are node IPs
				HostNetwork:  true,
				DNSPolicy:    corev1.DNSClusterFirstWithHostNet,
				NodeSelector: s.NodeSelector,
				Tolerations:  s.Tolerations,
				Volumes: []corev1.Volume{{Name: "control", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: s.ControlConfigMap}}}}},
				Containers: []corev1.Container{{
					Name:            container,
					Image:           s.Image,
					ImagePullPolicy: corev1.PullIfNotPresent,
					// admin entrypoint: "dmg ..." -> exec dmg -o /etc/daos/daos_control.yml ...
					Command:      append([]string{"dmg", "-j"}, s.Args...),
					VolumeMounts: []corev1.VolumeMount{{Name: "control", MountPath: "/etc/daos/daos_control.yml", SubPath: "daos_control.yml", ReadOnly: true}},
				}},
			},
		},
	}
	if s.Owner != nil && j.Scheme != nil {
		if err := controllerutil.SetControllerReference(s.Owner, job, j.Scheme); err != nil {
			return err
		}
	}
	err := j.Client.Create(ctx, job)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// Envelope is the `dmg -j` output frame.
type Envelope struct {
	Response json.RawMessage `json:"response"`
	Error    *string         `json:"error"`
	Status   int             `json:"status"`
}

// Parse extracts the JSON envelope from dmg output (log lines may precede it).
func Parse(out string) (*Envelope, error) {
	i := strings.Index(out, "{")
	if i < 0 {
		return nil, fmt.Errorf("no JSON in dmg output: %q", strings.TrimSpace(firstLine(out)))
	}
	var e Envelope
	if err := json.Unmarshal([]byte(out[i:]), &e); err != nil {
		return nil, fmt.Errorf("dmg output is not JSON: %w", err)
	}
	return &e, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ErrorKind classifies a dmg error string.
type ErrorKind int

const (
	// ErrNone: no error.
	ErrNone ErrorKind = iota
	// ErrUnformatted: the management service exists but was never formatted
	// (system.ErrUninitialized / ErrRaftUnavail in the DAOS control plane).
	ErrUnformatted
	// ErrUnreachable: no management-service replica answered.
	ErrUnreachable
	// ErrOther: anything else; the message is shown as is.
	ErrOther
)

// Classify maps a dmg error message to an ErrorKind.
func Classify(msg string) ErrorKind {
	m := strings.ToLower(msg)
	switch {
	case m == "":
		return ErrNone
	case strings.Contains(m, "system is uninitialized"), strings.Contains(m, "storage format required"),
		strings.Contains(m, "raft service unavailable"):
		return ErrUnformatted
	case strings.Contains(m, "unable to contact the daos management service"), strings.Contains(m, "connection refused"),
		strings.Contains(m, "no route to host"), strings.Contains(m, "deadline exceeded"), strings.Contains(m, "i/o timeout"):
		return ErrUnreachable
	default:
		return ErrOther
	}
}

// Member is one row of `dmg -j system query -v`.
type Member struct {
	Rank        int32  `json:"rank"`
	Addr        string `json:"addr"`
	State       string `json:"state"`
	FaultDomain string `json:"fault_domain"`
	UUID        string `json:"uuid"`
	Info        string `json:"info"`
}

// SystemQuery parses the response of `dmg -j system query -v`.
func SystemQuery(e *Envelope) ([]Member, error) {
	var resp struct {
		Members []Member `json:"members"`
	}
	if len(e.Response) == 0 || string(e.Response) == "null" {
		return nil, nil
	}
	if err := json.Unmarshal(e.Response, &resp); err != nil {
		return nil, fmt.Errorf("system query response: %w", err)
	}
	sort.Slice(resp.Members, func(i, j int) bool { return resp.Members[i].Rank < resp.Members[j].Rank })
	return resp.Members, nil
}

// StorageFormat parses `dmg -j storage format`: per-host errors keyed by message.
func StorageFormat(e *Envelope) (hostErrors map[string]string, err error) {
	var resp struct {
		HostErrors map[string]string `json:"host_errors"`
	}
	if len(e.Response) == 0 || string(e.Response) == "null" {
		return nil, nil
	}
	if err := json.Unmarshal(e.Response, &resp); err != nil {
		return nil, fmt.Errorf("storage format response: %w", err)
	}
	return resp.HostErrors, nil
}

// Host strips the port from a member address ("10.0.0.1:10001" -> "10.0.0.1").
func Host(addr string) string {
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[:i]
	}
	return addr
}
