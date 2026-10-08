"""The same query against a Prometheus and against the case frozen from it, output compared.

    promdiff.py queries <the live Prometheus's URL> <queries.json> <requests.json> [--runs <dir of run records for this case>]...
    promdiff.py capture <queries.json> <dir holding promq> <PROM_URL> <out.json> [--at <Unix seconds>]
    promdiff.py fetch   <requests.json> <an endpoint's URL> <Unix seconds of the freeze> <out.json>
    promdiff.py compare <live-1.json> <frozen-named.json> <live-2.json> <frozen-as-typed.json, or - > [<known.txt>]
    promdiff.py spoil   <live-1.json> <frozen-named.json> <live-2.json>

`replaydiff.py` does this for `kubectl`. This is the other half of a case: what `promq`, the one
metrics tool an agent is given, prints.

`promq` asks about now, and no two askings share a now, so a metric's value is not something two
answers can be held to. An instant can be: a Prometheus asked about 19:24:05 says the same thing
whenever it is asked. So every query is put about one named instant — the freeze — to the live
Prometheus, to the frozen store, and to the live Prometheus again, and the three answers have to be
the same text. `queries` writes what to ask: a fixed set about every metric the Prometheus has, and
every `promq` command the recorded agents typed. `capture` asks. `compare` prints Markdown and exits
1 if an answer differs that <known.txt> does not excuse.

The frozen store is asked a second time the way an agent asks it, with no instant named, and that is
compared with its own named answer: what is measured against the Prometheus is then what an agent
gets, or the report says where it is not.

Not every agent reads through `promq`: one with a Prometheus tool of its own asks the HTTP API and
reads what comes back. So `queries` also writes the request `promq` makes for each query, and the
ones a client makes to find its way about — label names, label values, series — and `fetch` puts
them to an endpoint and keeps the answer as it was sent: the series in the order they came, each
value as the string it was, and whatever stands beside the result.

`spoil` says what the comparison is worth: it spoils every recorded frozen answer in seven ways and
counts the ones that still pass. It exits 1 if any does.
"""
import collections, concurrent.futures, datetime, glob, json, os, re, shlex, subprocess, sys, time, urllib.error, urllib.parse, urllib.request

sys.dont_write_bytecode = True
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import replaydiff as rd


def api(base, path, **params):
    query = urllib.parse.urlencode(params, doseq=True)
    with urllib.request.urlopen(f"{base.rstrip('/')}{path}?{query}", timeout=30) as resp:
        body = json.load(resp)
    if body.get("status") != "success":
        sys.exit(f"{path} of the live Prometheus failed, and nothing can be asked without it: {body}")
    return body["data"]


def typed_queries(run_dirs, left=None):
    """Every `promq` command in the recorded steps that can be asked as it stands, as an argument list."""
    out = []
    for d in run_dirs:
        for f in sorted(glob.glob(os.path.join(d, "*.json"))):
            for step in json.load(open(f))["transcript"].get("steps") or []:
                if step.get("tool") != "bash":
                    continue
                text = step["input"].split("\n")[0] if "holmes" in f else step["input"]
                for command in rd.shell_commands(text) or []:
                    try:
                        argv = None if rd.needs_a_shell(command) else shlex.split(command)
                    except ValueError:
                        argv = None
                    if argv is None:
                        if left is not None and re.search(r"\bpromq\b", command):
                            left.append(command)
                        continue
                    while argv and argv[0] in rd.BEFORE_A_COMMAND:
                        argv = argv[1:]
                    if argv[:1] == ["promq"] and len(argv) > 1:
                        out.append(argv[1:])
    return out


def whole(seconds):
    """Seconds as a duration Prometheus reads, and as a number an address can carry."""
    return f"{seconds:.3f}".rstrip("0").rstrip(".")


