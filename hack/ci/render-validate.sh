#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# Tier 0: operator 가 렌더하는 daos_server/daos_agent/daos_control 설정을 실제 DAOS 2.8 바이너리로
# 검증한다(daos-images scripts/validate-config.sh). README 의 "2.8 스키마 미검증" 항목을 닫는다.
#   VALIDATOR: validate-config.sh 경로(기본: $DAOS_IMAGES_DIR/scripts/validate-config.sh)
#   IMAGE_TAG: 검증에 쓸 daos-images 태그. DOCKER: docker|podman
set -euo pipefail
repo=$(cd "$(dirname "$0")/../.." && pwd)
: "${DAOS_IMAGES_DIR:?clone of exastor/daos-images}"
validator=${VALIDATOR:-$DAOS_IMAGES_DIR/scripts/validate-config.sh}
out=$repo/dist/render
# CI 에서는 Go 가 있는 잡이 렌더하고(artifacts) podman 이 있는 잡이 검증한다: RENDER=0
[ "${RENDER:-1}" = 0 ] || { cd "$repo" && go run ./hack/render-samples -out "$out"; }
ls "$out"/*.yml >/dev/null
fail=0
for f in "$out"/*.yml; do
    case $(basename "$f") in server-*) kind=server ;; agent*) kind=agent ;; control*) kind=control ;; *) continue ;; esac
    if "$validator" "$kind" "$f"; then :; else fail=1; fi
done
[ $fail = 0 ] && echo "render-validate: all $(ls "$out"/*.yml | wc -l) configurations accepted by DAOS ${IMAGE_TAG:-default}"
exit $fail
