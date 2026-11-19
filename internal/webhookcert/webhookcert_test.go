// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Gluesys Co., Ltd.

package webhookcert

import (
	"bytes"
	"context"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

var t0 = time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)

func opts(t *testing.T) Options {
	return Options{Namespace: "daos-system", SecretName: "wh-cert", ServiceName: "wh",
		WebhookConfig: "wh-config", CertDir: t.TempDir()}
}

func fakeClient(t *testing.T, objs ...client.Object) client.Client {
	return fakeBuilder(t, objs...).Build()
}

func fakeBuilder(t *testing.T, objs ...client.Object) *fake.ClientBuilder {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...)
}

func webhookConfig() *admissionregistrationv1.MutatingWebhookConfiguration {
	return &admissionregistrationv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "wh-config"},
		Webhooks:   []admissionregistrationv1.MutatingWebhook{{Name: "a.daos.gluesys.com"}, {Name: "b.daos.gluesys.com"}},
	}
}

func TestGenerateVerify(t *testing.T) {
	o := opts(t)
	data, err := Generate(o.DNSNames(), t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(data, o.DNSNames(), t0); err != nil {
		t.Fatalf("fresh certificate: %v", err)
	}
	if err := Verify(data, []string{"other.daos-system.svc"}, t0); err == nil {
		t.Fatalf("certificate accepted for another service")
	}
	if err := Verify(data, o.DNSNames(), t0.Add(Validity-RenewBefore+time.Hour)); err == nil {
		t.Fatalf("certificate inside the renewal window accepted")
	}
}

func TestEnsureCreatesWritesAndPatches(t *testing.T) {
	o := opts(t)
	c := fakeClient(t, webhookConfig())
	ctx := context.Background()
	if err := Ensure(ctx, c, o, t0); err != nil {
		t.Fatal(err)
	}
	var sec corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: o.Namespace, Name: o.SecretName}, &sec); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{keyCert, keyKey} {
		b, err := os.ReadFile(filepath.Join(o.CertDir, k))
		if err != nil || !bytes.Equal(b, sec.Data[k]) {
			t.Fatalf("%s not written from the Secret: %v", k, err)
		}
	}
	var mwc admissionregistrationv1.MutatingWebhookConfiguration
	if err := c.Get(ctx, types.NamespacedName{Name: o.WebhookConfig}, &mwc); err != nil {
		t.Fatal(err)
	}
	for _, w := range mwc.Webhooks {
		if !bytes.Equal(w.ClientConfig.CABundle, sec.Data[keyCA]) {
			t.Fatalf("webhook %s caBundle not set", w.Name)
		}
	}

	// a second run (restart, another replica) keeps the same certificate
	first, firstCA := sec.Data[keyCert], sec.Data[keyCA]
	if err := Ensure(ctx, c, o, t0.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: o.Namespace, Name: o.SecretName}, &sec); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, sec.Data[keyCert]) {
		t.Fatalf("valid certificate regenerated")
	}

	// close to expiry it is renewed and the new CA reaches the configuration
	if err := Ensure(ctx, c, o, t0.Add(Validity-RenewBefore+time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: o.Namespace, Name: o.SecretName}, &sec); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, sec.Data[keyCert]) {
		t.Fatalf("expiring certificate not renewed")
	}
	if err := c.Get(ctx, types.NamespacedName{Name: o.WebhookConfig}, &mwc); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(mwc.Webhooks[0].ClientConfig.CABundle, sec.Data[keyCA]) {
		t.Fatalf("renewed CA not patched in")
	}
	// the old CA stays trusted so replicas still serving the old certificate keep working
	if !bytes.Contains(mwc.Webhooks[0].ClientConfig.CABundle, firstCA) {
		t.Fatalf("previous CA dropped from caBundle on renewal")
	}
}

func TestEnsureWithoutWebhookConfig(t *testing.T) {
	if err := Ensure(context.Background(), fakeClient(t), opts(t), t0); err != nil {
		t.Fatalf("a missing configuration must not fail start-up: %v", err)
	}
}

func TestLateRecheckKeepsRenewedCA(t *testing.T) {
	// Replica A re-checks a still-valid certificate and is slow; replica B
	// renews in between. A must not write its older bundle over B's.
	for _, at := range []string{"after A read the Secret", "before A updates the configuration"} {
		t.Run(at, func(t *testing.T) { lateRecheck(t, at) })
	}
}

func lateRecheck(t *testing.T, at string) {
	o := opts(t)
	ctx := context.Background()
	base := fakeClient(t, webhookConfig())
	if err := Ensure(ctx, base, o, t0); err != nil {
		t.Fatal(err)
	}
	var sec corev1.Secret
	secKey := types.NamespacedName{Namespace: o.Namespace, Name: o.SecretName}
	if err := base.Get(ctx, secKey, &sec); err != nil {
		t.Fatal(err)
	}
	firstCA := sec.Data[keyCA]

	renewAt := t0.Add(Validity - RenewBefore + time.Hour)
	renewed := false
	renewB := func(ctx context.Context) {
		if renewed {
			return
		}
		renewed = true
		if err := Ensure(ctx, base, o, renewAt); err != nil {
			t.Errorf("replica B renewal: %v", err)
		}
	}
	replicaA := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if _, ok := obj.(*corev1.Secret); ok && at == "after A read the Secret" {
				renewB(ctx)
			}
			return nil
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*admissionregistrationv1.MutatingWebhookConfiguration); ok && at == "before A updates the configuration" {
				renewB(ctx)
			}
			return c.Update(ctx, obj, opts...)
		},
	})
	// A's own view: the certificate is valid but A has a reason to write the
	// bundle (the chart recreated the configuration without caBundle)
	var mwc admissionregistrationv1.MutatingWebhookConfiguration
	if err := base.Get(ctx, types.NamespacedName{Name: o.WebhookConfig}, &mwc); err != nil {
		t.Fatal(err)
	}
	for i := range mwc.Webhooks {
		mwc.Webhooks[i].ClientConfig.CABundle = nil
	}
	if err := base.Update(ctx, &mwc); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(ctx, replicaA, o, renewAt.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !renewed {
		t.Fatal("the race was not exercised")
	}
	if err := base.Get(ctx, secKey, &sec); err != nil {
		t.Fatal(err)
	}
	if err := base.Get(ctx, types.NamespacedName{Name: o.WebhookConfig}, &mwc); err != nil {
		t.Fatal(err)
	}
	leaf, err := parse(sec.Data[keyCert])
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range mwc.Webhooks {
		b := w.ClientConfig.CABundle
		if !bytes.Contains(b, sec.Data[keyCA]) || !bytes.Contains(b, firstCA) {
			t.Fatalf("webhook %s: caBundle lost the renewed or the previous CA", w.Name)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(b) {
			t.Fatal("caBundle not PEM")
		}
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: o.DNSNames()[2], Roots: pool, CurrentTime: renewAt}); err != nil {
			t.Fatalf("renewed certificate not trusted by caBundle: %v", err)
		}
	}
}
