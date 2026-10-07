"""Round 38: what was looked at AFTER the runs were in. Not part of the rule.

    posthoc.py <runs dir> <key.json> <verdicts judge 1> <verdicts judge 2>

`analyze.py` was committed before any run existed and computes what Part 1 fixed. This file was
written with the results in hand, to explain them: which evidence item each agent did not retrieve,
which statement of the key was not conveyed, what the failed steps were, and whether the one cell
that missed the `describe` rule differs for a command typed the same way in both conditions.
Nothing here can rescue or sink a rule; it is printed apart and labelled.
"""
import collections, glob, json, os, re, sys

runs_dir, key_path, v1_path, v2_path = sys.argv[1:5]
runs = [json.load(open(f)) for f in sorted(glob.glob(os.path.join(runs_dir, "*", "*.json")))]
key = json.load(open(key_path))
by_run = {run: pid for pid, run in key.items()}
V = {"judge 1": json.load(open(v1_path)), "judge 2": json.load(open(v2_path))}
for r in runs:
    r["agent"] = r["transcript"]["agent"]
    r["v"] = {j: v.get(by_run.get(r["run_id"])) for j, v in V.items()}

agents = sorted({r["agent"] for r in runs}); cases = sorted({r["case"] for r in runs}); conds = ("live", "frozen")
def sel(agent=None, cond=None, case=None):
    return [r for r in runs if (agent is None or r["agent"] == agent) and (cond is None or r["condition"] == cond) and (case is None or r["case"] == case)]

print("Post hoc: computed after the results were known. None of it is a rule of Part 1.\n")

print("### Each decisive evidence item, retrieved in how many runs\n")
print("| case | evidence pattern | " + " | ".join(f"{a} {c}" for a in agents for c in conds) + " |")
print("|---|---|" + "---|" * (len(agents) * 2))
tot = collections.Counter(); den = collections.Counter()
for case in cases:
    for pat in sel(case=case)[0]["process"]["evidence_retrieved"]:
        cells = []
        for a in agents:
            for c in conds:
                rs = sel(a, c, case); k = sum(r["process"]["evidence_retrieved"][pat] for r in rs)
                tot[(a, c)] += k; den[(a, c)] += len(rs); cells.append(f"{k}/{len(rs)}")
        print(f"| {case} | `{pat}` | " + " | ".join(cells) + " |")
print("| **all items** | | " + " | ".join(f"{tot[(a, c)]}/{den[(a, c)]}" for a in agents for c in conds) + " |")

print("\n### Each statement of the key, conveyed in how many answers\n")
print("Live and frozen together, twelve answers a cell. `expected` statements in the order of `case.yaml`; then answers that did a `must_not`.\n")
print("| case | agent | judge 1: expected | judge 1: must_not done | judge 2: expected | judge 2: must_not done |")
print("|---|---|---|---|---|---|")
for case in cases:
    for a in agents:
        cell = []
        for j in V:
            vs = [r["v"][j] for r in sel(a, None, case) if r["v"][j]]
            n = len(vs[0]["expected"])
            cell.append(" · ".join(f"{sum(v['expected'][i] for v in vs)}/{len(vs)}" for i in range(n)))
            cell.append(f"{sum(any(v['must_not']) for v in vs)}/{len(vs)}")
        print(f"| {case} | {a} | " + " | ".join(cell) + " |")

print("\nAnswers that did a `must_not` according to either judge, by condition:\n")
print("| case | " + " | ".join(f"{a} {c}" for a in agents for c in conds) + " |")
print("|---|" + "---|" * (len(agents) * 2))
for case in cases:
    cells = []
    for a in agents:
        for c in conds:
            rs = sel(a, c, case)
            cells.append(f"{sum(any(any(r['v'][j]['must_not']) for j in V if r['v'][j]) for r in rs)}/{len(rs)}")
    print(f"| {case} | " + " | ".join(cells) + " |")

