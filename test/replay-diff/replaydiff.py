"""The same command against a cluster and against its frozen copy, output compared.

    replaydiff.py commands <kubeconfig> <out.json> [--runs <dir of run records for this case> --old-snapshot <kubernetes.tar.gz>]...
    replaydiff.py capture  <commands.json> <dir holding the guarded kubectl> <its kubeconfig> <out.json>
    replaydiff.py compare  <live-before.json> <frozen.json> <live-after.json> [<known.txt>]

`commands` writes what to ask: a fixed set of questions about every kind the cluster has, and every
`kubectl` command the recorded agents typed, with the pod names of their cluster replaced by this
one's. `capture` asks them. `compare` prints Markdown and exits 1 if a command differs that
<known.txt> does not excuse.

No judge, no model, no reading. A frozen case is faithful where this says so and nowhere else.
"""
import collections, concurrent.futures, glob, json, os, re, shlex, subprocess, sys, tarfile, time

READ_VERBS = {"get", "describe", "logs", "events", "top", "rollout", "api-resources", "api-versions", "explain", "auth", "version", "cluster-info"}
SKIP_FLAGS = ("-w", "--watch", "--watch-only", "-f", "--follow", "--since", "--since-time", "-i", "-t", "-it", "--raw", "--v", "-v", "--chunk-size")
# Flags whose value is the next argument, so that it is not taken for the name of something.
VALUE_FLAGS = {"-n", "--namespace", "-l", "--selector", "-o", "--output", "--field-selector", "--sort-by", "-c", "--container", "--tail", "-L", "--label-columns",
               "--for", "--types", "--request-timeout", "--timeout", "--revision", "--template"}
# Kinds whose listing is not a property of the incident: tokens, leases renewed every few seconds.
SKIP_KINDS = {"leases.coordination.k8s.io", "componentstatuses", "events.events.k8s.io", "bindings", "tokenreviews", "localsubjectaccessreviews",
              "selfsubjectreviews", "selfsubjectaccessreviews", "selfsubjectrulesreviews", "subjectaccessreviews"}
SYSTEM_NS = {"kube-system", "kube-public", "kube-node-lease", "default", "local-path-storage"}


def kubectl(kubeconfig, *args, needed=False):
    r = subprocess.run(["kubectl", "--kubeconfig", kubeconfig, *args], capture_output=True, text=True, timeout=60)
    if r.returncode != 0 and needed:
        sys.exit(f"kubectl {' '.join(args)} failed, and nothing can be asked without it: {r.stderr.strip()[:300]}")
    return r.stdout if r.returncode == 0 else ""


def typed_commands(run_dirs):
    """Every simple `kubectl …` command in the recorded steps, as an argument list."""
    out = []
    for d in run_dirs:
        for f in sorted(glob.glob(os.path.join(d, "*.json"))):
            for step in json.load(open(f))["transcript"].get("steps") or []:
                if step.get("tool") != "bash":
                    continue
                for line in step["input"].split("\n")[:1 if "holmes" in f else None]:
                    if "$" in line or "`" in line or "<<" in line:
                        continue  # needs a shell to mean anything
                    line = re.sub(r"\s\d?>>?\s*&?\S+", " ", line)  # redirections
                    try:
                        lex = shlex.shlex(line, posix=True, punctuation_chars=True)
                        lex.whitespace_split = True
                        tokens = list(lex)
                    except ValueError:
                        continue
                    argv = []
                    for tok in tokens + [";"]:
                        if tok and set(tok) <= set("();<>|&"):
                            if argv[:1] == ["kubectl"] and len(argv) > 1:
                                out.append(argv[1:])
                            argv = []
                        else:
                            argv.append(tok)
    return out


def words(argv):
    """The arguments that are neither flags nor the values of flags: the verb, the kind, the names."""
    out, skip = [], False
    for a in argv:
        if skip:
            skip = False
        elif a in VALUE_FLAGS:
            skip = True
        elif not a.startswith("-"):
            out.append(a)
    return out


