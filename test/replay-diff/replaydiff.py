"""The same command against a cluster and against its frozen copy, output compared.

    replaydiff.py commands <kubeconfig> <out.json> [--runs <dir of run records for this case> --old-snapshot <kubernetes.tar.gz>]...
    replaydiff.py capture  <commands.json> <dir holding the guarded kubectl> <its kubeconfig> <out.json>
    replaydiff.py compare  <live-before.json> <frozen.json> <live-after.json> [<known.txt>]

`commands` writes what to ask: a fixed set of questions about every kind the cluster has, and every
`kubectl` command the recorded agents typed, with the pod names of their cluster replaced by this
one's. `capture` asks them. `compare` prints Markdown and exits 1 if a command differs that
<known.txt> does not list.

No judge, no model, no reading. A frozen case is faithful where this says so and nowhere else.
"""
import collections, concurrent.futures, glob, io, json, os, re, shlex, subprocess, sys, tarfile

READ_VERBS = {"get", "describe", "logs", "events", "top", "rollout", "api-resources", "api-versions", "explain", "auth", "version", "cluster-info"}
SKIP_FLAGS = ("-w", "--watch", "--watch-only", "-f", "--follow", "--since", "--since-time", "-i", "-t", "-it", "--raw", "--v", "-v", "--chunk-size")
# Kinds whose listing is not a property of the incident: tokens, leases renewed every few seconds.
SKIP_KINDS = {"leases.coordination.k8s.io", "componentstatuses", "events.events.k8s.io", "bindings", "tokenreviews", "localsubjectaccessreviews",
              "selfsubjectreviews", "selfsubjectaccessreviews", "selfsubjectrulesreviews", "subjectaccessreviews"}
SYSTEM_NS = {"kube-system", "kube-public", "kube-node-lease", "default", "local-path-storage"}


def kubectl(kubeconfig, *args):
    r = subprocess.run(["kubectl", "--kubeconfig", kubeconfig, *args], capture_output=True, text=True, timeout=60)
    return r.stdout if r.returncode == 0 else ""


def typed_commands(run_dirs):
    """Every simple `kubectl …` command in the recorded steps, as an argument list."""
    out = []
    for d in run_dirs:
        for f in sorted(glob.glob(os.path.join(d, "*.json"))):
            for step in json.load(open(f))["transcript"]["steps"]:
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


def wanted(argv):
    verb = next((a for a in argv if not a.startswith("-")), "")
    if verb not in READ_VERBS or any(a == f or a.startswith(f + "=") for a in argv for f in SKIP_FLAGS):
        return False
    return verb != "rollout" or any(a in ("history", "status") for a in argv)


