// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Gluesys Co., Ltd.

// Package webhookcert keeps the operator's webhook serving certificate without
// cert-manager: a private CA and a serving certificate for the webhook
// Service, stored in a Secret so every replica and restart serves the same
// one, written to the webhook server's cert directory, and the CA put into the
// MutatingWebhookConfiguration's caBundle. Renewal happens when the operator
// starts or re-checks and finds the certificate within RenewBefore of expiry.
package webhookcert

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	keyCA     = "ca.crt"
	keyPrevCA = "ca-previous.crt"
	keyCert   = "tls.crt"
	keyKey    = "tls.key"

	// Validity of a freshly generated CA and serving certificate.
	Validity = 5 * 365 * 24 * time.Hour
	// RenewBefore regenerates a certificate this close to expiry.
	RenewBefore = 30 * 24 * time.Hour
)

// Options names the objects the certificate is for.
type Options struct {
	Namespace     string // the operator's namespace (Secret and Service live here)
	SecretName    string
	ServiceName   string
	WebhookConfig string // MutatingWebhookConfiguration name
	CertDir       string // where the webhook server reads tls.crt / tls.key
}

// DNSNames are the names the API server may use for the Service.
func (o Options) DNSNames() []string {
	s, ns := o.ServiceName, o.Namespace
	return []string{s, s + "." + ns, s + "." + ns + ".svc", s + "." + ns + ".svc.cluster.local"}
}

// Ensure makes sure the Secret holds a valid certificate for the Service,
// writes it to CertDir and patches the webhook configuration's caBundle.
// c must not be a cached client: it runs before the manager starts.
func Ensure(ctx context.Context, c client.Client, o Options, now time.Time) error {
	var sec corev1.Secret
	key := types.NamespacedName{Namespace: o.Namespace, Name: o.SecretName}
	err := c.Get(ctx, key, &sec)
	switch {
	case apierrors.IsNotFound(err):
		data, gerr := Generate(o.DNSNames(), now)
		if gerr != nil {
			return gerr
		}
		sec = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: o.Namespace, Name: o.SecretName},
			Type:       corev1.SecretTypeTLS,
			Data:       data,
		}
		if cerr := c.Create(ctx, &sec); cerr != nil {
			if !apierrors.IsAlreadyExists(cerr) {
				return fmt.Errorf("create %s: %w", key, cerr)
			}
			// another replica won the race: use its certificate
			if gerr := c.Get(ctx, key, &sec); gerr != nil {
				return gerr
			}
		}
	case err != nil:
		return fmt.Errorf("get %s: %w", key, err)
	default:
		if verr := Verify(sec.Data, o.DNSNames(), now); verr != nil {
			data, gerr := Generate(o.DNSNames(), now)
			if gerr != nil {
				return gerr
			}
			// Keep trusting the old CA for a while: other replicas serve the old
			// certificate until their own re-check reloads the Secret.
			if old, perr := parse(sec.Data[keyCA]); perr == nil && now.Before(old.NotAfter) {
				data[keyPrevCA] = sec.Data[keyCA]
			}
			sec.Data = data
			if uerr := c.Update(ctx, &sec); uerr != nil {
				return fmt.Errorf("renew %s (%v): %w", key, verr, uerr)
			}
		}
	}
	if err := writeFiles(o.CertDir, sec.Data); err != nil {
		return err
	}
	return PatchCABundle(ctx, c, o.WebhookConfig, caBundle(sec.Data, now))
}

// caBundle is the current CA plus the previous one while it is still valid.
func caBundle(data map[string][]byte, now time.Time) []byte {
	out := append([]byte{}, data[keyCA]...)
	if prev, err := parse(data[keyPrevCA]); err == nil && now.Before(prev.NotAfter) {
		out = append(out, data[keyPrevCA]...)
	}
	return out
}

// PatchCABundle sets caBundle on every webhook of the configuration. A
// missing configuration is not an error (the chart may not render it).
func PatchCABundle(ctx context.Context, c client.Client, name string, ca []byte) error {
	var mwc admissionregistrationv1.MutatingWebhookConfiguration
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &mwc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get MutatingWebhookConfiguration %s: %w", name, err)
	}
	changed := false
	for i := range mwc.Webhooks {
		if !bytes.Equal(mwc.Webhooks[i].ClientConfig.CABundle, ca) {
			mwc.Webhooks[i].ClientConfig.CABundle = ca
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return c.Update(ctx, &mwc)
}

// Generate creates a CA and a serving certificate for dnsNames.
func Generate(dnsNames []string, now time.Time) (map[string][]byte, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "daos-operator webhook CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(Validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(Validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{
		keyCA:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		keyCert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		keyKey:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// Verify checks that data holds a serving certificate for dnsNames, signed by
// its CA and not within RenewBefore of expiry.
func Verify(data map[string][]byte, dnsNames []string, now time.Time) error {
	ca, err := parse(data[keyCA])
	if err != nil {
		return fmt.Errorf("ca: %w", err)
	}
	leaf, err := parse(data[keyCert])
	if err != nil {
		return fmt.Errorf("cert: %w", err)
	}
	if len(data[keyKey]) == 0 {
		return errors.New("no key")
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	for _, n := range dnsNames {
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: n, Roots: pool, CurrentTime: now}); err != nil {
			return err
		}
	}
	for _, c := range []*x509.Certificate{ca, leaf} {
		if now.Add(RenewBefore).After(c.NotAfter) {
			return fmt.Errorf("expires %s", c.NotAfter.Format(time.RFC3339))
		}
	}
	if !slices.Contains(leaf.DNSNames, dnsNames[0]) {
		return errors.New("wrong service name")
	}
	return nil
}

func writeFiles(dir string, data map[string][]byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, k := range []string{keyCert, keyKey} {
		if err := os.WriteFile(filepath.Join(dir, k), data[k], 0o600); err != nil {
			return err
		}
	}
	return nil
}

func parse(p []byte) (*x509.Certificate, error) {
	b, _ := pem.Decode(p)
	if b == nil {
		return nil, errors.New("no PEM block")
	}
	return x509.ParseCertificate(b.Bytes)
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}
