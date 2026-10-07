"""Round 39: what Part 1 says is described and not claimed, from the run records and the two judges' verdicts.

    describe.py <runs dir> <key.json> <verdicts judge 1> <verdicts judge 2>

Prints Markdown. Everything it prints can be recomputed from the files it is given.
"""
import collections, glob, json, os, re, sys

runs_dir, key_path, v1_path, v2_path = sys.argv[1:5]
runs = [json.load(open(f)) for f in sorted(glob.glob(os.path.join(runs_dir, "*", "*.json")))]
by_run = {run: pid for pid, run in json.load(open(key_path)).items()}
v1, v2 = json.load(open(v1_path)), json.load(open(v2_path))


def verdict(v, run_id):
    pid = by_run.get(run_id)
    return None if pid is None or pid not in v else v[pid]["verdict"] == "PASS"


for r in runs:
    r["j1"], r["j2"] = verdict(v1, r["run_id"]), verdict(v2, r["run_id"])
    r["pass"] = None if r["j1"] is None or r["j2"] is None else (r["j1"] and r["j2"])

describe = re.compile(r"kubectl\s+describe\s+pods?\b")
cases = sorted({r["case"] for r in runs})
print(f"runs: {len(runs)} | judged by both: {sum(r['pass'] is not None for r in runs)}\n")
print("| case | runs | pass (both judges) | judge 1 | judge 2 | all decisive evidence retrieved | mean steps | failed steps | `describe pod` steps a run | cost |")
print("|---|---|---|---|---|---|---|---|---|---|")
for case in cases + ["all"]:
    rs = [r for r in runs if case in ("all", r["case"])]; n = len(rs); j = [r for r in rs if r["pass"] is not None]
    dp = sum(bool(describe.search(s["input"])) and not s["error"] for r in rs for s in r["transcript"]["steps"])
    cost = sum(float((r["transcript"].get("usage") or {}).get("cost_usd") or 0) for r in rs)
    print(f"| {case} | {n} | {sum(r['pass'] for r in j)}/{len(j)} | {sum(r['j1'] for r in j)}/{len(j)} | {sum(r['j2'] for r in j)}/{len(j)} | "
          f"{sum(r['process']['evidence_all'] for r in rs)}/{n} | {sum(r['process']['steps'] for r in rs) / n:.1f} | {sum(r['process']['failed_steps'] for r in rs)} | {dp / n:.1f} | ${cost:.2f} |")

items = collections.Counter()
for r in runs:
    for name, got in r["process"]["evidence_retrieved"].items():
        items[(r["case"], name, bool(got))] += 1
print("\n| case | evidence item | retrieved |")
print("|---|---|---|")
for case, name in sorted({(c, n) for c, n, _ in items}):
    print(f"| {case} | `{name}` | {items[(case, name, True)]}/{items[(case, name, True)] + items[(case, name, False)]} |")

j = [r for r in runs if r["pass"] is not None]
if j:
    n = len(j); agree = sum(r["j1"] == r["j2"] for r in j) / n
    pa, pb = sum(r["j1"] for r in j) / n, sum(r["j2"] for r in j) / n
    pe = pa * pb + (1 - pa) * (1 - pb)
    print(f"\nThe two judges: {n} packets, agree on {100 * agree:.0f}%, Cohen's kappa {(agree - pe) / (1 - pe) if pe < 1 else float('nan'):.2f}.")
    dis = [r for r in j if r["j1"] != r["j2"]]
    if dis:
        print("Disagreements: " + "; ".join(f"`{r['run_id']}` (judge 1 {'PASS' if r['j1'] else 'FAIL'}, judge 2 {'PASS' if r['j2'] else 'FAIL'})" for r in dis))

guard = collections.Counter(); failed = collections.Counter()
for r in runs:
    for s in r["transcript"]["steps"]:
        if "refused by lapilli-case" in s["output"]:
            m = re.search(r"refused by lapilli-case: ([^\n]{0,90})", s["output"]); guard[m.group(1) if m else "?"] += 1
        elif s["error"]:
            failed[(s["output"].strip().split("\n") or [""])[-1][:110]] += 1
print("\nRefused by the guard:\n")
for what, n in guard.most_common():
    print(f"- {n}× {what}")
if not guard:
    print("- nothing")
print("\nSteps that failed otherwise, by their last line:\n")
for what, n in failed.most_common():
    print(f"- {n}× `{what}`")
if not failed:
    print("- none")

mean = sum(bool(describe.search(s["input"])) and not s["error"] for r in runs for s in r["transcript"]["steps"]) / len(runs) if runs else float("nan")
print(f"\nThe prediction of Part 1: `kubectl describe pod` steps a run below 2.0. It is {mean:.2f}: the prediction {'held' if mean < 2.0 else 'did not hold'}.")
