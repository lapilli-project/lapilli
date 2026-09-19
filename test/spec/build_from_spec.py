#!/usr/bin/env python3
"""Build an ieb/v1 bundle directory from spec/IEB-SPEC.md alone (standard library only).

This file deliberately shares no code with Kairn: if `kairn verify` accepts its output, the
normative section of the spec is sufficient for an independent producer.

Usage: build_from_spec.py <out_dir>        then: kairn verify <out_dir>   (expect exit 0)
"""
import hashlib
import json
import os
import sys

out = sys.argv[1]
os.makedirs(os.path.join(out, "logs"), exist_ok=True)

# Files of the bundle (spec §2: paths of [A-Za-z0-9._-] segments).
files = {
    "logs/app-previous.log": b"independent producer: panic: boom\n",
    "logs/index.json": json.dumps({"containers": []}).encode(),
    # spec §7: redaction.json is required.
    "redaction.json": json.dumps({"policy_version": "v1", "mode": "default"}).encode(),
}
for path, data in files.items():
    with open(os.path.join(out, path), "wb") as f:
        f.write(data)

# spec §3: hash each file; root over "path:hash\n" sorted by the UTF-8 bytes of the path.
tree = {p: hashlib.sha256(d).hexdigest() for p, d in files.items()}
root = hashlib.sha256(
    b"".join(f"{p}:{h}\n".encode() for p, h in sorted(tree.items(), key=lambda kv: kv[0].encode()))
).hexdigest()

# spec §5/§6: the manifest; `logs` ran, so logs/index.json is required and present.
manifest = {
    "schema_version": "kairn.dev/ieb/v1",
    "incident": {
        "id": "spec-incident",
        "cluster_id": "spec-cluster",
        "trigger": {"rule": "SpecOnly", "firing_ts": "2026-09-19T00:00:00Z"},
        "window": {"start": "2026-09-18T23:55:00Z", "end": "2026-09-19T00:05:00Z"},
    },
    "producer": {"kairn_version": "independent-python", "image_digest": "none"},
    "signing": None,
    "hash_tree": {"files": tree, "root": root},
    "coverage": {"collectors_run": ["logs"], "collectors_intended": ["logs"]},
    "timing": {
        "capture_started": "2026-09-19T00:00:01Z",
        "sealed_at": "2026-09-19T00:00:02Z",
        "capture_to_seal_ms": 1000,
    },
    # Readers must ignore unknown fields (spec §5): prove it.
    "x_future_field": {"added_by": "a later minor"},
}
with open(os.path.join(out, "manifest.json"), "w") as f:
    json.dump(manifest, f)
print(f"wrote an ieb/v1 bundle to {out}")