print("\n### R3, for each judge alone\n")
print("| judge | passes with all decisive evidence | passes without |")
print("|---|---|---|")
for j in V:
    w = [r for r in runs if r["v"][j] and r["v"][j]["verdict"] == "PASS"]
    print(f"| {j} | {sum(r['process']['evidence_all'] for r in w)} | {sum(not r['process']['evidence_all'] for r in w)} |")
print("\nWhere the judges disagree:\n")
for r in runs:
    a, b = r["v"]["judge 1"], r["v"]["judge 2"]
    if a and b and a["verdict"] != b["verdict"]:
        miss = [p for p, ok in r["process"]["evidence_retrieved"].items() if not ok]
        print(f"- `{r['run_id']}` — not retrieved: {', '.join('`' + m + '`' for m in miss) or 'nothing'}")
        print(f"  - judge 1 {a['verdict']} {a['expected']}: {a['reason']}")
        print(f"  - judge 2 {b['verdict']} {b['expected']}: {b['reason']}")

flags = collections.Counter(); total = 0
for r in runs:
    a, b = r["v"]["judge 1"], r["v"]["judge 2"]
    if not (a and b): continue
    for part, done_is_strict in (("expected", False), ("must_not", True)):
        for i, (x, y) in enumerate(zip(a[part], b[part])):
            total += 1
            if x != y:
                flags[(r["case"], f"{part} {i + 1}", "judge 1" if (x == done_is_strict) else "judge 2")] += 1
print(f"\nStatement by statement, the two judges differ on {sum(flags.values())} of {total} rulings: " +
      "; ".join(f"{case}, {what}: {n}, {who} the stricter" for (case, what, who), n in sorted(flags.items())) + ".")

print("\n### `kubectl describe pod`: the same command in both conditions\n")
print("The rule of Part 1 compares the mean over whatever each condition's runs happened to describe. Here only a "
      "command typed character for character the same in a live run and in a frozen run of the same case and agent is compared.\n")
describe = re.compile(r"kubectl\s+describe\s+pods?\b")
print("| case | agent | commands in both | frozen / live, lowest to highest | exactly the same length |")
print("|---|---|---|---|---|")
pairs = same = 0
for case in cases:
    for a in agents:
        size = {c: collections.defaultdict(list) for c in conds}
        for c in conds:
            for r in sel(a, c, case):
                for s in r["transcript"]["steps"]:
                    if describe.search(s["input"]) and not s["error"]:
                        size[c][" ".join(s["input"].split())].append(len(s["output"]))
        both = sorted(set(size["live"]) & set(size["frozen"]))
        if not both: continue
        ratios = [(sum(size["frozen"][k]) / len(size["frozen"][k])) / (sum(size["live"][k]) / len(size["live"][k])) for k in both]
        eq = sum(set(size["frozen"][k]) == set(size["live"][k]) for k in both)
        pairs += len(both); same += eq
        print(f"| {case} | {a} | {len(both)} | {min(ratios):.3f} to {max(ratios):.3f} | {eq} |")
print(f"\nCommands compared: {pairs}; with the same output length in both conditions: {same}. "
      "(`describe` prints ages, which kubectl counts from the wall clock; a few bytes move with the minute.)")

print("\nWhat the cell outside the band described, by the shape of the command:\n")
print("| case | agent | condition | one named pod | several pods or a selector | piped or cut down (`| head`, `| grep`, …) |")
print("|---|---|---|---|---|---|")
for case in cases:
    for a in agents:
        row = {}
        for c in conds:
            kinds = collections.defaultdict(list)
            for r in sel(a, c, case):
                for s in r["transcript"]["steps"]:
                    if not describe.search(s["input"]) or s["error"]: continue
                    cmd = s["input"]
                    k = "piped" if re.search(r"\|\s*(head|tail|grep|sed|awk|cut)\b", cmd) else ("many" if re.search(r"\s-l\s|--selector|\s-A\b|describe\s+pods?\s+(-n\s+\S+\s*)?($|[|;&])", cmd) else "one")
                    kinds[k].append(len(s["output"]))
            row[c] = kinds
        if not any(row[c] for c in conds): continue
        for c in conds:
            f = lambda v: f"{len(v)} steps, mean {sum(v) / len(v):.0f}" if v else "—"
            print(f"| {case} | {a} | {c} | {f(row[c]['one'])} | {f(row[c]['many'])} | {f(row[c]['piped'])} |")