def tool_calls(run_dirs, left=None):
    """What an agent with a Prometheus tool of its own asked, which is not a `promq` in a shell: a
    query, as an instant or as a window of the length it asked for, and the ways it looked for what
    there is to ask about. Its window is put to end at the freeze, like every other; the times it
    named are its own run's. What its tool sent word for word is not in a record, so the requests
    here are what such a call asks for, not a copy. A call that cannot be read is appended to `left`."""
    queries, requests = [], []
    stamp = lambda t: datetime.datetime.fromisoformat(t.replace("Z", "+00:00")).timestamp()
    for d in run_dirs:
        for f in sorted(glob.glob(os.path.join(d, "*.json"))):
            for step in json.load(open(f))["transcript"].get("steps") or []:
                tool = step.get("tool") or ""
                if tool not in TOOLS:
                    continue
                try:
                    asked = json.loads(step.get("input", "").split("\n", 1)[1])
                    if not isinstance(asked, dict):
                        raise ValueError
                    window = [f"start=<freeze>-{FROZEN_WINDOW}", "end=<freeze>"]
                    match = ["match[]=" + asked["match"]] if asked.get("match") else []
                    if tool == "execute_prometheus_instant_query":
                        queries.append([asked["query"]])
                    elif tool == "execute_prometheus_range_query":
                        length, every = stamp(asked["end"]) - stamp(asked["start"]), float(asked.get("step") or 15)
                        if length < 0 or every <= 0:
                            raise ValueError
                        queries.append([asked["query"], "--range", whole(length) + "s", "--step", whole(every) + "s"])
                    elif tool == "get_metric_names":
                        requests.append(["/api/v1/label/__name__/values"] + match + window)
                    elif tool == "get_label_values":
                        requests.append([f"/api/v1/label/{asked['label']}/values"] + match + window)
                    elif tool == "get_series":
                        requests.append(["/api/v1/series", "match[]=" + asked["match"]] + window)
                    elif tool == "get_metric_metadata":
                        requests.append(["/api/v1/metadata"] + (["metric=" + asked["metric"]] if asked.get("metric") else []))
                except (IndexError, KeyError, TypeError, ValueError, AttributeError):
                    if left is not None:
                        left.append(f"{tool}: {' '.join(step.get('input', '').split())}")
    return queries, requests


TOOLS = {"execute_prometheus_instant_query", "execute_prometheus_range_query", "get_metric_names", "get_label_values", "get_series", "get_metric_metadata"}


