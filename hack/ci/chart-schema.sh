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
# values.schema.json 은 values.yaml 에서 생성한다(Artifact Hub 표시, helm 의 타입 검사). 어긋나면 실패.
python3 "$repo/hack/gen-values-schema.py" --check || fail=1
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
# A tag that YAML/--set reads as a number (all-digit commit hash) must be refused, not rendered
# as "%!s(float64=...)" (daos-operator MR !8, 2026-10-03). Only once the chart has the guard.
if grep -q 'daos-operator.tag' "$chart/templates/_helpers.tpl"; then
    if helm template daos-operator "$chart" -n daos-system --set image.tag=17519342 >/dev/null 2>&1; then
        echo "FAIL  numeric image.tag was accepted"; fail=1
    else
        echo "ok    numeric image.tag refused"
    fi
fi
# A StorageClass for a pool in a DaosSystem daos-csi does not serve must be refused (MR !79 review).
# The pool is complete (name, size, storageClass) and the error text is checked, so a render that
# fails for some other reason does not count as the refusal.
if [ -f "$chart/templates/pools.yaml" ] && grep -q 'csiSys' "$chart/templates/pools.yaml"; then
    other="$out/pools-other-system.yaml"
    printf '%s\n' 'pools:' '  - {name: elsewhere, size: 1Ti, systemRef: other, storageClass: {}}' > "$other"
    if helm template daos-operator "$chart" -n daos-system -f "$chart/ci/pools-values.yaml" -f "$other" \
          > "$out/pools-other-system.txt" 2>&1; then
        echo "FAIL  pools[] StorageClass for another DaosSystem was accepted"; fail=1
    elif grep -q 'daos-csi serves' "$out/pools-other-system.txt"; then
        echo "ok    pools[] StorageClass for another DaosSystem refused"
    else
        echo "FAIL  pools[] other-system render failed for another reason:"; sed 's/^/    /' "$out/pools-other-system.txt" | head -5; fail=1
    fi
fi
exit $fail
