"""One target of the metrics fixture: a metric of each kind a case carries — a counter, a gauge, a
histogram in buckets, a summary, one of no kind — and every value a function of the clock.

    exporter.py             serve /metrics on :8080 as the target named in $TARGET (a, b or c)
    exporter.py --history   print the last $HISTORY_SECONDS of the targets, for promtool to make blocks of

Because a value is a function of the clock, what `--history` writes of the past and what a
Prometheus then scrapes are the same series with values that go on from one another: in blocks,
and in the head. They do not join: the past is written every fifteen seconds and ends half a
minute before it is written, the head is scraped every five and begins when the Prometheus has
started, so there is about a minute between them with no sample in it; and what is written of the
past has no staleness marker in it, which only a scrape makes.

The past has a fourth target in it, d, which stopped ten minutes before the past ends: series that
are in the blocks and never in the head.

Targets a and b answer in the Prometheus text format and c in OpenMetrics, which is the one that
can say a unit. They do not agree about everything: b has other words of help for one metric and
no type for another, so that what a Prometheus knows about a metric is more than one thing.
"""
import math, os, sys, time
from http.server import ThreadingHTTPServer, BaseHTTPRequestHandler

BUSY = {"a": 1.0, "b": 1.7, "c": 0.4, "d": 0.7}  # how busy each target is
BUCKETS = [0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10]
PATHS = {"/a": 3.0, "/b": 0.5, '/c "quoted"': 0.1}  # requests a second, of the busiest target at rest
MEAN = {"/a": 0.08, "/b": 1.9}  # seconds a request takes


