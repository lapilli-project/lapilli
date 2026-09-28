#!/usr/bin/env bash
# Does the third-party attribution obligation still hold for the crate set we actually ship?
#
# Split out of scripts/release-check.sh so CI can run it on every pull request. That matters
# because the thing most likely to break it is not a human edit: Dependabot's `cargo` updates move
# Cargo.lock, and THIRD-PARTY-LICENSES.md is generated from the lock. Checked at release time only,
# a bump would leave the listing stale — a crate whose licence text we stopped distributing — and
# nothing would say so until the next tag.
#
# Everything here is portable: `cargo metadata`, `cargo tree`, and the unpacked registry sources.
# The two checks that need a built image (the copies inside it, the OCI labels, the binaries'
# embedded dependency lists) stay in release-check.sh.
set -euo pipefail
cd "$(dirname "$0")/.."
# Lapilli links ~280 crates statically into every binary it publishes. Their licences require
# the licence text and the copyright notices to be distributed with the binary, and `object_store`
# — an Apache Arrow crate — ships a NOTICE that Apache-2.0 §4(d) makes travel with any derivative
# distribution. `cargo deny check licenses` answers a different question (are these licences
# compatible with ours); these checks answer whether the obligation actually reaches a downloader.
# v0.1.0-rc.1 shipped with none of it, which is what this step exists to stop happening again.
for f in LICENSE NOTICE THIRD-PARTY-LICENSES.md; do
  [ -s "$f" ] || { echo "  MISSING or empty: $f"; exit 1; }
done
grep -q 'Copyright 2026 The Lapilli Authors' NOTICE \
  || { echo "  NOTICE has no copyright line"; exit 1; }
if grep -q 'Copyright \[yyyy\]' LICENSE; then
  echo "  LICENSE's appendix still has the [yyyy] placeholder — no copyright holder is named"
  exit 1
fi

# Each publishable crate carries its own copy of the licence, because `cargo package` cannot reach
# outside the crate directory: a crate published with only the `license = "Apache-2.0"` field
# distributes the terms by reference, and this project spent a release making the opposite argument
# about its binaries. A copy can drift where a symlink cannot, so the copies are asserted identical
# rather than trusted (a symlink was the first attempt and was dropped: a Windows checkout turns it
# into a 13-byte text file, which would publish a crate whose LICENSE says "../../LICENSE").
for d in crates/*/; do
  crate=$(basename "$d")
  [ -f "$d/LICENSE" ] || { echo "  $crate has no LICENSE of its own; cargo package cannot reach the root one"; exit 1; }
  cmp -s LICENSE "$d/LICENSE" \
    || { echo "  $crate/LICENSE differs from the root LICENSE"; exit 1; }
done
echo "  every crate carries a LICENSE byte-identical to the root one"

# Check 1 reads each dependency's own NOTICE out of its unpacked source. `cargo fetch` only puts
# the `.crate` archives in place; reading a manifest is what unpacks them, so this runs first and
# on a fresh runner it is the step that makes the check able to see anything at all.
cargo metadata --all-features --locked --format-version 1 >/dev/null

# The crate set the artifacts actually link: normal edges only (dev and build dependencies are
# not in a shipped binary), all features on (the image's CLI is built --no-default-features
# --features mcp and the tarball CLI with defaults, so only the union covers both), and every
# target a binary is published for. Same five targets as about.toml — they must agree.
deps=$(mktemp)
for t in x86_64-unknown-linux-gnu aarch64-unknown-linux-gnu \
         x86_64-unknown-linux-musl aarch64-unknown-linux-musl aarch64-apple-darwin; do
  cargo tree --workspace --all-features --target "$t" --edges normal --prefix none --format '{p}'
done | awk 'NF >= 2 { sub(/^v/, "", $2); print $1, $2 }' | sort -u > "$deps"
echo "  $(wc -l < "$deps" | tr -d ' ') crates linked into a published binary"

# 1. Every crate that ships a NOTICE of its own must have it reproduced in ours. Today that is
#    object_store alone; this check is for the day a `cargo update` makes it two. It reads the
#    unpacked registry sources, and fails rather than skipping when one is not there.
python3 - "$deps" <<'PY'
import glob, os, sys
home = os.environ.get("CARGO_HOME") or os.path.expanduser("~/.cargo")
ours = open("NOTICE").read()
missing, unvendored = [], []
for line in open(sys.argv[1]):
    name, _, version = line.strip().partition(" ")
    dirs = glob.glob(os.path.join(home, "registry", "src", "*", f"{name}-{version}"))
    if not dirs:
        unvendored.append(f"{name} {version}")   # workspace member, or not fetched
        continue
    for f in sorted(os.listdir(dirs[0])):
        if "NOTICE" not in f.upper():
            continue
        text = open(os.path.join(dirs[0], f), encoding="utf-8", errors="replace").read()
        absent = [l for l in text.splitlines() if l.strip() and l.strip() not in ours]
        if absent:
            missing.append(f"{name} {version} ({f}): {absent[0]!r} and {len(absent) - 1} more line(s)")
        else:
            print(f"  NOTICE of {name} {version} is reproduced in ours")
if missing:
    sys.exit("  these dependencies ship a NOTICE that ours does not carry:\n    "
             + "\n    ".join(missing))
# A crate whose source is not unpacked is a crate this check did not look at, and on a fresh
# runner that is *every* crate — which would turn the whole step into a pass that proves nothing.
# So it fails instead of noting it. `cargo metadata` above is what unpacks them; if this still
# fires, run a build first.
skipped = [u for u in unvendored if not u.startswith("lapilli-")]
if skipped:
    sys.exit(f"  {len(skipped)} crate(s) have no unpacked source, so their NOTICE was never read: "
             + ", ".join(skipped[:5]) + (" …" if len(skipped) > 5 else "")
             + "\n  run `cargo metadata --all-features --locked` or a build first")
PY

# 2. THIRD-PARTY-LICENSES.md is generated (about.toml, about.hbs) and goes stale the moment
#    Cargo.lock moves. It must list exactly the crate set above — a missing crate is a licence
#    whose text is not being distributed, an extra one is a claim about something not shipped.
python3 - "$deps" <<'PY'
import re, sys
want = {l.strip() for l in open(sys.argv[1]) if l.strip()}
have = {m.group(1) + " " + m.group(2) for m in
        re.finditer(r"^- \[(\S+) (\S+)\]\(", open("THIRD-PARTY-LICENSES.md").read(), re.M)}
problems = []
for label, diff in (("not listed in THIRD-PARTY-LICENSES.md", want - have),
                    ("listed but not linked into anything", have - want)):
    if diff:
        problems.append(f"  {len(diff)} crate(s) {label}: {', '.join(sorted(diff))}")
if problems:
    sys.exit("\n".join(problems) + "\n  regenerate it: the command is at the top of about.toml")
print(f"  THIRD-PARTY-LICENSES.md covers all {len(want)} of them")
PY
rm -f "$deps"

