#!/usr/bin/env bash
# Run inside the telemetry-io pod after the pool/container are Ready.
set -euo pipefail
mkdir -p /mnt/telemetry
if ! mountpoint -q /mnt/telemetry; then
  dfuse --sys-name=gpftelemetry --pool=telemetry-lab-pool --container=telemetry-io --disable-caching -t 2 -e 1 /mnt/telemetry
fi
dd if=/dev/urandom of=/tmp/telemetry-input bs=1M count=32 status=none
for round in 1 2 3; do
  dd if=/tmp/telemetry-input of=/mnt/telemetry/roundtrip bs=1M conv=fsync status=none
  dd if=/mnt/telemetry/roundtrip of=/tmp/telemetry-output bs=1M status=none
  read -r expected _ < <(sha256sum /tmp/telemetry-input)
  read -r actual _ < <(sha256sum /tmp/telemetry-output)
  test "$expected" = "$actual"
  printf 'round=%s bytes=33554432 checksum=PASS\n' "$round"
  sleep 2
done
sha256sum /tmp/telemetry-input /tmp/telemetry-output
# Keep dfuse mounted for client telemetry to remain observable.
