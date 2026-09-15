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

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	daosv1alpha1 "gitlab.gluesys.com/exastor/daos-operator/api/v1alpha1"
)

func newApp(t *testing.T, input string, yes bool, objs ...runtime.Object) (*app, *bytes.Buffer) {
	s := runtime.NewScheme()
	if err := daosv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	c := fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(objs...).WithStatusSubresource(&daosv1alpha1.DaosSystem{}, &daosv1alpha1.DaosPool{}, &daosv1alpha1.DaosContainer{}).Build()
	return &app{c: c, in: strings.NewReader(input), out: out, yes: yes, ns: "default"}, out
}

func sysPending() *daosv1alpha1.DaosSystem {
	return &daosv1alpha1.DaosSystem{ObjectMeta: metav1.ObjectMeta{Name: "d1"},
		Spec: daosv1alpha1.DaosSystemSpec{Version: "2.8.0", Images: daosv1alpha1.ImagesSpec{Server: "srv:1"}},
		Status: daosv1alpha1.DaosSystemStatus{PendingFormat: true, NodeConfigs: []daosv1alpha1.NodeConfigStatus{{Node: "n1", FabricIface: "ens2", BdevCount: 4, Ready: true}},
			Conditions: []metav1.Condition{{Type: daosv1alpha1.ConditionFormatted, Status: metav1.ConditionFalse, Reason: "AwaitingApproval", Message: "not formatted"}}}}
}

func TestSystemFormatNeedsYes(t *testing.T) {
	ctx := context.Background()
	a, out := newApp(t, "no\n", false, sysPending())
	if err := a.run(ctx, []string{"system", "format", "d1"}); err == nil || err.Error() != "aborted" {
		t.Fatalf("expected abort, got %v", err)
	}
	if !strings.Contains(out.String(), "ERASES") || !strings.Contains(out.String(), "n1") {
		t.Errorf("prompt must list drives: %s", out.String())
	}
	sys := &daosv1alpha1.DaosSystem{}
	_ = a.c.Get(ctx, types.NamespacedName{Name: "d1"}, sys)
	if sys.Annotations[daosv1alpha1.AnnotationFormatApproved] != "" {
		t.Fatal("no annotation without yes")
	}

	a, _ = newApp(t, "yes\n", false, sysPending())
	if err := a.run(ctx, []string{"system", "format", "d1"}); err != nil {
		t.Fatal(err)
	}
	_ = a.c.Get(ctx, types.NamespacedName{Name: "d1"}, sys)
	if sys.Annotations[daosv1alpha1.AnnotationFormatApproved] != "true" {
		t.Fatal("annotation missing after yes")
	}
}

func TestSystemFormatRefusesWhenNotPending(t *testing.T) {
	s := sysPending()
	s.Status.PendingFormat = false
	a, _ := newApp(t, "", true, s)
	if err := a.run(context.Background(), []string{"system", "format", "d1"}); err == nil || !strings.Contains(err.Error(), "pendingFormat") {
		t.Fatalf("expected refusal, got %v", err)
	}
	s = sysPending()
	s.Status.Formatted, s.Status.PendingFormat = true, false
	a, _ = newApp(t, "", true, s)
	if err := a.run(context.Background(), []string{"system", "format", "d1"}); err == nil || !strings.Contains(err.Error(), "already formatted") {
		t.Fatalf("expected already formatted, got %v", err)
	}
}

func TestSystemUpgradeSetsImageAndApproval(t *testing.T) {
	ctx := context.Background()
	a, out := newApp(t, "", true, sysPending())
	if err := a.run(ctx, []string{"system", "upgrade", "d1", "--image", "srv:2", "--version", "2.8.1"}); err != nil {
		t.Fatal(err)
	}
	sys := &daosv1alpha1.DaosSystem{}
	_ = a.c.Get(ctx, types.NamespacedName{Name: "d1"}, sys)
	if !sys.Spec.Upgrade.Approved || sys.Spec.Images.Server != "srv:2" || sys.Spec.Version != "2.8.1" {
		t.Fatalf("%+v", sys.Spec)
	}
	if !strings.Contains(out.String(), "FULL-STOP") || !strings.Contains(out.String(), "drained") {
		t.Errorf("prompt: %s", out.String())
	}
}

func TestSystemCerts(t *testing.T) {
	ctx := context.Background()
	s := sysPending()
	s.Spec.AllowInsecure = true
	a, _ := newApp(t, "", true, s)
	if err := a.run(ctx, []string{"system", "certs", "d1"}); err == nil || !strings.Contains(err.Error(), "allowInsecure") {
		t.Fatalf("insecure system: %v", err)
	}
	s = sysPending()
	s.Status.Certificates = &daosv1alpha1.CertificatesStatus{SecretName: "d1-certs", NotAfter: &metav1.Time{Time: time.Now().Add(240 * time.Hour)}}
	a, out := newApp(t, "", true, s)
	if err := a.run(ctx, []string{"system", "certs", "d1"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "FULL-STOP") || !strings.Contains(out.String(), "d1-certs-previous") {
		t.Errorf("prompt must explain the outage and the backup: %s", out.String())
	}
	sys := &daosv1alpha1.DaosSystem{}
	_ = a.c.Get(ctx, types.NamespacedName{Name: "d1"}, sys)
	if sys.Annotations[daosv1alpha1.AnnotationCertsRenewApproved] != "true" {
		t.Fatal("annotation missing")
	}
}

func TestPoolAndContainerDestroy(t *testing.T) {
	ctx := context.Background()
	pool := &daosv1alpha1.DaosPool{ObjectMeta: metav1.ObjectMeta{Name: "p1"}, Status: daosv1alpha1.DaosPoolStatus{UUID: "u", TotalBytes: 10 << 30, FreeBytes: 4 << 30}}
	cont := &daosv1alpha1.DaosContainer{ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "default"}, Spec: daosv1alpha1.DaosContainerSpec{PoolRef: "p1", Label: "c1"}}
	a, out := newApp(t, "", true, pool, cont)
	if err := a.run(ctx, []string{"pool", "destroy", "p1"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "6.0 GiB used of 10.0 GiB") {
		t.Errorf("usage line: %s", out.String())
	}
	// fake client has no finalizer semantics: the object is simply gone, which is what Delete asked for
	err := a.c.Get(ctx, types.NamespacedName{Name: "p1"}, &daosv1alpha1.DaosPool{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("pool should be deleted: %v", err)
	}
	if err := a.run(ctx, []string{"cont", "destroy", "c1"}); err != nil {
		t.Fatal(err)
	}
	if err := a.c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "c1"}, &daosv1alpha1.DaosContainer{}); !apierrors.IsNotFound(err) {
		t.Fatalf("container should be deleted: %v", err)
	}
}

func TestStatusShowsDecisions(t *testing.T) {
	s := sysPending()
	s.Status.Upgrade = &daosv1alpha1.UpgradeStatus{Phase: "Pending", FromImage: "srv:1", ToImage: "srv:2"}
	a, out := newApp(t, "", true, s)
	if err := a.run(context.Background(), []string{"system", "status", "d1"}); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "DECISION PENDING") != 2 {
		t.Errorf("both decisions must be surfaced:\n%s", out.String())
	}
}
