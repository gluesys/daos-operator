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
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	daosv1alpha1 "github.com/gluesys/daos-operator/api/v1alpha1"
)

const (
	clientAgentConfigKey       = "daos_agent.yml"
	clientAgentMetricsPortName = "metrics"
)

// clientAgentName is the DaemonSet (and its pods' label) for spec.clientAgent.
func clientAgentName(sys *daosv1alpha1.DaosSystem) string { return sys.Name + "-agent" }

// clientAgentHostNetwork is spec.clientAgent.hostNetwork with its default of true.
func clientAgentHostNetwork(sys *daosv1alpha1.DaosSystem) bool {
	return sys.Spec.ClientAgent.HostNetwork == nil || *sys.Spec.ClientAgent.HostNetwork
}

// clientAgentConfigRole labels the hash-named ConfigMaps the client agent mounts.
const clientAgentConfigRole = "client-agent-config"

// ensureClientAgentConfig creates the immutable ConfigMap holding one version
// of the client agent's config and returns its name.
func (r *DaosSystemReconciler) ensureClientAgentConfig(ctx context.Context, sys *daosv1alpha1.DaosSystem, ns, hash, cfg string) (string, error) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: clientAgentName(sys) + "-config-" + hash[:10], Namespace: ns}}
	labelManaged(&cm.ObjectMeta, sys, "")
	cm.Labels["app.kubernetes.io/name"] = "daos-agent"
	cm.Labels[daosv1alpha1.LabelRole] = clientAgentConfigRole
	cm.Annotations = map[string]string{"daos.gluesys.com/agent-config-hash": hash}
	cm.Data = map[string]string{clientAgentConfigKey: cfg}
	cm.Immutable = new(true)
	if err := controllerutil.SetControllerReference(sys, cm, r.Scheme); err != nil {
		return "", err
	}
	err := r.Create(ctx, cm)
	if apierrors.IsAlreadyExists(err) {
		cur := &corev1.ConfigMap{}
		if err = r.Get(ctx, client.ObjectKeyFromObject(cm), cur); err == nil && cur.Data[clientAgentConfigKey] != cfg {
			// a 40-bit hash prefix collision: refuse rather than mount the wrong config
			err = fmt.Errorf("client agent config %s exists with different content", cm.Name)
		}
	}
	if err != nil {
		return "", fmt.Errorf("client agent config: %w", err)
	}
	return cm.Name, nil
}

// clientAgentRolledOut reports whether every scheduled agent runs the
// DaemonSet's current template, so no pod still needs an older config.
func clientAgentRolledOut(ds *appsv1.DaemonSet) bool {
	st := ds.Status
	return ds.Generation != 0 && st.ObservedGeneration >= ds.Generation &&
		st.UpdatedNumberScheduled == st.DesiredNumberScheduled && st.NumberUnavailable == 0
}

// pruneClientAgentConfigs deletes the client agent config versions other than keep.
func (r *DaosSystemReconciler) pruneClientAgentConfigs(ctx context.Context, sys *daosv1alpha1.DaosSystem, ns, keep string) error {
	list := &corev1.ConfigMapList{}
	if err := r.List(ctx, list, client.InNamespace(ns),
		client.MatchingLabels{daosv1alpha1.LabelSystem: sys.Name, daosv1alpha1.LabelRole: clientAgentConfigRole}); err != nil {
		return err
	}
	for i := range list.Items {
		if cm := &list.Items[i]; cm.Name != keep {
			if err := client.IgnoreNotFound(r.Delete(ctx, cm)); err != nil {
				return err
			}
		}
	}
	return nil
}

// clientAgentSocketDir is where the node-level agent publishes its socket on the host.
func clientAgentSocketDir(sys *daosv1alpha1.DaosSystem) string {
	if d := sys.Spec.ClientAgent.HostSocketDir; d != "" {
		return filepath.Clean(d)
	}
	// deliberately not under /var/run/daos_agent: the host's daos_agent.service
	// declares RuntimeDirectory=daos_agent, so systemd deletes that directory --
	// and everything we put under it -- whenever that service stops. A node
	// recycled from a native install hits this the moment someone stops the old
	// service, and the agent pod keeps running with its socket gone (#33).
	return filepath.Join("/var/run/daos-operator/agent", sys.Name)
}

