"""The same query against a Prometheus and against the case frozen from it, output compared.

    promdiff.py queries <the live Prometheus's URL> <queries.json> <requests.json> [--runs <dir of run records for this case>]...
    promdiff.py capture <queries.json> <dir holding promq> <PROM_URL> <out.json> [--at <Unix seconds>]
    promdiff.py fetch   <requests.json> <an endpoint's URL> <Unix seconds of the freeze> <out.json>
    promdiff.py compare <live-1.json> <frozen-named.json> <live-2.json> <frozen-as-typed.json, or - > [<known.txt> [<the frozen case's freeze.json>]]
    promdiff.py spoil   <live-1.json> <frozen-named.json> <live-2.json> [<the frozen case's freeze.json>]

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

Three of the queries look back further than the hour a sweep freezes, on purpose. A case has
nothing there and says so beside its answer, and with its freeze.json given, `compare` holds those
three to what they then have to be: the Prometheus's answer with that much taken off, in the words
the case's own beginning gives. Without it they differ, as the same words on any other answer do.

`spoil` says what the comparison is worth: it spoils every recorded frozen answer in seventeen ways,
six of them of the answers that look back alone, and counts the ones that still pass. It exits 1 if
any does.
"""
import collections, concurrent.futures, datetime, glob, http.client, json, os, re, shlex, subprocess, sys, threading, time, urllib.error, urllib.parse, urllib.request

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
                # What an engine works out and does not merely pick: where two engines part past the tenth digit, if they do.
                [f"last_over_time({m}[1m])"], [f"min_over_time({m}[5m])"], [f"sum_over_time({m}[2m])"], [f"stddev_over_time({m}[5m])"], [f"stdvar_over_time({m}[5m])"],
                [f"deriv({m}[5m])"], [f"predict_linear({m}[5m], 600)"], [f"idelta({m}[1m])"], [f"present_over_time({m}[5m])"], [f"first_over_time({m}[5m])"],
                [m, "--range", "5m"], [m, "--range", "2m", "--step", "5s"], [f"rate({m}[1m])", "--range", "10m", "--step", "30s"],
                [f"sum({m})", "--range", "30m", "--step", "1m"], [f"increase({m}[5m])", "--range", "1h", "--step", "5m"]]
        for label in names:
            value = sorted({s[label] for s in series if label in s})[0]
            out += [[f"sum by ({label}) ({m})"], [f"count without ({label}) ({m})"], [f'{m}{{{label}="{value}"}}'], [f'{m}{{{label}!="{value}"}}'],
                    [f'{m}{{{label}=~".+"}}'], [f"sum by ({label}) (rate({m}[2m]))", "--range", "5m", "--step", "1m"]]
        if m.endswith("_bucket"):
            out += [[f"histogram_quantile(0.9, sum by (le) (rate({m}[5m])))"], [f"histogram_quantile(0.5, rate({m}[1m]))"],
                    [f"histogram_quantile(0.99, sum by (le) (rate({m}[1m])))", "--range", "10m", "--step", "30s"],
                    [f"histogram_quantile(0, rate({m}[5m]))"], [f"histogram_quantile(1, rate({m}[5m]))"], [f"histogram_quantile(1.5, rate({m}[5m]))"],
                    [f"histogram_quantile(0.9, {m})"], [f"histogram_fraction(0, 0.1, rate({m}[5m]))"], [f"histogram_fraction(0, 0.1, sum by (le) (rate({m}[5m])))"],
                    [f"histogram_fraction(0.05, 2, rate({m}[5m]))", "--range", "10m", "--step", "1m"], [f"histogram_count(rate({m}[5m]))"]]
    everything = '{__name__=~".+"}'
    out += [["up"], ["up", "--range", "1h"], ["up", "--range", "2m", "--step", "1s"], ["up", "--range", "30m", "--step", "5s"], ["up offset 10m"], ["up offset 2h"],
            # Further back than the hour that is frozen, on purpose (LOOKS_BACK): the case has to say so, and to answer with what is left.
            *[list(asked) for asked in LOOKS_BACK if list(asked) != ["up offset 2h"]],
            [everything], [f"count({everything})"], [f"count by (__name__) ({everything})"], [f"sort_desc(count by (__name__) ({everything}))"],
            [f"topk(3, count by (__name__) ({everything}))"], [f"count by (job) ({everything})"], [f"group by (instance) ({everything})"],
            [f"count_over_time({everything}[1m])"], [f"timestamp({everything})"], [f"{everything}[12s]"],
            ["absent(up)"], ["absent(no_such_metric)"], ["no_such_metric"], ["no_such_metric", "--range", "5m"], ["vector(1)"], ["time()"], ["scalar(count(up))"],
            ["count(up) + 1"], ["up == 1"], ["up == bool 1"], ["up and on (job) up"], ["sum(up) / count(up)"], ["up * on (instance) group_left () up"],
            ['label_replace(up, "target", "$1", "instance", "([^:]+):.*")'], ['count_values("value", up)'], ["up @ end()"], ["up @ start()"], ["timestamp(up @ end())"],
            ["up offset -1m"], ["up offset -1h"], ["count_over_time(up[5m:])"], ["up[1m:10s]"], [f"sort(count by (__name__) ({everything}))"],
            [f"bottomk(2, count by (__name__) ({everything}))"], [f"sort_desc(count by (job) ({everything}))"], [f"count by (__name__, job) ({everything})"],
            ["time()", "--range", "1m", "--step", "20s"], ["vector(1)", "--range", "1m"], ["1"], ['"a string"'],
            # A scalar's value is written in full by a Prometheus, and a sample's with an exponent where it is very large or very small.
            ["1e30"], ["1e-7"], ["scalar(vector(1e21))"], ["vector(1e21)"], ["vector(1e-7)"], ["1e30", "--range", "1m", "--step", "20s"],
            # And what fails: the words a client gets back are part of the answer.
            ["sum("], ["rate(up)"], ["up[5m"], ["up{job=}"], ["no_such_function(up)"], ["up", "--range", "soon"], ["up", "--range", "5m", "--step", "0s"],
            ["up", "--range", "30d", "--step", "1s"], ["sum by (job) (up) by (job)"], ["up offset"], ["1 +"], ["histogram_quantile(up)"]]
    return out


