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
      and role and set(role[0]["rules"][0]["verbs"]) >= {"create", "delete", "get", "list"}
      and [c for c in ds["containers"] if c["name"] == "daos-csi"][0]["image"].endswith(":mount-pod"))
print("ok" if ok else "bad: " + args)' 2>&1)
    if [ "$mp" = ok ]; then echo "ok    csi mountMode pod renders mount pod flags and Role"; else echo "FAIL  csi mountMode pod: $mp"; fail=1; fi
    if helm template daos-operator "$chart" -n daos-system -f "$chart/ci/csi-values.yaml" 2>&1 | grep -q -e '--mount-mode' -e 'daos-csi-node-mount'; then
        echo "FAIL  csi mountMode process (default) renders mount pod settings"; fail=1
    else
        echo "ok    csi mountMode process renders no mount pod settings"
    fi
    # the mount pods' agent socket is that of csi.system: this release's clientAgent.hostSocketDir
    # only for its own system; another system's default path, unless mountPod.agentSocketHostPath
    agentdir() { helm template daos-operator "$chart" -n daos-system -f "$chart/ci/csi-mountpod-values.yaml" \
        --set system.spec.clientAgent.hostSocketDir=/run/daos-local "$@" --show-only templates/csi.yaml 2>&1 |
        grep -o -e '--mount-agent-dir=[^"]*' | head -1; }
    for case in "|--mount-agent-dir=/run/daos-local" \
                "--set csi.system=other|--mount-agent-dir=/var/run/daos-operator/agent/other" \
                "--set csi.system=other --set csi.mountPod.agentSocketHostPath=/run/other|--mount-agent-dir=/run/other"; do
        args="${case%%|*}"; want="${case#*|}"; got=$(agentdir $args)
        if [ "$got" = "$want" ]; then echo "ok    csi mount agent dir ${args:-own system}"; else echo "FAIL  csi mount agent dir ${args:-own system}: $got, want $want"; fail=1; fi
    done
    for case in "csi.mountMode=pod --set system.spec.clientAgent.enabled=false --set csi.image.tag=x|enable system.spec.clientAgent" "csi.mountMode=Pod|must be process or pod" \
                "csi.mountMode=pod --set system.spec.clientAgent.enabled=true|needs a daos-csi image with --mount-mode"; do
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
# hostMounts: values that would give a mount nobody can use, or two entries fighting over a
# path, are refused with their own message; and the launch script detaches a stale mount before
# it mounts, waits for the agent socket, and hands dfuse the chart's flags unchanged.
if [ -f "$chart/templates/hostmounts.yaml" ]; then
    hm() { helm template daos-operator "$chart" -n daos-system -f "$chart/ci/hostmounts-values.yaml" "$@"; }
    for case in \
        "hostMounts[0].hostPath=/vfroot|sits directly under /" \
        "hostMounts[0].hostPath=rel/x|must be absolute" \
        "hostMounts[1].hostPath=/vfroot/daos/sub|overlaps" \
        "hostMounts[1].name=vfroot|is used twice" \
        "hostMounts[0].pool=a b|is not a DAOS label or UUID" \
        "hostMounts[0].updateStrategy.type=Recreate|must be OnDelete or RollingUpdate" \
        "hostMounts[0].updateStrategy.type=RollingUpdate,hostMounts[0].updateStrategy.rollingUpdate.maxSurge=1,hostMounts[0].updateStrategy.rollingUpdate.maxUnavailable=0|hostMounts\[vfroot\].*maxSurge must be 0" \
        "hostMounts[0].updateStrategy.type=RollingUpdate,hostMounts[0].updateStrategy.rollingUpdate.maxSurge=25%|hostMounts\[vfroot\].*maxSurge must be 0" \
        "system.spec.clientAgent.enabled=false|dfuse uses the node agent"; do
        set_="${case%%|*}"; want="${case#*|}"
        if hm --set "$set_" > "$out/hostmounts-bad.txt" 2>&1; then
            echo "FAIL  hostMounts $set_ was accepted"; fail=1
        elif grep -q "$want" "$out/hostmounts-bad.txt"; then
            echo "ok    hostMounts $set_ refused"
        else
            echo "FAIL  hostMounts $set_ failed for another reason:"; sed 's/^/    /' "$out/hostmounts-bad.txt" | head -5; fail=1
        fi
    done
    # a DaemonSet that rolls by itself cuts every consumer of the mount: the default must stay OnDelete
    if hm --set system.create=true | python3 -c '
