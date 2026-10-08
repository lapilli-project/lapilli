"""The same command against a cluster and against its frozen copy, output compared.

    replaydiff.py commands <kubeconfig> <out.json> [--runs <dir of run records for this case> --old-snapshot <kubernetes.tar.gz>]...
    replaydiff.py capture  <commands.json> <dir holding the guarded kubectl> <its kubeconfig> <out.json>
    replaydiff.py compare  <live-before.json> <frozen.json> <live-after.json> [<known.txt> [<the case's freeze.json>]]

`commands` writes what to ask: a fixed set of questions about every kind the cluster has, and every
`kubectl` command the recorded agents typed, with the pod names of their cluster replaced by this
one's. `capture` asks them. `compare` prints Markdown and exits 1 if a command differs that
<known.txt> does not excuse.

No judge, no model, no reading. A frozen case is faithful where this says so and nowhere else.
"""
import collections, concurrent.futures, glob, json, os, re, shlex, subprocess, sys, tarfile, time

READ_VERBS = {"get", "describe", "logs", "events", "top", "rollout", "api-resources", "api-versions", "explain", "auth", "version", "cluster-info"}
SKIP_FLAGS = ("-w", "--watch", "--watch-only", "-f", "--follow", "-i", "-t", "-it", "--raw", "--v", "-v", "--chunk-size")
# Flags whose value is the next argument, so that it is not taken for the name of something.
VALUE_FLAGS = {"-n", "--namespace", "-l", "--selector", "-o", "--output", "--field-selector", "--sort-by", "-c", "--container", "--tail", "-L", "--label-columns",
               "--for", "--types", "--request-timeout", "--timeout", "--revision", "--template", "--since", "--since-time", "--limit-bytes"}
# Kinds whose listing is not a property of the incident: tokens, leases renewed every few seconds.
SKIP_KINDS = {"leases.coordination.k8s.io", "componentstatuses", "bindings", "tokenreviews", "localsubjectaccessreviews",
              "selfsubjectreviews", "selfsubjectaccessreviews", "selfsubjectrulesreviews", "subjectaccessreviews"}
SYSTEM_NS = {"kube-system", "kube-public", "kube-node-lease", "default", "local-path-storage"}


def kubectl(kubeconfig, *args, needed=False):
    r = subprocess.run(["kubectl", "--kubeconfig", kubeconfig, *args], capture_output=True, text=True, timeout=60)
    if r.returncode != 0 and needed:
        sys.exit(f"kubectl {' '.join(args)} failed, and nothing can be asked without it: {r.stderr.strip()[:300]}")
    return r.stdout if r.returncode == 0 else ""


def needs_a_shell(command):
    """A command means what it says only if a shell reads it: it has `$` or a backtick where a shell
    would expand it — anywhere but inside single quotes, or behind a backslash. `-o jsonpath='{$.x}'`
    and `grep 'x$'` do not. (`"\\$x"` is said to need one too: a shell takes the backslash off there
    and what splits the command into arguments here does not.)"""
    quote, i = None, 0
    while i < len(command):
        ch = command[i]
        if ch == "\\" and quote != "'":
            if quote and command[i + 1:i + 2] in ("$", "`"):
                return True
            i += 1
        elif quote:
            if ch == quote:
                quote = None
            elif quote == '"' and ch in "$`":
                return True
        elif ch in "'\"":
            quote = ch
        elif ch in "$`":
            return True
        i += 1
    return False