DURATION = re.compile(r"(\d+(?:\.\d+)?)(ms|s|m|h|d|w|y)")
SECONDS = {"ms": 0.001, "s": 1, "m": 60, "h": 3600, "d": 86400, "w": 7 * 86400, "y": 365 * 86400}
FROZEN_WINDOW = 3600  # what the sweep freezes: --metrics-window 1h


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
    # What kind of metric each is. Not with a limit that cuts anything: a Prometheus keeps whichever its maps hand it first.
    described = api(base, "/api/v1/metadata")
    out += [["/api/v1/metadata"], ["/api/v1/metadata", "metric=no_such_metric"], ["/api/v1/metadata", "metric="], ["/api/v1/metadata", "limit=0"], ["/api/v1/metadata", "limit=-1"],
            ["/api/v1/metadata", f"limit={len(described) + 5}"], ["/api/v1/metadata", "limit=some"], ["/api/v1/metadata", "limit_per_metric=0"], ["/api/v1/metadata", "limit_per_metric=50"],
            ["/api/v1/metadata", "limit_per_metric=few"], ["/api/v1/metadata", "metric=up"]]
    out += [["/api/v1/metadata", "metric=" + m] for m in sorted(set(names) | set(described))]
    # An instant other than the freeze, of every metric.
    out += [["/api/v1/query", "query=" + m, "time=<freeze>-90"] for m in names]
    # And instants whose thousandths end in nothing. A Prometheus writes the instant of a scalar and of
    # a string as the shortest number that is it, and a sample's to the thousandth: half past a
    # second is `.5` beside the one and `.500` beside the other. A freeze falls on such an instant one
    # time in ten, which is how this was found; these are asked of one every time. `<second>` is the
    # whole second the freeze is in.
    out += [["/api/v1/query", "query=" + q, "time=<second>-" + ago] for q in ("time()", '"a string"', "1", "scalar(count(up))", "up", "vector(1)") for ago in ("0.5", "0.75", "1")]
    out += [["/api/v1/query_range", "query=" + q, "start=<second>-60.5", "end=<second>-0.5", "step=15"] for q in ("time()", "up")]
    # A listing of series is of the whole of a case, whatever window it is asked for: which series a
    # Prometheus lists for a window is by its chunks, and a case has samples. So no listing is asked
    # for less than the whole but this one, of the last minute and of the series a Prometheus scrapes
    # of itself where it does: they are all there all the while, and the window changes their order
    # alone — the head's, where the whole window has them by label.
    out += [["/api/v1/series", 'match[]={job="prometheus"}', "start=<freeze>-60", "end=<freeze>"]]
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

    def named(value):  # `<freeze>` and `<freeze>-90`, as the seconds they stand for; `<second>` is the whole second the freeze is in
        return re.sub(r"<(freeze|second)>(?:-([0-9.]+))?", lambda m: f"{(freeze if m.group(1) == 'freeze' else freeze // 1) - float(m.group(2) or 0):.3f}", value)

    # One connection a worker, kept open: a connection a request ran this machine out of ports to
    # connect from at fifteen thousand requests, and a third of them were never made.
    where, mine = urllib.parse.urlsplit(base), threading.local()

    def get(target):
        failed = None
        for again in (False, True):  # a kept connection the other end has closed is found by asking on it
            if getattr(mine, "conn", None) is None:
                mine.conn = http.client.HTTPConnection(where.hostname, where.port, timeout=60)
            try:
                mine.conn.request("GET", where.path.rstrip("/") + target)
                resp = mine.conn.getresponse()
                return resp.status, resp.read()
            except (OSError, http.client.HTTPException) as err:
                failed = err
                mine.conn.close()
                mine.conn = None
        return 0, f"no answer: {type(failed).__name__}".encode()  # no answer at all is an answer to compare: it is how a closed connection was found

    def ask(request):
        path, params = request["argv"][1], [tuple(named(p).split("=", 1)) for p in request["argv"][2:]]
        code, body = get(f"{path}?{urllib.parse.urlencode(params)}")
        try:  # as it was sent, but for what JSON does not mean, the order of an object's keys: a number is kept as it was written
            sent = json.loads(body, parse_int=lambda n: "#" + n, parse_float=lambda n: "#" + n)
            for beside in ("warnings", "infos"):  # and the order of the engine's remarks, which is a map's
                if isinstance(sent, dict) and isinstance(sent.get(beside), list):
                    sent[beside].sort()
            if path == "/api/v1/metadata" and isinstance(sent, dict) and isinstance(sent.get("data"), dict):  # and of what is said of one metric, a map's too
                for said in sent["data"].values():
                    if isinstance(said, list):
                        said.sort(key=lambda entry: json.dumps(entry, sort_keys=True))
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


