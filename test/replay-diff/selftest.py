"""What `replaydiff.py compare` must call a difference, and what it must not.

    python3 test/replay-diff/selftest.py

A comparison that passes is worth what it would have failed. Each line here is an answer a frozen
case once gave, or one a looser comparison let through, with the verdict it has to get.
"""
import os, sys

sys.dont_write_bytecode = True
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import replaydiff as rd


def answer(out, err="", rc=0):
    return {"rc": rc, "out": out, "err": err}


lines = lambda prefix, n: "\n".join(f"{prefix}{i}" for i in range(n))
VERDICTS = [  # what, the command, live before, frozen, live after, the verdict
    ("ages a few seconds apart", ["get", "pods"], answer("NAME AGE\na 88s\n"), answer("NAME AGE\na 90s\n"), answer("NAME AGE\na 95s\n"), "same"),
    ("a minute's rounding", ["get", "pods"], answer("NAME AGE\na 12m\n"), answer("NAME AGE\na 13m\n"), answer("NAME AGE\na 13m\n"), "same"),
    ("an age an hour out", ["get", "pods"], answer("NAME AGE\na 12m\n"), answer("NAME AGE\na 1h\n"), answer("NAME AGE\na 12m\n"), "differs"),
    ("an age written to the hour where a cluster writes minutes", ["get", "pods"], answer("NAME AGE\na 89m\n"), answer("NAME AGE\na 1h\n"), answer("NAME AGE\na 89m\n"), "differs"),
    ("an age crossing from seconds to minutes", ["get", "pods"], answer("NAME AGE\na 119s\n"), answer("NAME AGE\na 2m\n"), answer("NAME AGE\na 2m3s\n"), "same"),
    ("an age crossing from hours to days", ["get", "pods"], answer("NAME AGE\na 47h\n"), answer("NAME AGE\na 2d\n"), answer("NAME AGE\na 2d\n"), "same"),
    ("CPU of 250m against 3m is not an age", ["top", "pods"], answer("NAME CPU\na 250m\n"), answer("NAME CPU\na 3m\n"), answer("NAME CPU\na 250m\n"), "differs"),
    ("a probe every 10s against every 60s", ["describe", "pod", "x"], answer("Liveness: delay=0s period=10s\n"), answer("Liveness: delay=0s period=60s\n"), answer("Liveness: delay=0s period=10s\n"), "differs"),
    ("a restart count of 6 against 41", ["get", "pods"], answer("a 0/1 CrashLoopBackOff 6 (2m ago) 13m\n"), answer("a 0/1 CrashLoopBackOff 41 (2m ago) 13m\n"), answer("a 0/1 CrashLoopBackOff 6 (2m ago) 13m\n"), "differs"),
    ("a restart between the two live askings", ["get", "pods"], answer("a 0/1 CrashLoopBackOff 6 (2m ago) 13m\n"), answer("a 0/1 CrashLoopBackOff 7 (10s ago) 13m\n"), answer("a 0/1 CrashLoopBackOff 7 (20s ago) 13m\n"), "same"),
    ("an event's count between the two live ones", ["describe", "pod", "x"], answer("Warning Unhealthy 5s (x10 over 2m) kubelet\n"), answer("Warning Unhealthy 3s (x11 over 2m) kubelet\n"), answer("Warning Unhealthy 9s (x12 over 2m) kubelet\n"), "same"),
    ("an event's count between the two live ones, the second of which lists two events of one second the other way round", ["events", "-n", "shop"],
     answer("H\n5s (x8 over 2m) a\n5s (x3 over 1m) b\n"), answer("H\n9s (x9 over 2m4s) a\n9s (x3 over 1m4s) b\n"), answer("H\n15s (x3 over 1m10s) b\n15s (x11 over 2m10s) a\n"), "same"),
    ("an event's count outside them, the same way", ["events", "-n", "shop"],
     answer("H\n5s (x8 over 2m) a\n5s (x3 over 1m) b\n"), answer("H\n9s (x12 over 2m4s) a\n9s (x3 over 1m4s) b\n"), answer("H\n15s (x3 over 1m10s) b\n15s (x11 over 2m10s) a\n"), "differs"),
    ("an event's count outside them", ["describe", "pod", "x"], answer("Warning Unhealthy 5s (x10 over 2m) kubelet\n"), answer("Warning Unhealthy 3s (x40 over 2m) kubelet\n"), answer("Warning Unhealthy 9s (x12 over 2m) kubelet\n"), "differs"),
    ("a table with other columns", ["get", "pods", "-o", "wide"], answer("NAME READY AGE IP NODE\na 1/1 5m 10.0.0.1 w\n"), answer("NAME READY AGE\na 1/1 5m\n"), answer("NAME READY AGE IP NODE\na 1/1 5m 10.0.0.1 w\n"), "differs"),
    ("an empty answer", ["get", "events", "--sort-by=.lastTimestamp"], answer("LAST SEEN TYPE\n5m Normal\n"), answer("", "No resources found in shop namespace.\n"), answer("LAST SEEN TYPE\n5m Normal\n"), "differs"),
    ("another order", ["get", "pods"], answer("H\nb\na\n"), answer("H\na\nb\n"), answer("H\nb\na\n"), "differs"),
    ("another order under --sort-by, where equal keys may stand either way", ["get", "events", "--sort-by=.lastTimestamp"], answer("H\nb\na\n"), answer("H\na\nb\n"), answer("H\nb\na\n"), "order"),
    ("the cluster itself moved", ["get", "events"], answer("H\ne1\n"), answer("H\ne1\ne2\ne3\n"), answer("H\ne1\ne2\ne3\ne4\n"), "moved"),
    ("the cluster itself moved, in a listing asked for without its heading", ["get", "events", "--no-headers"], answer("5s e1 13\n4s e2 15\n"), answer("4s e2 15\n0s e1 14\n"), answer("4s e1 17\n1s e2 20\n"), "moved"),
    ("a listing without its heading that differs while the cluster stands still", ["get", "events", "--no-headers"], answer("5s e1 13\n"), answer("5s e9 13\n"), answer("9s e1 13\n"), "differs"),
    ("another heading while the cluster moved", ["get", "events"], answer("H\ne1\n"), answer("G\ne1\ne2\n"), answer("H\ne1\ne2\ne3\n"), "differs"),
    ("the cluster moved, in a listing without its heading, and the frozen copy has nothing", ["get", "pods", "--no-headers"],
     answer("a 0/1 CrashLoopBackOff 6 (2m ago) 13m\nb 1/1 Running 0 13m\n"), answer("", "No resources found in shop namespace.\n"), answer("a 0/1 Error 7 (3s ago) 13m\nb 1/1 Running 0 13m\n"), "differs"),
    ("the cluster moved, and the frozen copy lacks a row that never changed", ["get", "pods"],
     answer("NAME READY STATUS RESTARTS AGE\na 0/1 CrashLoopBackOff 6 (2m ago) 13m\nb 1/1 Running 0 13m\n"), answer("NAME READY STATUS RESTARTS AGE\na 0/1 Error 7 (1s ago) 13m\n"),
     answer("NAME READY STATUS RESTARTS AGE\na 0/1 Error 7 (3s ago) 13m\nb 1/1 Running 0 13m\n"), "differs"),
    ("the cluster moved, and the frozen copy has only the heading", ["get", "pods"],
     answer("NAME READY STATUS RESTARTS AGE\na 0/1 CrashLoopBackOff 6 (2m ago) 13m\nb 1/1 Running 0 13m\n"), answer("NAME READY STATUS RESTARTS AGE\n"),
     answer("NAME READY STATUS RESTARTS AGE\na 0/1 Error 7 (3s ago) 13m\nb 1/1 Running 0 13m\n"), "differs"),
    ("the cluster moved, and the frozen copy is a state in between, with every row that stood still", ["get", "pods"],
     answer("NAME READY STATUS RESTARTS AGE\na 0/1 CrashLoopBackOff 6 (2m ago) 13m\nb 1/1 Running 0 13m\n"), answer("NAME READY STATUS RESTARTS AGE\na 0/1 Running 7 (1s ago) 13m\nb 1/1 Running 0 13m\n"),
     answer("NAME READY STATUS RESTARTS AGE\na 0/1 Error 7 (3s ago) 13m\nb 1/1 Running 0 13m\n"), "moved"),
    ("the cluster moved, and the frozen copy has a row twice", ["get", "pods"],
     answer("NAME READY STATUS RESTARTS AGE\na 0/1 CrashLoopBackOff 6 (2m ago) 13m\nb 1/1 Running 0 13m\n"), answer("NAME READY STATUS RESTARTS AGE\na 0/1 Running 7 (1s ago) 13m\nb 1/1 Running 0 13m\nb 1/1 Running 0 13m\n"),
     answer("NAME READY STATUS RESTARTS AGE\na 0/1 Error 7 (3s ago) 13m\nb 1/1 Running 0 13m\n"), "differs"),
    ("the cluster moved, and the frozen copy has the rows that stood still the other way round", ["get", "pods"],
     answer("NAME READY STATUS RESTARTS AGE\na 0/1 CrashLoopBackOff 6 (2m ago) 13m\nb 1/1 Running 0 13m\nc 1/1 Running 0 9m\n"), answer("NAME READY STATUS RESTARTS AGE\na 0/1 Running 7 (1s ago) 13m\nc 1/1 Running 0 9m\nb 1/1 Running 0 13m\n"),
     answer("NAME READY STATUS RESTARTS AGE\na 0/1 Error 7 (3s ago) 13m\nb 1/1 Running 0 13m\nc 1/1 Running 0 9m\n"), "differs"),
    ("pods sorted by when they started, and the frozen copy the other way up", ["get", "pods", "--sort-by=.status.startTime"], answer("NAME AGE\na 9m\nb 5m\nc 70s\n"), answer("NAME AGE\nc 70s\nb 5m\na 9m\n"), answer("NAME AGE\na 9m\nb 5m\nc 70s\n"), "differs"),
    ("pods sorted by when they started, two of one minute either way round", ["get", "pods", "--sort-by=.status.startTime"], answer("NAME AGE\na 9m\nb 5m\nc 5m\n"), answer("NAME AGE\na 9m\nc 5m\nb 5m\n"), answer("NAME AGE\na 9m\nb 5m\nc 5m\n"), "order"),
    ("a log that went by faster than its tail, and one frozen line where the cluster gave two each time", ["logs", "p", "--tail=2"], answer("l1\nl2"), answer("l5"), answer("l9\nl10"), "differs"),
    ("a tail whose container started again: the first line of its next run", ["logs", "p", "--tail=2"], answer("r1y\nr1z"), answer("r2a"), answer("r2a\nr2b"), "moved"),
    ("a tail with fewer lines than the cluster gave at either asking", ["logs", "p", "--tail=5"], answer(lines("l", 5)), answer("l4"), answer("l4\nl5\nl6\nl7\nl8"), "differs"),
    ("`kubectl events` from new to old where a cluster's runs from old to new", ["events", "-n", "shop"], answer("H\n5m a\n3m b\n5s c\n"), answer("H\n5s c\n3m b\n5m a\n"), answer("H\n5m a\n3m b\n5s c\n"), "differs"),
    ("what a LimitRange limits, which kubectl writes in no order", ["describe", "limitranges", "defaults"], answer("Type Resource\nContainer memory\nContainer cpu\n"), answer("Type Resource\nContainer cpu\nContainer memory\n"),
     answer("Type Resource\nContainer memory\nContainer cpu\n"), "order"),
    ("what a quota limits, which kubectl writes in order", ["describe", "resourcequotas", "counts"], answer("Resource Used\npods 1\nsecrets 2\n"), answer("Resource Used\nsecrets 2\npods 1\n"), answer("Resource Used\npods 1\nsecrets 2\n"), "differs"),
    ("one side fails", ["describe", "secret", "t"], answer("Name: t\n"), answer("", "error: illegal base64 data", 1), answer("Name: t\n"), "differs"),
    ("both refuse, in other words", ["get", "pod", "x"], answer("", 'pods "x" not found', 1), answer("", "the server could not find the requested resource", 1), answer("", 'pods "x" not found', 1), "worded"),
    ("both fail, having printed something else first", ["get", "pods,foo"], answer("NAME\na\n", "error: x", 1), answer("NAME\nb\n", "error: x", 1), answer("NAME\na\n", "error: x", 1), "worded"),
    ("three timeouts are not three answers", ["get", "pods"], answer("", "timed out", -1), answer("", "timed out", -1), answer("", "timed out", -1), "differs"),
    ("the same answer, and a warning beside it on one side", ["get", "endpoints"], answer("NAME\na\n", "Warning: deprecated"), answer("NAME\na\n"), answer("NAME\na\n", "Warning: deprecated"), "differs"),
    ("a log that grows", ["logs", "p"], answer("a\nb"), answer("a\nb\nc"), answer("a\nb\nc\nd"), "same"),
    ("a log that is another log", ["logs", "p"], answer("a\nb"), answer("x\ny\nz"), answer("a\nb\nc\nd"), "differs"),
    ("a tail whose window moved on", ["logs", "p", "--tail=5"], answer(lines("l", 5)), answer("l1\nl2\nl3\nl4\nl5"), answer("l3\nl4\nl5\nl6\nl7"), "same"),
    ("a tail with nothing in it", ["logs", "p", "--tail=20"], answer(lines("l", 20)), answer(""), answer(lines("l", 20)), "differs"),
    ("a tail of other lines", ["logs", "p", "--tail=20"], answer(lines("l", 20)), answer(lines("x", 20)), answer(lines("l", 20)), "differs"),
    ("a tail longer than was asked for", ["logs", "p", "--tail=2"], answer("a\nb"), answer("z\na\nb"), answer("a\nb"), "differs"),
    ("a short log under its tail, with other lines", ["logs", "p", "--tail=20"], answer("a\nb"), answer("x\ny\nz"), answer("a\nb\nc\nd"), "differs"),
    ("a log that went by faster than its tail", ["logs", "p", "--tail=2"], answer("l1\nl2"), answer("l5\nl6"), answer("l9\nl10"), "moved"),
    ("three pods' logs, one after another, each grown where it stands", ["logs", "-l", "app=agent", "--tail=50"], answer("a1\nb1\nc1"), answer("a1\na2\nb1\nc1"), answer("a1\na2\nb1\nb2\nc1\nc2"), "same"),
    ("three pods' logs with a line no pod wrote", ["logs", "-l", "app=agent", "--tail=50"], answer("a1\nb1\nc1"), answer("a1\nx\nb1\nc1"), answer("a1\na2\nb1\nc1"), "differs"),
    ("three pods' logs in another order of pods", ["logs", "-l", "app=agent"], answer("a1\nb1\nc1"), answer("c1\nb1\na1"), answer("a1\nb1\nc1\nc2"), "differs"),
    ("the last so many seconds of a log: a window that moves with the clock", ["logs", "p", "--since=45s"], answer("l1\nl2\nl3"), answer("l2\nl3\nl4"), answer("l3\nl4\nl5"), "same"),
    ("the last so many seconds, and other lines", ["logs", "p", "--since=45s"], answer("l1\nl2\nl3"), answer("x1\nx2"), answer("l1\nl2\nl3"), "differs"),
    ("a log since a time: it only grows", ["logs", "p", "--since-time=2026-10-07T09:00:00Z"], answer("l2\nl3"), answer("l2\nl3\nl4"), answer("l2\nl3\nl4"), "same"),
    ("a log since a time that was not obeyed", ["logs", "p", "--since-time=2026-10-07T09:00:00Z"], answer("l2\nl3"), answer("l1\nl2\nl3"), answer("l2\nl3\nl4"), "differs"),
    ("`kubectl events` sorts by time itself: two of one second either way round", ["events", "-n", "shop"], answer("H\n5s b\n5s a\n"), answer("H\n5s a\n5s b\n"), answer("H\n5s b\n5s a\n"), "order"),
    ("a pod that would not pull its image, and then backed off: the frozen refusal is the first of the two", ["logs", "p"], answer("", "image can't be pulled", 1), answer("", "image can't be pulled", 1), answer("", "trying and failing to pull image", 1), "same"),
    ("the cluster refused in one way and then in another, and the frozen copy in a third", ["logs", "p"], answer("", "image can't be pulled", 1), answer("", "unknown reason", 1), answer("", "trying and failing to pull image", 1), "worded"),
    ("several logs asked for in no fixed order, one of them refused", ["logs", "p", "--all-containers"], answer("first\n", "container is waiting", 1), answer("", "container is waiting", 1), answer("first\n", "container is waiting", 1), "same"),
    ("one log refused, having printed something else", ["logs", "p"], answer("first\n", "container is waiting", 1), answer("", "container is waiting", 1), answer("first\n", "container is waiting", 1), "worded"),
    ("the log of the run before, and the container started again after the freeze", ["logs", "p", "--previous"], answer("r1a\nr1b"), answer("r1a\nr1b"), answer("r2a"), "same"),
    ("the container started again before the freeze: the start of its next run", ["logs", "p"], answer("r1a\nr1b"), answer("r2a"), answer("r2a\nr2b"), "same"),
    ("the container started again, and the frozen log is of neither run", ["logs", "p"], answer("r1a\nr1b"), answer("x\ny"), answer("r2a\nr2b"), "differs"),
    ("the container started again, and the frozen log has nothing in it", ["logs", "p"], answer("r1a\nr1b"), answer(""), answer("r2a\nr2b"), "differs"),
    ("the container had not run before, and then had: the frozen copy refuses as the cluster first did", ["logs", "p", "--previous"],
     answer("", "previous terminated container not found", 1), answer("", "previous terminated container not found", 1), answer("r1a\nr1b"), "same"),
    ("the container had not run before, and then had: the frozen copy has its log, as the cluster then did", ["logs", "p", "--previous"],
     answer("", "previous terminated container not found", 1), answer("r1a\nr1b"), answer("r1a\nr1b"), "same"),
    ("the container had not run before, and then had: the frozen copy says something else", ["logs", "p", "--previous"],
     answer("", "previous terminated container not found", 1), answer("", "unknown reason", 1), answer("r1a\nr1b"), "differs"),
    ("several logs, one refused, and a line before the refusal that no log has", ["logs", "p", "--tail=5", "--all-containers"],
     answer("schema is locked\n", "container is waiting", 1), answer("some other text nobody logged\n", "container is waiting", 1), answer("schema is locked\n", "container is waiting", 1), "worded"),
    ("a tail of -1 is all of it", ["logs", "p", "--tail=-1"], answer("a\nb"), answer("a\nb\nc"), answer("a\nb\nc\nd"), "same"),
    ("the last so many seconds, the flag and its value as two words", ["logs", "p", "--since", "45s"], answer("l1\nl2\nl3"), answer("l2\nl3\nl4"), answer("l3\nl4\nl5"), "same"),
    ("the last so many seconds, not obeyed: older lines than the cluster gave", ["logs", "p", "--since=45s"], answer(""), answer("old1\nold2\nl1"), answer("l1"), "differs"),
    ("the last so many seconds, with a line no asking of the cluster saw", ["logs", "p", "--since=45s"], answer("l1\nl2\nl3"), answer("l3\nGARBAGE\nl4\nl5"), answer("l4\nl5\nl6"), "differs"),
    ("a tail with a line no asking of the cluster saw", ["logs", "p", "--tail=5"], answer(lines("l", 5)), answer("l3\nl4\nGARBAGE\nl5\nl6"), answer("l5\nl6\nl7\nl8\nl9"), "differs"),
    ("a log since a time is not a window that moves: it may not lose its first lines", ["logs", "p", "--since-time=2026-10-07T09:00:00Z"], answer("a\nb\nc"), answer("b\nc"), answer("c\nd"), "differs"),
    ("the same answers for the last so many seconds, where the first lines do go", ["logs", "p", "--since=45s"], answer("a\nb\nc"), answer("b\nc"), answer("c\nd"), "same"),
    ("a log that went by faster than its tail, and a frozen one with nothing in it", ["logs", "p", "--tail=2"], answer("l1\nl2"), answer(""), answer("l9\nl10"), "differs"),
    ("the same log, and something said beside it on one side", ["logs", "p"], answer("a\nb", "Defaulted container"), answer("a\nb"), answer("a\nb", "Defaulted container"), "differs"),
    ("a log's lines with a time the snapshot server made up", ["logs", "p", "--timestamps", "--tail=3"], answer("unable to retrieve container logs"), answer("2026-10-07T15:55:31.795458000Z unable to retrieve container logs"), answer("unable to retrieve container logs"), "differs"),
]
EXCUSED = [  # the command, what compare said of it, whether known.txt lets it by
    (["explain", "pods"], "exit codes live 0, frozen 1, live 0: Error from server (NotFound)", True),
    (["cluster-info"], "live: Kubernetes control plane is running at https://a | frozen: Kubernetes control plane is running at http://b", True),
    (["cluster-info"], "live: \x1b[0;32mKubernetes control plane\x1b[0m is running at \x1b[0;33mhttps://a\x1b[0m | frozen: \x1b[0;32mKubernetes control plane\x1b[0m is running at \x1b[0;33mhttp://b\x1b[0m", True),
    (["cluster-info"], "exit codes live 0, frozen 1, live 0: The connection to the server was refused", False),
    (["describe", "secrets", "t", "-n", "kube-system"], "live: auth-extra-groups: 47 bytes | frozen: auth-extra-groups: 19 bytes", True),
    (["describe", "secret", "t"], "exit codes live 0, frozen 1, live 0: error: illegal base64 data at input byte 8", False),  # it once failed, and was excused
    (["describe", "secretproviderclass", "x"], "live: a: 47 bytes | frozen: a: 19 bytes", False),
    (["get", "pods", "-l", "app=explainer"], "live: x | frozen: y", False),
    (["get", "configmap", "cluster-info", "-n", "kube-public"], "live: x | frozen: y", False),
    (["explain", "pods"], "live: KIND: Pod | frozen: KIND: Deployment", False),
]
ASKED = [  # a command, and whether it is one to ask
    (["get", "pods", "-n", "shop"], True), (["-n", "shop", "get", "pods"], True), (["rollout", "history", "deploy/x"], True),
    (["-l", "version", "delete", "pods", "-A"], False), (["--field-manager", "get", "annotate", "pod", "x", "a=b"], False),
    (["rollout", "--field-manager", "history", "restart", "deploy/x"], False), (["rollout", "restart", "deploy/x"], False),
    (["get", "pods", "-w"], False), (["get", "--raw", "/api/v1/nodes"], False), (["delete", "pod", "x"], False),
    (["logs", "p", "--since=45s"], True), (["logs", "p", "--since-time=2026-10-07T09:00:00Z"], True), (["logs", "p", "--timestamps", "--tail=3"], True), (["logs", "p", "-f"], False),
]

