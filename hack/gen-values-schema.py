#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Gluesys Co., Ltd.
#
# Writes charts/daos-operator/values.schema.json from values.yaml. The schema types what the
# chart ships and carries each key's comment as its description (Artifact Hub renders it); it
# never refuses a key it does not know, so values that work today keep working. Paths that the
# chart passes through to a CRD (system.spec) are left to the CRD's own validation.
#
#   hack/gen-values-schema.py            # rewrite the schema
#   hack/gen-values-schema.py --check    # exit 1 if the schema is out of date
import json
import re
import sys
from pathlib import Path

import yaml

CHART = Path(__file__).resolve().parent.parent / "charts" / "daos-operator"
PASSTHROUGH = {("system", "spec"): "DaosSystem spec, passed through as-is and validated by the CRD"}
KEY = re.compile(r"^(\s*)([A-Za-z0-9_.-]+):(.*)$")


def comments(lines):
    """Map key path -> description: the trailing comment, else the comment block right above."""
    out, stack, block = {}, [], []
    for line in lines:
        s = line.strip()
        if s.startswith("#"):
            block.append(s.lstrip("#").strip())
            continue
        m = KEY.match(line)
        if not m or s.startswith("-"):
            block = []
            continue
        indent, key, rest = len(m.group(1)), m.group(2), m.group(3)
        while stack and stack[-1][0] >= indent:
            stack.pop()
        path = tuple(k for _, k in stack) + (key,)
        stack.append((indent, key))
        trailing = rest.split(" #", 1)[1].strip() if " #" in rest else ""
        text = trailing or " ".join(b for b in block if b)
        if text:
            out[path] = text
        block = []
    return out


def schema_for(value, path, desc):
    if path in PASSTHROUGH:
        node = {"type": "object", "description": PASSTHROUGH[path]}
    elif isinstance(value, dict):
        node = {"type": "object", "additionalProperties": True}
        if value:
            node["properties"] = {k: schema_for(v, path + (k,), desc) for k, v in value.items()}
    elif isinstance(value, bool):
        node = {"type": "boolean"}
    elif isinstance(value, int):
        node = {"type": "integer"}
    elif isinstance(value, float):
        node = {"type": "number"}
    elif isinstance(value, str):
        node = {"type": "string"}
    elif isinstance(value, list):
        node = {"type": "array"}
    else:  # null: any value
        node = {}
    if path in desc and "description" not in node:
        node["description"] = desc[path]
    return node


def main():
    text = (CHART / "values.yaml").read_text()
    root = schema_for(yaml.safe_load(text), (), comments(text.splitlines()))
    root = {"$schema": "http://json-schema.org/draft-07/schema#", "title": "daos-operator chart values", **root}
    out = json.dumps(root, indent=2, ensure_ascii=False) + "\n"
    target = CHART / "values.schema.json"
    if "--check" in sys.argv:
        if not target.exists() or target.read_text() != out:
            print(f"{target} is out of date: run hack/gen-values-schema.py", file=sys.stderr)
            return 1
        return 0
    target.write_text(out)
    return 0


if __name__ == "__main__":
    sys.exit(main())