def wanted(argv):
    """A read, with its verb where kubectl reads it: first, or after a namespace."""
    lead = argv[2:] if argv[:1] in (["-n"], ["--namespace"]) else argv
    verb = lead[0] if lead else ""
    if verb not in READ_VERBS or any(a == f or a.startswith(f + "=") for a in argv for f in SKIP_FLAGS):
        return False
    return verb != "rollout" or lead[1:2] in (["history"], ["status"])


def cmd_commands(kubeconfig, out_path, extra):
    runs, snapshots = [], []
    for flag, value in zip(extra[::2], extra[1::2]):
        (runs if flag == "--runs" else snapshots).append(value)
    pods = json.loads(kubectl(kubeconfig, "get", "pods", "-A", "-o", "json", needed=True))["items"]
    new_by_prefix = collections.defaultdict(list)
    for p in pods:
        if p["metadata"].get("generateName"):
            new_by_prefix[(p["metadata"]["namespace"], p["metadata"]["generateName"])].append(p["metadata"]["name"])
    rename = {}
    for snap in snapshots:  # the pods of the cluster the recorded agents saw
        old_by_prefix = collections.defaultdict(list)
        for m in tarfile.open(snap):
            hit = re.match(r"namespaces/([^/]+)/v1/pod/([^/]+)\.yaml$", m.name)
            if hit and "-" in hit.group(2):
                old_by_prefix[(hit.group(1), hit.group(2).rsplit("-", 1)[0] + "-")].append(hit.group(2))
        for key, old in old_by_prefix.items():
            for a, b in zip(sorted(old), sorted(new_by_prefix.get(key, []))):
                rename[a] = b

    fixed = []
    namespaces = sorted({p["metadata"]["namespace"] for p in pods} - SYSTEM_NS)
    for kind in sorted(set(kubectl(kubeconfig, "api-resources", "--verbs=list", "-o", "name", needed=True).split()) - SKIP_KINDS):
        fixed += [["get", kind, "-A"], ["get", kind, "-A", "-o", "wide"], ["get", kind, "-A", "-o", "name"]]
        items = json.loads(kubectl(kubeconfig, "get", kind, "-A", "-o", "json") or '{"items":[]}')["items"]
        own = [i for i in items if i["metadata"].get("namespace") in namespaces] or items
        for i in own[:2]:
            where = ["-n", i["metadata"]["namespace"]] if i["metadata"].get("namespace") else []
            fixed += [["get", kind, i["metadata"]["name"], *where], ["get", kind, i["metadata"]["name"], *where, "-o", "wide"], ["describe", kind, i["metadata"]["name"], *where]]
    for ns in namespaces:
        fixed += [["get", "all", "-n", ns], ["get", "all", "-n", ns, "-o", "wide"], ["get", "pods", "-n", ns, "--show-labels"], ["get", "pods", "-n", ns, "-L", "app"],
                  ["get", "pods", "-n", ns, "--sort-by=.status.startTime"], ["get", "pods", "-n", ns, "--sort-by=.metadata.name", "-o", "wide"],
                  ["get", "pods", "-n", ns, "--sort-by=.status.containerStatuses[0].restartCount"],
                  ["get", "events", "-n", ns, "--sort-by=.lastTimestamp"], ["get", "events", "-n", ns, "--sort-by=.metadata.creationTimestamp"], ["get", "events", "-n", ns, "-o", "wide"],
                  ["get", "pods", "-n", ns, "--field-selector=status.phase=Running"], ["get", "pods", "-n", ns, "--field-selector=status.phase!=Running"],
                  ["get", "pods", "-n", ns, "--no-headers"], ["-n", ns, "get", "pods"],
                  ["get", "pods", "-n", ns, "-o", "custom-columns=NAME:.metadata.name,NODE:.spec.nodeName,PHASE:.status.phase"],
                  ["get", "pods", "-n", ns, "-o", "jsonpath={range .items[*]}{.metadata.name} {.spec.nodeName}{\"\\n\"}{end}"],
                  ["get", "configmaps", "-n", ns, "-o", "yaml"], ["get", "deployments", "-n", ns, "-o", "json"], ["events", "-n", ns],
                  ["get", "configmaps,secrets,persistentvolumeclaims", "-n", ns], ["get", "pods,services", "-n", ns, "-o", "wide"], ["top", "pods", "-n", ns],
                  ["auth", "can-i", "get", "pods", "-n", ns], ["auth", "can-i", "delete", "pods", "-n", ns]]
        for d in json.loads(kubectl(kubeconfig, "get", "deployments", "-n", ns, "-o", "json") or '{"items":[]}')["items"]:
            fixed += [["rollout", "history", "deployment/" + d["metadata"]["name"], "-n", ns], ["rollout", "status", "deployment/" + d["metadata"]["name"], "-n", ns],
                      ["logs", "deployment/" + d["metadata"]["name"], "-n", ns, "--tail=3"]]
        for p in [p for p in pods if p["metadata"]["namespace"] == ns]:
            name, node = p["metadata"]["name"], p["spec"].get("nodeName", "")
            fixed += [["describe", "pod", name, "-n", ns], ["logs", name, "-n", ns, "--tail=20"], ["logs", name, "-n", ns, "--tail=5", "--all-containers"], ["logs", name, "-n", ns],
                      ["logs", name, "-n", ns, "--previous"], ["get", "events", "-n", ns, "--field-selector", "involvedObject.name=" + name], ["get", "pod", name, "-n", ns, "-o", "wide"]]
            if node:
                fixed.append(["get", "pods", "-A", "--field-selector", "spec.nodeName=" + node, "-o", "wide"])
    fixed += [["api-resources"], ["api-versions"], ["version"], ["cluster-info"], ["explain", "pods"], ["explain", "deployment.spec.strategy"], ["top", "nodes"],
              ["get", "pod", "no-such-pod", "-n", "default"], ["logs", "no-such-pod", "-n", "default"]]
    renamed = re.compile("|".join(map(re.escape, sorted(rename, key=len, reverse=True))) or r"(?!)")
    typed = [[renamed.sub(lambda m: rename[m.group(0)], a) for a in argv] for argv in typed_commands(runs)]

    # A command an agent typed is marked as one even when the fixed set asks it too.
    was_typed, seen, commands = {tuple(a) for a in typed}, set(), []
    for argv in fixed + typed:
        if wanted(argv) and tuple(argv) not in seen:
            seen.add(tuple(argv))
            commands.append({"argv": argv, "typed": tuple(argv) in was_typed})
    if len(commands) < 100:
        sys.exit(f"only {len(commands)} commands to ask: the cluster was not read")
    json.dump(commands, open(out_path, "w"), indent=0)
    print(f"{len(commands)} commands, {sum(c['typed'] for c in commands)} of them typed by a recorded agent ({len(rename)} pod names of the recorded cluster mapped to this one)")