def fixed_queries(base):
    """Questions about every metric the Prometheus holds: its samples as they are stored, which of
    them an instant picks, what a window holds at its edges, the functions an investigation reaches
    for, and a window of each as a range. Then the ones about no metric in particular, and the ones
    that fail."""
    out = []
    for m in api(base, "/api/v1/label/__name__/values"):
        series = api(base, "/api/v1/series", **{"match[]": m})
        names = sorted({k for s in series for k in s if k != "__name__"})
        out += [[m], [f"{m}[30s]"], [f"timestamp({m})"], [f"count({m})"], [f"sum({m})"], [f"{m} offset 1m"], [f"{m} offset 20m"],
                [f"count_over_time({m}[2m])"], [f"rate({m}[1m])"], [f"irate({m}[1m])"], [f"increase({m}[5m])"], [f"delta({m}[2m])"],
                [f"max_over_time({m}[5m])"], [f"avg_over_time({m}[10m])"], [f"changes({m}[5m])"], [f"resets({m}[5m])"], [f"quantile_over_time(0.9, {m}[5m])"],
                [f"topk(3, {m})"], [f"bottomk(2, {m})"], [f"sort_desc({m})"], [f"sort({m})"], [f"max_over_time(rate({m}[1m])[5m:30s])"], [f"max_over_time({m}[2m:])"],
                [f"{m}[1m:20s]"], [f"round({m}, 10)"], [f"sort_desc(round({m}, 1000))"],
                [m, "--range", "5m"], [m, "--range", "2m", "--step", "5s"], [f"rate({m}[1m])", "--range", "10m", "--step", "30s"],
                [f"sum({m})", "--range", "30m", "--step", "1m"], [f"increase({m}[5m])", "--range", "1h", "--step", "5m"]]
        for label in names:
            value = sorted({s[label] for s in series if label in s})[0]
            out += [[f"sum by ({label}) ({m})"], [f"count without ({label}) ({m})"], [f'{m}{{{label}="{value}"}}'], [f'{m}{{{label}!="{value}"}}'],
                    [f'{m}{{{label}=~".+"}}'], [f"sum by ({label}) (rate({m}[2m]))", "--range", "5m", "--step", "1m"]]
        if m.endswith("_bucket"):
            out += [[f"histogram_quantile(0.9, sum by (le) (rate({m}[5m])))"], [f"histogram_quantile(0.5, rate({m}[1m]))"],
                    [f"histogram_quantile(0.99, sum by (le) (rate({m}[1m])))", "--range", "10m", "--step", "30s"]]
    everything = '{__name__=~".+"}'
    out += [["up"], ["up", "--range", "1h"], ["up", "--range", "2m", "--step", "1s"], ["up", "--range", "30m", "--step", "5s"], ["up offset 10m"], ["up offset 2h"],
            [everything], [f"count({everything})"], [f"count by (__name__) ({everything})"], [f"sort_desc(count by (__name__) ({everything}))"],
            [f"topk(3, count by (__name__) ({everything}))"], [f"count by (job) ({everything})"], [f"group by (instance) ({everything})"],
            [f"count_over_time({everything}[1m])"], [f"timestamp({everything})"], [f"{everything}[12s]"],
            ["absent(up)"], ["absent(no_such_metric)"], ["no_such_metric"], ["no_such_metric", "--range", "5m"], ["vector(1)"], ["time()"], ["scalar(count(up))"],
            ["count(up) + 1"], ["up == 1"], ["up == bool 1"], ["up and on (job) up"], ["sum(up) / count(up)"], ["up * on (instance) group_left () up"],
            ['label_replace(up, "target", "$1", "instance", "([^:]+):.*")'], ['count_values("value", up)'], ["up @ end()"], ["up @ start()"], ["timestamp(up @ end())"],
            ["up offset -1m"], ["up offset -1h"], ["count_over_time(up[5m:])"], ["up[1m:10s]"], [f"sort(count by (__name__) ({everything}))"],
            [f"bottomk(2, count by (__name__) ({everything}))"], [f"sort_desc(count by (job) ({everything}))"], [f"count by (__name__, job) ({everything})"],
            ["time()", "--range", "1m", "--step", "20s"], ["vector(1)", "--range", "1m"], ["1"], ['"a string"'],
            # And what fails: the words a client gets back are part of the answer.
            ["sum("], ["rate(up)"], ["up[5m"], ["up{job=}"], ["no_such_function(up)"], ["up", "--range", "soon"], ["up", "--range", "5m", "--step", "0s"],
            ["up", "--range", "30d", "--step", "1s"], ["sum by (job) (up) by (job)"], ["up offset"], ["1 +"], ["histogram_quantile(up)"]]
    return out


DURATION = re.compile(r"(\d+(?:\.\d+)?)(ms|s|m|h|d|w|y)")
SECONDS = {"ms": 0.001, "s": 1, "m": 60, "h": 3600, "d": 86400, "w": 7 * 86400, "y": 365 * 86400}
FROZEN_WINDOW = 1800  # what the sweep freezes: --metrics-window 30m


def seconds(text):
    """A duration as Prometheus writes one, in seconds; None if it is not one."""
    if not text or DURATION.sub("", text):
        return None
    return sum(float(n) * SECONDS[unit] for n, unit in DURATION.findall(text))


def request_of(argv):
    """The request promq makes for these arguments, about an instant named later: the path, then
    each parameter, with `<freeze>` or `<freeze>-<seconds>` where a time goes. None where promq
    makes none, because it does not understand its own arguments."""
    opt = dict(zip(argv[1::2], argv[2::2]))
    if "--range" not in opt:
        return ["/api/v1/query", "query=" + argv[0], "time=<freeze>"]
    window = seconds(opt["--range"])
    if window is None:
        return None
    return ["/api/v1/query_range", "query=" + argv[0], "start=<freeze>-" + whole(window), "end=<freeze>", "step=" + opt.get("--step", "15s")]


