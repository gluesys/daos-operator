#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# Copy the images a public `helm install` needs from the internal registry to a
# public one (ghcr.io by default). Only these: the operator, its hostprep image, the
# CSI driver and the four DAOS runtime images, which are stock upstream DAOS 2.8 RPMs on a
# Rocky 9 base. versitygw-daos and vllm-lmcache-daos are deliberately NOT here --
# the first embeds a fork whose source is not public, the second is 10 GB and
# neither is referenced by the chart's defaults (s3.services and vllm.services
# are empty, csi.enabled is false).
#
#   GHCR_OWNER=gluesys ./hack/publish-public-images.sh [--dry-run]
#
# The internal registry tags operator and hostprep images by commit (CI_COMMIT_SHORT_SHA);
# OPERATOR_SRC_TAG defaults to the commit OPERATOR_TAG points at, and both images are
# published under OPERATOR_TAG. hostprep was missing here, so v0.1.0-rc.4 shipped without
# it and a public install could not pull it (#15).
#
# Needs: docker login ghcr.io with a token carrying write:packages.
# Afterwards each package must be set to Public once (GitHub defaults to private).
set -euo pipefail

SRC=${SRC_REGISTRY:-registry.gitlab.gluesys.com/exastor}
DST=${DST_REGISTRY:-ghcr.io/${GHCR_OWNER:-gluesys}}
DRY=${1:-}
OPERATOR_TAG=${OPERATOR_TAG:-v0.1.0-rc.4}
OPERATOR_SRC_TAG=${OPERATOR_SRC_TAG:-$(git rev-parse --short=8 "${OPERATOR_TAG}^{commit}")}

# source path : destination name : tag (source tag:destination tag when they differ)
IMAGES=(
  "daos-operator/daos-operator:daos-operator:${OPERATOR_SRC_TAG}:${OPERATOR_TAG}"
  "daos-operator/daos-hostprep:daos-hostprep:${OPERATOR_SRC_TAG}:${OPERATOR_TAG}"
  "daos-csi/daos-csi:daos-csi:${CSI_TAG:-v0.1.0-rc.4}"
  "daos-images/daos-server:daos-server:${DAOS_TAG:-2.8.0-20260922}"
  "daos-images/daos-agent:daos-agent:${DAOS_TAG:-2.8.0-20260914}"
  "daos-images/daos-admin:daos-admin:${DAOS_TAG:-2.8.0-20260914}"
  "daos-images/daos-client:daos-client:${DAOS_TAG:-2.8.0-20260914}"
)

for spec in "${IMAGES[@]}"; do
  src_path=${spec%%:*}; rest=${spec#*:}
  name=${rest%%:*}; tag=${rest#*:}
  src_tag=${tag%%:*}; dst_tag=${tag#*:}
  from="${SRC}/${src_path}:${src_tag}"
  to="${DST}/${name}:${dst_tag}"
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
