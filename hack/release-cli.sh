#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# Build the kubectl-daos plugin for the platforms krew needs, package each as
# a tar.gz with the license, and write dist/SHA256SUMS. Called by `make
# release-cli` and by the GitLab CI release job.
#
#   VERSION=v0.1.0 hack/release-cli.sh
set -euo pipefail
VERSION="${VERSION:-$(cat VERSION)}"
OUT="${OUT:-dist}"
PLATFORMS="${PLATFORMS:-linux/amd64 linux/arm64 darwin/amd64 darwin/arm64}"

rm -rf "$OUT" && mkdir -p "$OUT"
for p in $PLATFORMS; do
  os="${p%/*}"; arch="${p#*/}"
  workdir="$(mktemp -d)"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION" -o "$workdir/kubectl-daos" ./cmd/kubectl-daos
  cp LICENSE "$workdir/LICENSE"
  tar -C "$workdir" -czf "$OUT/kubectl-daos_${VERSION}_${os}_${arch}.tar.gz" kubectl-daos LICENSE
  rm -rf "$workdir"
done
( cd "$OUT" && sha256sum *.tar.gz > SHA256SUMS )
echo "built $VERSION:"
cat "$OUT/SHA256SUMS"