def cmd_capture(commands_path, bin_dir, kubeconfig, out_path):
    commands = json.load(open(commands_path))
    # What an agent's environment is: the guard first on PATH, and the one kubeconfig it will accept.
    env = dict(os.environ, PATH=bin_dir + os.pathsep + os.environ["PATH"], KUBECONFIG=kubeconfig)

    def run(c):
        at = time.time()  # when it was asked: an age may differ by as long as lay between two askings
        try:
            r = subprocess.run(["kubectl", *c["argv"]], capture_output=True, text=True, timeout=90, env=env)
            return dict(c, at=at, rc=r.returncode, out=r.stdout, err=r.stderr)
        except subprocess.TimeoutExpired:
            return dict(c, at=at, rc=-1, out="", err="timed out")

    with concurrent.futures.ThreadPoolExecutor(6) as pool:
        results = list(pool.map(run, commands))
    json.dump(results, open(out_path, "w"))
    refused = sum("refused by lapilli-case" in r["err"] for r in results)
    print(f"{len(results)} commands asked; {sum(r['rc'] != 0 for r in results)} did not exit 0; {refused} refused by the guard")
    if refused * 2 > len(results):
        sys.exit("the guard refused most of them: the kubeconfig is not the one it was given")


UNIT = {"s": 1, "m": 60, "h": 3600, "d": 86400, "y": 365 * 86400}
AGE = re.compile(r"(?:\d+y)?(?:\d+d)?(?:\d+h)?(?:\d+m)?(?:\d+s)?")
REPEATS = re.compile(r"\(?x(\d+)")