def shell_commands(text):
    """The commands of a step, parted where a shell parts them — at a newline, `;`, `|`, `&`, `(` or
    `)` that stands outside quotes — each as it was written and without its redirections. A quoted
    argument may run over several lines and stays one argument. None if the step cannot be read so:
    a quote is left open, or there is a here-document, whose body is not commands.

    It reads the text as a shell does, a character at a time, because twice it did not. A redirection
    used to be taken off by a pattern before the line was split, and with `2>&1;` the pattern took the
    semicolon: two commands were asked as one. And a line was read or left out whole, so that a
    command standing after a loop on the same line was never asked."""
    out, cur, quote, i, n = [], [], None, 0, len(text)
    while i < n:
        ch = text[i]
        if quote:
            cur.append(ch)
            if ch == "\\" and quote == '"' and i + 1 < n:
                i += 1
                cur.append(text[i])
            elif ch == quote:
                quote = None
        elif ch == "\\" and i + 1 < n:
            i += 1
            cur.append(" " if text[i] == "\n" else "\\" + text[i])  # a line continued, or a character escaped
        elif ch in "'\"":
            quote = ch
            cur.append(ch)
        elif ch in "<>" or ch == "&" and text[i + 1:i + 2] == ">":
            if text.startswith("<<", i):
                return None
            # A redirection: the file descriptor that stands against it, the arrow, and the word it
            # points at. `--tail 2 >out` keeps its 2; `2>out` does not.
            j = len(cur)
            while j and cur[j - 1].isdigit():
                j -= 1
            if j < len(cur) and (j == 0 or cur[j - 1].isspace()):
                del cur[j:]
            while i < n and text[i] in "<>&":
                i += 1
            while i < n and text[i] in " \t":
                i += 1
            within = None
            while i < n and (within or text[i] not in " \t\n;|&()<>"):
                if within:
                    within = None if text[i] == within else within
                elif text[i] in "'\"":
                    within = text[i]
                elif text[i] == "\\":
                    i += 1
                i += 1
            cur.append(" ")
            continue
        elif ch in ";|&()\n":
            out.append("".join(cur))
            cur = []
        else:
            cur.append(ch)
        i += 1
    if quote:
        return None
    out.append("".join(cur))
    return [c.strip() for c in out if c.strip()]


# Words a shell lets stand before a command, which is still the command: `do kubectl get pods`.
BEFORE_A_COMMAND = {"do", "then", "else", "elif", "if", "while", "until", "time", "!", "{"}


def typed_commands(run_dirs, left=None):
    """Every `kubectl` command in the recorded steps that can be asked as it stands, as an argument
    list. What holds a `kubectl` and cannot be — it needs a shell to mean anything, or its step
    cannot be parted into commands — is appended to `left`, so that it is counted and not lost."""
    out = []
    for d in run_dirs:
        for f in sorted(glob.glob(os.path.join(d, "*.json"))):
            for step in json.load(open(f))["transcript"].get("steps") or []:
                if step.get("tool") != "bash":
                    continue
                text = step["input"].split("\n")[0] if "holmes" in f else step["input"]
                commands = shell_commands(text)
                if commands is None:
                    commands = []
                    if left is not None and "kubectl" in text:
                        left.append(text)
                for command in commands:
                    try:
                        argv = None if needs_a_shell(command) else shlex.split(command)
                    except ValueError:
                        argv = None
                    if argv is None:
                        if left is not None and "kubectl" in command:
                            left.append(command)
                        continue
                    while argv and argv[0] in BEFORE_A_COMMAND:
                        argv = argv[1:]
                    if argv[:1] == ["kubectl"] and len(argv) > 1:
                        out.append(argv[1:])
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


def past_namespace(argv):
    """A command from its verb on, where it begins with a namespace in any of the ways one is written:
    `-n shop`, `--namespace shop`, `--namespace=shop`, `-n=shop`, `-nshop`. No other flag is read
    past: in `--field-manager get annotate pod x` the verb is `annotate`, and telling a flag that
    takes a value from one that does not is the guard's work, not this file's."""
    if argv[:1] in (["-n"], ["--namespace"]):
        return argv[2:]
    if argv and re.fullmatch(r"--namespace=.+|-n=?[a-z0-9][a-z0-9.-]*", argv[0]):
        return argv[1:]
    return argv