print("\n### Failed steps, by what the output says\n")
kinds = [("refused by the agent's own harness", r"requires approval|permission|not allowed|blocked for security|was blocked|not auto-executed|haven't granted|denied"),
         ("`kubectl top`: no metrics API", r"Metrics API not available|metrics\.k8s\.io"),
         ("not found", r"NotFound|not found"),
         ("unknown flag or bad usage", r"unknown flag|unknown shorthand|unknown command|See 'kubectl|invalid argument"),
         ("the shell: no matches, parse error", r"no matches found|parse error|syntax error"),
         ("no result from a metrics query", r"no result|No data|empty result"),
         ("refused by lapilli-case", r"refused by lapilli-case")]
count = collections.Counter()
for r in runs:
    for s in r["transcript"]["steps"]:
        if not s["error"]: continue
        for name, rx in kinds:
            if re.search(rx, s["output"], re.I): break
        else: name = "other"
        count[(r["agent"], r["condition"], name)] += 1
print("| what | " + " | ".join(f"{a} {c}" for a in agents for c in conds) + " |")
print("|---|" + "---|" * (len(agents) * 2))
for name in [k for k, _ in kinds] + ["other"]:
    row = [count[(a, c, name)] for a in agents for c in conds]
    if any(row): print(f"| {name} | " + " | ".join(map(str, row)) + " |")
print("| **all** | " + " | ".join(str(sum(n for (a2, c2, _), n in count.items() if (a2, c2) == (a, c))) for a in agents for c in conds) + " |")

print("\nWhat the agent's own harness refused, by the first `kubectl` verb of the command (a command with no `kubectl` is `(shell)`):\n")
refused = collections.Counter()
for r in runs:
    for s in r["transcript"]["steps"]:
        if s["error"] and re.search(kinds[0][1], s["output"], re.I):
            m = re.search(r"\bkubectl\s+(?:-\S+\s+\S+\s+)*([a-z-]+)", s["input"])
            refused[(r["agent"], r["condition"], m.group(1) if m else "(shell)")] += 1
print("| verb | " + " | ".join(f"{a} {c}" for a in agents for c in conds) + " |")
print("|---|" + "---|" * (len(agents) * 2))
for verb in sorted({v for _, _, v in refused}, key=lambda v: (-sum(n for (_, _, v2), n in refused.items() if v2 == v), v)):
    print(f"| `{verb}` | " + " | ".join(str(refused[(a, c, verb)]) for a in agents for c in conds) + " |")

print("\nOf what Claude Code refused, by what the command was:\n")
shape_of = [("`kubectl exec`", r"\bkubectl\s+(-n\s+\S+\s+)?exec\b"), ("`kubectl debug`", r"\bkubectl\s+(-n\s+\S+\s+)?debug\b"), ("`kubectl port-forward`", r"\bkubectl\s+(-n\s+\S+\s+)?port-forward\b"),
            ("`kubectl run`", r"\bkubectl\s+(-n\s+\S+\s+)?run\b"), ("`kubectl rollout history`: a read the guard allows", r"\bkubectl\s+rollout\s+history\b"),
            ("a read with a filter in `-o custom-columns`: `[?(@.type==\"Ready\")]`", r"custom-columns.*\?\("), ("a read piped into `awk`", r"\bkubectl\b.*\|\s*awk\b"), ("a read piped into `xargs`", r"\bkubectl\b.*\|\s*xargs\b")]