def age(token):
    """(seconds, the size of its last unit) if a token is written the way a cluster writes an age."""
    t = token.strip("(),")
    if not t or not AGE.fullmatch(t):
        return None
    parts = re.findall(r"(\d+)([ydhms])", t)
    return sum(int(n) * UNIT[u] for n, u in parts), UNIT[parts[-1][1]]


def tokens(text):
    return [line.split() for line in text.split("\n") if line.strip()]


def alike(x, y, other=None, apart=30):
    """A frozen token x says what the live token y says. An age may differ by the time that lay
    between the two askings and by what the finer of the two last units hides — `12m` and `13m`,
    `119s` and `2m`; never `89m` and `1h`, nor `250m` of CPU and `3m`. The count of a repeating
    event may lie between the two live counts, y and other."""
    if x == y:
        return True
    ax, ay = age(x), age(y)
    if ax and ay:
        return abs(ax[0] - ay[0]) <= apart + min(ax[1], ay[1])
    rx, ry, ro = REPEATS.fullmatch(x), REPEATS.fullmatch(y), REPEATS.fullmatch(other or "")
    if rx and ry and ro:
        return min(int(ro.group(1)), int(ry.group(1))) <= int(rx.group(1)) <= max(int(ro.group(1)), int(ry.group(1)))
    return False


def matches(f, x, other=None, apart=30):
    """The frozen answer f is the live answer x, token for token, time aside."""
    same_shape = lambda p, q: len(p) == len(q) and all(len(a) == len(b) for a, b in zip(p, q))
    if not same_shape(f, x):
        return False
    third = other if other is not None and same_shape(other, x) else None
    return all(alike(a, b, third[i][j] if third else None, apart) for i, (fl, xl) in enumerate(zip(f, x)) for j, (a, b) in enumerate(zip(fl, xl)))


def loose(text, any_order=True):
    """Lines with every age and every count of a repeating event taken out: for saying what kind of
    difference a difference is, and nothing else."""
    lines = [" ".join("<age>" if age(t) else REPEATS.sub("xN", t) for t in line) for line in tokens(text)]
    return sorted(lines) if any_order else lines


def within(x, y):
    """Every line of x is in y, in order."""
    rest = iter(y)
    return all(line in rest for line in x)


def continues(x, y):
    """y is the window x was, or that window moved on: some end of x is the start of y."""
    return not x or any(y[:len(x) - k] == x[k:] for k in range(len(x)))