def discovery(base):
    """What a client asks to find its way about, and what it asks wrongly."""
    window = [f"start=<freeze>-{FROZEN_WINDOW}", "end=<freeze>"]
    names, labels = api(base, "/api/v1/label/__name__/values"), api(base, "/api/v1/labels")
    everything = 'match[]={__name__=~".+"}'
    out = [["/api/v1/labels"], ["/api/v1/labels"] + window, ["/api/v1/labels", "match[]=up"] + window, ["/api/v1/labels", "match[]=no_such_metric"] + window,
           ["/api/v1/series", everything] + window, ["/api/v1/series", "match[]=up", "match[]=" + names[0]] + window, ["/api/v1/series", "match[]=no_such_metric"] + window,
           ["/api/v1/series"] + window, ["/api/v1/series", "match[]=up{"] + window, ["/api/v1/label/no_such_label/values"] + window, ["/api/v1/label/__name__/values"],
           ["/api/v1/series", "match[]={}"] + window, ["/api/v1/series", 'match[]={job=~".*"}'] + window, ["/api/v1/series", "match[]=up", "start=yesterday"],
           ["/api/v1/labels", "match[]=up{"] + window, ["/api/v1/labels", "end=later"], ["/api/v1/label/job/values", "match[]={}"] + window, ["/api/v1/label/job/values", "start=soon"],
           # A query with no time named is about the endpoint's own now, which is not one instant for two endpoints: not asked.
           ["/api/v1/query", "time=<freeze>"], ["/api/v1/query", "query=", "time=<freeze>"], ["/api/v1/query", "query=up", "time=yesterday"], ["/api/v1/query", "query=up", "time=<freeze>-60"],
           ["/api/v1/query", "query=time()", "time=<freeze>-60"], ["/api/v1/query_range", "query=up"], ["/api/v1/query_range", "query=up", "start=<freeze>-60"],
           ["/api/v1/query_range", "query=up", "start=<freeze>-60", "end=<freeze>"], ["/api/v1/query_range", "query=up", "start=<freeze>", "end=<freeze>-60", "step=15"],
           ["/api/v1/query_range", "query=up", "start=<freeze>-60", "end=<freeze>", "step=often"], ["/api/v1/query_range", "query=up", "start=<freeze>-60", "end=<freeze>", "step=15"],
           ["/api/v1/query_range", "query=up", "start=<freeze>-600", "end=<freeze>-300", "step=1m"], ["/api/v1/no_such_endpoint"],
           # What a request may be asked with besides itself and its times, which changes the answer.
           ["/api/v1/query", "query=" + everything[8:], "time=<freeze>", "limit=2"], ["/api/v1/query", "query=up", "time=<freeze>", "limit=many"],
           ["/api/v1/query", "query=up", "time=<freeze>", "lookback_delta=1ms"], ["/api/v1/query", "query=up", "time=<freeze>", "lookback_delta=10m"],
           ["/api/v1/query", "query=up", "time=<freeze>", "lookback_delta=far"], ["/api/v1/query", "query=up", "time=<freeze>", "timeout=30s"],
           ["/api/v1/query", "query=up", "time=<freeze>", "timeout=soon"],  # not `stats`: what a Prometheus sends for it is how long it took
           ["/api/v1/query_range", "query=" + everything[8:], "start=<freeze>-60", "end=<freeze>", "step=15", "limit=3"],
           ["/api/v1/query_range", "query=up", "start=<freeze>-60", "end=<freeze>", "step=1e30"], ["/api/v1/query_range", "query=up", "start=<freeze>", "end=<freeze>", "step=15"],
           # Not a label's values with a limit: a Prometheus sends whichever of them it met first, and not the same ones twice.
           ["/api/v1/labels", "limit=2"] + window, ["/api/v1/labels", "limit=some"], ["/api/v1/label/__name__/values", "limit=-3"] + window,
           ["/api/v1/series", everything, "limit=4"] + window, ["/api/v1/series", everything, "limit=-1"] + window]
    for label in labels:
        out += [[f"/api/v1/label/{label}/values"] + window, [f"/api/v1/label/{label}/values", "match[]=up"] + window]
    out += [["/api/v1/series", "match[]=" + m] + window for m in names]
    return out