// ensureClientAgent keeps the per-node daos_agent DaemonSet in step with
// spec.clientAgent: one agent per selected node, socket on the host at
// clientAgentSocketDir. Its config is the <sys>-agent ConfigMap every other
// agent the operator runs reads (plus telemetry settings), copied into a
// hash-named version (see ensureClientAgentConfig); with TLS it also mounts the
// agent certificate set. Returns the DaemonSet's ready/desired counts for the
// status condition.
func (r *DaosSystemReconciler) ensureClientAgent(ctx context.Context, sys *daosv1alpha1.DaosSystem, ns string) (ready, desired int32, err error) {
	name := clientAgentName(sys)
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	telemetry := sys.Spec.ClientAgent.Enabled && sys.Spec.ClientAgent.Telemetry.Enabled
	port := sys.Spec.ClientAgent.Telemetry.Port
	if port == 0 {
		port = 9192
	}
	metricsName := name + "-metrics"
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: metricsName, Namespace: ns}}
	telemetryConfig := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name + "-telemetry", Namespace: ns}}
	if !telemetry {
		if e := client.IgnoreNotFound(r.Delete(ctx, svc)); e != nil {
			return 0, 0, e
		}
	}
	if !sys.Spec.ClientAgent.Enabled {
		if e := client.IgnoreNotFound(r.Delete(ctx, ds)); e != nil {
			return 0, 0, e
		}
		if e := client.IgnoreNotFound(r.Delete(ctx, telemetryConfig)); e != nil {
			return 0, 0, e
		}
		return 0, 0, r.pruneClientAgentConfigs(ctx, sys, ns, "")
	}
	// The agent reads its config once, through a subPath mount, so a pod keeps
	// the file it started with. Each distinct config therefore gets its own
	// immutable ConfigMap named after its hash, and the pod template mounts
	// that name: a pod created for a new config cannot start from an older
	// copy (kubelet has never seen an older version of that name), and any
	// change -- an external system's cache_expiration, telemetry -- rolls the
	// agents. Versions the rollout no longer needs are pruned below.
	base := &corev1.ConfigMap{}
	if e := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: sys.Name + "-agent"}, base); e != nil {
		return 0, 0, e
	}
	cfg := base.Data[clientAgentConfigKey]
	if telemetry {
		// Never enable HTTP on the shared sidecar/job config: several agents
		// can share a host network and would all try to bind the same port.
		cfg += fmt.Sprintf("telemetry_enabled: true\ntelemetry_port: %d\n", port)
	}
	configHash := fmt.Sprintf("%x", sha256.Sum256([]byte(cfg)))
	configName, err := r.ensureClientAgentConfig(ctx, sys, ns, configHash, cfg)
	if err != nil {
		return 0, 0, err
	}

	labels := map[string]string{"app.kubernetes.io/name": "daos-agent", daosv1alpha1.LabelSystem: sys.Name, daosv1alpha1.LabelRole: "client-agent"}
	hostDir, hostDirCreate := corev1.HostPathDirectory, corev1.HostPathDirectoryOrCreate
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, ds, func() error {
		labelManaged(&ds.ObjectMeta, sys, "")
		ds.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		ds.Spec.Template.Labels = labels
		if ds.Spec.Template.Annotations == nil {
			ds.Spec.Template.Annotations = map[string]string{}
		}
		ds.Spec.Template.Annotations["daos.gluesys.com/agent-config-hash"] = configHash
		pod := &ds.Spec.Template.Spec
		pod.NodeSelector = sys.Spec.ClientAgent.NodeSelector
		pod.Tolerations = sys.Spec.ClientAgent.Tolerations
		pod.ImagePullSecrets = sys.Spec.ImagePullSecrets
		// the agent enumerates the fabric of the namespace it runs in, and its
		// clients must see the same interfaces: host namespace for hostNetwork
		// clients, pod namespace for ordinary pods (see ClientAgentSpec)
		pod.HostNetwork = clientAgentHostNetwork(sys)
		pod.HostIPC = telemetry
		// Resolve SO_PEERCRED PIDs of clients in other pods, including their
		// NUMA node and process lifetime, from the host PID namespace.
		pod.HostPID = telemetry
		if pod.HostNetwork {
			pod.DNSPolicy = corev1.DNSClusterFirstWithHostNet
		} else {
			pod.DNSPolicy = corev1.DNSClusterFirst
		}
		pod.PriorityClassName = "system-node-critical"
		pod.Volumes = []corev1.Volume{
			{Name: "agentcfg", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: configName}}}},
			{Name: "agentsock", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: clientAgentSocketDir(sys), Type: &hostDirCreate}}},
			{Name: "dev", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev", Type: &hostDir}}},
		}
		c := corev1.Container{
			Name:            "agent",
			Image:           sys.Spec.Images.Agent,
			ImagePullPolicy: corev1.PullIfNotPresent,
			// RDMA devices for the fabric scan, same reason as the sidecars
			SecurityContext: &corev1.SecurityContext{Privileged: new(true)},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "agentcfg", MountPath: "/etc/daos/daos_agent.yml", SubPath: "daos_agent.yml", ReadOnly: true},
				{Name: "agentsock", MountPath: agentSocketDir},
				{Name: "dev", MountPath: "/dev"},
			},
			// the socket can vanish under a running agent (something wipes the
			// host directory); clients then fail with DER_NONEXIST while the pod
			// still looks healthy. Restart on that instead of staying up (#33).
			LivenessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
					Command: []string{"test", "-S", agentSocketDir + "/daos_agent.sock"}}},
				InitialDelaySeconds: 30, PeriodSeconds: 30, FailureThreshold: 2,
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
			},
		}
		if telemetry {
			c.Ports = []corev1.ContainerPort{{Name: clientAgentMetricsPortName, ContainerPort: port, Protocol: corev1.ProtocolTCP}}
		}
		if s := adminCertsSecret(sys); s != "" {
			pod.Volumes = append(pod.Volumes, corev1.Volume{Name: "certs", VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: s, DefaultMode: new(int32(0o400))}}})
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "certs", MountPath: "/etc/daos/certs", ReadOnly: true})
		}
		// The socket lives on a hostPath, so a socket file from a previous agent
		// survives the pod and daos_agent then refuses to start with "Configured
		// dRPC socket file is already in use" -- permanently, after any node or
		// runtime restart (cxl2, 2026-09-27). Remove a stale one first; a live
		// agent on the same directory would already be a misconfiguration.
		pod.InitContainers = []corev1.Container{{
			Name:            "clean-socket",
			Image:           sys.Spec.Images.Agent,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         []string{"sh", "-c", "rm -f " + agentSocketDir + "/daos_agent.sock"},
			VolumeMounts:    []corev1.VolumeMount{{Name: "agentsock", MountPath: agentSocketDir}},
		}}
		pod.Containers = []corev1.Container{c}
		return controllerutil.SetControllerReference(sys, ds, r.Scheme)
	})
	if err != nil {
		return 0, 0, fmt.Errorf("client agent daemonset: %w", err)
	}
	// Older config versions (and the pre-versioning <name>-telemetry) stay
	// until every agent runs the current template: a pod being replaced, or
	// one restarted in place, still needs the ConfigMap its spec names.
	if clientAgentRolledOut(ds) {
		if e := client.IgnoreNotFound(r.Delete(ctx, telemetryConfig)); e != nil {
			return 0, 0, e
		}
		if e := r.pruneClientAgentConfigs(ctx, sys, ns, configName); e != nil {
			return 0, 0, e
		}
	}
	if telemetry {
		if _, e := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
			labelManaged(&svc.ObjectMeta, sys, "")
			svc.Spec.ClusterIP = corev1.ClusterIPNone
			svc.Spec.Selector = labels
			svc.Spec.Ports = []corev1.ServicePort{{Name: clientAgentMetricsPortName, Port: port, TargetPort: intstr.FromString(clientAgentMetricsPortName)}}
			return controllerutil.SetControllerReference(sys, svc, r.Scheme)
		}); e != nil {
			return 0, 0, e
		}
	}
	return ds.Status.NumberReady, ds.Status.DesiredNumberScheduled, nil
}