# An age may differ by as long as lay between two askings, which is written beside each answer.
slow = lambda out, at: dict(answer(out), at=at)
VERDICTS += [
    ("describe, asked a minute apart on a slow machine", ["describe", "pod", "x"], slow("Age: 60s\n", 0), slow("Age: 2m\n", 60), slow("Age: 3m\n", 120), "same"),
    ("describe, asked two seconds apart and a minute older", ["describe", "pod", "x"], slow("Age: 60s\n", 0), slow("Age: 2m\n", 2), slow("Age: 64s\n", 4), "differs"),
]

# Whose clock an age is counted on. In a table it is the server's, and a case's server counts to the
# freeze however much later it is asked: here the cluster is asked at 99, frozen at 100, and the
# frozen copy asked a minute after that. `describe` counts for itself, from when it is run.
AGES = [  # what, the command, the three answers and when each was asked, when the case was frozen, the verdict
    ("a table's age, counted to the freeze a minute before the frozen copy was asked", ["get", "pods"], ("NAME AGE\na 60s\n", 99), ("NAME AGE\na 61s\n", 160), ("NAME AGE\na 2m3s\n", 162), 100, "same"),
    ("a table's age counted to when the frozen copy was asked, and not to the freeze", ["get", "pods"], ("NAME AGE\na 60s\n", 99), ("NAME AGE\na 2m1s\n", 160), ("NAME AGE\na 2m3s\n", 162), 100, "differs"),
    ("the same, the cluster asked again only four minutes later", ["get", "pods"], ("NAME AGE\na 60s\n", 99), ("NAME AGE\na 2m1s\n", 160), ("NAME AGE\na 5m1s\n", 340), 100, "differs"),
    ("a table's age ten minutes out, with a freeze that is known", ["get", "pods"], ("NAME AGE\na 60s\n", 99), ("NAME AGE\na 11m\n", 118), ("NAME AGE\na 81s\n", 120), 100, "differs"),
    ("a pod gone before the freeze: the frozen table is the second live one, twenty seconds younger for a freeze twenty seconds earlier", ["get", "pods"],
     ("NAME AGE\na 60s\nb 5s\n", 99), ("NAME AGE\na 61s\n", 118), ("NAME AGE\na 81s\n", 120), 100, "same"),
    ("the same answers with no freeze to count from: twenty seconds out", ["get", "pods"], ("NAME AGE\na 60s\nb 5s\n", 99), ("NAME AGE\na 61s\n", 118), ("NAME AGE\na 81s\n", 120), None, "differs"),
    ("an age that runs backwards: older in the frozen table than the cluster said after it", ["get", "pods"], ("NAME AGE\na 60s\n", 99), ("NAME AGE\na 75s\n", 101), ("NAME AGE\na 63s\n", 102), 100, "differs"),
    ("describe counts from when it is run: twenty seconds out is out, whenever the freeze was", ["describe", "pod", "a"], ("Age: 60s\n", 99), ("Age: 99s\n", 118), ("Age: 81s\n", 120), 100, "differs"),
    ("describe, asked nineteen seconds later and nineteen seconds older", ["describe", "pod", "a"], ("Age: 60s\n", 99), ("Age: 79s\n", 118), ("Age: 81s\n", 120), 100, "same"),
    ("describe that did not age at all between askings a minute apart", ["describe", "pod", "a"], ("Age: 60s\n", 0), ("Age: 61s\n", 60), ("Age: 3m\n", 120), 30, "differs"),
    ("an event seen again: when it was last seen is younger in the frozen table than growing would make it, and in the cluster's second answer too", ["get", "events"],
     ("LAST REASON\n8s BackOff\n3m Pulled\n", 99), ("LAST REASON\n1s BackOff\n3m Pulled\n", 118), ("LAST REASON\n3s BackOff\n3m Pulled\n", 120), 100, "same"),
    ("an event that was not seen again, and is younger in the frozen table all the same", ["get", "events"],
     ("LAST REASON\n60s BackOff\n3m Pulled\n", 99), ("LAST REASON\n1s BackOff\n3m Pulled\n", 118), ("LAST REASON\n81s BackOff\n3m Pulled\n", 120), 100, "differs"),
    ("an event seen again, and older in the frozen table than either answer of the cluster allows", ["get", "events"],
     ("LAST REASON\n8s BackOff\n3m Pulled\n", 99), ("LAST REASON\n60s BackOff\n3m Pulled\n", 118), ("LAST REASON\n3s BackOff\n3m Pulled\n", 120), 100, "differs"),
    ("a duration that is not an age stands still, whenever it is asked", ["get", "jobs"], ("NAME DURATION AGE\nj 5s 60s\n", 99), ("NAME DURATION AGE\nj 5s 61s\n", 160), ("NAME DURATION AGE\nj 5s 2m3s\n", 162), 100, "same"),
]