def verdict(argv, before, frozen, after):
    """same | order | moved | worded | differs, and a line of explanation."""
    if -1 in (before["rc"], frozen["rc"], after["rc"]):
        return "differs", "timed out: " + ", ".join(n for n, r in (("live", before), ("frozen", frozen), ("live again", after)) if r["rc"] == -1)
    a, f, b = tokens(before["out"]), tokens(frozen["out"]), tokens(after["out"])
    ea, ef, eb = tokens(before["err"]), tokens(frozen["err"]), tokens(after["err"])
    said = lambda t: " ".join(" ".join(line) for line in t)[:160] or "(nothing)"
    failed = [r["rc"] != 0 for r in (before, frozen, after)]
    if any(failed) and not all(failed):
        return "differs", f"exit codes live {before['rc']}, frozen {frozen['rc']}, live {after['rc']}: {said(ef) if failed[1] else said(eb)}"
    if all(failed):
        if (matches(ef, ea) or matches(ef, eb)) and (matches(f, a) or matches(f, b)):
            return "same", ""
        return "worded", f"live: {said(eb)} | frozen: {said(ef)}"

    verb = (argv[2:] if argv[:1] in (["-n"], ["--namespace"]) else argv)[0]
    if verb == "logs":
        raw = lambda r: r["out"].rstrip("\n").split("\n") if r["out"].strip() else []
        ra, rf, rb = raw(before), raw(frozen), raw(after)
        tail = next((int(x.split("=")[1]) for x in argv if x.startswith("--tail=")), None)
        if any(x in ("-l", "--selector", "--all-containers", "--all-pods") or x.startswith(("-l=", "--selector=")) for x in argv):
            # Several logs, one after another: each grows where it stands, so the whole grows in the
            # middle. Every line the first asking saw is in the frozen one, in order, and every line of
            # the frozen one in the last.
            if within(ra, rf) and within(rf, rb) or tail is not None and continues(ra, rf) and continues(rf, rb):  # or it is one log after all
                return "same", ""
            if not within(ra, rb):
                return "moved", "the logs went by faster than the tail asked for"
            return "differs", f"the frozen logs ({len(rf)} lines) do not sit between the two live ones ({len(ra)}, {len(rb)})"
        if tail is None or max(len(ra), len(rf), len(rb)) < tail:  # the whole log, which only grows
            ok = rf[:len(ra)] == ra and rb[:len(rf)] == rf
            return ("same", "") if ok else ("differs", f"the frozen log ({len(rf)} lines) does not sit between the two live ones ({len(ra)}, {len(rb)})")
        if len(rf) > tail:
            return "differs", f"{len(rf)} lines for --tail={tail}"
        if continues(ra, rf) and continues(rf, rb):
            return "same", ""
        if not continues(ra, rb):
            return "moved", "the log went by faster than the tail asked for"
        return "differs", f"the frozen tail is neither a live one nor a live one moved on: live {ra[-1:]}, frozen {rf[-1:]}"

    # How far apart two askings were is how far apart their ages may be, and five seconds for a slow one.
    apart = lambda x, y: abs(x["at"] - y["at"]) + 5 if "at" in x and "at" in y else 30
    fa, fb, ab = apart(frozen, before), apart(frozen, after), apart(before, after)
    if matches(f, a, b, fa) or matches(f, b, a, fb):
        if matches(ef, ea) or matches(ef, eb):
            return "same", ""
        return "differs", f"stderr, live: {said(eb)} | frozen: {said(ef)}"  # the same answer, and something else said beside it
    live, near = (b, fb) if not matches(a, b, None, ab) else (a, fa)
    pair = next(((x, y) for x, y in zip(live, f) if not matches([y], [x], None, near)), None)
    if pair is None:
        pair = (live[len(f)] if len(live) > len(f) else [], f[len(live)] if len(f) > len(live) else [])
    where = f"live: {' '.join(pair[0])[:170] or '(no such line)'} | frozen: {' '.join(pair[1])[:170] or '(no such line)'}"
    if loose(frozen["out"], False) in (loose(before["out"], False), loose(after["out"], False)):
        return "differs", "an age further from the live one than the askings were apart. " + where
    if loose(frozen["out"]) in (loose(before["out"]), loose(after["out"])):
        # The same lines. Under --sort-by two rows of equal key may stand either way round; without it the order is the cluster's, and matters.
        return ("order", "") if any(x.startswith("--sort-by") for x in argv) else ("differs", "the same lines in another order. " + where)
    if not matches(a, b, None, ab) and f[:1] == b[:1] == a[:1]:
        return "moved", where  # the same heading, and the cluster itself changed between the two live passes
    return "differs", where


def shape(argv):
    """What kind of question a command is, for grouping."""
    w = words(argv)
    verb, rest = (w[0], w[1:]) if w else ("", [])
    kind = rest[0].split("/")[0] if rest and verb in ("get", "describe") else ""
    flags = sorted({x.split("=")[0] for x in argv if x.startswith("-") and x not in ("-n", "-A", "--namespace", "--all-namespaces", "-o", "--output")})
    out = next((argv[i + 1].split("=")[0] for i, x in enumerate(argv[:-1]) if x in ("-o", "--output")), "")
    named = "one named" if verb == "get" and len(rest) > 1 else ""
    return " ".join(x for x in [verb, kind, named, " ".join(flags), ("-o " + out) if out else ""] if x)


def load_known(path):
    """Lines of `<verb> [<kind, a pattern>] :: <a pattern the difference must show>`."""
    known = []
    for line in open(path) if path else []:
        if line.strip() and not line.startswith("#"):
            what, _, shows = line.partition("::")
            verb, *kind = what.split()
            known.append((verb, re.compile(kind[0]) if kind else None, re.compile(shows.strip())))
    return known