// setClientAgentCondition writes ConditionClientAgent from the DaemonSet state.
func (r *DaosSystemReconciler) setClientAgentCondition(ctx context.Context, sys *daosv1alpha1.DaosSystem, ns string, status *daosv1alpha1.DaosSystemStatus) error {
	ready, desired, err := r.ensureClientAgent(ctx, sys, ns)
	if err != nil {
		return err
	}
	switch {
	case !sys.Spec.ClientAgent.Enabled:
		setCond(status, daosv1alpha1.ConditionClientAgent, metav1.ConditionFalse, "Disabled", "spec.clientAgent.enabled is false")
	case !clientAgentHostNetwork(sys) && !providerIsTCP(sys.Spec.Provider):
		// lane A 2026-11-18: in the pod network the agent sees eth0 and lo only,
		// finds no interface for the RDMA provider and fails every GetAttachInfo;
		// a host-network agent instead hands the pod "ib0", which the pod does not
		// have (na_ofi: no provider found). Say so here rather than leave the
		// DaemonSet Ready and every client failing.
		setCond(status, daosv1alpha1.ConditionClientAgent, metav1.ConditionFalse, "PodNetworkNeedsTCP",
			fmt.Sprintf("spec.clientAgent.hostNetwork is false but the provider is %q: the pod network has no RDMA interface, so the agent finds none to hand out; "+
				"use hostNetwork true with hostNetwork clients, or a TCP provider", providerOrDefault(sys.Spec.Provider)))
	case desired == 0:
		setCond(status, daosv1alpha1.ConditionClientAgent, metav1.ConditionFalse, "NoNodes", "no node matches spec.clientAgent.nodeSelector")
	case ready < desired:
		setCond(status, daosv1alpha1.ConditionClientAgent, metav1.ConditionFalse, "Starting", fmt.Sprintf("%d/%d node agent(s) ready", ready, desired))
	default:
		setCond(status, daosv1alpha1.ConditionClientAgent, metav1.ConditionTrue, "Ready",
			fmt.Sprintf("%d node agent(s), %s; clients mount hostPath %s as %s", ready,
				map[bool]string{true: "host network", false: "pod network"}[clientAgentHostNetwork(sys)], clientAgentSocketDir(sys), agentSocketDir))
	}
	return nil
}

// providerOrDefault is spec.provider with the CRD default filled in.
func providerOrDefault(p string) string {
	if p == "" {
		return "ofi+verbs;ofi_rxm"
	}
	return p
}

// providerIsTCP reports whether the fabric provider runs over plain TCP/IP
// (ofi+tcp, ofi+tcp;ofi_rxm, ucx+tcp), the only kind a pod-network agent can serve.
func providerIsTCP(p string) bool {
	return strings.Contains(providerOrDefault(p), "tcp")
}