shapes = collections.Counter()
for r in sel("claude-code"):
    for s in r["transcript"]["steps"]:
        if s["error"] and re.search(kinds[0][1], s["output"], re.I):
            shapes[(next((name for name, rx in shape_of if re.search(rx, s["input"], re.S)), "a shell command that is not kubectl"), r["condition"])] += 1
print("| what | live | frozen |")
print("|---|---|---|")
for name in [n for n, _ in shape_of] + ["a shell command that is not kubectl"]:
    if shapes[(name, "live")] + shapes[(name, "frozen")]:
        print(f"| {name} | {shapes[(name, 'live')]} | {shapes[(name, 'frozen')]} |")

print("\n### The mix of commands, live against frozen\n")
print("Steps of each kind, summed over the eighteen runs of a cell. `p` is a two-sided permutation test on the per-run counts "
      "(50,000 shuffles, fixed seed). These are many comparisons looked at after the fact and **not corrected for that**.\n")
import random
random.seed(38)
def perm(x, y, n=50000):
    obs = abs(sum(x) / len(x) - sum(y) / len(y)); z = x + y; k = 0
    for _ in range(n):
        random.shuffle(z)
        k += abs(sum(z[:len(x)]) / len(x) - sum(z[len(x):]) / len(y)) >= obs - 1e-12
    return k / n
piped = re.compile(r"\|\s*(head|tail|grep|sed|awk|cut|sort|uniq|wc|jq)\b")
what = [("all steps", lambda s: True),
        ("failed steps", lambda s: bool(s["error"])),
        ("`kubectl get`", lambda s: bool(re.search(r"\bkubectl\s+get\b", s["input"]))),
        ("`kubectl logs` or a log tool", lambda s: bool(re.search(r"\bkubectl\s+logs\b", s["input"])) or s.get("tool") == "fetch_pod_logs"),
        ("`kubectl describe`, anything", lambda s: bool(re.search(r"\bkubectl\s+describe\b", s["input"]))),
        ("`kubectl describe pod`", lambda s: bool(describe.search(s["input"]))),
        ("a metrics query", lambda s: "promq" in s["input"] or "prometheus" in (s.get("tool") or "") or (s.get("tool") or "").startswith("get_")),
        ("piped through a filter", lambda s: bool(piped.search(s["input"])))]
print("| agent | what | live | frozen | p |")
print("|---|---|---|---|---|")
looks = 0; smallest = (1.0, "")
for a in agents:
    for name, f in what:
        x = {c: [sum(f(s) for s in r["transcript"]["steps"]) for r in sel(a, c)] for c in conds}
        if sum(x["live"]) + sum(x["frozen"]) < 10: continue
        p = perm(x["live"], x["frozen"]); looks += 1
        if p < smallest[0]: smallest = (p, f"{a}, {name}")
        print(f"| {a} | {name} | {sum(x['live'])} | {sum(x['frozen'])} | {p:.2f} |")
print(f"\nComparisons: {looks}. Smallest p: {smallest[0]:.3f} ({smallest[1]}). "
      f"With {looks} looks and nothing going on, at least one this small turns up about {100 * (1 - (1 - smallest[0]) ** looks):.0f}% of the time.")

print("\n### Answers only the replay gives, that are not errors\n")
print("`analyze.py` searched for errors only the replay can produce and found none. These are not errors: the step succeeds and says something a live cluster does not.\n")
sort_by = re.compile(r"--sort-by[= ]\s*['\"]?\{?\.?([A-Za-z0-9_.\[\]]+)")
sorted_list = collections.Counter(); wide = collections.Counter(); hit = set()
for r in runs:
    for s in r["transcript"]["steps"]:
        if s["error"]: continue
        cmd = s["input"]
        m = sort_by.search(cmd)
        if m and re.search(r"\bkubectl\s+get\b", cmd):
            field = "a field under `metadata`" if m.group(1).startswith("metadata") else "any other field"
            empty = "No resources found" in s["output"]
            sorted_list[(field, r["condition"], "empty" if empty else "answered")] += 1
            if empty: hit.add(r["run_id"])
        if re.search(r"\bkubectl\s+get\s+(pods?|po)\b[^|;&\n]*-o\s*wide", cmd) and re.search(r"^.*\bNAME\b.*\bAGE\b", s["output"], re.M):
            wide[(r["agent"], r["condition"], "with NODE" if re.search(r"\bNAME\b.*\bNODE\b", s["output"]) else "without")] += 1