def wanted(argv):
    """A read, with its verb where kubectl reads it: first, or after a namespace."""
    lead = past_namespace(argv)
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
    a_minute_ago = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() - 60))
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
                      ["logs", name, "-n", ns, "--previous"], ["get", "events", "-n", ns, "--field-selector", "involvedObject.name=" + name], ["get", "pod", name, "-n", ns, "-o", "wide"],
                      ["logs", name, "-n", ns, "--timestamps", "--tail=3"], ["logs", name, "-n", ns, "--since-time=" + a_minute_ago], ["logs", name, "-n", ns, "--since=45s"]]
            for c in p["spec"].get("initContainers", []):  # where a pod that cannot start says why
                fixed += [["logs", name, "-n", ns, "-c", c["name"]], ["logs", name, "-n", ns, "-c", c["name"], "--previous"]]
            if node:
                fixed.append(["get", "pods", "-A", "--field-selector", "spec.nodeName=" + node, "-o", "wide"])
    fixed += [["api-resources"], ["api-versions"], ["version"], ["cluster-info"], ["explain", "pods"], ["explain", "deployment.spec.strategy"], ["top", "nodes"],
              ["get", "pod", "no-such-pod", "-n", "default"], ["logs", "no-such-pod", "-n", "default"]]
    renamed = re.compile("|".join(map(re.escape, sorted(rename, key=len, reverse=True))) or r"(?!)")
    left = []
    typed = [[renamed.sub(lambda m: rename[m.group(0)], a) for a in argv] for argv in typed_commands(runs, left)]

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
    if left:  # said every time, so that what is not asked is a number and not a silence
        print(f"{len(left)} more that an agent typed hold a kubectl and are not asked: each needs a shell to mean anything, or stands in a step that cannot be parted into commands")
        for command in sorted(set(left)):
            print("  not asked: " + " ".join(command.split())[:200])


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
    """(seconds, how finely an age of that size is written) if a token is written the way a cluster
    writes an age. How finely is by size and not by the last letter: to the second under ten minutes,
    to the minute under eight hours, to the hour under eight days, to the day under eight years
    (apimachinery's duration.HumanDuration). So `3h` is three hours to the minute, and `1h` is not
    how a cluster writes eighty-nine minutes."""
    t = token.strip("(),")
    if not t or not AGE.fullmatch(t):
        return None
    seconds = sum(int(n) * UNIT[u] for n, u in re.findall(r"(\d+)([ydhms])", t))
    fine = next((f for under, f in ((600, 1), (8 * 3600, 60), (8 * 86400, 3600), (8 * 365 * 86400, 86400)) if seconds < under), UNIT["y"])
    return seconds, fine


def tokens(text):
    return [line.split() for line in text.split("\n") if line.strip()]


def alike(x, y, other=None, shift=None, between=None):
    """A frozen token x says what the live token y says. An age is written to its last unit and no
    finer — `12m` is anything short of thirteen minutes — and x may be older than y by shift seconds,
    which is how much later it was counted, give or take five: `12m` and `13m`, `119s` and `2m`; never
    `89m` and `1h`, nor `250m` of CPU and `3m`. Where it is not known when each was counted, half a
    minute either way. The count of a repeating event may lie between the two live counts, y and other.

    Not every age only grows. When an event was last seen is when it was last seen, and it is seen
    again; so where the cluster's own two answers show an age that did not simply grow by the time
    between them — other was counted between seconds after y — the frozen one may be any age, so
    long as it is no older than either live answer allows."""
    if x == y:
        return True
    ax, ay, ao = age(x), age(y), age(other or "")
    if ax and ay:
        if shift is None:
            return abs(ax[0] - ay[0]) <= 30 + min(ax[1], ay[1])
        if shift - 5 - ax[1] <= ax[0] - ay[0] <= shift + 5 + ay[1]:
            return True
        if ao and between is not None and not (between - 5 - ao[1] <= ao[0] - ay[0] <= between + 5 + ay[1]):
            return ax[0] <= max(ay[0] + shift + ay[1], ao[0] + shift - between + ao[1]) + 5
        return False
    rx, ry, ro = REPEATS.fullmatch(x), REPEATS.fullmatch(y), REPEATS.fullmatch(other or "")
    if rx and ry and ro:
        return min(int(ro.group(1)), int(ry.group(1))) <= int(rx.group(1)) <= max(int(ro.group(1)), int(ry.group(1)))
    return False


def beside(x, other):
    """For each line of x, the line of other that says the same but for its ages and counts — wherever
    in other it stands, since two events of one second may be listed either way round."""
    if other is None:
        return [None] * len(x)
    key = lambda line: tuple("<age>" if age(t) else REPEATS.sub("xN", t) for t in line)
    there = {}
    for line in other:
        there.setdefault(key(line), []).append(line)
    return [there[key(line)].pop(0) if there.get(key(line)) else None for line in x]


def matches(f, x, other=None, shift=None, between=None):
    """The frozen answer f is the live answer x, token for token, time aside: f's ages counted shift
    seconds after x's, and those of other, the second live answer, between seconds after x's, if
    those are known."""
    if len(f) != len(x) or any(len(a) != len(b) for a, b in zip(f, x)):
        return False
    third = beside(x, other)
    return all(alike(a, b, third[i][j] if third[i] else None, shift, between) for i, (fl, xl) in enumerate(zip(f, x)) for j, (a, b) in enumerate(zip(fl, xl)))


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


