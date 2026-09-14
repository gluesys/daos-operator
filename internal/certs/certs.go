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

// Package certs generates the DAOS transport certificates the way upstream
// utils/certs/gen_certificates.sh does: one private CA ("DAOS CA", RSA 3072,
// SHA-512, pathlen 1) and three leaf certificates -- server (serverAuth +
// clientAuth), agent (clientAuth), admin (clientAuth) -- with organization
// "DAOS" and the fixed common names DAOS expects. The result is written into a
// Kubernetes Secret and mounted at /etc/daos/certs (#16).
package certs

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

// Secret data keys, matching the file names DAOS configs reference.
const (
	KeyCA        = "daosCA.crt"
	KeyServerCrt = "server.crt"
	KeyServerKey = "server.key"
	KeyAgentCrt  = "agent.crt"
	KeyAgentKey  = "agent.key"
	KeyAdminCrt  = "admin.crt"
	KeyAdminKey  = "admin.key"

	rsaBits  = 3072
	validity = 1095 * 24 * time.Hour // upstream DAYS=1095
	org      = "DAOS"
)

// Bundle is the generated material, PEM encoded.
type Bundle map[string][]byte

// Generate creates a CA and the server/agent/admin certificates. now is the
// notBefore; tests pass a fixed time.
func Generate(now time.Time) (Bundle, error) {
	caKey, err := rsa.GenerateKey(rand.Reader, rsaBits)
	if err != nil {
		return nil, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{Organization: []string{org}, CommonName: "DAOS CA"},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageContentCommitment | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
		SignatureAlgorithm:    x509.SHA512WithRSA,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, err
	}
	b := Bundle{KeyCA: pemCert(caDER)}
	leaves := []struct {
		cn, crt, key string
		eku          []x509.ExtKeyUsage
	}{
		{"server", KeyServerCrt, KeyServerKey, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}},
		{"agent", KeyAgentCrt, KeyAgentKey, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}},
		{"admin", KeyAdminCrt, KeyAdminKey, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}},
	}
	for _, l := range leaves {
		key, err := rsa.GenerateKey(rand.Reader, rsaBits)
		if err != nil {
			return nil, err
		}
		tmpl := &x509.Certificate{
			SerialNumber:          serial(),
			Subject:               pkix.Name{Organization: []string{org}, CommonName: l.cn},
			NotBefore:             now.Add(-5 * time.Minute),
			NotAfter:              now.Add(validity),
			KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage:           l.eku,
			BasicConstraintsValid: true,
			SignatureAlgorithm:    x509.SHA512WithRSA,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", l.cn, err)
		}
		b[l.crt] = pemCert(der)
		b[l.key] = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	}
	return b, nil
}

// Verify checks that a bundle is complete, that each leaf chains to the CA
// with the expected usage, and reports when the earliest certificate expires.
func Verify(b Bundle, now time.Time) (expires time.Time, err error) {
	caPEM, ok := b[KeyCA]
	if !ok {
		return expires, fmt.Errorf("%s missing", KeyCA)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return expires, fmt.Errorf("%s is not a PEM certificate", KeyCA)
	}
	ca, _ := parsePEM(caPEM)
	if ca != nil {
		expires = ca.NotAfter
	}
	for _, l := range []struct {
		crt, key string
		eku      x509.ExtKeyUsage
	}{{KeyServerCrt, KeyServerKey, x509.ExtKeyUsageServerAuth}, {KeyAgentCrt, KeyAgentKey, x509.ExtKeyUsageClientAuth}, {KeyAdminCrt, KeyAdminKey, x509.ExtKeyUsageClientAuth}} {
		c, err := parsePEM(b[l.crt])
		if err != nil {
			return expires, fmt.Errorf("%s: %w", l.crt, err)
		}
		if _, err := c.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{l.eku}}); err != nil {
			return expires, fmt.Errorf("%s: %w", l.crt, err)
		}
		if _, ok := b[l.key]; !ok {
			return expires, fmt.Errorf("%s missing", l.key)
		}
		if c.NotAfter.Before(expires) {
			expires = c.NotAfter
		}
	}
	return expires, nil
}

func parsePEM(p []byte) (*x509.Certificate, error) {
	blk, _ := pem.Decode(p)
	if blk == nil {
		return nil, fmt.Errorf("not PEM")
	}
	return x509.ParseCertificate(blk.Bytes)
}

func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}
