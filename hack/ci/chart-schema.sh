#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# Tier 0: 차트를 프로파일(charts/daos-operator/ci/*-values.yaml)마다 렌더해 kubeconform 으로
# K8s 1.29/1.31/1.34 스키마에 대어 본다. 커스텀 리소스(DaosSystem 등)는 건너뛰지 않고 차트의
# CRD 에서 뽑은 JSON 스키마로 검증한다(-strict: 스키마에 없는 키도 실패).
# 설계: doc/ci-design-2026-10-02.md 6절 Tier 0. 필요: helm, kubeconform, python3 + PyYAML.
set -euo pipefail
repo=$(cd "$(dirname "$0")/../.." && pwd)
chart=$repo/charts/daos-operator
out=${OUT:-$repo/dist/chart-schema}
versions=${K8S_VERSIONS:-"1.29.0 1.31.0 1.34.0"}
rm -rf "$out"; mkdir -p "$out/schemas"

# CRD → kubeconform 스키마: <group>/<kind 소문자>_<version>.json
python3 - "$chart/crds" "$out/schemas" <<'PY'
import json, os, sys, glob, yaml
src, dst = sys.argv[1], sys.argv[2]
for f in sorted(glob.glob(os.path.join(src, "*.yaml"))):
    for doc in yaml.safe_load_all(open(f)):
        if not doc or doc.get("kind") != "CustomResourceDefinition":
            continue
        g, kind = doc["spec"]["group"], doc["spec"]["names"]["kind"].lower()
        for v in doc["spec"]["versions"]:
            s = v["schema"]["openAPIV3Schema"]
            os.makedirs(os.path.join(dst, g), exist_ok=True)
            json.dump(s, open(os.path.join(dst, g, f"{kind}_{v['name']}.json"), "w"))
            print(f"schema {g}/{kind}_{v['name']}")
PY

fail=0
for values in "$chart"/ci/*-values.yaml; do
    name=$(basename "$values" -values.yaml)
    helm template daos-operator "$chart" -n daos-system --include-crds -f "$values" > "$out/$name.yaml"
    for v in $versions; do
        # CustomResourceDefinition 자체는 상류 스키마 모음에 없다(404). CRD 는 controller-gen 이 만들고
        # go-build 잡의 `make manifests` diff 가 지킨다. CRD 로 정의된 리소스는 아래 스키마로 검증한다.
        if ! kubeconform -strict -summary -output text -kubernetes-version "$v" -skip CustomResourceDefinition \
              -schema-location default \
              -schema-location "$out/schemas/{{ .Group }}/{{ .ResourceKind }}_{{ .ResourceAPIVersion }}.json" \
              "$out/$name.yaml" > "$out/$name-$v.txt" 2>&1; then
            echo "FAIL  $name  k8s $v"; sed 's/^/    /' "$out/$name-$v.txt" | grep -v -E '^\s+Summary' | head -20; fail=1
        else
            echo "ok    $name  k8s $v  $(tail -1 "$out/$name-$v.txt")"
        fi
    done
done
exit $fail
