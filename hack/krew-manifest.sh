#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# Render krew/daos.yaml from krew/daos.yaml.tmpl using the checksums in dist/.
# BASE_URL must point at publicly downloadable release assets (krew-index
# requires public URIs; the GitLab instance is internal, so a public mirror
# release is needed before submitting upstream).
#
#   VERSION=v0.1.0 BASE_URL=https://github.com/gluesys/daos-operator/releases/download/v0.1.0 hack/krew-manifest.sh
set -euo pipefail
VERSION="${VERSION:-$(cat VERSION)}"
OUT="${OUT:-dist}"
BASE_URL="${BASE_URL:?public base URL of the release assets is required}"
TMPL="${TMPL:-krew/daos.yaml.tmpl}"
DEST="${DEST:-krew/daos.yaml}"

sum() { awk -v f="kubectl-daos_${VERSION}_$1_$2.tar.gz" '$2 == f {print $1}' "$OUT/SHA256SUMS"; }

out=$(sed -e "s|@VERSION@|$VERSION|g" -e "s|@BASE_URL@|$BASE_URL|g" "$TMPL")
for p in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os="${p%/*}"; arch="${p#*/}"
  s="$(sum "$os" "$arch")"
  [ -n "$s" ] || { echo "no checksum for $os/$arch in $OUT/SHA256SUMS" >&2; exit 1; }
  out="${out//@SHA256_${os^^}_${arch^^}@/$s}"
done
printf '%s\n' "$out" > "$DEST"
echo "wrote $DEST"