VERDICTS = ["same", "order", "moved", "edge", "worded", "unasked", "differs"]
MEANS = {"same": "the same", "order": "the same series and values in another order", "moved": "the Prometheus's own answer changed between its two askings",
         "edge": "look back further than the case's metrics reach, as they are meant to, and are the Prometheus's answer with that much taken off",
         "worded": "both fail, worded differently", "unasked": "got no answer at all from one side or the other", "differs": "differ"}

# What a frozen store says beside an answer to a query that looked further back than its metrics
# reach (internal/metrics: beginRemark): among the warnings of what the API sends, and in a line
# under what promq prints. Such an answer cannot be the Prometheus's, since the case holds a window
# of the Prometheus and nothing before it. But a store that could excuse itself by saying so would
# be excused of anything, so it is this file that says which questions look back: three it asks for
# that, each of a selector and nothing worked out from one, with how far back it looks in seconds.
# Of those the answer has to be the Prometheus's own with the points before the case's beginning
# taken off and nothing else, said in the words the case's own beginning gives. Of any other
# question the words are a difference like any other.
#
# The window's steps are ten minutes apart, and not five: a case holds what an instant looks back
# before its hour, and a step that falls there is one whose point the case may have or not, by
# where its samples lie, which nothing here could hold it to. With the hour a sweep freezes and the
# five minutes an instant looks back, no step of ten minutes falls there: every point is one the
# case has to have or one it cannot (selftest.py holds the questions to that).
BEGINS = "frozen case: it holds no samples before "
LOOKS_BACK = {("up offset 2h",): 7200, ("up offset 90m",): 5400, ("up", "--range", "3h", "--step", "10m"): 10800}


