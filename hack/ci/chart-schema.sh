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

# VolumeSnapshotClass (csi.snapshots) 는 external-snapshotter CRD 라 상류 스키마 모음에 없다.
# 그 CRD(snapshot.storage.k8s.io/v1)의 필수 필드와 deletionPolicy enum 만 옮겨 둔다.
mkdir -p "$out/schemas/snapshot.storage.k8s.io"
cat > "$out/schemas/snapshot.storage.k8s.io/volumesnapshotclass_v1.json" <<'JSON'
{"type": "object", "required": ["driver", "deletionPolicy"],
 "properties": {"apiVersion": {"type": "string"}, "kind": {"type": "string"}, "metadata": {"type": "object"},
  "driver": {"type": "string"}, "deletionPolicy": {"type": "string", "enum": ["Delete", "Retain"]},
  "parameters": {"type": "object", "additionalProperties": {"type": "string"}}},
 "additionalProperties": false}
JSON

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
# CSI node update strategy (MR !124 review): an upgrade with --reuse-values keeps the old values,
# which have no nodeUpdateStrategy, and Kubernetes takes maxUnavailable as a number or a percentage.
strategy() {
    helm template daos-operator "$chart" -n daos-system -f "$chart/ci/csi-values.yaml" "$@" 2>&1 |
        python3 -c 'import sys, yaml, json
ds = [d for d in yaml.safe_load_all(sys.stdin) if d and d.get("kind") == "DaemonSet" and "csi" in d["metadata"]["name"]]
print(json.dumps(ds[0]["spec"]["updateStrategy"], sort_keys=True) if ds else "none")' 2>&1
}
check_strategy() {
    want=$1; shift
    got=$(strategy "$@")
    if [ "$got" = "$want" ]; then echo "ok    csi updateStrategy $*"; else echo "FAIL  csi updateStrategy $*: $got, want $want"; fail=1; fi
}
check_strategy '{"rollingUpdate": {"maxUnavailable": 1}, "type": "RollingUpdate"}' --set csi.nodeUpdateStrategy=null
check_strategy '{"rollingUpdate": {"maxUnavailable": "10%"}, "type": "RollingUpdate"}' --set csi.nodeUpdateStrategy.type=RollingUpdate --set-string 'csi.nodeUpdateStrategy.rollingUpdate.maxUnavailable=10%'
check_strategy '{"rollingUpdate": {"maxSurge": 1, "maxUnavailable": 0}, "type": "RollingUpdate"}' --set csi.nodeUpdateStrategy.type=RollingUpdate --set csi.nodeUpdateStrategy.rollingUpdate.maxSurge=1 --set csi.nodeUpdateStrategy.rollingUpdate.maxUnavailable=0
check_strategy '{"type": "OnDelete"}' --set csi.nodeUpdateStrategy.rollingUpdate.maxUnavailable=2
# the output first: under pipefail a helm that fails (as it should) would fail the grep pipeline too
for bad in Ondelete '""'; do
    refusal=$(helm template daos-operator "$chart" -n daos-system -f "$chart/ci/csi-values.yaml" --set "csi.nodeUpdateStrategy.type=$bad" 2>&1 || true)
    if grep -q 'csi.nodeUpdateStrategy.type must be RollingUpdate or OnDelete' <<<"$refusal"; then
        echo "ok    csi updateStrategy type $bad refused"
    else
        echo "FAIL  csi updateStrategy type $bad was not refused"; fail=1
    fi
