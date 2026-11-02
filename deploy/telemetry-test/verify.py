#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
"""Verify the local-agent -> collector -> DB test path through a loopback forward."""
import argparse
import json
import time
import urllib.parse
import urllib.request
from pathlib import Path

parser = argparse.ArgumentParser()
parser.add_argument('--url', default='http://127.0.0.1:18438')
parser.add_argument('--output', required=True)
args = parser.parse_args()
queries = {
    'all_scrapes_up': ('sum(up{cluster="daos-operator-telemetry-test"})', 4),
    'read_bytes': ('sum(gpuflow_storage_target_read_bytes_total)', 100663296),
    'write_bytes': ('sum(gpuflow_storage_target_write_bytes_total)', 100663296),
    # This pinned DAOS build exports DFS request sizes as histograms: _sum is bytes.
    'client_read_bytes': ('sum(client_dfs_read_bytes_sum)', 100663296),
    'client_write_bytes': ('sum(client_dfs_write_bytes_sum)', 100663296),
    'csi_create_ok': ('sum(csi_sidecar_operations_seconds_count{method_name="/csi.v1.Controller/CreateVolume",grpc_status_code="OK"})', 1),
    'csi_delete_ok': ('sum(csi_sidecar_operations_seconds_count{method_name="/csi.v1.Controller/DeleteVolume",grpc_status_code="OK"})', 1),
}
result = {'checked_at_unix': time.time(), 'checks': {}, 'passed': True}
for name, (query, minimum) in queries.items():
    with urllib.request.urlopen(args.url + '/api/v1/query?' + urllib.parse.urlencode({'query': query}), timeout=10) as response:
        data = json.load(response)
    samples = data['data']['result']
    value = float(samples[0]['value'][1]) if samples else None
    passed = value is not None and value >= minimum
    if name == 'all_scrapes_up':
        passed = value == minimum
    result['checks'][name] = {'query': query, 'value': value, 'minimum': minimum, 'passed': passed}
    result['passed'] &= passed
    print(name, value, 'PASS' if passed else 'FAIL')
Path(args.output).write_text(json.dumps(result, indent=2) + '\n')
raise SystemExit(0 if result['passed'] else 1)