print("| `kubectl get … --sort-by=` | live: answered | live: \"No resources found\" | frozen: answered | frozen: \"No resources found\" |")
print("|---|---|---|---|---|")
for field in ("a field under `metadata`", "any other field"):
    print(f"| {field} | " + " | ".join(str(sorted_list[(field, c, k)]) for c in conds for k in ("answered", "empty")) + " |")
print(f"\nRuns that were given an empty sorted list: " + ", ".join(f"{a} {c} {sum(r['run_id'] in hit for r in sel(a, c))}/{len(sel(a, c))}" for a in agents for c in conds) + ".")
both = lambda r: all(r["v"][j] and r["v"][j]["verdict"] == "PASS" for j in V)
for a in agents:
    given = [r for r in sel(a, "frozen") if r["run_id"] in hit]; rest = [r for r in sel(a, "frozen") if r["run_id"] not in hit]
    print(f"- {a}, frozen: the {len(given)} runs given one passed {sum(map(both, given))} times and retrieved all decisive evidence {sum(r['process']['evidence_all'] for r in given)} times; "
          f"the {len(rest)} not given one, {sum(map(both, rest))} and {sum(r['process']['evidence_all'] for r in rest)}.")
print("\n| `kubectl get pods -o wide` | " + " | ".join(f"{a} {c}" for a in agents for c in conds) + " |")
print("|---|" + "---|" * (len(agents) * 2))
for k in ("with NODE", "without"):
    print(f"| listing {k} column | " + " | ".join(str(wide[(a, c, k)]) for a in agents for c in conds) + " |")

alias = {"po": "pods", "pod": "pods", "deploy": "deployments", "deployment": "deployments", "rs": "replicasets", "replicaset": "replicasets",
         "svc": "services", "service": "services", "cm": "configmaps", "configmap": "configmaps", "ev": "events", "event": "events",
         "node": "nodes", "no": "nodes", "ns": "namespaces", "namespace": "namespaces", "ep": "endpoints", "ds": "daemonsets", "daemonset": "daemonsets"}
head = collections.defaultdict(lambda: collections.defaultdict(collections.Counter))
for r in runs:
    for s in r["transcript"]["steps"]:
        if s["error"]: continue
        cmd = s["input"].strip().split("\n")[0]
        m = re.match(r"^kubectl\s+get\s+([a-z]+)\b(.*)$", cmd)
        if not m or re.search(r"-o\s*(json|yaml|name|jsonpath|custom-columns|go-template)|\|\s*(?!head|tail)\w|--no-headers|--show-labels|\s-L\s", m.group(2)): continue
        out = s["output"]
        if r["agent"] == "holmes": out = out.split("\n", 1)[1] if "\n" in out else ""     # its bash tool echoes the command first
        lines = [l for l in out.split("\n") if l.strip()]
        if not lines: continue
        h = re.sub(r"\s{2,}", " · ", lines[0].strip())
        if re.match(r"^[A-Z][A-Z0-9 ·()/-]+$", h):
            head[(alias.get(m.group(1), m.group(1)), "-o wide" if re.search(r"-o\s*wide", m.group(2)) else "")][r["condition"]][h] += 1
print("\nThe header line of `kubectl get <kind>` alone on a line, where it was typed in both conditions and the headers differ:\n")
print("| kind | live | frozen |")
print("|---|---|---|")
for (kind, w), d in sorted(head.items()):
    if d["live"] and d["frozen"] and set(d["live"]) != set(d["frozen"]):
        print(f"| {kind} {w} | " + " | ".join("<br>".join(f"{n}× `{h}`" for h, n in d[c].most_common()) for c in conds) + " |")
