"""How much of a frozen answer the comparison holds it to.

    python3 test/replay-diff/spoil.py <a case's directory under a sweep's output>...

A comparison that passes is worth what it would have failed, and `selftest.py` says so for answers
somebody thought of. This says it for the answers a sweep recorded: for every command whose two live
answers have at least three lines, the frozen answer is spoiled in six ways — replaced by nothing,
cut to its first line, its last line taken off, its last line written twice, its lines turned the
other way up, its first line moved to its end — and judged again against the same two live answers. A spoiled answer that still
passes, as the same, as another order, or as the cluster having moved, is one the comparison cannot
tell from the real one.

It prints a count for each way and the first few commands that pass, and exits 1 if an answer
replaced by nothing or cut to its first line passes: those two nothing should let through.
"""
import json, os, sys

sys.dont_write_bytecode = True
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import replaydiff as rd

lines = lambda out: out.rstrip("\n").split("\n")
SPOILED = [
    ("replaced by nothing", lambda out: ""),
    ("cut to its first line", lambda out: lines(out)[0] + "\n"),
    ("its last line taken off", lambda out: "\n".join(lines(out)[:-1]) + "\n"),
    ("its last line written twice", lambda out: out + lines(out)[-1] + "\n"),
    ("its lines the other way up", lambda out: "\n".join(reversed(lines(out))) + "\n"),
    ("its first line moved to its end", lambda out: "\n".join(lines(out)[1:] + lines(out)[:1]) + "\n"),
]

passed = {name: [] for name, _ in SPOILED}
tried = dict.fromkeys(passed, 0)
for case in sys.argv[1:]:
    before, frozen, after = (json.load(open(os.path.join(case, name + ".json"))) for name in ("live-before", "frozen", "live-after"))
    freeze = json.load(open(os.path.join(case, "frozen", "freeze.json")))["freeze_time"]
    for b, f, a in zip(before, frozen, after):
        if b["rc"] or f["rc"] or a["rc"] or len(lines(b["out"].strip())) < 3 or len(lines(a["out"].strip())) < 3:
            continue
        for name, spoil in SPOILED:
            spoiled = dict(f, out=spoil(f["out"]))
            if spoiled["out"] == f["out"]:
                continue  # nothing to tell apart: a listing that reads the same both ways up
            tried[name] += 1
            verdict = rd.verdict(f["argv"], b, spoiled, a, freeze)[0]
            if verdict in ("same", "order", "moved"):
                passed[name].append(f"[{verdict}] kubectl {' '.join(f['argv'])}")

print("| the frozen answer, spoiled | still passes | of |")
print("|---|---|---|")
for name, _ in SPOILED:
    print(f"| {name} | {len(passed[name])} | {tried[name]} |")
for name, _ in SPOILED:
    if passed[name]:
        print(f"\n{name}, and still passing:\n")
        for line in passed[name][:8]:
            print(f"- `{line}`")
        if len(passed[name]) > 8:
            print(f"- and {len(passed[name]) - 8} more")
sys.exit(1 if passed["replaced by nothing"] or passed["cut to its first line"] else 0)
