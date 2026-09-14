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
	Args []string
	// Command replaces the default `dmg -j <Args>` entirely (e.g. a bash -c wrapper
	// that writes an ACL file first, or a `daos` client invocation).
	Command      []string
	Env          []corev1.EnvVar
	NodeSelector map[string]string
	Tolerations  []corev1.Toleration
	Labels       map[string]string
	// DeadlineSeconds bounds the Job (default 600).
	DeadlineSeconds int64
	// Sidecar runs next to the main container as a native sidecar (initContainer
	// with restartPolicy Always, Kubernetes >= 1.29) -- used for daos_agent, which
	// the `daos` client needs. The Job still completes when the main container exits.
	Sidecar *corev1.Container
	// Volumes and Mounts are added to the pod / main container (control config is always mounted).
	Volumes []corev1.Volume
	Mounts  []corev1.VolumeMount
	// CertsSecret, when set, is mounted at /etc/daos/certs: CertsFiles (path -> key)
	// into the main container and SidecarCertsFiles into the sidecar.
	CertsSecret       string
	CertsFiles        map[string]string
	SidecarCertsFiles map[string]string
	// Privileged runs the main container privileged (RDMA device access for the client library).
	Privileged bool
	// ShareProcessNamespace lets the agent sidecar validate the client process.
	ShareProcessNamespace bool
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
	command := s.Command
	if len(command) == 0 {
		// admin entrypoint: "dmg ..." -> exec dmg -o /etc/daos/daos_control.yml ...
		command = append([]string{"dmg", "-j"}, s.Args...)
	}
	volumes := append([]corev1.Volume{{Name: "control", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: s.ControlConfigMap}}}}}, s.Volumes...)
	mounts := append([]corev1.VolumeMount{{Name: "control", MountPath: "/etc/daos/daos_control.yml", SubPath: "daos_control.yml", ReadOnly: true}}, s.Mounts...)
	var sec *corev1.SecurityContext
	if s.Privileged {
		sec = &corev1.SecurityContext{Privileged: ptr.To(true)}
	}
	if s.CertsSecret != "" {
		volumes = append(volumes, secretVolume("certs", s.CertsSecret, s.CertsFiles))
		mounts = append(mounts, corev1.VolumeMount{Name: "certs", MountPath: "/etc/daos/certs", ReadOnly: true})
	}
	var inits []corev1.Container
	if s.Sidecar != nil {
		sc := *s.Sidecar
		sc.RestartPolicy = ptr.To(corev1.ContainerRestartPolicyAlways)
		if s.CertsSecret != "" && len(s.SidecarCertsFiles) > 0 {
			volumes = append(volumes, secretVolume("sidecar-certs", s.CertsSecret, s.SidecarCertsFiles))
			sc.VolumeMounts = append(sc.VolumeMounts, corev1.VolumeMount{Name: "sidecar-certs", MountPath: "/etc/daos/certs", ReadOnly: true})
		}
		inits = append(inits, sc)
	}
	var shareNS *bool
	if s.ShareProcessNamespace {
		shareNS = ptr.To(true)
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
				HostNetwork:           true,
				DNSPolicy:             corev1.DNSClusterFirstWithHostNet,
				ShareProcessNamespace: shareNS,
				NodeSelector:          s.NodeSelector,
				Tolerations:           s.Tolerations,
				Volumes:               volumes,
				InitContainers:        inits,
				Containers: []corev1.Container{{
					Name:            container,
					Image:           s.Image,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Command:         command,
					Env:             s.Env,
					SecurityContext: sec,
					VolumeMounts:    mounts,
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

// secretVolume projects selected Secret keys (path -> key); *.key files are 0400.
func secretVolume(name, secret string, files map[string]string) corev1.Volume {
	items := make([]corev1.KeyToPath, 0, len(files))
	for path, key := range files {
		mode := int32(0o444)
		if strings.HasSuffix(key, ".key") {
			mode = 0o400
		}
		items = append(items, corev1.KeyToPath{Key: key, Path: path, Mode: ptr.To(mode)})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secret, Items: items, DefaultMode: ptr.To(int32(0o444))}}}
}

// Envelope is the `dmg -j` output frame.
type Envelope struct {
	Response json.RawMessage `json:"response"`
	Error    *string         `json:"error"`
	Status   int             `json:"status"`
}

// Parse extracts the JSON envelope from dmg/daos output. Pod logs merge stdout
// and stderr, so client-library log lines may precede the JSON and the tool's
// own "ERROR: dmg: ..." line may follow it; only the first JSON value counts.
func Parse(out string) (*Envelope, error) {
	i := strings.Index(out, "{")
	if i < 0 {
		return nil, fmt.Errorf("no JSON in dmg output: %q", strings.TrimSpace(firstLine(out)))
	}
	var e Envelope
	if err := json.NewDecoder(strings.NewReader(out[i:])).Decode(&e); err != nil {
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
	// ErrNotFound: the pool or container does not exist.
	ErrNotFound
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
	case strings.Contains(m, "unable to find pool service with label"), strings.Contains(m, "der_nonexist"),
		strings.Contains(m, "does not exist"), strings.Contains(m, "not found"):
		return ErrNotFound
	case strings.Contains(m, "unable to contact the daos management service"), strings.Contains(m, "connection refused"),
		strings.Contains(m, "no route to host"), strings.Contains(m, "deadline exceeded"), strings.Contains(m, "i/o timeout"),
		strings.Contains(m, "der_unreach"), strings.Contains(m, "unreachable node"), strings.Contains(m, "refused the connection"):
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

// PoolInfo is the subset of `dmg -j pool query [--show-enabled]` the operator mirrors.
type PoolInfo struct {
	UUID            string `json:"uuid"`
	Label           string `json:"label"`
	State           string `json:"state"`
	TotalTargets    int32  `json:"total_targets"`
	ActiveTargets   int32  `json:"active_targets"`
	DisabledTargets int32  `json:"disabled_targets"`
	Rebuild         struct {
		State string `json:"state"`
	} `json:"rebuild"`
	TierStats []struct {
		Total     int64  `json:"total"`
		Free      int64  `json:"free"`
		MediaType string `json:"media_type"`
	} `json:"tier_stats"`
	EnabledRanks  json.RawMessage `json:"enabled_ranks"`
	DisabledRanks json.RawMessage `json:"disabled_ranks"`
}

// Totals sums the storage tiers.
func (p *PoolInfo) Totals() (total, free int64) {
	for _, t := range p.TierStats {
		total += t.Total
		free += t.Free
	}
	return total, free
}

// PoolQuery parses `dmg -j pool query`.
func PoolQuery(e *Envelope) (*PoolInfo, error) {
	var p PoolInfo
	if len(e.Response) == 0 || string(e.Response) == "null" {
		return nil, fmt.Errorf("pool query: empty response")
	}
	if err := json.Unmarshal(e.Response, &p); err != nil {
		return nil, fmt.Errorf("pool query response: %w", err)
	}
	return &p, nil
}

// Ranks decodes a rank set that dmg emits either as a JSON array or as a
// range string such as "[0-3,5]".
func Ranks(raw json.RawMessage) []int32 {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var arr []int32
	if err := json.Unmarshal(raw, &arr); err == nil {
		if len(arr) == 0 {
			return nil
		}
		sort.Slice(arr, func(i, j int) bool { return arr[i] < arr[j] })
		return arr
	}
	var str string
	if err := json.Unmarshal(raw, &str); err != nil {
		return nil
	}
	str = strings.Trim(strings.TrimSpace(str), "[]")
	if str == "" {
		return nil
	}
	var out []int32
	for _, part := range strings.Split(str, ",") {
		var lo, hi int32
		switch n, _ := fmt.Sscanf(part, "%d-%d", &lo, &hi); n {
		case 2:
			for r := lo; r <= hi; r++ {
				out = append(out, r)
			}
		case 1:
			out = append(out, lo)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// PoolCreate parses `dmg -j pool create` (control.PoolCreateResp).
func PoolCreate(e *Envelope) (uuid string, tgtRanks []int32, err error) {
	var r struct {
		UUID     string  `json:"uuid"`
		TgtRanks []int32 `json:"tgt_ranks"`
	}
	if len(e.Response) == 0 || string(e.Response) == "null" {
		return "", nil, fmt.Errorf("pool create: empty response")
	}
	if err := json.Unmarshal(e.Response, &r); err != nil {
		return "", nil, fmt.Errorf("pool create response: %w", err)
	}
	return r.UUID, r.TgtRanks, nil
}

// ContainerInfo is `daos -j cont query` / `daos -j cont create` output.
type ContainerInfo struct {
	PoolUUID         string `json:"pool_uuid"`
	UUID             string `json:"container_uuid"`
	Label            string `json:"container_label"`
	RedundancyFactor int32  `json:"redundancy_factor"`
	Type             string `json:"container_type"`
	Health           string `json:"health"`
	ChunkSize        int64  `json:"chunk_size"`
	DirObjectClass   string `json:"dir_object_class"`
	FileObjectClass  string `json:"file_object_class"`
}

// ContainerQuery parses `daos -j cont query` (also the create output, same struct).
func ContainerQuery(e *Envelope) (*ContainerInfo, error) {
	var c ContainerInfo
	if len(e.Response) == 0 || string(e.Response) == "null" {
		return nil, fmt.Errorf("container query: empty response")
	}
	if err := json.Unmarshal(e.Response, &c); err != nil {
		return nil, fmt.Errorf("container query response: %w", err)
	}
	return &c, nil
}

// ShellQuote quotes s for a POSIX shell.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// WithACLFile wraps a command so that the ACL entries are written to path first.
// Empty entries return the command unchanged.
func WithACLFile(entries []string, path string, command []string) []string {
	if len(entries) == 0 {
		return command
	}
	var b strings.Builder
	b.WriteString("printf '%s\\n'")
	for _, e := range entries {
		b.WriteString(" " + ShellQuote(e))
	}
	b.WriteString(" > " + ShellQuote(path) + " && exec")
	for _, c := range command {
		b.WriteString(" " + ShellQuote(c))
	}
	return []string{"bash", "-c", b.String()}
}