def looking_back(argv):
    """The question and how far back it looks, if it is one of this file's own that look back — as promq is given it, or as the request promq makes for it."""
    return next(((asked, reach) for asked, reach in LOOKS_BACK.items() if list(asked) == argv or ["GET"] + request_of(list(asked)) == argv), None)


def case_of(freeze_path):
    """Where a frozen case ends and where its metrics begin, from its freeze.json — and whether the
    beginning is where the sweep's freeze has to have put it: the hour it asks for before the freeze,
    and before that what an instant looks back."""
    said = json.load(open(freeze_path))
    whole, part = divmod(float(said["freeze_time"]), 1)
    metrics = said.get("metrics") or {}  # a case without any says so with nothing, or with null
    case = {"freeze_ms": int(whole) * 1000 + round(part * 1000), "from_ms": metrics.get("from_ms"), "lookback_ms": metrics.get("lookback_delta_ms") or 300000}
    case["askew"] = case["from_ms"] != case["freeze_ms"] - FROZEN_WINDOW * 1000 - case["lookback_ms"]
    return case


def stamp_of(ms):
    return datetime.datetime.fromtimestamp(ms // 1000, datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S") + f".{ms % 1000:03d}Z"


def go_duration(seconds):
    """A whole number of seconds as Go writes a duration: 1h0m0s, 30m0s, 45s."""
    h, rest = divmod(int(seconds), 3600)
    m, sec = divmod(rest, 60)
    return f"{h}h{m}m{sec}s" if h else f"{m}m{sec}s" if m else f"{sec}s"


def remark_for(asked, reach, case):
    """What the case has to say of one of the questions that look back: where it begins, how much
    further the question looks, and — of a window — the instant from which its points are whole,
    which is as far after its first step as it looks back too far."""
    said = BEGINS + stamp_of(case["from_ms"]) + ", and this query looks " + go_duration(reach - FROZEN_WINDOW) + " further back than that"
    if "--range" in asked:
        return said + ": its points before " + stamp_of(case["freeze_ms"] - FROZEN_WINDOW * 1000) + " may be missing, or come of less than its Prometheus had"
    return said + ": its Prometheus may have had more to answer from"


def said_of_its_beginning(answer):
    """Each time an answer says where its case begins: a line under what promq printed, or a warning beside what the API sent."""
    try:
        warnings = json.loads(answer["out"]).get("warnings", [])
    except (ValueError, AttributeError):
        return [line[1:-1] for line in answer["out"].split("\n") if line.startswith("(" + BEGINS) and line.endswith(")")]
    return [said for said in warnings if isinstance(said, str) and said.startswith(BEGINS)] if isinstance(warnings, list) else []  # warnings are a list, or they are not warnings


def unremarked(frozen):
    """A frozen answer without what it says of its own beginning: what is left to hold to the Prometheus's."""
    try:  # of the API: one of the warnings beside the answer. What promq prints of a series with no name begins with a brace too, and is no JSON.
        sent = json.loads(frozen["out"])
        warnings = sent.get("warnings")
    except (ValueError, AttributeError):
        return "".join(line + "\n" for line in frozen["out"].rstrip("\n").split("\n") if not (line.startswith("(" + BEGINS) and line.endswith(")")))
    if isinstance(warnings, list):  # and what is no list is left where it is, to be a difference
        sent["warnings"] = [said for said in warnings if not (isinstance(said, str) and said.startswith(BEGINS))]
        if not sent["warnings"]:
            del sent["warnings"]
    return json.dumps(sent, sort_keys=True, indent=1) + "\n"


STEPS = re.compile(r"^(.*?)((?: \d\d:\d\d:\d\d=\S+)+)$")  # a line of what promq prints of a window: a series, and for each step a time of day and a value


def kept_rightly(had, kept, at, case):
    """Whether the series a case kept of a window are what it has to keep of the Prometheus's. Both
    are a list of series, each a name and its points as they were written; `at` reads the instant
    of one of the Prometheus's points. Each series the case kept has to be one of the Prometheus's,
    in the Prometheus's order and once, with the last of its points as they were written: as many
    as the case has to have — every point a lookback or more after its beginning, since the case
    holds all that an instant there looks at — and no more than it can have, which is none from
    before its beginning. And none of the Prometheus's may be left out that has a point the case
    has to have."""
    has_to = lambda point: at(point) >= case["from_ms"] + case["lookback_ms"]
    where = {name: i for i, (name, _) in enumerate(had)}
    found = []
    for name, points in kept:
        if name not in where:
            return False
        theirs = had[where[name]][1]
        if theirs[-len(points):] != points or not sum(map(has_to, theirs)) <= len(points) <= sum(at(point) >= case["from_ms"] for point in theirs):
            return False
        found.append(where[name])
    return found == sorted(set(found)) and not any(has_to(point) for i, (_, theirs) in enumerate(had) if i not in found for point in theirs)


def looked_back_rightly(asked, reach, live, frozen, case):
    """Whether the frozen answer to one of the questions that look back is what it has to be: said in
    the case's own words, under what promq printed or among the warnings of what the API sent, with
    the Prometheus's status; and the Prometheus's answer with the points before the case's beginning
    taken off and nothing else changed. Of an instant wholly before the case, that is no result. Of
    the window: the Prometheus's series, each with the last of its points (kept_rightly).

    What the API sent is held as it was sent. What promq printed is held line by line, each a series
    and its steps. An answer of more series than promq prints ends in a line that says so and is no
    series: this cannot hold one, and it differs."""
    words = remark_for(asked, reach, case) if case and not case["askew"] else None
    if words is None or live["rc"] not in (0, 200) or frozen["rc"] != live["rc"] or said_of_its_beginning(frozen) != [words]:
        return False  # and a Prometheus that refused the question answered nothing to hold a case to
    rest = unremarked(frozen)
    try:
        sent, theirs = json.loads(rest), json.loads(live["out"])
    except ValueError:
        sent = theirs = None
    try:
        if isinstance(sent, dict):  # of the API
            mine, had = sent["data"]["result"], theirs["data"]["result"]
            if dict(sent, data=dict(sent["data"], result=None)) != dict(theirs, data=dict(theirs["data"], result=None)) or not isinstance(mine, list):
                return False
            if "--range" not in asked:
                return mine == []
            if not all(set(series) == {"metric", "values"} for series in mine):
                return False
            name = lambda series: json.dumps(series["metric"], sort_keys=True)
            return kept_rightly([(name(r), r["values"]) for r in had], [(name(r), r["values"]) for r in mine], lambda pair: round(float(pair[0].lstrip("#")) * 1000), case)
        if frozen["out"] != rest + "(" + words + ")\n":
            return False  # promq prints it under the answer, and nowhere else
        if "--range" not in asked:
            return rest == "(empty result)\n"
        day = case["freeze_ms"] - case["freeze_ms"] % 86400000

        def at(step):  # a time of day: the last one that is not after the freeze, to the freeze's own thousandths
            h, m, sec = (int(part) for part in step.partition("=")[0].split(":"))
            when = day + (h * 3600 + m * 60 + sec) * 1000 + case["freeze_ms"] % 1000
            return when if when <= case["freeze_ms"] else when - 86400000
        read = lambda text: [] if text == "(empty result)\n" else [STEPS.match(line) for line in text.rstrip("\n").split("\n")]
        had, mine = read(live["out"]), read(rest)  # a line that is no series is None, and has no name to take
        return kept_rightly([(m.group(1), m.group(2).split()) for m in had], [(m.group(1), m.group(2).split()) for m in mine], at, case)
    except (KeyError, TypeError, AttributeError):  # no result where one is looked for, something that is no series, a line that is none
        return False


def unanswered(argv, answer):
    """No answer at all: a request nothing came back for, a promq that timed out or could not reach its
    endpoint. Three of those alike are not three answers that agree."""
    if argv[:1] == ["GET"]:
        return answer["rc"] == 0
    return answer["rc"] == 124 or answer["out"].startswith('query failed: Get "')


MAP_ORDERED = re.compile(r"(count_values|histogram_quantile|histogram_fraction)\s*\(")


def map_ordered(argv):
    """An instant query that is, all of it, one call of something an engine keeps in a map: the call's
    own bracket is the one that closes the query. Not a window, whose series the engine sorts; and
    not `count_values(…) or sort_desc(x)`, which begins the same way."""
    if argv[1] != "/api/v1/query":
        return False
    query = next((part[len("query="):].strip() for part in argv[2:] if part.startswith("query=")), "")
    while query.startswith("(") and query.endswith(")"):
        query = query[1:-1].strip()
    call = MAP_ORDERED.match(query)
    if not call:
        return False
    depth, quote = 0, None
    for i, ch in enumerate(query[call.end() - 1:], call.end() - 1):
        if quote:
            quote = None if ch == quote else quote
        elif ch in "'\"`":
            quote = ch
        elif ch == "(":
            depth += 1
        elif ch == ")":
            depth -= 1
            if depth == 0:
                return i == len(query) - 1
    return False


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
    return map_ordered(argv) or reordered(live1, live2)


def verdict(argv, live1, frozen, live2, case=None):
    """One query's three answers about one instant. They are text, and the same text or not: nothing
    here is an age. If the Prometheus itself answered twice differently — a sample stamped before the
    freeze and committed after the first asking — the frozen answer has to be one of the two. Of the
    three questions that look back further than the case reaches, the frozen answer has to be what
    looked_back_rightly says, and is never simply the same: the case has to say that it looked."""
    if any(unanswered(argv, answer) for answer in (live1, frozen, live2)):
        return "unasked"
    if back := looking_back(argv):
        if (live1["out"], live1["rc"]) == (live2["out"], live2["rc"]):
            return "edge" if looked_back_rightly(*back, live1, frozen, case) else "differs"
        return "moved" if looked_back_rightly(*back, live1, frozen, case) or looked_back_rightly(*back, live2, frozen, case) else "differs"
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
    """Why a difference is known, if it is: a line of known.txt names the query, and the whole of what
    the Prometheus has to answer and of what the frozen store has to answer — its exit code or status,
    a space, and what it said — for it to be that difference and no other."""
    whole = lambda pattern, answer: re.fullmatch(pattern, f"{answer['rc']} {answer['out']}", re.S)
    return next((why for pattern, there, here, why in known if re.search(pattern, shown(argv)) and whole(there, live) and whole(here, frozen)), None)


# More answers than this that the Prometheus itself changed between its two askings, and nothing was
# compared: its second asking was of something else, or of nothing.
MOVED_AT_MOST = 0.02


def cmd_compare(live1_path, frozen_path, live2_path, typed_path, known_path=None, freeze_path=None):
    live1, frozen, live2 = (json.load(open(p)) for p in (live1_path, frozen_path, live2_path))
    case = case_of(freeze_path) if freeze_path else None
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
    rows = [(f["argv"], f["typed"], verdict(f["argv"], a, f, b, case), a, f, b, t) for a, f, b, t in zip(live1, frozen, live2, as_typed)]
    count = collections.Counter(r[2] for r in rows)
    print(f"{what}: {len(rows)} | " + " | ".join(f"{MEANS[v]}: {count[v]}" if v != "differs" else f"**differ: {count[v]}**" for v in VERDICTS))
    typed = collections.Counter(r[2] for r in rows if r[1])
    print(f"\nOf the {sum(typed.values())} that a recorded agent had typed: " + ", ".join(f"{MEANS[v]} {typed[v]}" for v in VERDICTS) + ".")
    fine = lambda a: a["rc"] == (200 if what == "requests" else 0)  # promq's exit code, or the API's status: no answer at all is neither
    empty = lambda a: a["out"].strip() == "(empty result)" or bool(re.search(r'"(result|data)": \[\]', a["out"]))
    answered = sum(fine(r[3]) and not empty(r[3]) for r in rows)
    print(f"\nOf the {len(rows)}, the Prometheus answered {answered} with something, {sum(fine(r[3]) for r in rows) - answered} with an empty result, and refused {sum(not fine(r[3]) for r in rows)}.")

    # The questions that look back further than the case reaches, each held to what it has to be.
    if count["edge"]:
        print(f"\n### {MEANS['edge'][0].upper() + MEANS['edge'][1:]}\n")
        for argv, _, got, a, f, _, _ in rows:
            if got == "edge":
                print(f"- `{shown(argv)}`\n  - {said_of_its_beginning(f)[0]}")

    unexcused = 0
    for v in ("unasked", "differs", "worded", "order", "moved"):
        if count[v]:
            print(f"\n### {MEANS[v][0].upper() + MEANS[v][1:]}\n")
        for argv, _, got, a, f, b, _ in rows:
            if got != v:
                continue
            fails = v in ("differs", "worded", "unasked")
            why = excused(argv, a, f, known) if v in ("differs", "worded") and (a["out"], a["rc"]) == (b["out"], b["rc"]) else None  # of an answer the Prometheus gave twice
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


def by_lines(spoil):
    return lambda answer, live=None, case=None: dict(answer, out="".join(line + "\n" for line in spoil(answer["out"].rstrip("\n").split("\n"))))


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


def of_the_api(change):
    """A spoiling of what the API sent that leaves it what a store could send: the answer with one thing in its result changed; as it was, if it has none."""
    def spoil(answer, live=None, case=None):
        try:
            sent = json.loads(answer["out"])
            result = sent["data"]["result"]
            if not result or not all(isinstance(r, dict) for r in result):
                return answer
        except (ValueError, KeyError, TypeError):
            return answer
        change(sent, result)
        return dict(answer, out=json.dumps(sent, sort_keys=True, indent=1) + "\n")
    return spoil


def last_digit(sent, result):
    pair = result[-1]["values"][-1] if "values" in result[-1] else result[-1]["value"]
    pair[1] = pair[1][:-1] + ("7" if pair[1][-1:] != "7" else "3")


def said_to_look_back(answer, live=None, case=None):
    """The answer with the words that it looked back put beside it, as a store that would excuse itself might: of the case's own beginning, where that is known."""
    words = BEGINS + stamp_of(case["from_ms"] if case and case["from_ms"] else 1767225600000) + ", and this query looks 1h0m0s further back than that: its Prometheus may have had more to answer from"
    try:
        sent = json.loads(answer["out"])
        sent["warnings"] = sent.get("warnings", []) + [words]
        return dict(answer, out=json.dumps(sent, sort_keys=True, indent=1) + "\n")
    except (ValueError, AttributeError, TypeError):
        return dict(answer, out=answer["out"] + "(" + words + ")\n")


def of_what_looks_back(change):
    """A spoiling of an answer that says it looked back, which leaves what it says as it is: a change
    to what promq printed above its last line, or to the result of what the API sent, given the
    Prometheus's own answer to take from. As it was, if the answer says no such thing or the change
    does not apply."""
    def spoil(answer, live=None, case=None):
        if not said_of_its_beginning(answer) or live is None:
            return answer
        try:
            sent, theirs = json.loads(answer["out"]), json.loads(live["out"])
        except ValueError:
            sent = theirs = None
        try:
            if isinstance(sent, dict):
                change(sent["data"]["result"], theirs["data"]["result"], lambda series: series["values"], lambda series, points: series.__setitem__("values", points), lambda series: json.dumps(series["metric"], sort_keys=True))
                return dict(answer, out=json.dumps(sent, sort_keys=True, indent=1) + "\n")
            lines = answer["out"].rstrip("\n").split("\n")
            mine, had = [STEPS.match(line) for line in lines[:-1]], [STEPS.match(line) for line in live["out"].rstrip("\n").split("\n")]
            series = [[m.group(1), m.group(2).split()] for m in mine]  # a line that is no series is None: the answer is left as it was
            change(series, [[m.group(1), m.group(2).split()] for m in had], lambda one: one[1], lambda one, points: one.__setitem__(1, points), lambda one: one[0])
            return dict(answer, out="".join(name + " " + " ".join(points) + "\n" for name, points in series) + lines[-1] + "\n")
        except (LookupError, TypeError, AttributeError):  # no series to change, no result, a line that is no series
            return answer
    return spoil


def a_point_more(mine, had, points, put, named):
    """The point the Prometheus has before the first one a series kept, put back — of the first series that
    the Prometheus has such a point of: for a window, that is a point from before the case's beginning."""
    theirs = {named(series): points(series) for series in had}
    for series in mine:
        more = theirs.get(named(series), [])
        if len(more) > len(points(series)):
            return put(series, more[len(more) - len(points(series)) - 1:])
    raise LookupError


def answered_after_all(answer, live=None, case=None):
    """What the Prometheus answered put where the case had nothing, under the same words: of an instant
    wholly before its beginning. As it was, if the answer says nothing of looking back or is not empty."""
    words = said_of_its_beginning(answer)
    if not words or live is None:
        return answer
    try:
        sent, theirs = json.loads(answer["out"]), json.loads(live["out"])
    except ValueError:
        return dict(answer, out=live["out"] + "(" + words[0] + ")\n") if unremarked(answer) == "(empty result)\n" else answer
    try:
        if sent["data"]["result"] != []:
            return answer
        sent["data"]["result"] = theirs["data"]["result"]
    except (KeyError, TypeError):
        return answer
    return dict(answer, out=json.dumps(sent, sort_keys=True, indent=1) + "\n")


def another_beginning(answer, live=None, case=None):
    """What an answer says of its beginning, said of an instant a second later."""
    said = said_of_its_beginning(answer)
    if not said or not case or not case["from_ms"]:
        return answer
    return dict(answer, out=answer["out"].replace(stamp_of(case["from_ms"]), stamp_of(case["from_ms"] + 1000)))


SPOILED = [
    ("replaced by nothing", by_lines(lambda lines: [])),
    ("replaced by an empty result", by_lines(lambda lines: ["(empty result)"])),
    ("its last line taken off", by_lines(lambda lines: lines[:-1])),
    ("its last line written twice", by_lines(lambda lines: lines + lines[-1:])),
    ("the last digit of its last line another", by_lines(lambda lines: lines[:-1] + [lines[-1][:-1] + ("7" if lines[-1][-1:] != "7" else "3")])),
    ("its lines the other way up", by_lines(lambda lines: lines[::-1])),
    ("its series the other way round, of the API", by_lines(series_reversed)),
    # And what a store could send, where the six above mostly break what the API sends as JSON.
    ("its last series taken out, of the API", of_the_api(lambda sent, result: result.pop())),
    ("the last digit of its last value another, of the API", of_the_api(last_digit)),
    ("answered with another status", lambda answer, live=None, case=None: dict(answer, rc={200: 500, 0: 1}.get(answer["rc"], answer["rc"] + 1))),
    ("the words that it looked back put beside it", said_to_look_back),
    # And of the answers that look back, which the changes above mostly spoil by spoiling what they say: what they say left as it is.
    ("what looks back: the Prometheus's point before its first put back", of_what_looks_back(a_point_more)),
    ("what looks back: its first point taken off", of_what_looks_back(lambda mine, had, points, put, named: put(mine[0], points(mine[0])[1:]))),
    ("what looks back: its first series taken out", of_what_looks_back(lambda mine, had, points, put, named: mine.pop(0))),
    ("what looks back: its series the other way round", of_what_looks_back(lambda mine, had, points, put, named: mine.reverse() if len(mine) > 1 else mine.pop(len(mine)))),
    ("what looks back: answered with what the Prometheus has, where the case has nothing", answered_after_all),
    ("what looks back: said of a beginning a second later", another_beginning),
]


def cmd_spoil(live1_path, frozen_path, live2_path, freeze_path=None):
    live1, frozen, live2 = (json.load(open(p)) for p in (live1_path, frozen_path, live2_path))
    case = case_of(freeze_path) if freeze_path else None
    back = sum(verdict(f["argv"], a, f, b, case) == "edge" for a, f, b in zip(live1, frozen, live2))
    print(f"Unspoiled, {back} of the {len(frozen)} look back further than the case reaches and are what they then have to be.\n")
    print("| the frozen answer, spoiled | still passes | of |\n|---|---|---|")
    failing = 0
    for name, spoil in SPOILED:
        tried, passed = 0, []
        for a, f, b in zip(live1, frozen, live2):
            spoiled = spoil(f, a, case)
            if (spoiled["out"], spoiled["rc"]) == (f["out"], f["rc"]):
                continue  # nothing to tell apart: one line, the other way up
            tried += 1
            if verdict(f["argv"], a, spoiled, b, case) in ("same", "order", "moved", "edge"):
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