print("\nTyped in the frozen condition only, with a header a live cluster does not print: " +
      "; ".join(f"{kind} {w}".strip() + " — " + ", ".join(f"`{h}`" for h in d["frozen"]) for (kind, w), d in sorted(head.items()) if not d["live"] and kind in ("replicasets", "endpoints")) + ".")

signs = [("an empty sorted list", lambda s: bool(sort_by.search(s["input"])) and "No resources found" in s["output"]),
         ("a pod listing asked for `-o wide`, without NODE", lambda s: bool(re.search(r"\bkubectl\s+get\s+(pods?|po)\b[^|;&\n]*-o\s*wide", s["input"])) and bool(re.search(r"^.*\bNAME\b.*\bAGE\b", s["output"], re.M)) and not re.search(r"\bNAME\b.*\bNODE\b", s["output"])),
         ("a listing whose whole header is `NAME AGE`", lambda s: bool(re.search(r"\bkubectl\s+get\b", s["input"])) and bool(re.search(r"^NAME\s+AGE\s*$", s["output"], re.M))),
         ("an event listing headed `LASTTIMESTAMP`", lambda s: bool(re.search(r"^LASTTIMESTAMP\s+TYPE\b", s["output"], re.M)))]
print("\nSteps that show one of these, and the runs they are in:\n")
print("| what the step shows | live steps | live runs | frozen steps | frozen runs |")
print("|---|---|---|---|---|")
any_step = collections.Counter(); any_run = collections.defaultdict(set)
for name, f in signs + [("**any of them**", lambda s: any(g(s) for _, g in signs))]:
    cell = []
    for c in conds:
        n = 0; rs = set()
        for r in sel(cond=c):
            k = sum(not s["error"] and f(s) for s in r["transcript"]["steps"])
            n += k
            if k: rs.add(r["run_id"])
        cell += [str(n), f"{len(rs)}/{len(sel(cond=c))}"]
    print(f"| {name} | " + " | ".join(cell) + " |")

get_all = collections.Counter()
for r in runs:
    for s in r["transcript"]["steps"]:
        if re.search(r"\bkubectl\s+get\s+all\b", s["input"]) and not s["error"]:
            get_all[(r["condition"], bool(re.search(r"^(pod|service|deployment\.apps)/", s["output"], re.M)))] += 1
print(f"\n`kubectl get all`: names written with their kind (`pod/…`, `deployment.apps/…`) in {get_all[('live', True)]} of {get_all[('live', True)] + get_all[('live', False)]} live listings "
      f"and {get_all[('frozen', True)]} of {get_all[('frozen', True)] + get_all[('frozen', False)]} frozen ones.")

dp = {True: [], False: []}
for r in sel("claude-code", "frozen"):
    no_node = any(not s["error"] and signs[1][1](s) for s in r["transcript"]["steps"])
    dp[no_node].append(sum(bool(describe.search(s["input"])) and not s["error"] for s in r["transcript"]["steps"]))
live_dp = [sum(bool(describe.search(s["input"])) and not s["error"] for s in r["transcript"]["steps"]) for r in sel("claude-code", "live")]
mean = lambda v: f"{sum(v) / len(v):.1f}" if v else "—"
print(f"\n`kubectl describe pod` steps a run, claude-code: live {mean(live_dp)} ({len(live_dp)} runs); frozen runs given a wide pod listing without its NODE column {mean(dp[True])} ({len(dp[True])} runs); "
      f"other frozen runs {mean(dp[False])} ({len(dp[False])} runs); frozen runs given an empty sorted list "
      f"{mean([sum(bool(describe.search(s['input'])) and not s['error'] for s in r['transcript']['steps']) for r in sel('claude-code', 'frozen') if r['run_id'] in hit])}, the others "
      f"{mean([sum(bool(describe.search(s['input'])) and not s['error'] for s in r['transcript']['steps']) for r in sel('claude-code', 'frozen') if r['run_id'] not in hit])}.")