known = rd.load_known(os.path.join(os.path.dirname(os.path.abspath(__file__)), "known.txt"))
failures = []
for what, argv, before, frozen, after, want in VERDICTS:
    if (got := rd.verdict(argv, before, frozen, after)[0]) != want:
        failures.append(f"{what}: {got}, want {want}")
for what, argv, before, frozen, after, freeze, want in AGES:
    if (got := rd.verdict(argv, slow(*before), slow(*frozen), slow(*after), freeze)[0]) != want:
        failures.append(f"{what}: {got}, want {want}")
for argv, why, want in EXCUSED:
    if rd.excused(known, argv, why) != want:
        failures.append(f"kubectl {' '.join(argv)} ({why[:50]}…): excused {not want}, want {want}")
for argv, want in ASKED:
    if rd.wanted(argv) != want:
        failures.append(f"kubectl {' '.join(argv)}: asked {not want}, want {want}")
for argv, want in ((["get", "pods", "-n", "shop"], "get pods"), (["get", "pods", "-A", "-o", "wide"], "get pods -o wide"), (["get", "pod", "x", "-n", "shop", "-o", "wide"], "get pod one named -o wide")):
    if rd.shape(argv) != want:
        failures.append(f"shape of kubectl {' '.join(argv)}: {rd.shape(argv)!r}, want {want!r}")