def cmd_queries(base, out_path, requests_path, extra):
    runs = [value for flag, value in zip(extra[::2], extra[1::2]) if flag == "--runs"]
    left = []
    by_tool, tool_requests = tool_calls(runs, left)
    typed = typed_queries(runs, left) + by_tool
    was_typed, seen, queries = {tuple(a) for a in typed}, set(), []
    for argv in fixed_queries(base) + typed:
        if tuple(argv) not in seen:
            seen.add(tuple(argv))
            queries.append({"argv": argv, "typed": tuple(argv) in was_typed})
    if len(queries) < 50:
        sys.exit(f"only {len(queries)} queries to ask: the Prometheus was not read")
    json.dump(queries, open(out_path, "w"), indent=0)
    requests, asked = [], {}
    for request, was in [(request_of(q["argv"]), q["typed"]) for q in queries] + [(r, True) for r in tool_requests] + [(r, False) for r in discovery(base)]:
        if request and tuple(request) in asked:
            asked[tuple(request)]["typed"] |= was  # asked by an agent, whoever else asks it too
        elif request:
            asked[tuple(request)] = {"argv": ["GET"] + request, "typed": was}
            requests.append(asked[tuple(request)])
    json.dump(requests, open(requests_path, "w"), indent=0)
    print(f"{len(queries)} queries, {sum(q['typed'] for q in queries)} of them asked by a recorded agent; {len(requests)} requests to the API, {sum(r['typed'] for r in requests)} of them")
    if left:
        print(f"{len(left)} more that an agent asked are not asked: a promq that needs a shell to mean anything, or a call of its own tool that cannot be read")
        for command in sorted(set(left)):
            print("  not asked: " + " ".join(command.split())[:200])


def cmd_capture(queries_path, bin_dir, prom_url, out_path, extra):
    env = {"PATH": bin_dir + os.pathsep + os.environ.get("PATH", ""), "PROM_URL": prom_url, "HOME": os.environ.get("HOME", "")}
    at = extra[:2] if extra[:1] == ["--at"] else []  # promq's own flag: the instant, named

    def ask(q):
        try:
            r = subprocess.run([os.path.join(bin_dir, "promq"), *q["argv"], *at], capture_output=True, text=True, timeout=60, env=env)
            return {"argv": q["argv"], "typed": q["typed"], "rc": r.returncode, "out": r.stdout + r.stderr}
        except subprocess.TimeoutExpired:
            return {"argv": q["argv"], "typed": q["typed"], "rc": 124, "out": "(timed out after 60 seconds)\n"}

    queries = json.load(open(queries_path))
    began = time.time()
    with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
        answers = list(pool.map(ask, queries))
    json.dump(answers, open(out_path, "w"), indent=0)
    print(f"{len(answers)} queries asked in {time.time() - began:.0f}s; {sum(a['rc'] != 0 for a in answers)} did not exit 0")
    if sum(a["rc"] != 0 for a in answers) > len(answers) / 2:
        sys.exit("more than half the queries failed: the endpoint was not there to ask")