print("\n### What round 37 §9 repaired, as the agents used it\n")
tail = collections.Counter(); over = 0; fsel = collections.Counter()
for r in runs:
    for s in r["transcript"]["steps"]:
        cmd = s["input"]
        m = re.search(r"kubectl\s+logs\b[^|;&\n]*--tail[= ](\d+)", cmd)
        if m and "|" not in cmd and ";" not in cmd and "&&" not in cmd and not s["error"]:
            tail[r["condition"]] += 1
            over += len(s["output"].rstrip("\n").split("\n")) > int(m.group(1)) + 2   # a harness may add a line of its own
        if "--field-selector" in cmd:
            fsel[(r["agent"], r["condition"], "failed" if s["error"] else "answered")] += 1
print(f"- `kubectl logs --tail=N` alone on a line: {tail['live']} live steps, {tail['frozen']} frozen; returning more than N lines: {over}.")
print("- steps with `--field-selector`: " + ("; ".join(f"{a} {c} {k}: {n}" for (a, c, k), n in sorted(fsel.items())) or "none") + ".")

print("\n### Times after the freeze\n")
import datetime
committed, corrected = re.compile(r"\b20\d\d-\d\d-\d\d\b"), re.compile(r"(?<!\d)20\d\d-\d\d-\d\d(?!\d)")
frozen_cases = os.path.join(runs_dir, "frozen-cases")
n_committed = n_corrected = with_date = 0; days = collections.Counter(); late = []
for r in sel(cond="frozen"):
    answer = r["transcript"]["answer"]
    at = json.load(open(os.path.join(frozen_cases, r["case"], "freeze.json")))["frozen_at"]
    freeze = datetime.datetime.strptime(at, "%Y-%m-%dT%H:%M:%SZ")
    n_committed += len(committed.findall(answer)); found = corrected.findall(answer)
    n_corrected += len(found); with_date += bool(found); days.update(found)
    for clock in re.findall(r"(?<![\d:])(\d\d:\d\d:\d\d)(?![\d:])", answer):
        if datetime.datetime.strptime(at[:10] + " " + clock, "%Y-%m-%d %H:%M:%S") > freeze:
            late.append(f"`{r['run_id']}` says {clock}; frozen at {at[11:19]}")
print(f"- The pattern committed in `analyze.py` matches {n_committed} dates in the 36 frozen answers: it needs a word boundary after the date, and `2026-10-07T09:17` has none. "
      f"Without that, {n_corrected} dates in {with_date} answers, on {', '.join(f'{d} ({n})' for d, n in sorted(days.items()))}.")
print(f"- Times of day later than the freeze, in a frozen answer: {len(late)}." + ("".join("\n  - " + x for x in late)))
stamped = collections.Counter()
for r in runs:
    stamped[(r["agent"], r["condition"])] += any("Query executed at:" in s["output"] for s in r["transcript"]["steps"])
print("- Runs in which a tool of the agent's own stamped its answer with the wall clock (`Query executed at:`): " + ", ".join(f"{a} {c} {stamped[(a, c)]}/{len(sel(a, c))}" for a in agents for c in conds) + ".")

print("\n### Time and cost\n")
print("| agent | condition | mean seconds a run | mean model calls | mean cost a run |")
print("|---|---|---|---|---|")
for a in agents:
    for c in conds:
        us = [r["transcript"].get("usage") or {} for r in sel(a, c)]
        n = len(us)
        print(f"| {a} | {c} | {sum(float(u.get('seconds') or 0) for u in us) / n:.0f} | {sum(float(u.get('llm_calls') or 0) for u in us) / n:.1f} | ${sum(float(u.get('cost_usd') or 0) for u in us) / n:.3f} |")
print(f"\nRuns that ended without an answer: {sum(not r['transcript'].get('answer') or bool(r['transcript'].get('error')) for r in runs)}.")
