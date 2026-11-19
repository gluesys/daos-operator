//go:build e2e

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Gluesys Co., Ltd.

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The pull secret copied by case 15 must not reach the log when kubectl fails.
func TestApplySecretKeepsCredentialsOutOfError(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "kubectl")
	// echoes its input like a failing server-side validation would, then fails
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ncat; echo 'error: refused' >&2; exit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	const cred = "c2VjcmV0LWRvY2tlci1jb25maWc="
	manifest := []byte(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"gitlab-registry"},"type":"kubernetes.io/dockerconfigjson","data":{".dockerconfigjson":"` + cred + `"}}`)
	err := applySecret(fake, "e2e-il-1", "gitlab-registry", manifest)
	if err == nil {
		t.Fatal("no error from a failing kubectl")
	}
	if !strings.Contains(err.Error(), "e2e-il-1/gitlab-registry") {
		t.Fatalf("error does not name the object: %v", err)
	}
	if strings.Contains(err.Error(), cred) || strings.Contains(err.Error(), "refused") {
		t.Fatalf("error carries kubectl's input or output: %v", err)
	}
}