def cmd_fetch(requests_path, base, freeze, out_path):
    freeze = float(freeze)

    def named(value):  # `<freeze>` and `<freeze>-90`, as the seconds they stand for
        return re.sub(r"<freeze>(?:-([0-9.]+))?", lambda m: f"{freeze - float(m.group(1) or 0):.3f}", value)

    def ask(request):
        path, params = request["argv"][1], [tuple(named(p).split("=", 1)) for p in request["argv"][2:]]
        try:
            with urllib.request.urlopen(f"{base.rstrip('/')}{path}?{urllib.parse.urlencode(params)}", timeout=60) as resp:
                code, body = resp.status, resp.read()
        except urllib.error.HTTPError as refused:
            code, body = refused.code, refused.read()
        except OSError as failed:  # no answer at all is an answer to compare: it is how a closed connection was found
            code, body = 0, f"no answer: {failed}".encode()
        try:  # as it was sent, but for what JSON does not mean, the order of an object's keys: a number is kept as it was written
            sent = json.loads(body, parse_int=lambda n: "#" + n, parse_float=lambda n: "#" + n)
            for beside in ("warnings", "infos"):  # and the order of the engine's remarks, which is a map's
                if isinstance(sent, dict) and isinstance(sent.get(beside), list):
                    sent[beside].sort()
            out = json.dumps(sent, sort_keys=True, indent=1) + "\n"
        except ValueError:
            out = body.decode(errors="replace")
        return {"argv": request["argv"], "typed": request["typed"], "rc": code, "out": out}

    requests = json.load(open(requests_path))
    with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
        answers = list(pool.map(ask, requests))
    json.dump(answers, open(out_path, "w"), indent=0)
    print(f"{len(answers)} requests made; {sum(a['rc'] != 200 for a in answers)} were not answered 200")
    if sum(a["rc"] != 200 for a in answers) > len(answers) / 2:
        sys.exit("more than half the requests were not answered: the endpoint was not there to ask")


VERDICTS = ["same", "order", "moved", "worded", "differs"]
MEANS = {"same": "the same", "order": "the same series and values in another order", "moved": "the Prometheus's own answer changed between its two askings",
         "worded": "both fail, worded differently", "differs": "differ"}


MAP_ORDERED = re.compile(r"^query=[\s(]*(count_values|histogram_quantile|histogram_fraction)\b")


def reordered(live, frozen):
    """Two answers of the API that hold the same series with the same values, in another order."""
    try:
        a, f = json.loads(live["out"]), json.loads(frozen["out"])
        x, y = a["data"]["result"], f["data"]["result"]
    except (ValueError, KeyError, TypeError):
        return False
    if not (isinstance(x, list) and isinstance(y, list) and all(isinstance(r, dict) for r in x + y)):
        return False
    key = lambda r: json.dumps(r, sort_keys=True)
    a["data"]["result"], f["data"]["result"] = sorted(x, key=key), sorted(y, key=key)
    return x != y and a == f


def unordered(argv, live1, live2):
    """Whether the order of this answer is no one's. PromQL leaves the order of a result open, and for
    some of what an engine does it is a map's and changes from one asking to the next of one
    Prometheus: `count_values`, the groups of `histogram_quantile`. That is so of a request whose
    outermost operation is one of those, and of any whose two answers from the Prometheus itself
    came in two orders. Of nothing else: the series of a selector come as the store holds them, a
    sorted answer as it was sorted and a window by label, and another order of those is a difference."""
    return any(MAP_ORDERED.search(part) for part in argv[2:]) or reordered(live1, live2)


def verdict(argv, live1, frozen, live2):
    """One query's three answers about one instant. They are text, and the same text or not: nothing
    here is an age. If the Prometheus itself answered twice differently — a sample stamped before the
    freeze and committed after the first asking — the frozen answer has to be one of the two."""
    if frozen["out"] == live1["out"] == live2["out"] and frozen["rc"] == live1["rc"] == live2["rc"]:
        return "same"
    if frozen["rc"] == live1["rc"] == live2["rc"] == 200 and unordered(argv, live1, live2) and reordered(live1, frozen) and (live1["out"] == live2["out"] or reordered(live1, live2)):
        return "order"
    if (live1["out"], live1["rc"]) != (live2["out"], live2["rc"]):
        return "moved" if (frozen["out"], frozen["rc"]) in ((live1["out"], live1["rc"]), (live2["out"], live2["rc"])) else "differs"
    if frozen["rc"] == live1["rc"] and frozen["rc"] not in (0, 200):  # promq's exit code, or the API's status
        return "worded"
    return "differs"


