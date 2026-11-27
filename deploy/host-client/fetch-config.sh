#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# Write the client configuration of a DaosSystem for a node outside the cluster
# (doc/host-mount.md): the daos_agent.yml the operator keeps for its own agents and,
# when the system uses certificates, the agent certificate set.
#
#   deploy/host-client/fetch-config.sh <DaosSystem name> [namespace] [output dir]
#
# Run it where kubectl reaches the cluster, then copy the output directory to
# /etc/daos on each node (certs/ keeps mode 0700, the key 0400, owner daos_agent).
set -euo pipefail
sys=${1:?usage: fetch-config.sh <DaosSystem name> [namespace] [output dir]}
ns=${2:-daos-system}
out=${3:-./daos-client-$sys}
kubectl=${KUBECTL:-kubectl}

mkdir -p "$out"
$kubectl -n "$ns" get configmap "$sys-agent" -o 'jsonpath={.data.daos_agent\.yml}' > "$out/daos_agent.yml"
[ -s "$out/daos_agent.yml" ] || { echo "ConfigMap $ns/$sys-agent has no daos_agent.yml" >&2; exit 1; }
echo "wrote $out/daos_agent.yml"

if grep -q 'allow_insecure: false' "$out/daos_agent.yml"; then
    umask 077; mkdir -p "$out/certs"
    for key in daosCA.crt agent.crt agent.key; do
        $kubectl -n "$ns" get secret "$sys-certs" -o "go-template={{ index .data \"$key\" | base64decode }}" > "$out/certs/$key"
        [ -s "$out/certs/$key" ] || { echo "Secret $ns/$sys-certs has no $key" >&2; exit 1; }
    done
    chmod 0400 "$out/certs/agent.key"; chmod 0644 "$out/certs/daosCA.crt" "$out/certs/agent.crt"
    echo "wrote $out/certs/{daosCA.crt,agent.crt,agent.key}"
else
    echo "the system allows insecure transport: no certificates needed"
fi
# The agent picks its fabric interface on the node it runs on. include_fabric_ifaces in the
# file names interfaces of the cluster's nodes; check that this node has the same names.
grep -A3 '^include_fabric_ifaces' "$out/daos_agent.yml" || true