def joined(x, y, of_time=False):
    """Every way two windows that overlap can be one run of lines — x, and then what y adds to it —
    each with where in it y begins. Two windows of time need share no line, if nothing was written
    while both were open, and then y begins where x ends: that run is given too, where no line says
    otherwise — the two share none, or every line of both is the same line, as in a log that says
    one thing over and over and cannot show where a window ends."""
    if not x:
        return [(y, 0)]
    runs = [(x + y[len(x) - k:], k) for k in range(len(x)) if y[:len(x) - k] == x[k:]]
    if of_time and (not runs or len(set(x + y)) == 1):
        runs.append((x + y, len(x)))
    return runs


def flag(argv, name):
    """The value of a flag written as --name=value or as --name value, or None."""
    for i, x in enumerate(argv):
        if x.startswith(name + "="):
            return x.split("=", 1)[1]
        if x == name and i + 1 < len(argv):
            return argv[i + 1]
    return None


def falls(lines, at):
    """Which way the ages in one place on each line — its first word, or its last — run down a
    listing: -1 if they never rise, 1 if they never fall, 0 if they do neither or both, or there are
    none, which says nothing."""
    ages = [age(line[at])[0] for line in lines if line and age(line[at])]
    down, up = all(x >= y for x, y in zip(ages, ages[1:])), all(x <= y for x, y in zip(ages, ages[1:]))
    return 0 if down == up else (-1 if down else 1)