def cmd_commands(kubeconfig, out_path, extra):
    runs, snapshots = [], []
    for flag, value in zip(extra[::2], extra[1::2]):
        (runs if flag == "--runs" else snapshots).append(value)
    pods = json.loads(kubectl(kubeconfig, "get", "pods", "-A", "-o", "json") or '{"items":[]}')["items"]
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

    commands = []
    namespaces = sorted({p["metadata"]["namespace"] for p in pods} - SYSTEM_NS)
    for kind in sorted(set(kubectl(kubeconfig, "api-resources", "--verbs=list", "-o", "name").split()) - SKIP_KINDS):
        commands += [["get", kind, "-A"], ["get", kind, "-A", "-o", "wide"]]
        items = json.loads(kubectl(kubeconfig, "get", kind, "-A", "-o", "json") or '{"items":[]}')["items"]
        own = [i for i in items if i["metadata"].get("namespace") in namespaces] or items
        for i in own[:2]:
            where = ["-n", i["metadata"]["namespace"]] if i["metadata"].get("namespace") else []
            commands += [["get", kind, i["metadata"]["name"], *where], ["get", kind, i["metadata"]["name"], *where, "-o", "wide"], ["describe", kind, i["metadata"]["name"], *where]]
    for ns in namespaces:
        commands += [["get", "all", "-n", ns], ["get", "all", "-n", ns, "-o", "wide"], ["get", "pods", "-n", ns, "--show-labels"], ["get", "pods", "-n", ns, "-L", "app"],
                     ["get", "pods", "-n", ns, "--sort-by=.status.startTime"], ["get", "pods", "-n", ns, "--sort-by=.metadata.name", "-o", "wide"],
                     ["get", "pods", "-n", ns, "--sort-by=.status.containerStatuses[0].restartCount"],
                     ["get", "events", "-n", ns, "--sort-by=.lastTimestamp"], ["get", "events", "-n", ns, "--sort-by=.metadata.creationTimestamp"], ["get", "events", "-n", ns, "-o", "wide"],
                     ["get", "pods", "-n", ns, "--field-selector=status.phase=Running"], ["get", "pods", "-n", ns, "--field-selector=status.phase!=Running"],
                     ["get", "pods", "-n", ns, "--no-headers"], ["get", "pods", "-n", ns, "-o", "name"],
                     ["get", "pods", "-n", ns, "-o", "custom-columns=NAME:.metadata.name,NODE:.spec.nodeName,PHASE:.status.phase"],
                     ["get", "pods", "-n", ns, "-o", "jsonpath={range .items[*]}{.metadata.name} {.spec.nodeName}{\"\\n\"}{end}"],
                     ["get", "configmaps", "-n", ns, "-o", "yaml"], ["get", "deployments", "-n", ns, "-o", "json"], ["events", "-n", ns],
                     ["get", "configmaps,secrets,persistentvolumeclaims", "-n", ns], ["get", "pods,services", "-n", ns, "-o", "wide"], ["top", "pods", "-n", ns],
                     ["auth", "can-i", "get", "pods", "-n", ns], ["auth", "can-i", "delete", "pods", "-n", ns]]
        for d in json.loads(kubectl(kubeconfig, "get", "deployments", "-n", ns, "-o", "json") or '{"items":[]}')["items"]:
            commands += [["rollout", "history", "deployment/" + d["metadata"]["name"], "-n", ns], ["rollout", "status", "deployment/" + d["metadata"]["name"], "-n", ns, "--timeout=3s"],
                         ["logs", "deployment/" + d["metadata"]["name"], "-n", ns, "--tail=3"]]
        for p in [p for p in pods if p["metadata"]["namespace"] == ns]:
            name, node = p["metadata"]["name"], p["spec"].get("nodeName", "")
            commands += [["describe", "pod", name, "-n", ns], ["logs", name, "-n", ns, "--tail=20"], ["logs", name, "-n", ns, "--tail=5", "--all-containers"],
                         ["get", "events", "-n", ns, "--field-selector", "involvedObject.name=" + name], ["get", "pod", name, "-n", ns, "-o", "wide"]]
            if node:
                commands.append(["get", "pods", "-A", "--field-selector", "spec.nodeName=" + node, "-o", "wide"])
    commands += [["api-resources"], ["api-versions"], ["version"], ["cluster-info"], ["explain", "pods"], ["explain", "deployment.spec.strategy"], ["top", "nodes"]]
    for argv in typed_commands(runs):
        commands.append([re.sub("|".join(map(re.escape, sorted(rename, key=len, reverse=True))) or r"(?!)", lambda m: rename[m.group(0)], a) for a in argv])

    seen, unique = set(), []
    for argv in commands:
        if wanted(argv) and tuple(argv) not in seen:
            seen.add(tuple(argv)); unique.append(argv)
    json.dump(unique, open(out_path, "w"), indent=0)
    print(f"{len(unique)} commands ({len(rename)} pod names of the recorded cluster mapped to this one)")


def cmd_capture(commands_path, bin_dir, kubeconfig, out_path):
    commands = json.load(open(commands_path))
    # What an agent's environment is: the guard first on PATH, and the one kubeconfig it will accept.
    env = dict(os.environ, PATH=bin_dir + os.pathsep + os.environ["PATH"], KUBECONFIG=kubeconfig)

    def run(argv):
        try:
            r = subprocess.run(["kubectl", *argv], capture_output=True, text=True, timeout=90, env=env)
            return {"argv": argv, "rc": r.returncode, "out": r.stdout, "err": r.stderr}
        except subprocess.TimeoutExpired:
            return {"argv": argv, "rc": -1, "out": "", "err": "timed out"}

    with concurrent.futures.ThreadPoolExecutor(6) as pool:
        results = list(pool.map(run, commands))
    json.dump(results, open(out_path, "w"))
    print(f"{len(results)} commands asked; {sum(r['rc'] != 0 for r in results)} did not exit 0")


AGE = re.compile(r"(?<![\w.:/-])(?:\d+y)?(?:\d+d)?(?:\d+h)?(?:\d+m)?(?:\d+s)?(?<=[ydhms])(?![\w.:/-])")


def normal(text):
    """What is left of an output when the passage of time is taken out of it."""
    text = AGE.sub("<age>", text)
    text = re.sub(r"\(x\d+ over <age>\)", "(xN over <age>)", text)
    text = re.sub(r"<age> \(xN over <age>\)|\d+ \(<age> ago\)", lambda m: "<age>" if m.group(0).startswith("<") else "N (<age> ago)", text)
    return [re.sub(r"\s+", " ", line).strip() for line in text.split("\n") if line.strip()]


