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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	daosv1alpha1 "gitlab.gluesys.com/exastor/daos-operator/api/v1alpha1"
)

// Telemetry (#12): daos_server exposes Prometheus metrics on telemetry_port
// (engine_* series the official DAOS Grafana dashboard reads). The operator
// renders the port, fronts the server pods with a headless Service and, when
// the prometheus-operator CRD exists, a ServiceMonitor. The ServiceMonitor is
// built as an unstructured object so the operator has no build dependency on
// prometheus-operator and works on clusters without it.

const (
	DefaultTelemetryPort  = int32(9191)
	defaultScrapeInterval = "15s"
)

var serviceMonitorGVK = schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor"}

// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;create;update;patch;delete

func telemetryEnabled(sys *daosv1alpha1.DaosSystem) bool {
	return sys.Spec.Telemetry.Enabled == nil || *sys.Spec.Telemetry.Enabled
}

func telemetryPort(sys *daosv1alpha1.DaosSystem) int32 {
	if !telemetryEnabled(sys) {
		return 0
	}
	if sys.Spec.Telemetry.Port > 0 {
		return sys.Spec.Telemetry.Port
	}
	return DefaultTelemetryPort
}

func serviceMonitorWanted(sys *daosv1alpha1.DaosSystem) bool {
	return sys.Spec.Telemetry.ServiceMonitor == nil || *sys.Spec.Telemetry.ServiceMonitor
}

func metricsServiceName(sys *daosv1alpha1.DaosSystem) string { return sys.Name + "-metrics" }

// serviceMonitorCRDPresent asks the RESTMapper whether monitoring.coreos.com/v1 ServiceMonitor is served.
func (r *DaosSystemReconciler) serviceMonitorCRDPresent() bool {
	if r.RESTMapper() == nil {
		return false
	}
	_, err := r.RESTMapper().RESTMapping(serviceMonitorGVK.GroupKind(), serviceMonitorGVK.Version)
	return err == nil
}

// ensureTelemetry creates/updates (or removes) the metrics Service and ServiceMonitor
// and sets the Telemetry condition.
func (r *DaosSystemReconciler) ensureTelemetry(ctx context.Context, sys *daosv1alpha1.DaosSystem, ns string, status *daosv1alpha1.DaosSystemStatus) error {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: metricsServiceName(sys), Namespace: ns}}
	sm := &unstructured.Unstructured{}
	sm.SetGroupVersionKind(serviceMonitorGVK)
	sm.SetName(metricsServiceName(sys))
	sm.SetNamespace(ns)

	if !telemetryEnabled(sys) || !serverEnabled(sys) {
		if err := client.IgnoreNotFound(r.Delete(ctx, svc)); err != nil {
			return err
		}
		if r.serviceMonitorCRDPresent() {
			if err := client.IgnoreNotFound(r.Delete(ctx, sm)); err != nil {
				return err
			}
		}
		setCond(status, daosv1alpha1.ConditionTelemetry, metav1.ConditionFalse, "Disabled", "telemetry disabled or no server workloads")
		return nil
	}
	port := telemetryPort(sys)
	selector := map[string]string{daosv1alpha1.LabelSystem: sys.Name, daosv1alpha1.LabelRole: "server"}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		labelManaged(&svc.ObjectMeta, sys, "")
		svc.Labels["app.kubernetes.io/name"] = "daos-server"
		svc.Spec.ClusterIP = corev1.ClusterIPNone // headless: one endpoint per engine host (hostNetwork)
		svc.Spec.Selector = selector
		svc.Spec.Ports = []corev1.ServicePort{{Name: "metrics", Port: port, TargetPort: intstr.FromInt32(port), Protocol: corev1.ProtocolTCP}}
		return controllerutil.SetControllerReference(sys, svc, r.Scheme)
	}); err != nil {
		return fmt.Errorf("metrics service: %w", err)
	}

	if !serviceMonitorWanted(sys) {
		if r.serviceMonitorCRDPresent() {
			if err := client.IgnoreNotFound(r.Delete(ctx, sm)); err != nil {
				return err
			}
		}
		setCond(status, daosv1alpha1.ConditionTelemetry, metav1.ConditionTrue, "ServiceOnly", fmt.Sprintf("Service %s:%d; ServiceMonitor disabled by spec", metricsServiceName(sys), port))
		return nil
	}
	if !r.serviceMonitorCRDPresent() {
		setCond(status, daosv1alpha1.ConditionTelemetry, metav1.ConditionTrue, "NoPrometheusOperator",
			fmt.Sprintf("Service %s:%d created; monitoring.coreos.com/v1 ServiceMonitor CRD not installed, scrape the Service yourself", metricsServiceName(sys), port))
		return nil
	}
	interval := sys.Spec.Telemetry.Interval
	if interval == "" {
		interval = defaultScrapeInterval
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sm, func() error {
		labels := map[string]string{"app.kubernetes.io/managed-by": "daos-operator", daosv1alpha1.LabelSystem: sys.Name}
		for k, v := range sys.Spec.Telemetry.ServiceMonitorLabels {
			labels[k] = v
		}
		sm.SetLabels(labels)
		spec := map[string]any{
			"selector":          map[string]any{"matchLabels": map[string]any{daosv1alpha1.LabelSystem: sys.Name, daosv1alpha1.LabelRole: "server"}},
			"namespaceSelector": map[string]any{"matchNames": []any{ns}},
			"endpoints": []any{map[string]any{
				"port":     "metrics",
				"interval": interval,
				"path":     "/metrics",
				// hostNetwork pods: keep the node name as a label for the dashboard
				"relabelings": []any{map[string]any{"sourceLabels": []any{"__meta_kubernetes_pod_node_name"}, "targetLabel": "node"}},
			}},
		}
		if err := unstructured.SetNestedField(sm.Object, spec, "spec"); err != nil {
			return err
		}
		return controllerutil.SetControllerReference(sys, sm, r.Scheme)
	}); err != nil {
		return fmt.Errorf("servicemonitor: %w", err)
	}
	setCond(status, daosv1alpha1.ConditionTelemetry, metav1.ConditionTrue, "ServiceMonitor", fmt.Sprintf("Service %s:%d and ServiceMonitor every %s", metricsServiceName(sys), port, interval))
	return nil
}

// telemetryCond is a helper for tests.
func telemetryCond(st *daosv1alpha1.DaosSystemStatus) *metav1.Condition {
	return meta.FindStatusCondition(st.Conditions, daosv1alpha1.ConditionTelemetry)
}