def first_difference(a, b):
    """Where two answers part: the line of each, and how many lines each has."""
    x, y = a.rstrip("\n").split("\n"), b.rstrip("\n").split("\n")
    for i, (p, q) in enumerate(zip(x, y)):
        if p != q:
            j = next((k for k, (c, d) in enumerate(zip(p, q)) if c != d), min(len(p), len(q)))
            lo = max(0, j - 60)
            return f"line {i + 1} of {len(x)} and {len(y)}: live `…{p[lo:j + 60]}` | frozen `…{q[lo:j + 60]}`"
    return f"{len(x)} lines live, {len(y)} frozen; the shorter is the start of the longer"


def shown(argv):
    if argv[:1] == ["GET"]:  # a request to the API, as it reads before it is encoded
        return "GET " + argv[1] + ("?" + "&".join(argv[2:]) if argv[2:] else "")
    return "promq " + " ".join(shlex.quote(a) for a in argv)


def excused(argv, live, frozen, known):
    """Why a difference is known, if it is: a line of known.txt names the query, what the Prometheus
    has to say and what the frozen store has to say for it to be that difference and no other."""
    return next((why for pattern, there, here, why in known if re.search(pattern, shown(argv)) and re.search(there, live["out"]) and re.search(here, frozen["out"])), None)


# More answers than this that the Prometheus itself changed between its two askings, and nothing was
# compared: its second asking was of something else, or of nothing.
MOVED_AT_MOST = 0.02


def cmd_compare(live1_path, frozen_path, live2_path, typed_path, known_path=None):
    live1, frozen, live2 = (json.load(open(p)) for p in (live1_path, frozen_path, live2_path))
    as_typed = json.load(open(typed_path)) if typed_path != "-" else frozen
    what = "requests" if frozen and frozen[0]["argv"][:1] == ["GET"] else "queries"
    known = []
    for line in open(known_path) if known_path else []:
        if line.strip() and not line.startswith("#"):
            pattern, there, here, why = line.rstrip("\n").split("\t")
            known.append((pattern, there, here, why))
    if not (len(live1) == len(frozen) == len(live2) == len(as_typed)) or len(frozen) < 50:
        print(f"{what}: {len(frozen)} — the askings are not of the same {what}, or of too few to be a comparison")
        return 1
    rows = [(f["argv"], f["typed"], verdict(f["argv"], a, f, b), a, f, b, t) for a, f, b, t in zip(live1, frozen, live2, as_typed)]
    count = collections.Counter(r[2] for r in rows)
    print(f"{what}: {len(rows)} | " + " | ".join(f"{MEANS[v]}: {count[v]}" if v != "differs" else f"**differ: {count[v]}**" for v in VERDICTS))
    typed = collections.Counter(r[2] for r in rows if r[1])
    print(f"\nOf the {sum(typed.values())} that a recorded agent had typed: " + ", ".join(f"{MEANS[v]} {typed[v]}" for v in VERDICTS) + ".")
    fine = lambda a: a["rc"] in (0, 200)
    empty = lambda a: a["out"].strip() == "(empty result)" or bool(re.search(r'"(result|data)": \[\]', a["out"]))
    answered = sum(fine(r[3]) and not empty(r[3]) for r in rows)
    print(f"\nOf the {len(rows)}, the Prometheus answered {answered} with something, {sum(fine(r[3]) for r in rows) - answered} with an empty result, and refused {sum(not fine(r[3]) for r in rows)}.")

    unexcused = 0
    for v in ("differs", "worded", "order", "moved"):
        if count[v]:
            print(f"\n### {MEANS[v][0].upper() + MEANS[v][1:]}\n")
        for argv, _, got, a, f, b, _ in rows:
            if got != v:
                continue
            fails = v in ("differs", "worded")
            why = excused(argv, a, f, known) if fails and (a["out"], a["rc"]) == (b["out"], b["rc"]) else None  # of an answer the Prometheus gave twice
            unexcused += fails and why is None
            print(f"- `{shown(argv)}`" + (f" (known: {why})" if why else ""))
            other = a if (a["out"], a["rc"]) != (f["out"], f["rc"]) else b
            print("  - " + (f"exit codes live {a['rc']}, frozen {f['rc']}, live {b['rc']}: " if len({a['rc'], f['rc'], b['rc']}) > 1 else "") + first_difference(other["out"], f["out"]))

    # The frozen store as an agent asks it, beside the frozen store asked about its freeze by name.
    apart = [(f["argv"], f, t) for _, _, _, _, f, _, t in rows if (f["out"], f["rc"]) != (t["out"], t["rc"])]
    if typed_path != "-":
        print(f"\n### As an agent asks: no instant named\n\nThe frozen store answered {len(rows) - len(apart)} of the {len(rows)} the same with no instant named as about its freeze by name.")
    for argv, f, t in apart:
        unexcused += 1
        print(f"- `{shown(argv)}`")
        print("  - " + first_difference(f["out"], t["out"]).replace("live `", "named `").replace("frozen `", "not named `").replace("lines live", "lines named").replace("frozen;", "not named;"))
    print(f"\nDiffering and not excused: {unexcused}.")
    if count["moved"] > MOVED_AT_MOST * len(rows):
        print(f"\n**The Prometheus changed {count['moved']} of its own answers between its two askings: that is not one Prometheus asked twice about one instant, and nothing here was compared.**")
        return 1
    return 1 if unexcused else 0


