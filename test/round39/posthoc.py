"""Round 39, after the fact: R1's signs again, with the verb wherever `kubectl` reads it.

    posthoc.py [--only <pattern of file names>] <dir of run records>...

Three of the five signs of `signs.py` look for `kubectl get` with nothing between the two words. In
round 38, where the patterns come from, nothing ever stood there. In round 39 an agent wrote the
namespace first — `kubectl -n shop get pods -o wide` — and a step so written could not be counted
by those three whatever it showed. `signs.py` is the rule as it was fixed and is left as it is; this
is the same count with flags allowed before the verb, made when the results were known, and so an
observation and not the rule.

`describe.py` has the same fault in its count of `kubectl describe pod` steps, which Part 1 made a
prediction about; that count is made again here too.

It prints the count, the steps counted once each (a step can show two signs), how many of the steps
that did not fail put something between `kubectl` and `get`, and the `describe pod` steps. It exits
1 if any step is counted as showing a sign.

What it still does not see is what `signs.py` does not see for other reasons: `--output wide`, or
`-o wide` before the kind; a flag between `get` and the kind; pods that are not first in a list of
kinds; a step in which another command's answer has the NODE column or a `pod/` line. It is a count
by how a command was spelled, like the one it repeats.
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
describe_as_registered = re.compile(r"kubectl\s+describe\s+pods?\b")  # describe.py's
describe_pod = re.compile(LEAD + r"describe\s+pods?\b")

args = sys.argv[1:]
only = "*.json"
if args[:1] == ["--only"]:
    only, args = args[1] + ".json", args[2:]
records = sorted(f for d in args for f in glob.glob(os.path.join(d, only)))
times = collections.Counter(); runs = collections.defaultdict(set); counted = set(); unseen = 0; ran = 0; described = registered = 0
for f in records:
    for i, s in enumerate(json.load(open(f))["transcript"].get("steps") or []):
        if s.get("error"):
            continue
        ran += 1
        unseen += bool(between.search(s["input"]))
        described += bool(describe_pod.search(s["input"])); registered += bool(describe_as_registered.search(s["input"]))
        for name, shows in SIGNS:
            if shows(s):
                times[name] += 1; runs[name].add(f); runs["any"].add(f); counted.add((f, i))

print(f"{len(records)} run records, {ran} steps that did not fail; in {unseen} of them something stands between `kubectl` and `get`.\n")
print("| what the step shows | times shown | runs |")
print("|---|---|---|")
for name, _ in SIGNS:
    print(f"| {name} | {times[name]} | {len(runs[name])}/{len(records)} |")
print(f"| **any of them** | {sum(times.values())}, in {len(counted)} steps | {len(runs['any'])}/{len(records)} |")
print(f"\n`kubectl describe pod` steps that did not fail: {described}, {described / max(len(records), 1):.2f} a run; with the verb directly after `kubectl`, as `describe.py` looks for it, {registered}.")
sys.exit(1 if counted else 0)
