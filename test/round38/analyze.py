"""Round 38: the numbers of Part 2, computed from the run records and the two judges' verdicts.

    analyze.py <runs dir> <key.json> <verdicts judge 1> <verdicts judge 2> [<frozen cases dir>] [<excluded dir>]

Prints Markdown. Everything it prints can be recomputed from the files it is given.
"""
import collections, glob, json, math, os, re, sys

runs_dir, key_path, v1_path, v2_path = sys.argv[1:5]
frozen_dir = sys.argv[5] if len(sys.argv) > 5 else None
excluded_dir = sys.argv[6] if len(sys.argv) > 6 else None

runs = [json.load(open(f)) for f in sorted(glob.glob(os.path.join(runs_dir, "*", "*.json")))]
key = json.load(open(key_path))                       # packet id -> run id
by_run = {run: pid for pid, run in key.items()}
v1, v2 = json.load(open(v1_path)), json.load(open(v2_path))

def verdict(v, run_id):
    pid = by_run.get(run_id)
    return None if pid is None or pid not in v else v[pid]["verdict"] == "PASS"

for r in runs:
    r["j1"], r["j2"] = verdict(v1, r["run_id"]), verdict(v2, r["run_id"])
    r["pass"] = None if r["j1"] is None or r["j2"] is None else (r["j1"] and r["j2"])   # passes if both judges pass it
    r["agent"] = r["transcript"]["agent"]

def wilson(k, n, z=1.959964):
    if n == 0: return (0.0, 1.0)
    p = k / n; d = 1 + z * z / n
    c = (p + z * z / (2 * n)) / d; h = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n)) / d
    return (max(0.0, c - h), min(1.0, c + h))

def newcombe(k1, n1, k2, n2):
    """95% interval for p1 - p2 (Newcombe 1998, method 10)."""
    p1, p2 = k1 / n1, k2 / n2
    l1, u1 = wilson(k1, n1); l2, u2 = wilson(k2, n2)
    d = p1 - p2
    return d, d - math.sqrt((p1 - l1) ** 2 + (u2 - p2) ** 2), d + math.sqrt((u1 - p1) ** 2 + (p2 - l2) ** 2)

def kappa(pairs):
    n = len(pairs)
    if n == 0: return float("nan"), 0, 0
    agree = sum(a == b for a, b in pairs) / n
    pa, pb = sum(a for a, _ in pairs) / n, sum(b for _, b in pairs) / n
    pe = pa * pb + (1 - pa) * (1 - pb)
    return ((agree - pe) / (1 - pe) if pe < 1 else float("nan")), agree, n

def sel(agent=None, cond=None, case=None):
    return [r for r in runs if (agent is None or r["agent"] == agent) and (cond is None or r["condition"] == cond) and (case is None or r["case"] == case)]

agents = sorted({r["agent"] for r in runs}); cases = sorted({r["case"] for r in runs})
pct = lambda x: f"{100 * x:+.0f}"

print(f"runs: {len(runs)} | judged by both: {sum(r['pass'] is not None for r in runs)} | agents: {', '.join(agents)}\n")

print("### Counts\n")
print("| agent | condition | runs | pass (both judges) | judge 1 | judge 2 | all decisive evidence retrieved | mean steps | failed steps | cited, not observed | cost |")
print("|---|---|---|---|---|---|---|---|---|---|---|")
for a in agents + ["both"]:
    for c in ("live", "frozen"):
        rs = sel(None if a == "both" else a, c); n = len(rs)
        if not n: continue
        j = [r for r in rs if r["pass"] is not None]
        cost = sum(float((r["transcript"].get("usage") or {}).get("cost_usd") or 0) for r in rs)
        print(f"| {a} | {c} | {n} | {sum(r['pass'] for r in j)}/{len(j)} | {sum(r['j1'] for r in j)}/{len(j)} | {sum(r['j2'] for r in j)}/{len(j)} | "
              f"{sum(r['process']['evidence_all'] for r in rs)}/{n} | {sum(r['process']['steps'] for r in rs) / n:.1f} | {sum(r['process']['failed_steps'] for r in rs)} | "
              f"{sum(len(r['process']['ungrounded_entities']) for r in rs)} | ${cost:.2f} |")

print("\n### R1 — live minus frozen, with a 95% interval\n")
print("| agent | measure | live | frozen | difference (points) | 95% interval | half-width | reading |")
print("|---|---|---|---|---|---|---|---|")
for a in agents + ["both"]:
    for name, f, judged in (("pass, both judges", lambda r: r["pass"], True), ("all decisive evidence retrieved", lambda r: r["process"]["evidence_all"], False)):
        live = [r for r in sel(None if a == "both" else a, "live") if not judged or r["pass"] is not None]
        froz = [r for r in sel(None if a == "both" else a, "frozen") if not judged or r["pass"] is not None]
        if not live or not froz: continue
        k1, k2 = sum(bool(f(r)) for r in live), sum(bool(f(r)) for r in froz)
        d, lo, hi = newcombe(k1, len(live), k2, len(froz))
        reading = "not distinguished" if lo <= 0 <= hi else "**a difference**"
        print(f"| {a} | {name} | {k1}/{len(live)} | {k2}/{len(froz)} | {pct(d)} | {pct(lo)} to {pct(hi)} | {100 * (hi - lo) / 2:.0f} | {reading} |")

print("\n### The two judges\n")
print("| | packets | agree | Cohen's kappa | judge 1 passes | judge 2 passes |")
print("|---|---|---|---|---|---|")
for a in agents + ["both"]:
    j = [r for r in sel(None if a == "both" else a) if r["pass"] is not None]
    k, agree, n = kappa([(r["j1"], r["j2"]) for r in j])
    if n: print(f"| {a} | {n} | {100 * agree:.0f}% | {k:.2f} | {sum(r['j1'] for r in j)} | {sum(r['j2'] for r in j)} |")