SPOILED = [
    ("replaced by nothing", lambda lines: []),
    ("replaced by an empty result", lambda lines: ["(empty result)"]),
    ("its last line taken off", lambda lines: lines[:-1]),
    ("its last line written twice", lambda lines: lines + lines[-1:]),
    ("the last digit of its last line another", lambda lines: lines[:-1] + [lines[-1][:-1] + ("7" if lines[-1][-1:] != "7" else "3")]),
    ("its lines the other way up", lambda lines: lines[::-1]),
]


def series_reversed(lines):
    """An answer of the API with its series the other way round; as it was, if it has not two."""
    try:
        sent = json.loads("\n".join(lines))
        result = sent["data"]["result"]
        if len(result) < 2 or not all(isinstance(r, dict) for r in result):
            return lines
    except (ValueError, KeyError, TypeError):
        return lines
    sent["data"]["result"] = result[::-1]
    return json.dumps(sent, sort_keys=True, indent=1).split("\n")


SPOILED.append(("its series the other way round, of the API", series_reversed))


def cmd_spoil(live1_path, frozen_path, live2_path):
    live1, frozen, live2 = (json.load(open(p)) for p in (live1_path, frozen_path, live2_path))
    print("| the frozen answer, spoiled | still passes | of |\n|---|---|---|")
    failing = 0
    for name, spoil in SPOILED:
        tried, passed = 0, []
        for a, f, b in zip(live1, frozen, live2):
            lines = f["out"].rstrip("\n").split("\n")
            spoiled = dict(f, out="".join(line + "\n" for line in spoil(lines)))
            if spoiled["out"] == f["out"]:
                continue  # nothing to tell apart: one line, the other way up
            tried += 1
            if verdict(f["argv"], a, spoiled, b) in ("same", "order", "moved"):
                passed.append(shown(f["argv"]))
        print(f"| {name} | {len(passed)} | {tried} |")
        for query in passed[:5]:
            print(f"  - `{query}`")
        failing += len(passed)
    return 1 if failing else 0


if __name__ == "__main__":
    if len(sys.argv) < 3:
        sys.exit(__doc__)
    if sys.argv[1] == "queries":
        cmd_queries(sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5:])
    elif sys.argv[1] == "fetch":
        cmd_fetch(*sys.argv[2:6])
    elif sys.argv[1] == "capture":
        cmd_capture(sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5], sys.argv[6:])
    elif sys.argv[1] == "compare":
        sys.exit(cmd_compare(*sys.argv[2:]))
    elif sys.argv[1] == "spoil":
        sys.exit(cmd_spoil(*sys.argv[2:]))
    else:
        sys.exit(__doc__)