def families(target, t):
    """Every metric family of one target at one instant: (name, type, help, unit, samples), a sample
    being (suffix, labels, value)."""
    k, day = BUSY[target], t % 86400  # counters count from midnight, and so reset there
    out = []

    # A counter: a steady rate, and for one series a burst of forty seconds every four minutes.
    reqs = []
    for path, rate in PATHS.items():
        for code in ("200", "500"):
            burst = 60 * k * (day // 240 + min(day % 240, 40) / 40) if (path, code) == ("/a", "200") else 0
            reqs.append(("", {"path": path, "code": code}, math.floor(rate * k * (0.02 if code == "500" else 1) * day + burst)))
    out.append(("fix_requests_total", "counter", "Requests served, by path and status code.", "", reqs))
    # One that starts again from nothing every seven minutes, as a process restarted does.
    out.append(("fix_restarting_total", "counter", "A counter of a process that restarts every seven minutes.", "", [("", {}, math.floor(0.5 * k * (t % 420)))]))

    # A histogram: two paths that are observed, one fast and one slow, and one that never is.
    hist = []
    for path in ("/a", "/b", "/idle"):
        n = math.floor(PATHS.get(path, 0) * k * day)
        for le in BUCKETS:
            hist.append(("_bucket", {"path": path, "le": repr(float(le)) if le != int(le) else f"{int(le)}.0"}, math.floor(n * (1 - math.exp(-le / MEAN[path]))) if n else 0))
        hist += [("_bucket", {"path": path, "le": "+Inf"}, n), ("_sum", {"path": path}, round(n * MEAN.get(path, 0), 6)), ("_count", {"path": path}, n)]
    out.append(("fix_request_duration_seconds", "histogram", "How long a request took.", "seconds", hist))

    # A summary: its quantiles move, and those of an operation nobody ran are not numbers.
    summ = []
    for op, base in (("read", 0.012), ("idle", None)):
        for q, factor in (("0.5", 1), ("0.9", 2.5), ("0.99", 7)):
            summ.append(("", {"op": op, "quantile": q}, float("nan") if base is None else base * factor * (1 + 0.5 * math.sin(t / 90))))
        n = math.floor(4 * k * day) if base else 0
        summ += [("_sum", {"op": op}, round(n * (base or 0) * 1.4, 9)), ("_count", {"op": op}, n)]
    out.append(("fix_latency_seconds", "summary", "Latency of an operation, as its client measured it.", "seconds", summ))

    # Gauges, for how a number is written: below nothing, a fraction, very small, very large, not finite.
    rooms = [("", {"room": room}, round(base + 3 * math.sin(t / 150 + i), 3)) for i, (room, base) in enumerate((("cellar", -12.5), ("hall", 19.25), ("attic", 0.5)))]
    out.append(("fix_temperature_celsius", "gauge", 'Temperature "as read" \\ per room, in °C.', "celsius", rooms))
    for name, value, text in (
        ("fix_tiny_ratio", 1.5e-9 * (1 + (t % 60) / 60), "A ratio too small to write without an exponent."),
        ("fix_huge_bytes", 3e21 + math.floor(t) * 1e6, "A size too large to write without one."),  # by a million a second: by less, a float this large does not move
        ("fix_past_integers", 9007199254740993, "An integer a float cannot hold."),
        ("fix_sum_of_tenths", 0.1 + 0.2, "A tenth and two tenths."),
        ("fix_infinite", float("inf"), "No limit."),
        ("fix_minus_infinite", float("-inf"), "No floor."),
        ("fix_not_a_number", float("nan"), "Nothing measured."),
        ("fix_nothing", 0, "Zero."),
        ("fix_last_success_timestamp_seconds", math.floor(t / 30) * 30, "When the last run succeeded."),
    ):
        out.append((name, "gauge", text, "seconds" if name.endswith("_seconds") else "", [("", {}, value)]))
    out.append(("fix_queue_depth", "gauge", "Items waiting in the queue (as b counts them)." if target == "b" else "Items waiting.", "", [("", {}, math.floor(20 + 15 * math.sin(t / 45) * k))]))
    out.append(("fix_flag", "" if target == "b" else "gauge", "Whether the flag is up.", "", [("", {}, 1)]))

    # Labels: what a value may hold. And one that says who built this.
    out.append(("fix_build_info", "gauge", "What this target was built from.", "", [("", {"version": "1.2.3", "commit": "abc1234", "target": target}, 1)]))
    soup = {"space": "a b", "quote": 'say "hi"', "backslash": "C:\\dir", "newline": "one\ntwo", "unicode": "온도 °C", "comma": "a,b", "brace": "{x}", "equals": "k=v"}
    out.append(("fix_label_soup", "gauge", "One series whose labels hold what a label may hold.", "", [("", soup, 1)]))
    # A series that is there for two minutes and gone for two: in the head, each going leaves a marker.
    # What it is, is said all the while: a Prometheus forgets what kind a metric is some ten scrapes
    # after its target last said, and what it knows of its metrics would then not be one thing at the
    # freeze and when it is asked again.
    out.append(("fix_job_running", "gauge", "Present while the job runs, absent otherwise.", "", [("", {"job_name": "nightly"}, 1)] if t % 240 < 120 else []))
    out.append(("fix_untyped", "", "", "", [("", {}, 42.5)]))
    return out


def number(v):
    if isinstance(v, float):
        return "NaN" if math.isnan(v) else ("+Inf" if v > 0 else "-Inf") if math.isinf(v) else repr(v)
    return str(v)


def labels(pairs):
    esc = lambda s: s.replace("\\", "\\\\").replace("\n", "\\n").replace('"', '\\"')
    return "{" + ",".join(f'{k}="{esc(v)}"' for k, v in pairs.items()) + "}" if pairs else ""


def text(target, t):
    """The Prometheus text format, version 0.0.4."""
    lines = []
    for name, kind, help_, _unit, samples in families(target, t):
        if help_:
            lines.append(f"# HELP {name} " + help_.replace("\\", "\\\\").replace("\n", "\\n"))
        if kind:
            lines.append(f"# TYPE {name} {kind}")
        lines += [f"{name}{suffix}{labels(ls)} {number(v)}" for suffix, ls, v in samples]
    return "\n".join(lines) + "\n"


def openmetrics(target, t):
    """OpenMetrics 1.0: a counter's family is named without `_total`, and a family can say its unit."""
    lines = []
    for name, kind, help_, unit, samples in families(target, t):
        family = name[: -len("_total")] if kind == "counter" else name
        lines.append(f"# TYPE {family} {kind or 'unknown'}")
        if unit:
            lines.append(f"# UNIT {family} {unit}")
        if help_:
            lines.append(f"# HELP {family} " + help_.replace("\\", "\\\\").replace("\n", "\\n").replace('"', '\\"'))
        lines += [f"{name}{suffix}{labels(ls)} {number(float(v)) if kind != 'counter' or suffix else number(v)}" for suffix, ls, v in samples]
    return "\n".join(lines) + "\n# EOF\n"


# What a Prometheus adds to each target's series when it scrapes it: prometheus.yaml says the same of
# the three it scrapes. The fourth it never scrapes: it is only in the past.
SCRAPED = {
    "a": {"instance": "exp-a.promfix.svc:8080", "job": "fix", "zone": "east"},
    "b": {"instance": "exp-b.promfix.svc:8080", "job": "fix", "zone": "west"},
    "c": {"instance": "exp-c.promfix.svc:8080", "job": "other"},
    "d": {"instance": "exp-d.promfix.svc:8080", "job": "fix", "zone": "south"},
}
RETIRED = {"d": 600}  # how long before the end of the past a target stopped, in seconds


def history(out, now, seconds, step=15):
    """The past of the targets, the one that stopped among them, as OpenMetrics with a time on every
    line. It ends a little before now: the head of the Prometheus that reads these blocks begins after them."""
    end = math.floor(now - 30) // step * step
    for t in range(end - seconds, end + 1, step):
        for target, added in SCRAPED.items():
            if t > end - RETIRED.get(target, 0):
                continue
            out.write(f"up{labels(added)} 1 {t}\n")
            for name, _kind, _help, _unit, samples in families(target, t):
                for suffix, ls, v in samples:
                    out.write(f"{name}{suffix}{labels({**ls, **added})} {number(v)} {t}\n")
    out.write("# EOF\n")


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.0"

    def log_message(self, *a):
        pass

    def do_GET(self):
        target = os.environ.get("TARGET", "a")
        if self.path.startswith("/metrics"):
            om = target == "c"
            body = (openmetrics if om else text)(target, time.time()).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/openmetrics-text; version=1.0.0; charset=utf-8" if om else "text/plain; version=0.0.4; charset=utf-8")
        else:
            body = b"ok"
            self.send_response(200)
        self.end_headers()
        self.wfile.write(body)


if __name__ == "__main__":
    if sys.argv[1:] == ["--history"]:
        history(sys.stdout, time.time(), int(os.environ.get("HISTORY_SECONDS", "10800")))
    else:
        print(f"metrics fixture target {os.environ.get('TARGET', 'a')} listening on :8080", flush=True)
        ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
