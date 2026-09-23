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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// S3IAMType selects where the gateway keeps S3 users and keys.
// +kubebuilder:validation:Enum=internal;external
type S3IAMType string

const (
	// S3IAMInternal is versitygw's flat-file store under --iam-dir.
	S3IAMInternal S3IAMType = "internal"
	// S3IAMExternal means the gateway is pointed at LDAP/Vault/S3 through
	// Spec.ExtraEnv; the operator then leaves replicas alone.
	S3IAMExternal S3IAMType = "external"
)

// S3IAMSpec says who owns the identity data. It matters because losing it
// loses every S3 user and key, even though the objects in DAOS are untouched
// (ADR-004).
type S3IAMSpec struct {
	// +kubebuilder:default=internal
	Type S3IAMType `json:"type,omitempty"`
	// ClaimName is an existing PVC for the internal store. Without it the
	// store is an emptyDir and does not survive the pod; the operator says so
	// in status and refuses more than one replica.
	ClaimName string `json:"claimName,omitempty"`
}

// S3ServiceSpec describes one versitygw-daos gateway in front of a DaosPool.
// The data path is libdfs, so the pod needs no PV and no dfuse (ADR-004).
type S3ServiceSpec struct {
	// PoolRef names the DaosPool whose containers become S3 buckets. The pool
	// carries the system name, so the gateway needs no other DAOS coordinates.
	PoolRef string `json:"poolRef"`
	// Image is the versitygw-daos image (exastor/daos-images).
	Image string `json:"image"`
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	Replicas int32 `json:"replicas,omitempty"`
	// +kubebuilder:default=7070
	Port int32 `json:"port,omitempty"`
	// +kubebuilder:default="us-east-1"
	Region string `json:"region,omitempty"`
	// RootCredentialsSecret holds the root access key: keys `accessKey` and
	// `secretKey`. Credentials never appear in the CR itself.
	RootCredentialsSecret string `json:"rootCredentialsSecret"`
	// ContainerCacheSize is versitygw's open-container handle cache
	// (`--container-cache`). Unset leaves the gateway default.
	ContainerCacheSize int32     `json:"containerCacheSize,omitempty"`
	IAM                S3IAMSpec `json:"iam,omitempty"`
	// ServiceType is the Service the operator creates for the gateway.
	// +kubebuilder:default=ClusterIP
	// +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
	ServiceType corev1.ServiceType `json:"serviceType,omitempty"`
	// NodePort is only read when ServiceType is NodePort.
	NodePort int32 `json:"nodePort,omitempty"`
	// TLSSecretName is a kubernetes.io/tls Secret served by the gateway.
	TLSSecretName string                      `json:"tlsSecretName,omitempty"`
	Resources     corev1.ResourceRequirements `json:"resources,omitempty"`
	NodeSelector  map[string]string           `json:"nodeSelector,omitempty"`
	Tolerations   []corev1.Toleration         `json:"tolerations,omitempty"`
	// ExtraEnv is how external IAM and anything else versitygw takes from the
	// environment is configured, without growing a field per flag.
	ExtraEnv []corev1.EnvVar `json:"extraEnv,omitempty"`
}

// S3ServiceStatus reports what clients can reach and what is at risk.
type S3ServiceStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Endpoint is the in-cluster address, plus the node port when there is one.
	Endpoint           string `json:"endpoint,omitempty"`
	ReadyReplicas      int32  `json:"readyReplicas,omitempty"`
	PoolUUID           string `json:"poolUUID,omitempty"`
	SystemName         string `json:"systemName,omitempty"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=s3svc
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolRef`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// S3Service is an S3 endpoint served by versitygw-daos over one DAOS pool.
type S3Service struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   S3ServiceSpec   `json:"spec,omitempty"`
	Status S3ServiceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// S3ServiceList contains a list of S3Service.
type S3ServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []S3Service `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &S3Service{}, &S3ServiceList{})
		return nil
	})
}
