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
#
# The output directory is replaced as a whole and only by a complete set: everything
# is fetched into a private directory next to it first. A run that fails half way
# leaves the previous output as it was, so a config never ends up next to the
# certificates of another fetch.
set -euo pipefail
sys=${1:?usage: fetch-config.sh <DaosSystem name> [namespace] [output dir]}
ns=${2:-daos-system}
out=${3:-./daos-client-$sys}
kubectl=${KUBECTL:-kubectl}

out=${out%/}
mkdir -p "$(dirname "$out")"
umask 077
stage=$(mktemp -d "$out.fetch.XXXXXX")
old=
trap 'rm -rf "$stage" ${old:+"$old"}' EXIT

$kubectl -n "$ns" get configmap "$sys-agent" -o 'jsonpath={.data.daos_agent\.yml}' > "$stage/daos_agent.yml"
[ -s "$stage/daos_agent.yml" ] || { echo "ConfigMap $ns/$sys-agent has no daos_agent.yml" >&2; exit 1; }

tls=no
if grep -q 'allow_insecure: false' "$stage/daos_agent.yml"; then
    tls=yes
    mkdir "$stage/certs"
    for key in daosCA.crt agent.crt agent.key; do
        $kubectl -n "$ns" get secret "$sys-certs" -o "go-template={{ index .data \"$key\" | base64decode }}" > "$stage/certs/$key"
        [ -s "$stage/certs/$key" ] || { echo "Secret $ns/$sys-certs has no $key" >&2; exit 1; }
    done
    chmod 0400 "$stage/certs/agent.key"; chmod 0644 "$stage/certs/daosCA.crt" "$stage/certs/agent.crt"
fi
chmod 0644 "$stage/daos_agent.yml"; chmod 0755 "$stage"

# publish: the complete set takes the place of the old one
if [ -e "$out" ]; then
    old=$(mktemp -d "$out.old.XXXXXX")
    mv "$out" "$old/"
fi
mv "$stage" "$out"

echo "wrote $out/daos_agent.yml"
if [ $tls = yes ]; then
    echo "wrote $out/certs/{daosCA.crt,agent.crt,agent.key}"
else
    echo "the system allows insecure transport: no certificates needed"
fi
# The agent picks its fabric interface on the node it runs on. include_fabric_ifaces in the
# file names interfaces of the cluster's nodes; check that this node has the same names.
grep -A3 '^include_fabric_ifaces' "$out/daos_agent.yml" || true