def verdict(argv, before, frozen, after):
    """same | order | moved | differs, and a line of explanation."""
    a, f, b = normal(before["out"]), normal(frozen["out"]), normal(after["out"])
    failed = [r["rc"] != 0 for r in (before, frozen, after)]
    if any(failed):
        if all(failed):
            ea, ef = normal(before["err"]), normal(frozen["err"])
            return ("same", "") if ea == ef else ("worded", f"live: {' '.join(ea)[:150]} | frozen: {' '.join(ef)[:150]}")
        return "differs", f"exit codes live {before['rc']}, frozen {frozen['rc']}, live {after['rc']}: {(' '.join(normal(frozen['err'])) or ' '.join(normal(before['err'])))[:200]}"
    if "logs" in argv[:1]:
        raw = lambda r: r["out"].rstrip("\n").split("\n")
        ra, rf, rb = raw(before), raw(frozen), raw(after)
        tail = next((int(x.split("=")[1]) for x in argv if x.startswith("--tail=")), None)
        if tail is not None:
            ok = len(rf) == len(rb) or (len(rf) <= tail and len(rb) <= tail)
            return ("same", "") if ok else ("differs", f"{len(rf)} lines frozen, {len(rb)} live, for --tail={tail}")
        return ("same", "") if rf[:len(ra)] == ra and rb[:len(rf)] == rf else ("differs", f"the frozen log ({len(rf)} lines) does not sit between the two live ones ({len(ra)}, {len(rb)})")
    ea, ef, eb = normal(before["err"]), normal(frozen["err"]), normal(after["err"])
    if (f == a or f == b) and ef not in (ea, eb):  # the same answer, and something else said beside it
        return "differs", f"stderr, live: {' '.join(eb)[:150] or '(nothing)'} | frozen: {' '.join(ef)[:150] or '(nothing)'}"
    if f == a or f == b:
        return "same", ""
    if sorted(f) in (sorted(a), sorted(b)):
        return "order", ""
    live = b if a != b else a
    first = next(((x, y) for x, y in zip(live, f) if x != y), (live[len(f):len(f) + 1] or [""], f[len(live):len(live) + 1] or [""]))
    where = f"live: {str(first[0])[:170]} | frozen: {str(first[1])[:170]}"
    if a != b and f[:1] == b[:1] and f[:1] == a[:1]:
        return "moved", where  # the same header, and the cluster itself changed between the two live passes
    return "differs", where


def shape(argv):
    """What kind of question a command is, for grouping."""
    verb = next((x for x in argv if not x.startswith("-")), "")
    rest = [x for x in argv[argv.index(verb) + 1:] if not x.startswith("-")] if verb in argv else []
    kind = rest[0].split("/")[0] if rest and verb in ("get", "describe") else ""
    flags = sorted({re.split(r"[= ]", x)[0] for x in argv if x.startswith("-") and x not in ("-n", "-A", "--namespace", "--all-namespaces")})
    out = next((argv[i + 1].split("=")[0] for i, x in enumerate(argv[:-1]) if x == "-o"), "")
    named = "one named" if verb == "get" and len(rest) > 1 else ""
    return " ".join(x for x in [verb, kind, named, " ".join(f for f in flags if f != "-o"), ("-o " + out) if out else ""] if x)


def cmd_compare(before_path, frozen_path, after_path, known_path=None):
    before, frozen, after = (json.load(open(p)) for p in (before_path, frozen_path, after_path))
    known = [l.strip() for l in open(known_path) if l.strip() and not l.startswith("#")] if known_path else []
    rows = []
    for b, f, a in zip(before, frozen, after):
        assert b["argv"] == f["argv"] == a["argv"]
        rows.append((f["argv"], *verdict(f["argv"], b, f, a)))
    count = collections.Counter(v for _, v, _ in rows)
    print(f"commands: {len(rows)} | the same: {count['same']} | the same lines in another order: {count['order']} | both fail, worded differently: {count['worded']} | "
          f"the cluster moved between the two live passes: {count['moved']} | **differ: {count['differs']}**\n")
    by_shape = collections.defaultdict(list)
    for argv, v, why in rows:
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
    for title, kind in (("Differ", "differs"), ("Both fail, worded differently", "worded"), ("The cluster moved", "moved")):
        some = [(argv, why) for argv, v, why in rows if v == kind]
        if not some:
            continue
        print(f"\n### {title}\n")
        shown = collections.Counter()
        for argv, why in some:
            s = shape(argv)
            is_known = any(k in s or k in " ".join(argv) for k in known)
            if kind == "differs" and not is_known:
                unexpected += 1
            if shown[s] < 2:
                print(f"- `kubectl {' '.join(argv)}`{' (known)' if is_known and kind == 'differs' else ''}\n  - {why}")
            shown[s] += 1
        more = sum(n - 2 for n in shown.values() if n > 2)
        if more:
            print(f"\n…and {more} more of the same kinds.")
    if known_path:
        print(f"\nDiffering and not listed as known: {unexpected}.")
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
