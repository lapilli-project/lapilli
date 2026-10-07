"""Round 39, R1: the answers round 38 found only the replay gave, counted in a set of run records.

    signs.py [--only <pattern of file names>] <dir of run records>...

A step is counted when it did not fail and shows one of the five signs below. They are the four of
`test/round38/posthoc.py` and its count of `kubectl get all`, copied and not rewritten: round 38
counted them with its results in hand, and this is the same count made a rule beforehand.

An empty sorted list is the one sign a cluster can show too — a namespace with no events sorts to
nothing. So every step counted is listed with its command, and each empty sorted list is asked
again of the same frozen case without `--sort-by`: empty both ways is the cluster's own answer, and
is reported and not counted. That asking is done by hand and written into Part 2.

It exits 1 if any step is counted.
"""
import collections, glob, json, os, re, sys

sort_by = re.compile(r"--sort-by")
SIGNS = [
    ("an empty sorted list", lambda s: bool(sort_by.search(s["input"])) and "No resources found" in s["output"]),
    ("a pod listing asked for `-o wide`, without NODE", lambda s: bool(re.search(r"\bkubectl\s+get\s+(pods?|po)\b[^|;&\n]*-o\s*wide", s["input"])) and bool(re.search(r"^.*\bNAME\b.*\bAGE\b", s["output"], re.M)) and not re.search(r"\bNAME\b.*\bNODE\b", s["output"])),
    ("a listing whose whole header is `NAME AGE`", lambda s: bool(re.search(r"\bkubectl\s+get\b", s["input"])) and bool(re.search(r"^NAME\s+AGE\s*$", s["output"], re.M))),
    ("an event listing headed `LASTTIMESTAMP`", lambda s: bool(re.search(r"^LASTTIMESTAMP\s+TYPE\b", s["output"], re.M))),
    ("`kubectl get all` with names and no kinds", lambda s: bool(re.search(r"\bkubectl\s+get\s+all\b", s["input"])) and not re.search(r"^(pod|service|deployment\.apps)/", s["output"], re.M)),
]

args = sys.argv[1:]
only = "*.json"
if args[:1] == ["--only"]:
    only, args = args[1] + ".json", args[2:]
records = sorted(f for d in args for f in glob.glob(os.path.join(d, only)))
steps = collections.Counter(); runs = collections.defaultdict(set); listed = []
for f in records:
    for i, s in enumerate(json.load(open(f))["transcript"].get("steps") or []):
        if s.get("error"):
            continue
        for name, shows in SIGNS:
            if shows(s):
                steps[name] += 1; runs[name].add(f); runs["any"].add(f)
                listed.append((os.path.relpath(f), i, name, s["input"].strip().split("\n")[0][:200]))

print(f"{len(records)} run records.\n")
print("| what the step shows | steps | runs |")
print("|---|---|---|")
for name, _ in SIGNS:
    print(f"| {name} | {steps[name]} | {len(runs[name])}/{len(records)} |")
print(f"| **any of them** | {sum(steps.values())} | {len(runs['any'])}/{len(records)} |")
if listed:
    print("\nEach step:\n")
    for f, i, name, cmd in listed:
        print(f"- `{f}` step {i}: {name} — `{cmd}`")
sys.exit(1 if listed else 0)
