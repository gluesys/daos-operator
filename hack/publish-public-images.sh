#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# Copy the images a public `helm install` needs from the internal registry to a
# public one (ghcr.io by default). Only these: the operator, the CSI driver and
# the four DAOS runtime images, which are stock upstream DAOS 2.8 RPMs on a
# Rocky 9 base. versitygw-daos and vllm-lmcache-daos are deliberately NOT here --
# the first embeds a fork whose source is not public, the second is 10 GB and
# neither is referenced by the chart's defaults (s3.services and vllm.services
# are empty, csi.enabled is false).
#
#   GHCR_OWNER=gluesys ./hack/publish-public-images.sh [--dry-run]
#
# Needs: docker login ghcr.io with a token carrying write:packages.
# Afterwards each package must be set to Public once (GitHub defaults to private).
set -euo pipefail

SRC=${SRC_REGISTRY:-registry.gitlab.gluesys.com/exastor}
DST=${DST_REGISTRY:-ghcr.io/${GHCR_OWNER:-gluesys}}
DRY=${1:-}

# source path : destination name : tag
IMAGES=(
  "daos-operator/daos-operator:daos-operator:${OPERATOR_TAG:-v0.1.0-rc.4}"
  "daos-csi/daos-csi:daos-csi:${CSI_TAG:-v0.1.0-rc.4}"
  "daos-images/daos-server:daos-server:${DAOS_TAG:-2.8.0-20260922}"
  "daos-images/daos-agent:daos-agent:${DAOS_TAG:-2.8.0-20260914}"
  "daos-images/daos-admin:daos-admin:${DAOS_TAG:-2.8.0-20260914}"
  "daos-images/daos-client:daos-client:${DAOS_TAG:-2.8.0-20260914}"
)

for spec in "${IMAGES[@]}"; do
  src_path=${spec%%:*}; rest=${spec#*:}
  name=${rest%%:*}; tag=${rest#*:}
  from="${SRC}/${src_path}:${tag}"
  to="${DST}/${name}:${tag}"
  echo "==> ${from}"
  echo "    -> ${to}"
  if [ "$DRY" = "--dry-run" ]; then continue; fi
  docker pull -q "$from"
  docker tag "$from" "$to"
  docker push -q "$to"
done

echo
echo "Done. Two things are not automatic:"
echo "  1. set each package to Public: https://github.com/orgs/${GHCR_OWNER:-gluesys}/packages"
echo "  2. point the chart at them (charts/daos-operator/values.yaml)"