import sys, yaml
ds = [d for d in yaml.safe_load_all(sys.stdin) if d and d["kind"] == "DaemonSet" and d["metadata"]["name"].endswith("hostmount-vfroot")]
c = ds[0]["spec"]["template"]["spec"]["containers"][0]
assert ds[0]["spec"]["updateStrategy"] == {"type": "OnDelete"}, ds[0]["spec"]["updateStrategy"]
for probe in ("livenessProbe", "readinessProbe"):  # a stat on the mount hangs uninterruptibly while the servers are away
    assert "stat " not in " ".join(c[probe]["exec"]["command"]), probe + " must not touch the file system"
prop = [m for m in c["volumeMounts"] if m["name"] == "parent"][0]
assert prop["mountPropagation"] == "Bidirectional"'; then
        echo "ok    hostMounts defaults: OnDelete, probes off the file system, Bidirectional"
    else
        echo "FAIL  hostMounts defaults"; fail=1
    fi
    t="$out/hostmount-launcher"; rm -rf "$t"; mkdir -p "$t/bin" "$t/mnt"
    # the socket gets a short path of its own: sun_path holds 107 bytes and $out can be deep
    ad=$(mktemp -d)
    hm | python3 -c '
import sys, yaml
t = sys.argv[1]
ds = [d for d in yaml.safe_load_all(sys.stdin) if d and d["kind"] == "DaemonSet" and d["metadata"]["name"].endswith("hostmount-vfroot")]
cmd = ds[0]["spec"]["template"]["spec"]["containers"][0]["command"]
open(t + "/launch.sh", "w").write(cmd[2].replace("/run/hostmount-", t + "/run-")); open(t + "/args", "w").write("\n".join(cmd[4:]) + "\n")' "$t"
    # findmnt: reports the target while $t/stale holds a count above 0; umount lowers it
    printf '%s\n' '#!/bin/sh' "n=\$(cat $t/stale); [ \"\$n\" -gt 0 ] && echo $t/mnt/daos; exit 0" > "$t/bin/findmnt"
    printf '%s\n' '#!/bin/sh' "n=\$(cat $t/stale); echo \$((n-1)) > $t/stale; echo \"\$@\" >> $t/umount-called" > "$t/bin/umount"
    printf '%s\n' '#!/bin/sh' "echo \"\$@\" >> $t/daos-called" > "$t/bin/daos"
    printf '%s\n' '#!/bin/sh' "[ -S $ad/daos_agent.sock ] || { echo no-socket >> $t/dfuse-called; exit 1; }" \
        "printf '%s\n' \"\$@\" >> $t/dfuse-called; exec sleep 300" > "$t/bin/dfuse"
    chmod +x "$t/bin/"*
    launch() { PATH="$t/bin:$PATH" MOUNT_TARGET="$t/mnt/daos" HOST_PATH=/vfroot/daos POOL=bai CONT=vfroot SYS_NAME=daos_k8s \
        AGENT_DIR=/x DAOS_AGENT_DRPC_DIR="$ad" timeout 60 bash -c "echo \$\$ > $t/pid; $(cat "$t/launch.sh")" dfuse $(cat "$t/args"); }
    # 1) two stale mounts, the agent socket shows up late: detach, wait, mount; TERM stops it cleanly
    echo 2 > "$t/stale"
    ( sleep 3; python3 -c "import socket,sys; socket.socket(socket.AF_UNIX).bind(sys.argv[1])" "$ad/daos_agent.sock" ) &
    launch > "$t/launch.log" 2>&1 & lp=$!
    for _ in $(seq 40); do [ -s "$t/dfuse-called" ] && break; sleep 0.5; done
    kill -TERM "$(cat "$t/pid")"
    if wait $lp && [ "$(wc -l < "$t/umount-called")" = 2 ] && [ -d "$t/mnt/daos" ] \
          && grep -qx -- '--sys-name=daos_k8s bai vfroot' <(sed 's/^container query //' "$t/daos-called") \
          && { echo --foreground; cat "$t/args"; echo "$t/mnt/daos"; } | cmp -s - "$t/dfuse-called" \
          && ! pgrep -f "^sleep 300$" >/dev/null; then
        echo "ok    hostMounts launcher: stale mounts detached, waited for the agent, dfuse got the chart's flags, TERM stops it"
    else
        echo "FAIL  hostMounts launcher:"; sed 's/^/    /' "$t/launch.log" | head -8; head -8 "$t/dfuse-called" 2>/dev/null; fail=1
    fi
    # 2) a dfuse that keeps exiting must fail the pod after five tries, not spin for ever
    printf '%s\n' '#!/bin/sh' "echo x >> $t/dfuse-tries; exit 1" > "$t/bin/dfuse"; rm -f "$t/dfuse-tries"
    if launch > "$t/launch3.log" 2>&1; then
        echo "FAIL  hostMounts launcher: exit 0 although dfuse never stayed up"; fail=1
    elif [ "$(wc -l < "$t/dfuse-tries")" = 5 ]; then
        echo "ok    hostMounts launcher gives up after 5 quick dfuse exits"
    else
        echo "FAIL  hostMounts launcher: $(wc -l < "$t/dfuse-tries") tries"; sed 's/^/    /' "$t/launch3.log" | tail -5; fail=1
    fi
    # 3) a mount that cannot be detached must stop the pod, not mount on top of it
    printf '%s\n' '#!/bin/sh' 'exit 1' > "$t/bin/umount"; echo 1 > "$t/stale"; rm -f "$t/dfuse-tries"
    if launch > "$t/launch2.log" 2>&1 || [ -e "$t/dfuse-tries" ]; then
        echo "FAIL  hostMounts launcher went on over a stale mount"; fail=1
    else
        echo "ok    hostMounts launcher stops when the stale mount cannot be detached"
    fi
    rm -rf "$ad"