# And through the command itself, files and all: a hundred commands whose verdict hangs on the freeze,
# which `compare` has to have read; and what it returns, which is what fails a sweep.
import contextlib, io, json, tempfile
with tempfile.TemporaryDirectory() as tmp:
    def written(name, doc):
        with open(os.path.join(tmp, name), "w") as f:
            json.dump(doc, f)
        return os.path.join(tmp, name)
    row = lambda out, at: [dict(slow(out, at), argv=["get", "pods", "-n", f"ns{i}"]) for i in range(100)]
    paths = [written("before.json", row("NAME AGE\na 60s\n", 99)), written("frozen.json", row("NAME AGE\na 61s\n", 160)), written("after.json", row("NAME AGE\na 2m3s\n", 162))]
    known_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "known.txt")
    for what, freeze_path, want in (("with the freeze it was given", written("freeze.json", {"freeze_time": 100}), 0), ("with no freeze", None, 1)):
        with contextlib.redirect_stdout(io.StringIO()) as said:
            got = rd.cmd_compare(*paths, known_path, freeze_path)
        if got != want or f"differ: {100 * want}**" not in said.getvalue():
            failures.append(f"compare, {what}: returned {got}, want {want}: {said.getvalue()[:120]}")
    with contextlib.redirect_stdout(io.StringIO()) as said:
        few = rd.cmd_compare(*[written(f"few-{n}.json", json.load(open(p))[:5]) for n, p in enumerate(paths)], known_path, None)
    if few != 1:
        failures.append("compare with five commands returned 0: nothing was compared, and that is not a pass")

print("\n".join(failures) or f"{len(VERDICTS) + len(AGES)} verdicts, {len(EXCUSED)} excuses and {len(ASKED)} commands are as they should be")
sys.exit(1 if failures else 0)