done
# csi.mountMode (daos-csi ADR-csi-002): pod passes the mount pod flags and a namespaced Role and
# drops the node plugin's agent sidecar; process (the default) renders none of that; pod without
# the node agent, and a mode that is neither, are refused with their own message.
if grep -q 'mountMode' "$chart/templates/csi.yaml"; then
    mp=$(helm template daos-operator "$chart" -n daos-system -f "$chart/ci/csi-mountpod-values.yaml" 2>&1 | python3 -c 'import sys, yaml
docs = [d for d in yaml.safe_load_all(sys.stdin) if d]
ds = [d for d in docs if d["kind"] == "DaemonSet" and d["metadata"]["name"] == "daos-csi-node"][0]["spec"]["template"]["spec"]
args = " ".join([c for c in ds["containers"] if c["name"] == "daos-csi"][0]["args"])
role = [d for d in docs if d["kind"] == "Role" and d["metadata"]["name"] == "daos-csi-node-mount"]
ok = ("--mount-mode=pod" in args and "--mount-agent-dir=/var/run/daos-operator/agent/daos" in args
      and "--mount-namespace=daos-system" in args and "--mount-image=" in args and not ds.get("initContainers")
      and role and set(role[0]["rules"][0]["verbs"]) >= {"create", "delete", "get", "list"})
print("ok" if ok else "bad: " + args)' 2>&1)
    if [ "$mp" = ok ]; then echo "ok    csi mountMode pod renders mount pod flags and Role"; else echo "FAIL  csi mountMode pod: $mp"; fail=1; fi
    if helm template daos-operator "$chart" -n daos-system -f "$chart/ci/csi-values.yaml" 2>&1 | grep -q -e '--mount-mode' -e 'daos-csi-node-mount'; then
        echo "FAIL  csi mountMode process (default) renders mount pod settings"; fail=1
    else
        echo "ok    csi mountMode process renders no mount pod settings"
    fi
    for case in "csi.mountMode=pod --set system.spec.clientAgent.enabled=false|enable system.spec.clientAgent" "csi.mountMode=Pod|must be process or pod"; do
        set_="${case%%|*}"; want="${case#*|}"
        refusal=$(helm template daos-operator "$chart" -n daos-system -f "$chart/ci/csi-values.yaml" --set $set_ 2>&1 || true)
        if grep -q "$want" <<<"$refusal"; then echo "ok    csi $set_ refused"; else echo "FAIL  csi $set_ was not refused"; fail=1; fi
    done
fi
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
# lmcacheMode: mp cannot carry the rank guard (in-process only) nor run on rbln; both must be refused
# with their own message, not render a pod that never attaches its connector.
if grep -q 'lmcacheMode' "$chart/templates/vllm.yaml"; then
    for case in "rankGuard: {enabled: true}|rankGuard is in-process only" "accelerator: rbln|not available with accelerator rbln"; do
        extra="${case%%|*}"; want="${case#*|}"
        bad="$out/vllm-mp-bad.yaml"
        printf '%s\n' 'vllm:' '  services:' '    - name: bad' '      image: x' '      lmcacheMode: mp' \
            '      poolRef: p' '      containerRef: c' '      model: {path: /m}' "      $extra" > "$bad"
        if helm template daos-operator "$chart" -n daos-system -f "$bad" > "$out/vllm-mp-bad.txt" 2>&1; then
            echo "FAIL  lmcacheMode mp with '$extra' was accepted"; fail=1
        elif grep -q "$want" "$out/vllm-mp-bad.txt"; then
            echo "ok    lmcacheMode mp with '$extra' refused"
        else
            echo "FAIL  lmcacheMode mp with '$extra' failed for another reason:"; sed 's/^/    /' "$out/vllm-mp-bad.txt" | head -5; fail=1
        fi
    done
fi
# The mp launcher itself (rendered bash): run it with a stub python3.12 that plays the MP server
# and vLLM. A port someone else already listens on must fail the pod without starting vLLM (the
# node's port under hostNetwork; !94 review); the normal path must hand vLLM the chart's args.
if grep -q 'lmcacheMode' "$chart/templates/vllm.yaml"; then
    t="$out/mp-launcher"; rm -rf "$t"; mkdir -p "$t/bin"
    port=$((20000 + RANDOM % 20000))
    printf '%s\n' 'vllm:' '  services:' '    - {name: l, image: x, lmcacheMode: mp, poolRef: p, containerRef: c, model: {path: /m},' \
        "       port: 8100, mp: {port: $port, startTimeoutSeconds: 20}}" > "$t/values.yaml"
    helm template daos-operator "$chart" -n daos-system -f "$t/values.yaml" --show-only templates/vllm.yaml \
        | python3 -c 'import sys,yaml
for d in yaml.safe_load_all(sys.stdin):
    if d and d["kind"] == "Deployment":
        c = d["spec"]["template"]["spec"]["containers"][0]
        open(sys.argv[1], "w").write(c["command"][2]); open(sys.argv[2], "w").write("\n".join(c["args"]) + "\n")' "$t/launch.sh" "$t/args"
    cat > "$t/bin/python3.12" <<STUB
#!/bin/bash
case "\$2" in
  lmcache_daos.mp.server) exec python3 -c 'import socket,time; s=socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); s.bind(("127.0.0.1", $port)); s.listen(); time.sleep(20)' ;;
  vllm.entrypoints.openai.api_server) shift 2; printf '%s\n' "\$@" > "$t/vllm-called"; exit 0 ;;
esac
STUB
    chmod +x "$t/bin/python3.12"
    # 1) occupied port: refused, vLLM never runs
    python3 -c "import socket,time; s=socket.socket(); s.bind(('127.0.0.1', $port)); s.listen(); time.sleep(15)" & occ=$!; sleep 1
    rm -f "$t/vllm-called"
    if PATH="$t/bin:$PATH" bash -c "$(cat "$t/launch.sh")" vllm $(cat "$t/args") > "$t/occupied.log" 2>&1; then
        echo "FAIL  mp launcher started with port $port already taken"; fail=1
    elif [ -e "$t/vllm-called" ]; then
        echo "FAIL  mp launcher ran vLLM against someone else's server"; fail=1
    else
        echo "ok    mp launcher refuses an occupied port"
    fi
    kill $occ 2>/dev/null || true; wait $occ 2>/dev/null || true; sleep 1
    # 2) normal: our server comes up, vLLM gets the chart's args unchanged
    rm -f "$t/vllm-called"
    if PATH="$t/bin:$PATH" bash -c "$(cat "$t/launch.sh")" vllm $(cat "$t/args") > "$t/normal.log" 2>&1 \
          && cmp -s "$t/args" "$t/vllm-called"; then
        echo "ok    mp launcher starts vLLM with the chart's args"
    else
        echo "FAIL  mp launcher normal path:"; sed 's/^/    /' "$t/normal.log" | head -5; fail=1
    fi
    pkill -f "s.bind((\"127.0.0.1\", $port))" 2>/dev/null || true
fi
exit $fail