dis = [r for r in runs if r["pass"] is not None and r["j1"] != r["j2"]]
if dis:
    print("\nDisagreements: " + "; ".join(f"`{r['run_id']}` (judge 1 {'PASS' if r['j1'] else 'FAIL'}, judge 2 {'PASS' if r['j2'] else 'FAIL'})" for r in dis))

print("\n### By case — reported, not claimed\n")
print("| case | agent | live: pass · evidence | frozen: pass · evidence |")
print("|---|---|---|---|")
for case in cases:
    for a in agents:
        cell = []
        for c in ("live", "frozen"):
            rs = sel(a, c, case); j = [r for r in rs if r["pass"] is not None]
            cell.append(f"{sum(r['pass'] for r in j)}/{len(j)} · {sum(r['process']['evidence_all'] for r in rs)}/{len(rs)}" if rs else "—")
        print(f"| {case} | {a} | {cell[0]} | {cell[1]} |")

print("\n### R2 — `kubectl describe pod`, output length\n")
print("| case | agent | live: steps, mean bytes | frozen: steps, mean bytes | frozen / live | within 0.8–1.25 |")
print("|---|---|---|---|---|---|")
describe = re.compile(r"kubectl\s+describe\s+pods?\b")
ruled = failed = 0
for case in cases:
    for a in agents:
        size = {}
        for c in ("live", "frozen"):
            size[c] = [len(s["output"]) for r in sel(a, c, case) for s in r["transcript"]["steps"] if describe.search(s["input"]) and not s["error"]]
        if not size["live"] and not size["frozen"]: continue
        m = {c: (sum(v) / len(v) if v else float("nan")) for c, v in size.items()}
        if len(size["live"]) >= 3 and len(size["frozen"]) >= 3:
            ratio = m["frozen"] / m["live"]; ok = 0.8 <= ratio <= 1.25; ruled += 1; failed += not ok
            print(f"| {case} | {a} | {len(size['live'])}, {m['live']:.0f} | {len(size['frozen'])}, {m['frozen']:.0f} | {ratio:.2f} | {'yes' if ok else '**no**'} |")
        else:
            print(f"| {case} | {a} | {len(size['live'])}, {m['live']:.0f} | {len(size['frozen'])}, {m['frozen']:.0f} | — | too few steps to rule |")
print(f"\nCells with at least three such steps in each condition: {ruled}; outside the band: {failed}.")

print("\n### R2 — failures only the replay can produce\n")
replay_only = re.compile(r"Bad Gateway|unexpected EOF|invalid field selector|unable to decode|the server rejected our request|Internal error occurred|proxy error|"
                         r"couldn't get resource list|unknown \(get|is not served by a frozen store|connection refused|context deadline exceeded", re.I)
hits = collections.Counter(); guard = collections.Counter()
for r in runs:
    for s in r["transcript"]["steps"]:
        if "refused by lapilli-case" in s["output"]:
            m = re.search(r"refused by lapilli-case: ([^\n]{0,90})", s["output"]); guard[(r["condition"], m.group(1) if m else "?")] += 1
        m = replay_only.search(s["output"])
        if m and (s["error"] or len(s["output"]) < 600):
            hits[(r["condition"], m.group(0).lower())] += 1
print("| what | live | frozen |"); print("|---|---|---|")
for what in sorted({w for _, w in hits}):
    print(f"| `{what}` | {hits[('live', what)]} | {hits[('frozen', what)]} |")
if not hits: print("| (none of the patterns searched for) | 0 | 0 |")
print("\nGuard refusals:\n")
print("| refused | live | frozen |"); print("|---|---|---|")
for what in sorted({w for _, w in guard}):
    print(f"| {what} | {guard[('live', what)]} | {guard[('frozen', what)]} |")
if not guard: print("| (none) | 0 | 0 |")

if frozen_dir:
    print("\n### R2 — dates after the freeze in a frozen answer\n")
    late = []
    for r in sel(cond="frozen"):
        try: day = json.load(open(os.path.join(frozen_dir, r["case"], "freeze.json")))["frozen_at"][:10]
        except OSError: continue
        dates = sorted(set(re.findall(r"\b20\d\d-\d\d-\d\d\b", r["transcript"]["answer"])))
        after = [d for d in dates if d > day]
        if after: late.append(f"`{r['run_id']}` (frozen {day}): {', '.join(after)}")
    print(f"Frozen answers naming a date after the day of their freeze: {len(late)}." + ("\n\n- " + "\n- ".join(late) if late else ""))

print("\n### R3 — passing without all the decisive evidence\n")
j = [r for r in runs if r["pass"] is not None]
without = [r for r in j if not r["process"]["evidence_all"]]
print("| all decisive evidence retrieved | runs | pass (both judges) |"); print("|---|---|---|")
print(f"| yes | {len(j) - len(without)} | {sum(r['pass'] for r in j if r['process']['evidence_all'])} |")
print(f"| no | {len(without)} | {sum(r['pass'] for r in without)} |")
for r in without:
    if r["pass"]:
        missing = [p for p, ok in r["process"]["evidence_retrieved"].items() if not ok]
        print(f"\n- `{r['run_id']}` passed without: {missing}")

if excluded_dir:
    ex = collections.Counter()
    for f in glob.glob(os.path.join(excluded_dir, "*", "*.json")):
        r = json.load(open(f)); ex[(r["transcript"]["agent"], r["condition"])] += 1
    print("\n### Excluded as provider failures\n")
    print(", ".join(f"{a} {c}: {n}" for (a, c), n in sorted(ex.items())) or "None.")
