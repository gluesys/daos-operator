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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	daosv1alpha1 "gitlab.gluesys.com/exastor/daos-operator/api/v1alpha1"
	"gitlab.gluesys.com/exastor/daos-operator/internal/certs"
)

// Transport certificates (#16). With spec.allowInsecure=false DAOS requires a
// CA plus server/agent/admin certificates on every component. The operator
// generates them once into Secret <sys>-certs (delete the Secret to rotate;
// a full-stop restart is then needed, which is the upgrade procedure's job)
// and mounts the right subset into servers, the agent sidecar and dmg Jobs:
//
//	server: daosCA.crt server.crt server.key clients/agent.crt clients/admin.crt
//	agent:  daosCA.crt agent.crt agent.key
//	admin:  daosCA.crt admin.crt admin.key
//
// DAOS refuses keys that are group/world readable, so key files get mode 0400.

const certsMountPath = "/etc/daos/certs"

// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch

func certsSecretName(sys *daosv1alpha1.DaosSystem) string { return sys.Name + "-certs" }

// certsNeeded is true when TLS is on.
func certsNeeded(sys *daosv1alpha1.DaosSystem) bool { return !sys.Spec.AllowInsecure }

// ensureCerts creates the Secret when TLS is on and it does not exist yet, and
// verifies an existing one. It returns the Secret name to mount ("" for insecure).
func (r *DaosSystemReconciler) ensureCerts(ctx context.Context, sys *daosv1alpha1.DaosSystem, ns string, status *daosv1alpha1.DaosSystemStatus) (string, error) {
	if !certsNeeded(sys) {
		setCond(status, daosv1alpha1.ConditionCertificates, metav1.ConditionFalse, "Insecure", "spec.allowInsecure=true: no transport certificates (Phase 0 only)")
		return "", nil
	}
	name := certsSecretName(sys)
	sec := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, sec)
	switch {
	case apierrors.IsNotFound(err):
		bundle, err := certs.Generate(time.Now())
		if err != nil {
			return "", fmt.Errorf("generate certificates: %w", err)
		}
		sec = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte(bundle)}
		labelManaged(&sec.ObjectMeta, sys, "")
		if err := controllerutil.SetControllerReference(sys, sec, r.Scheme); err != nil {
			return "", err
		}
		if err := r.Create(ctx, sec); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("create %s: %w", name, err)
		}
		r.event(sys, corev1.EventTypeNormal, "CertificatesGenerated", "transport certificates generated into Secret "+name)
		setCond(status, daosv1alpha1.ConditionCertificates, metav1.ConditionTrue, "Generated", "Secret "+name+" created (CA, server, agent, admin)")
		return name, nil
	case err != nil:
		return "", err
	}
	exp, verr := certs.Verify(certs.Bundle(sec.Data), time.Now())
	if verr != nil {
		// a user-supplied or damaged Secret: say so, never overwrite it
		setCond(status, daosv1alpha1.ConditionCertificates, metav1.ConditionFalse, "Invalid", fmt.Sprintf("Secret %s: %v (fix it or delete it to regenerate)", name, verr))
		return name, nil
	}
	reason, msg := "Valid", fmt.Sprintf("Secret %s valid until %s", name, exp.UTC().Format(time.RFC3339))
	if time.Until(exp) < 30*24*time.Hour {
		reason = "ExpiringSoon"
	}
	setCond(status, daosv1alpha1.ConditionCertificates, metav1.ConditionTrue, reason, msg)
	return name, nil
}

// certsVolume returns a Secret volume projecting the given files (path -> key).
func certsVolume(secret string, files map[string]string) corev1.Volume {
	items := make([]corev1.KeyToPath, 0, len(files))
	for path, key := range files {
		mode := ptr.To(int32(0o444))
		if len(key) > 4 && key[len(key)-4:] == ".key" {
			mode = ptr.To(int32(0o400))
		}
		items = append(items, corev1.KeyToPath{Key: key, Path: path, Mode: mode})
	}
	sortKeyToPath(items)
	return corev1.Volume{Name: "certs", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secret, Items: items, DefaultMode: ptr.To(int32(0o444))}}}
}

func serverCertFiles() map[string]string {
	return map[string]string{
		certs.KeyCA: certs.KeyCA, certs.KeyServerCrt: certs.KeyServerCrt, certs.KeyServerKey: certs.KeyServerKey,
		"clients/" + certs.KeyAgentCrt: certs.KeyAgentCrt, "clients/" + certs.KeyAdminCrt: certs.KeyAdminCrt,
	}
}

func agentCertFiles() map[string]string {
	return map[string]string{certs.KeyCA: certs.KeyCA, certs.KeyAgentCrt: certs.KeyAgentCrt, certs.KeyAgentKey: certs.KeyAgentKey}
}

func adminCertFiles() map[string]string {
	return map[string]string{certs.KeyCA: certs.KeyCA, certs.KeyAdminCrt: certs.KeyAdminCrt, certs.KeyAdminKey: certs.KeyAdminKey}
}

func sortKeyToPath(items []corev1.KeyToPath) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j-1].Path > items[j].Path; j-- {
			items[j-1], items[j] = items[j], items[j-1]
		}
	}
}
