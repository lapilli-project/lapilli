"""Round 39, after the fact: R1's signs again, with the verb wherever `kubectl` reads it.

    posthoc.py [--only <pattern of file names>] <dir of run records>...

Three of the five signs of `signs.py` look for `kubectl get` with nothing between the two words. In
round 38, where the patterns come from, nothing ever stood there. In round 39 an agent wrote the
namespace first — `kubectl -n shop get pods -o wide` — and a step so written could not be counted
by those three whatever it showed. `signs.py` is the rule as it was fixed and is left as it is; this
is the same count with flags allowed before the verb, made when the results were known, and so an
observation and not the rule.

It prints the count, the steps counted once each (a step can show two signs), and how many of the
steps that did not fail put something between `kubectl` and `get`. It exits 1 if any step is counted.
"""
import collections, glob, json, os, re, sys

LEAD = r"\bkubectl\s+(?:-\S+\s+(?:[^-\s]\S*\s+)??)*?"  # flags, and their values, before the verb
sort_by = re.compile(r"--sort-by")
SIGNS = [
    ("an empty sorted list", lambda s: bool(sort_by.search(s["input"])) and "No resources found" in s["output"]),
    ("a pod listing asked for `-o wide`, without NODE", lambda s: bool(re.search(LEAD + r"get\s+(pods?|po)\b[^|;&\n]*-o\s*wide", s["input"])) and bool(re.search(r"^.*\bNAME\b.*\bAGE\b", s["output"], re.M)) and not re.search(r"\bNAME\b.*\bNODE\b", s["output"])),
    ("a listing whose whole header is `NAME AGE`", lambda s: bool(re.search(LEAD + r"get\b", s["input"])) and bool(re.search(r"^NAME\s+AGE\s*$", s["output"], re.M))),
    ("an event listing headed `LASTTIMESTAMP`", lambda s: bool(re.search(r"^LASTTIMESTAMP\s+TYPE\b", s["output"], re.M))),
    ("`kubectl get all` with names and no kinds", lambda s: bool(re.search(LEAD + r"get\s+all\b", s["input"])) and not re.search(r"^(pod|service|deployment\.apps)/", s["output"], re.M)),
]
between = re.compile(r"\bkubectl\s+-\S+\s+(?:[^-\s]\S*\s+)??(?:-\S+\s+(?:[^-\s]\S*\s+)??)*?get\b")

args = sys.argv[1:]
only = "*.json"
if args[:1] == ["--only"]:
    only, args = args[1] + ".json", args[2:]
records = sorted(f for d in args for f in glob.glob(os.path.join(d, only)))
times = collections.Counter(); runs = collections.defaultdict(set); counted = set(); unseen = 0; ran = 0
for f in records:
    for i, s in enumerate(json.load(open(f))["transcript"].get("steps") or []):
        if s.get("error"):
            continue
        ran += 1
        unseen += bool(between.search(s["input"]))
        for name, shows in SIGNS:
            if shows(s):
                times[name] += 1; runs[name].add(f); runs["any"].add(f); counted.add((f, i))

print(f"{len(records)} run records, {ran} steps that did not fail; in {unseen} of them something stands between `kubectl` and `get`.\n")
print("| what the step shows | times shown | runs |")
print("|---|---|---|")
for name, _ in SIGNS:
    print(f"| {name} | {times[name]} | {len(runs[name])}/{len(records)} |")
print(f"| **any of them** | {sum(times.values())}, in {len(counted)} steps | {len(runs['any'])}/{len(records)} |")
sys.exit(1 if counted else 0)
