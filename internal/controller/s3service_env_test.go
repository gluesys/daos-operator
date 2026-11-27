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
	"testing"

	corev1 "k8s.io/api/core/v1"

	daosv1alpha1 "github.com/gluesys/daos-operator/api/v1alpha1"
)

// lastEnv returns the value the container sees for name: the last entry wins.
func lastEnv(env []corev1.EnvVar, name string) (string, bool) {
	v, ok := "", false
	for _, e := range env {
		if e.Name == name {
			v, ok = e.Value, true
		}
	}
	return v, ok
}

func TestS3EnvSSECOverHTTP(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tls    string
		extra  []corev1.EnvVar
		want   string
		wantOK bool
	}{
		{name: "http gateway lifts the SSE-C TLS enforcement", want: "true", wantOK: true},
		{name: "https gateway keeps it", tls: "gw-tls"},
		{name: "extraEnv overrides", extra: []corev1.EnvVar{{Name: sseCOverHTTPEnv, Value: "false"}},
			want: "false", wantOK: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &daosv1alpha1.S3Service{Spec: daosv1alpha1.S3ServiceSpec{
				RootCredentialsSecret: "root", TLSSecretName: tc.tls, ExtraEnv: tc.extra}}
			got, ok := lastEnv(s3Env(svc), sseCOverHTTPEnv)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("%s = %q (set %v), want %q (set %v)", sseCOverHTTPEnv, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