def verdict(argv, before, frozen, after, freeze=None):
    """same | order | moved | worded | differs, and a line of explanation. freeze is when the case was
    frozen, in seconds, if that is known."""
    if -1 in (before["rc"], frozen["rc"], after["rc"]):
        return "differs", "timed out: " + ", ".join(n for n, r in (("live", before), ("frozen", frozen), ("live again", after)) if r["rc"] == -1)
    a, f, b = tokens(before["out"]), tokens(frozen["out"]), tokens(after["out"])
    ea, ef, eb = tokens(before["err"]), tokens(frozen["err"]), tokens(after["err"])
    said = lambda t: " ".join(" ".join(line) for line in t)[:160] or "(nothing)"
    failed = [r["rc"] != 0 for r in (before, frozen, after)]
    codes = f"exit codes live {before['rc']}, frozen {frozen['rc']}, live {after['rc']}: {said(ef) if failed[1] else said(eb)}"
    if failed[0] != failed[2]:
        # The cluster refused and then answered, or the other way round: a container that had not run
        # before and now has. The frozen answer is then one of the two, all of it, or it is neither.
        for live, out, err in ((before, a, ea), (after, b, eb)):
            if (live["rc"] != 0) == failed[1] and matches(f, out) and matches(ef, err):
                return "same", ""
        return "differs", codes
    if failed[1] != failed[0]:
        return "differs", codes
    verb = (past_namespace(argv) + [""])[0]
    raw = lambda r: r["out"].rstrip("\n").split("\n") if r["out"].strip() else []
    if verb == "logs":
        # A blank line in a log is a line of it, and `--tail` counts it: an application that writes one —
        # two threads' lines run together, and the newline of the second left over — has it in its last
        # five. Taken off the end, a frozen tail of five was four, and "shorter than the cluster's".
        raw = lambda r: (r["out"][:-1] if r["out"].endswith("\n") else r["out"]).split("\n") if r["out"] else []
    ra, rf, rb = raw(before), raw(frozen), raw(after)
    of_the_cluster = lambda: all(line in ra or line in rb for line in rf)  # no line but one the cluster printed
    several = verb == "logs" and any(x in ("-l", "--selector", "--all-containers", "--all-pods") or x.startswith(("-l=", "--selector=")) for x in argv)
    if all(failed):
        # The refusal has to be one of the cluster's two, in its words — also when the cluster refused in
        # one way and then in another. And what was printed before it has to be the cluster's too:
        # kubectl asks for several logs in no fixed order, so which of them it had printed when one
        # failed is chance, but each line of them is a line of a log.
        if (matches(ef, ea) or matches(ef, eb)) and (of_the_cluster() if several else matches(f, a) or matches(f, b)):
            return "same", ""
        return "worded", f"live: {said(ea if matches(ea, eb) else ea + eb)} | frozen: {said(ef) if not (matches(ef, ea) or matches(ef, eb)) else said(f)}"

    if verb == "logs":
        tail = flag(argv, "--tail")
        tail = int(tail) if tail is not None and tail.lstrip("-").isdigit() and int(tail) >= 0 else None  # --tail=-1 is all of it
        # The last so many seconds: a window that moves with the clock. Not the last none: `--since=0s` is
        # no window at all to kubectl, which then prints the log, and an empty answer to it is not one.
        sliding = flag(argv, "--since") is not None and not re.fullmatch(r"0+(ms|s|m|h)?", flag(argv, "--since"))

        def slid():
            """The cluster's own two windows overlap, so the log from the first line of one to the last
            of the other is known, and the frozen window is a stretch of it: those lines, in that order,
            none left out and none put in. A tail is no shorter than both of the cluster's, a tail not
            shrinking, and is the cluster's one window if its two are one. A window of time holds at
            least what both of the cluster's hold, having been open between them."""
            if not sliding and ra == rb:
                return rf == ra
            for run, begins in joined(ra, rb, of_time=sliding):
                for i in range(len(run) - len(rf) + 1):
                    if run[i:i + len(rf)] == rf and (i <= begins and i + len(rf) >= len(ra) if sliding else len(rf) >= min(len(ra), len(rb))):
                        return True  # and that may be nothing at all: a window of time open between two that share no line
            return False

        def whole():
            """Where the log went by too fast to hold the frozen lines to the live ones: there are still
            as many of them as the cluster gave, a tail not being shorter for being later — or they are
            the start of what the cluster gave next, the container having started again."""
            return sliding and bool(rf or not (ra and rb)) or not sliding and (len(rf) >= min(len(ra), len(rb)) or bool(rf) and rb[:len(rf)] == rf)

        def log():
            if rf == ra or rf == rb:  # what the cluster itself said, at one asking or the other
                return "same", ""
            if several:
                # Several logs, one after another: each grows where it stands, so the whole grows in the
                # middle. Every line the first asking saw is in the frozen one, in order, and every line of
                # the frozen one in the last.
                if within(ra, rf) and within(rf, rb) or tail is not None and slid():  # or it is one log after all
                    return "same", ""
                # kubectl keeps the logs it was asked for in a map and prints them as the map hands them
                # over: mostly in the order of their names, and one time in four with another of them
                # first and the rest following round (30 of 120 askings of one frozen case, 2026-10-08).
                # That is kubectl's, of a cluster as of a case. So the later two answers are each read
                # turned to the order the first came in, if a turn makes them fit; the lines of each log
                # are held to their order as before.
                turned = lambda lines, fits: next((lines[k:] + lines[:k] for k in range(1, len(lines)) if fits(lines[k:] + lines[:k])), None)
                later = rb if within(ra, rb) else turned(rb, lambda t: within(ra, t)) or rb
                found = rf if within(ra, rf) and within(rf, later) else turned(rf, lambda t: within(ra, t) and within(t, later))
                if found is not None:
                    return "order", "kubectl printed the logs of several pods with another of them first"
                if not within(ra, later) and whole():  # lines came between that neither live asking saw: nothing to hold the frozen ones to but their number
                    return "moved", "the logs went by faster than the tail asked for"
                return "differs", f"the frozen logs ({len(rf)} lines) do not sit between the two live ones ({len(ra)}, {len(rb)})"
            if not sliding and (tail is None or max(len(ra), len(rf), len(rb)) < tail):  # the whole log, which only grows
                grew, grows = rf[:len(ra)] == ra, rb[:len(rf)] == rf
                if grew and grows:
                    return "same", ""
                if rb[:len(ra)] != ra and (grew or grows and rf):
                    # The cluster's own log did not grow: it was replaced. The container started again between
                    # the two live askings, and the log of its run — or of the run before, under --previous —
                    # went with it. The frozen log is then the first one grown, or the start of the last one;
                    # an empty one is the start of anything, and says nothing. (What the first one grew by
                    # before it was replaced, no asking saw: that much of a frozen log is taken on trust.)
                    return "same", ""
                return "differs", f"the frozen log ({len(rf)} lines) does not sit between the two live ones ({len(ra)}, {len(rb)})"
            if tail is not None and len(rf) > tail:
                return "differs", f"{len(rf)} lines for --tail={tail}"
            if slid():
                return "same", ""
            if not continues(ra, rb) and whole():  # the cluster's two windows share no line: what came between, no asking saw
                return "moved", "the log went by faster than the tail asked for"
            return "differs", f"the frozen tail is neither a live one nor a live one moved on: live {ra[-1:]}, frozen {rf[-1:]}"

        what, why = log()
        if what == "same" and not (matches(ef, ea) or matches(ef, eb)):
            return "differs", f"stderr, live: {said(eb)} | frozen: {said(ef)}"  # the same log, and something else said beside it
        return what, why

    # An age in one answer is older than in another by as long as lay between the two countings, and
    # who counts depends on the command. `kubectl get` prints a table the server made, and a case's
    # server counts every age to the freeze, whenever it is asked; `describe` and `kubectl events` count
    # for themselves, from the moment they are run.
    def later(live, other):
        """How many seconds after live's ages other's were counted, if that is known."""
        if other is frozen and verb == "get" and freeze and "at" in live:
            return freeze - live["at"]
        return other["at"] - live["at"] if "at" in live and "at" in other else None
    fa, fb, ab = later(before, frozen), later(after, frozen), later(after, before)
    if matches(f, a, b, fa, later(before, after)) or matches(f, b, a, fb, ab):
        if matches(ef, ea) or matches(ef, eb):
            return "same", ""
        return "differs", f"stderr, live: {said(eb)} | frozen: {said(ef)}"  # the same answer, and something else said beside it
    still = matches(a, b, None, ab)  # the cluster said the same thing twice
    live, near = (a, fa) if still else (b, fb)
    pair = next(((x, y) for x, y in zip(live, f) if not matches([y], [x], None, near)), None)
    if pair is None:
        pair = (live[len(f)] if len(live) > len(f) else [], f[len(live)] if len(f) > len(live) else [])
    where = f"live: {' '.join(pair[0])[:170] or '(no such line)'} | frozen: {' '.join(pair[1])[:170] or '(no such line)'}"
    if loose(frozen["out"], False) in (loose(before["out"], False), loose(after["out"], False)):
        return "differs", "an age further from the live one than the askings were apart. " + where
    for answer, other in ((before, a), (after, b)):
        if loose(frozen["out"]) != loose(answer["out"]):
            continue
        # The same lines. Where kubectl sorts them itself — under --sort-by, and `kubectl events`, by time — two
        # rows of equal key may stand either way round; anywhere else the order is the cluster's, and matters.
        # Either way round is not any way round: a listing that runs from old to new does so frozen too.
        # And kubectl writes what a LimitRange limits in the order a Go map gives them up, which is none.
        of = (words(argv) + ["", ""])[1].split(".")[0].split("/")[0]
        if not (verb == "events" or any(x.startswith("--sort-by") for x in argv) or verb == "describe" and of in ("limitrange", "limitranges", "limits")):
            return "differs", "the same lines in another order. " + where
        if any(falls(other, at) and falls(f, at) not in (0, falls(other, at)) for at in (0, -1)):
            return "differs", "the same lines, sorted the other way. " + where
        return "order", ""
    if not still:
        # The cluster itself changed between the two live passes, and the frozen answer was taken in
        # between: it cannot be told line by line. But a line the cluster printed both times — its ages
        # and counts aside — it printed in
        # between as well, the heading among them, and in the same order if it kept one; it printed no
        # more lines than the longer of its two answers and no fewer than the shorter, each thing that
        # changed being still one line; and it said beside the answer what it said beside one of the others.
        la, lf, lb = (loose(r["out"], False) for r in (before, frozen, after))
        kept = set(la) & set(lb)
        stood = [line for line in la if line in kept]
        in_order = stood != [line for line in lb if line in kept] or stood == [line for line in lf if line in kept]
        if kept <= set(lf) and in_order and min(len(la), len(lb)) <= len(lf) <= max(len(la), len(lb)) and (matches(ef, ea) or matches(ef, eb)):
            return "moved", where
        return "differs", "the cluster moved, and the frozen answer is not between its two: " + where
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


def cmd_compare(before_path, frozen_path, after_path, known_path=None, freeze_path=None):
    before, frozen, after = (json.load(open(p)) for p in (before_path, frozen_path, after_path))
    known = load_known(known_path)
    freeze = json.load(open(freeze_path))["freeze_time"] if freeze_path else None
    rows = []
    for b, f, a in zip(before, frozen, after):
        assert b["argv"] == f["argv"] == a["argv"]
        rows.append((f["argv"], f.get("typed"), *verdict(f["argv"], b, f, a, freeze)))
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
    for title, kind in (("Differ", "differs"), ("Both fail, worded differently", "worded"), ("The cluster moved", "moved"), ("The same lines in another order, under --sort-by or of several logs", "order")):
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
