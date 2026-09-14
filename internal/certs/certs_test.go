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

package certs

import (
	"crypto/x509"
	"testing"
	"time"
)

func TestGenerateAndVerify(t *testing.T) {
	now := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	b, err := Generate(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 7 {
		t.Fatalf("expected 7 files, got %d", len(b))
	}
	exp, err := Verify(b, now)
	if err != nil {
		t.Fatal(err)
	}
	if exp.Before(now.Add(1094 * 24 * time.Hour)) {
		t.Errorf("expiry too early: %s", exp)
	}
	ca, _ := parsePEM(b[KeyCA])
	if ca.Subject.CommonName != "DAOS CA" || ca.MaxPathLen != 1 || !ca.IsCA || ca.SignatureAlgorithm != x509.SHA512WithRSA {
		t.Errorf("CA: %+v", ca.Subject)
	}
	srv, _ := parsePEM(b[KeyServerCrt])
	if srv.Subject.CommonName != "server" || srv.Subject.Organization[0] != "DAOS" || len(srv.ExtKeyUsage) != 2 {
		t.Errorf("server cert: %+v %v", srv.Subject, srv.ExtKeyUsage)
	}
	adm, _ := parsePEM(b[KeyAdminCrt])
	// admin is clientAuth only: it must not verify as a server
	if _, err := adm.Verify(x509.VerifyOptions{Roots: certPool(b), CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
		t.Error("admin cert must not be usable for serverAuth")
	}
	// tamper: drop a key
	delete(b, KeyAgentKey)
	if _, err := Verify(b, now); err == nil {
		t.Error("missing key must fail verification")
	}
	// expired
	if _, err := Verify(b, now.Add(1200*24*time.Hour)); err == nil {
		t.Error("expired bundle must fail")
	}
}

func certPool(b Bundle) *x509.CertPool {
	p := x509.NewCertPool()
	p.AppendCertsFromPEM(b[KeyCA])
	return p
}