def excused(known, argv, why):
    w = words(argv)
    kind = w[1].split("/")[0] if len(w) > 1 else ""
    return any(w[:1] == [verb] and (pattern is None or pattern.fullmatch(kind)) and shows.search(why) for verb, pattern, shows in known)


def cmd_compare(before_path, frozen_path, after_path, known_path=None):
    before, frozen, after = (json.load(open(p)) for p in (before_path, frozen_path, after_path))
    known = load_known(known_path)
    rows = []
    for b, f, a in zip(before, frozen, after):
        assert b["argv"] == f["argv"] == a["argv"]
        rows.append((f["argv"], f.get("typed"), *verdict(f["argv"], b, f, a)))
    if len(rows) < 100:
        print(f"only {len(rows)} commands were asked: nothing was compared")
        return 1
    count = collections.Counter(v for _, _, v, _ in rows)
    print(f"commands: {len(rows)} | the same: {count['same']} | the same lines in another order: {count['order']} | both fail, worded differently: {count['worded']} | "
          f"the cluster moved between the two live passes: {count['moved']} | **differ: {count['differs']}**\n")
    if any(t is not None for _, t, _, _ in rows):
        typed = collections.Counter(v for _, t, v, _ in rows if t)
        print(f"Of the {sum(typed.values())} that a recorded agent had typed: the same {typed['same']}, another order {typed['order']}, worded differently {typed['worded']}, "
              f"the cluster moved {typed['moved']}, differ {typed['differs']}.\n")
    by_shape = collections.defaultdict(list)
    for argv, _, v, why in rows:
        by_shape[shape(argv)].append((argv, v, why))
    print("| question | asked | same | order | worded | moved | differ |")
    print("|---|---|---|---|---|---|---|")
    for s, group in sorted(by_shape.items(), key=lambda kv: (-sum(v == "differs" for _, v, _ in kv[1]), kv[0])):
        c = collections.Counter(v for _, v, _ in group)
        if c["differs"] or c["worded"] or c["moved"] or c["order"]:
            print(f"| `{s}` | {len(group)} | {c['same']} | {c['order']} | {c['worded']} | {c['moved']} | {c['differs'] or ''} |")
    clean = [s for s, g in by_shape.items() if all(v == "same" for _, v, _ in g)]
    print(f"\nThe same every time: {len(clean)} kinds of question, {sum(len(by_shape[s]) for s in clean)} commands.")
    unexpected = 0
    # A command that differs, or that both refuse in other words, fails unless known.txt excuses that very difference.
    for title, kind in (("Differ", "differs"), ("Both fail, worded differently", "worded"), ("The cluster moved", "moved"), ("The same lines in another order, under --sort-by", "order")):
        some = [(argv, why) for argv, _, v, why in rows if v == kind]
        if not some:
            continue
        print(f"\n### {title}\n")
        shown, left_out = collections.Counter(), 0
        for argv, why in some:
            s = shape(argv)
            gates = kind in ("differs", "worded")
            is_known = gates and excused(known, argv, why)
            unexpected += gates and not is_known
            if shown[s] < 2 or (gates and not is_known):  # every one that fails the sweep is printed
                print(f"- `kubectl {' '.join(argv)}`{' (known)' if is_known else ''}" + (f"\n  - {why}" if why else ""))
            else:
                left_out += 1
            shown[s] += 1
        if left_out:
            print(f"\n…and {left_out} more of the same kinds.")
    if known_path:
        print(f"\nDiffering and not excused by {os.path.basename(known_path)}: {unexpected}.")
    return 1 if unexpected and known_path else 0


if __name__ == "__main__":
    cmd, args = sys.argv[1], sys.argv[2:]
    if cmd == "commands":
        cmd_commands(args[0], args[1], args[2:])
    elif cmd == "capture":
        cmd_capture(*args)
    elif cmd == "compare":
        sys.exit(cmd_compare(*args))
    else:
        sys.exit(__doc__)