fi
# deploy/host-client/fetch-config.sh (MR !137 review): the output directory is replaced as a whole
# and only by a complete set. Twice in a row into the same path must work as an ordinary user
# (the key of the first run is 0400), and a run whose Secret read fails must leave the previous
# config, CA, certificate and key exactly as they were.
if [ -x "$repo/deploy/host-client/fetch-config.sh" ]; then
    t="$out/fetch-config"; rm -rf "$t"; mkdir -p "$t"
    cat > "$t/kubectl" <<'STUB'
#!/bin/sh
# stub: prints generation $GEN of what is asked for; fails on the key named in $FAIL_ON
case "$*" in
  *configmap*) printf 'name: daos\ntransport_config:\n  allow_insecure: false\n# gen %s\n' "$GEN";;
  *secret*) key=$(echo "$*" | sed 's/.*data \\\{0,1\}"\([^"\\]*\).*/\1/')
            [ "$key" = "${FAIL_ON:-}" ] && { echo "Error from server" >&2; exit 1; }
            echo "$key gen $GEN";;
esac
STUB
    chmod +x "$t/kubectl"
    fc() { KUBECTL="$t/kubectl" "$repo/deploy/host-client/fetch-config.sh" daos daos-system "$t/out"; }
    sum() { (cd "$t/out" && cat daos_agent.yml certs/daosCA.crt certs/agent.crt certs/agent.key 2>&1 | cksum); }
    if GEN=1 fc > "$t/1.log" 2>&1 && GEN=2 fc > "$t/2.log" 2>&1 \
          && [ "$(cat "$t/out/certs/agent.key")" = "agent.key gen 2" ] && grep -q 'gen 2' "$t/out/daos_agent.yml" \
          && [ "$(stat -c %a "$t/out/certs/agent.key")" = 400 ] && [ "$(stat -c %a "$t/out/certs")" = 700 ]; then
        echo "ok    fetch-config twice into the same directory: complete second set, key 0400"
    else
        echo "FAIL  fetch-config twice:"; cat "$t/1.log" "$t/2.log" | sed 's/^/    /' | head -8; fail=1
    fi
    before=$(sum)
    # a file of someone else's in the output directory would be deleted by a successful run:
    # refused before anything changes
    mkdir "$t/out/dfuse"; echo "POOL=bai" > "$t/out/dfuse/vfroot.conf"
    if GEN=9 fc > "$t/9.log" 2>&1; then
        echo "FAIL  fetch-config: ran over a directory holding other files"; fail=1
    elif [ "$(cat "$t/out/dfuse/vfroot.conf")" = "POOL=bai" ] && [ "$(sum)" = "$before" ] \
          && grep -q 'dfuse/vfroot.conf' "$t/9.log" && [ -z "$(ls -d "$t"/out.* 2>/dev/null)" ]; then
        echo "ok    fetch-config refuses an output directory holding other files, and changes nothing"
    else
        echo "FAIL  fetch-config: other files in the output directory"; sed 's/^/    /' "$t/9.log" | head -5; fail=1
    fi
    rm -rf "$t/out/dfuse"
    for failon in agent.crt agent.key; do
        if GEN=3 FAIL_ON=$failon fc > "$t/3.log" 2>&1; then
            echo "FAIL  fetch-config: exit 0 although the Secret read of $failon failed"; fail=1
        elif [ "$(sum)" = "$before" ] && [ -z "$(ls -d "$t"/out.* 2>/dev/null)" ]; then
            echo "ok    fetch-config: a failed read of $failon leaves the previous set untouched"
        else
            echo "FAIL  fetch-config: previous set changed after a failed read of $failon"; ls -la "$t" "$t/out" | sed 's/^/    /' | head -12; fail=1
        fi
    done
    # everything fetched, but the new set cannot be put in place: the previous one must come back.
    # mv stub: fails when it is asked to move a directory whose name matches $MV_FAIL
    mkdir -p "$t/bin"; realmv=$(command -v mv)
    printf '%s\n' '#!/bin/sh' \
        'for pat in $MV_FAIL; do case "$1" in *"$pat"*) echo "mv: stub failure on $1" >&2; exit 1;; esac; done' \
        "exec $realmv \"\$@\"" > "$t/bin/mv"
    chmod +x "$t/bin/mv"
    if GEN=4 MV_FAIL=".fetch." PATH="$t/bin:$PATH" fc > "$t/4.log" 2>&1; then
        echo "FAIL  fetch-config: exit 0 although the new set could not be put in place"; fail=1
    elif [ "$(sum)" = "$before" ] && [ -z "$(ls -d "$t"/out.* 2>/dev/null)" ] && grep -q 'previous output is back in place' "$t/4.log"; then
        echo "ok    fetch-config: a failed publish puts the previous set back"
    else
        echo "FAIL  fetch-config: previous set lost after a failed publish"; sed 's/^/    /' "$t/4.log" | head -5; ls -la "$t" | sed 's/^/    /' | head -8; fail=1
    fi
    # and when even that fails, the previous set is kept where it is and the path is printed
    if GEN=5 MV_FAIL=".fetch. /prev" PATH="$t/bin:$PATH" fc > "$t/5.log" 2>&1; then
        echo "FAIL  fetch-config: exit 0 although nothing could be moved"; fail=1
    else
        kept=$(sed -n 's/.*it is in \(.*\)$/\1/p' "$t/5.log")
        if [ -n "$kept" ] && [ "$(cd "$kept" && cat daos_agent.yml certs/daosCA.crt certs/agent.crt certs/agent.key | cksum)" = "$before" ]; then
            echo "ok    fetch-config: when the previous set cannot go back it is kept and its path printed"
        else
            echo "FAIL  fetch-config: previous set not kept after a failed restore"; sed 's/^/    /' "$t/5.log" | head -5; fail=1
        fi
    fi
fi
exit $fail
