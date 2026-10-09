"""What `replaydiff.py compare` must call a difference, and what it must not.

    python3 test/replay-diff/selftest.py

A comparison that passes is worth what it would have failed. Each line here is an answer a frozen
case once gave, or one a looser comparison let through, with the verdict it has to get.
"""
import os, re, sys, time

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
    ("a tail with the cluster's lines the other way up, the cluster having said the same twice", ["logs", "p", "--tail=5"], answer(lines("l", 5)), answer("l4\nl3\nl2\nl1\nl0"), answer(lines("l", 5)), "differs"),
    ("a tail that begins where the first ended and ends where the second begins, and is out of order between", ["logs", "p", "--tail=5"],
     answer("a\nb\nc\nd\ne"), answer("e\nc\nb\na\nd"), answer("d\ne\nf\ng\nh"), "differs"),
    ("the last so many seconds, the cluster's lines the other way up", ["logs", "p", "--since=45s"], answer("l1\nl2\nl3"), answer("l3\nl2\nl1"), answer("l1\nl2\nl3"), "differs"),
    ("the last so many seconds of a log that says one thing over and over: a line more or fewer, as the window falls", ["logs", "p", "--since=45s"],
     answer("alive\nalive\nalive\nalive"), answer("alive\nalive\nalive\nalive\nalive"), answer("alive\nalive\nalive\nalive"), "same"),
    ("the last so many seconds of a log that says one thing every half minute: one line, then two, then one", ["logs", "p", "--since=45s"], answer("alive"), answer("alive\nalive"), answer("alive"), "same"),
    ("the last so many seconds, two windows of the cluster's that share no line and a frozen one across where they meet", ["logs", "p", "--since=45s"], answer("l1\nl2"), answer("l2\nl3"), answer("l3\nl4"), "same"),
    ("the last so many seconds, with nothing in them: the one line the cluster had before was too old by then, and the next not yet written", ["logs", "p", "--since=45s"],
     answer("20:45:50 posted batch"), answer(""), answer("20:46:50 posted batch"), "same"),
    ("the last so many seconds, with nothing in them, where the cluster's two windows share a line", ["logs", "p", "--since=45s"], answer("l1\nl2"), answer(""), answer("l2\nl3"), "differs"),
    ("the last so many seconds, cut to one line where both of the cluster's windows share three", ["logs", "p", "--since=45s"], answer("l1\nl2\nl3\nl4"), answer("l2"), answer("l2\nl3\nl4\nl5"), "differs"),
    ("the last so many seconds of a log that had only begun", ["logs", "p", "--since=45s"], answer(""), answer("l1"), answer("l1\nl2"), "same"),
    ("a tail that bridges two windows of the cluster's that share no line", ["logs", "p", "--tail=5"], answer(lines("l", 5)), answer("l3\nl4\nl5\nl6\nl7"), answer("l6\nl7\nl8\nl9\nl10"), "moved"),
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
    # kubectl prints several logs as a map hands them over: now and then with another of them first, the rest following round.
    ("three pods' logs with another of them first", ["logs", "-l", "app=agent"], answer("a1\nb1\nc1"), answer("c1\na1\nb1"), answer("a1\nb1\nc1\nc2"), "order"),
    ("the same, each grown where it stands", ["logs", "-l", "app=agent", "--tail=300"], answer("a1\nb1\nc1"), answer("b1\nb2\nc1\na1"), answer("a1\na2\nb1\nb2\nc1"), "order"),
    ("the cluster's own later answer with another first, and the frozen one grown", ["logs", "-l", "app=agent"], answer("a1\nb1\nc1"), answer("a1\na2\nb1\nc1"), answer("b1\nc1\nc2\na1\na2"), "order"),
    ("another first, and a line no pod wrote", ["logs", "-l", "app=agent"], answer("a1\nb1\nc1"), answer("c1\nx\na1\nb1"), answer("a1\nb1\nc1"), "differs"),
    ("another first, and two lines of one log the wrong way round", ["logs", "-l", "app=agent"], answer("a1\na2\nb1\nc1"), answer("c1\na2\na1\nb1"), answer("a1\na2\nb1\nc1"), "differs"),
    ("another first, and a line missing", ["logs", "-l", "app=agent"], answer("a1\na2\nb1\nc1"), answer("c1\na1\nb1"), answer("a1\na2\nb1\nc1"), "differs"),
    ("one pod's log with its lines turned round is not several", ["logs", "agent-1"], answer("a1\na2\na3"), answer("a3\na1\na2"), answer("a1\na2\na3"), "differs"),
    ("the cluster's own later answer with another first, and a frozen one that is neither", ["logs", "-l", "app=agent"], answer("a1\nb1\nc1"), answer("x1\nx2\nx3"), answer("c1\na1\nb1"), "differs"),
    # Under --prefix a line says whose it is, and a turn is only where one log ends: not in the middle of one.
    ("with their names before them, another of them first", ["logs", "-l", "app=agent", "--prefix"], answer("[pod/a/x] 1\n[pod/a/x] 2\n[pod/b/x] 1"), answer("[pod/b/x] 1\n[pod/a/x] 1\n[pod/a/x] 2"), answer("[pod/a/x] 1\n[pod/a/x] 2\n[pod/b/x] 1"), "order"),
    ("with their names before them, turned in the middle of one", ["logs", "-l", "app=agent", "--prefix"], answer("[pod/a/x] 1\n[pod/a/x] 2\n[pod/b/x] 1"), answer("[pod/a/x] 2\n[pod/b/x] 1\n[pod/a/x] 1"), answer("[pod/a/x] 1\n[pod/a/x] 2\n[pod/b/x] 1"), "differs"),
    # `--since=0s` is no window: kubectl prints the tail, and nothing is not an answer to it.
    ("the last no seconds of a log, answered with nothing", ["logs", "p", "--tail=3", "--since=0s"], answer("a1\na2\na3\n"), answer(""), answer("a4\na5\na6\n"), "differs"),
    ("the last no seconds of a log, answered with its tail", ["logs", "p", "--tail=3", "--since=0s"], answer("a1\na2\na3\n"), answer("a2\na3\na4\n"), answer("a3\na4\na5\n"), "same"),
    ("the last five seconds of a log, in which nothing was written", ["logs", "p", "--since=5s"], answer("a1\n"), answer(""), answer("a9\n"), "same"),
    ("the last ten seconds, likewise", ["logs", "p", "--since=10s"], answer("a1\n"), answer(""), answer("a9\n"), "same"),
    ("the last no hours and no minutes of a log, answered with nothing", ["logs", "p", "--tail=3", "--since=0h0m0.0s"], answer("a1\na2\na3\n"), answer(""), answer("a4\na5\na6\n"), "differs"),
    ("a log of blank lines, answered with nothing", ["logs", "p"], answer("\n\n"), answer(""), answer("\n\n"), "differs"),
    ("a log with a blank line in the middle, answered without it", ["logs", "p"], answer("a1\n\na2\n"), answer("a1\na2\n"), answer("a1\n\na2\n"), "differs"),
    # A blank line an application wrote is a line of its log, at the end of a tail as anywhere.
    ("a tail of five that ends in a blank line, the log having moved on between the askings", ["logs", "p", "--tail=5", "--all-containers"],
     answer("a1\na2\n\na3\na4\n"), answer("\na3\na4\na5\n\n"), answer("b1\nb2\nb3\nb4\nb5\n"), "moved"),
    ("a tail of five that is four lines", ["logs", "p", "--tail=5", "--all-containers"], answer("a1\na2\n\na3\na4\n"), answer("\na3\na4\na5\n"), answer("b1\nb2\nb3\nb4\nb5\n"), "differs"),
    ("a log that ends in a blank line, whole", ["logs", "p"], answer("a1\n\n"), answer("a1\n\n"), answer("a1\n\na2\n"), "same"),
    ("a log that has lost the blank line it ended in", ["logs", "p"], answer("a1\n\n"), answer("a1\n"), answer("a1\n\n"), "differs"),
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
    ("the last so many seconds, with a line no asking of the cluster saw", ["logs", "p", "--since=45s"], answer("l1\nl2\nl3\nl4"), answer("l3\nGARBAGE\nl4\nl5"), answer("l4\nl5\nl6"), "differs"),
    ("a tail with a line no asking of the cluster saw", ["logs", "p", "--tail=5"], answer(lines("l", 5)), answer("l2\nl3\nGARBAGE\nl4\nl5"), answer("l3\nl4\nl5\nl6\nl7"), "differs"),
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
    # A namespace before the verb, in each way it is written; and nothing else read past.
    (["--namespace", "shop", "get", "pods"], True), (["--namespace=shop", "get", "pods"], True), (["-n=shop", "get", "pods"], True), (["-nshop", "get", "pods"], True),
    (["-nshop", "rollout", "history", "deploy/x"], True), (["-nshop", "rollout", "restart", "deploy/x"], False), (["--namespace=shop", "delete", "pod", "x"], False),
    (["-nshop"], False), (["-v6", "-n", "shop", "get", "pods"], False), (["-n", "shop", "-o", "get", "delete", "pod", "x"], False),
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
    ("an age written to the hour where a cluster writes minutes, the askings ten seconds apart", ["get", "pods"], ("NAME AGE\na 89m\n", 100), ("NAME AGE\na 1h\n", 110), ("NAME AGE\na 89m\n", 120), 105, "differs"),
    ("three hours and five minutes against three hours, ten seconds apart", ["get", "pods"], ("NAME AGE\na 3h5m\n", 100), ("NAME AGE\na 3h\n", 110), ("NAME AGE\na 3h5m\n", 120), 105, "differs"),
    ("five days and three hours against five days", ["get", "pods"], ("NAME AGE\na 5d3h\n", 100), ("NAME AGE\na 5d\n", 110), ("NAME AGE\na 5d3h\n", 120), 105, "differs"),
    ("an age crossing from fifty-nine minutes of an hour to the hour, ten seconds apart", ["get", "pods"], ("NAME AGE\na 3h59m\n", 100), ("NAME AGE\na 4h\n", 110), ("NAME AGE\na 4h\n", 120), 105, "same"),
    ("an age crossing from hours to days, ten seconds apart", ["get", "pods"], ("NAME AGE\na 47h\n", 100), ("NAME AGE\na 2d\n", 110), ("NAME AGE\na 2d\n", 120), 105, "same"),
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
import contextlib, inspect, io, json, subprocess, tempfile
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

# What an agent typed, read out of a run's record: each command of a step on its own, parted where a
# shell parts them; a redirection taken off and not the semicolon behind it, nor a number that only
# stands near it, nor an arrow inside quotes; and a command left alone only if a shell would change it.
TYPED = [  # a step's input, the kubectl commands in it
    ("kubectl get pods -n shop", [["get", "pods", "-n", "shop"]]),
    ("kubectl logs -n media deploy/web --tail=50 2>&1; echo ---; kubectl get events -n media 2>/dev/null | tail -40",
     [["logs", "-n", "media", "deploy/web", "--tail=50"], ["get", "events", "-n", "media"]]),
    ("kubectl -n ledger get pods -o wide 2>/dev/null || kubectl -n ledger get pods", [["-n", "ledger", "get", "pods", "-o", "wide"], ["-n", "ledger", "get", "pods"]]),
    ("kubectl get pods -n shop > /tmp/out.txt; kubectl get svc -n shop >>out 2>&1", [["get", "pods", "-n", "shop"], ["get", "svc", "-n", "shop"]]),
    ("kubectl get deploy x -n media -o jsonpath='{$.spec.replicas}' | head -1", [["get", "deploy", "x", "-n", "media", "-o", "jsonpath={$.spec.replicas}"]]),
    ("kubectl logs x -n shop | grep 'refused$'", [["logs", "x", "-n", "shop"]]),
    ("kubectl get pods -n $NS", []), ('kubectl get pods -n "$NS"', []), ("kubectl get pods -n `cat ns`", []), ("for d in a b; do kubectl get deploy $d; done", []),
    ("kubectl get pods -n shop <<EOF", []), ("echo hello; promq 'up'", []),
    ("kubectl get pods -n shop\nkubectl describe pod x -n shop", [["get", "pods", "-n", "shop"], ["describe", "pod", "x", "-n", "shop"]]),
    # Redirections: closed, with a number that is an argument and not a descriptor, both streams, and none at all inside quotes.
    ("kubectl get pods -n shop 2>&-", [["get", "pods", "-n", "shop"]]), ("kubectl logs x --tail 2 >/dev/null", [["logs", "x", "--tail", "2"]]),
    ("kubectl get pods &>/dev/null; kubectl get svc 2>&1 | head", [["get", "pods"], ["get", "svc"]]), ("kubectl get pods -n shop | grep ' > '", [["get", "pods", "-n", "shop"]]),
    ("kubectl get pods -o jsonpath='{.items[?(@.status.restarts > 0)].metadata.name}'; kubectl get svc", [["get", "pods", "-o", "jsonpath={.items[?(@.status.restarts > 0)].metadata.name}"], ["get", "svc"]]),
    ("kubectl get pods >'my out;put'; kubectl get svc", [["get", "pods"], ["get", "svc"]]),
    # A command that needs a shell is left out and the others of its line are not; one a loop only repeats is taken.
    ("for d in a b; do echo \"== $d\"; kubectl get deploy $d -n media; done; kubectl logs -n media deploy/a --tail=8", [["logs", "-n", "media", "deploy/a", "--tail=8"]]),
    ("for i in 1 2; do kubectl get pods -n shop; done", [["get", "pods", "-n", "shop"]]),
    ("kubectl logs $(kubectl get pods -n shop -o name | head -1) -n shop", [["get", "pods", "-n", "shop", "-o", "name"]]),
    ("kubectl get pod it\\'s -n shop; kubectl get pod $P", [["get", "pod", "it's", "-n", "shop"]]), ("kubectl get pods -l 'a=\\$b'", [["get", "pods", "-l", "a=\\$b"]]),
    ('kubectl get pods -l "a=\\$b"', []),
    # An argument over several lines is one argument; a line continued is one line; a here-document's body and an open quote are nothing.
    ("kubectl get cm x -n shop -o go-template='{{range $k,$v := .data}}== {{$k}}\n{{$v}}\n{{end}}'", [["get", "cm", "x", "-n", "shop", "-o", "go-template={{range $k,$v := .data}}== {{$k}}\n{{$v}}\n{{end}}"]]),
    ("kubectl get pods \\\n  -n shop", [["get", "pods", "-n", "shop"]]), ("cat <<EOF\nkubectl delete pod x\nEOF\nkubectl get pods", []), ("kubectl get pods -o jsonpath='{.items", []),
]
with tempfile.TemporaryDirectory() as tmp:
    def typed(text, left=None):
        d = tempfile.mkdtemp(dir=tmp)
        with open(os.path.join(d, "run.json"), "w") as f:
            json.dump({"transcript": {"steps": [{"tool": "bash", "input": text}]}}, f)
        return rd.typed_commands([d], left)
    for line, want in TYPED:
        if (got := typed(line)) != want:
            failures.append(f"what was typed in {line!r}: {got}, want {want}")
    # What is not asked is counted: a command that needs a shell, a step that cannot be parted; not a command with no kubectl in it.
    for line, want in (("echo $HOME; kubectl get pods -n $NS; kubectl get svc", 1), ("cat <<EOF\nkubectl get pods\nEOF", 1), ("echo $HOME", 0), ("kubectl get pods", 0)):
        left = []
        typed(line, left)
        if len(left) != want:
            failures.append(f"what was left of {line!r}: {left}, want {want} of them")

# promq, which has a comparison of its own (promdiff.py): three answers about one named instant, which
# are the same text or are not. Nothing in them is an age, so nothing is let by for having moved —
# unless the Prometheus's own two answers differ, and then the frozen one has to be one of the two.
import promdiff as pd
said = lambda out, rc=0: {"out": out, "rc": rc}
# An answer of the API, as `fetch` keeps one; and two queries, one whose order is a map's and one whose order is the store's.
sent = lambda *series, **more: said(json.dumps(dict({"status": "success", "data": {"resultType": "vector", "result": [{"metric": {"c": c}, "value": [5.0, v]} for c, v in series]}}, **more), sort_keys=True, indent=1) + "\n", 200)
counted, selected = 'query=count_values("v", x)', "query=x"
PROMQ = [  # what, live, frozen, live again, the verdict
    ("the same", said("a{x=1} 1\n"), said("a{x=1} 1\n"), said("a{x=1} 1\n"), "same"),
    ("another value", said("a{x=1} 1\n"), said("a{x=1} 2\n"), said("a{x=1} 1\n"), "differs"),
    ("a value a digit longer", said("a{x=1} 0.1\n"), said("a{x=1} 0.10000001\n"), said("a{x=1} 0.1\n"), "differs"),
    ("a series fewer", said("a{x=1} 1\na{x=2} 2\n"), said("a{x=1} 1\n"), said("a{x=1} 1\na{x=2} 2\n"), "differs"),
    ("a series more", said("a{x=1} 1\n"), said("a{x=1} 1\na{x=2} 2\n"), said("a{x=1} 1\n"), "differs"),
    ("the series in another order", said("a{x=1} 1\na{x=2} 2\n"), said("a{x=2} 2\na{x=1} 1\n"), said("a{x=1} 1\na{x=2} 2\n"), "differs"),
    ("a point of a range a second off", said("a{} 19:24:05=1 19:24:20=2\n"), said("a{} 19:24:04=1 19:24:19=2\n"), said("a{} 19:24:05=1 19:24:20=2\n"), "differs"),
    ("nothing where there was something", said("a{x=1} 1\n"), said("(empty result)\n"), said("a{x=1} 1\n"), "differs"),
    ("the Prometheus moved, and the frozen answer is its first", said("a{} 1\n"), said("a{} 1\n"), said("a{} 2\n"), "moved"),
    ("the Prometheus moved, and the frozen answer is its second", said("a{} 1\n"), said("a{} 2\n"), said("a{} 2\n"), "moved"),
    ("the Prometheus moved, and the frozen answer is neither", said("a{} 1\n"), said("a{} 3\n"), said("a{} 2\n"), "differs"),
    ("both refuse in the same words", said("query failed: x\n", 1), said("query failed: x\n", 1), said("query failed: x\n", 1), "same"),
    ("both refuse, in other words", said("query failed: x\n", 1), said("query failed: y\n", 1), said("query failed: x\n", 1), "worded"),
    ("the frozen store refuses what the Prometheus answers", said("a{} 1\n"), said("query failed: x\n", 1), said("a{} 1\n"), "differs"),
    ("the frozen store answers what the Prometheus refuses", said("query failed: x\n", 1), said("a{} 1\n"), said("query failed: x\n", 1), "differs"),
    ("the same words and another exit code", said("a{} 1\n"), said("a{} 1\n", 1), said("a{} 1\n"), "differs"),
]
for what, a, f, b, want in PROMQ:
    if (got := pd.verdict(["x"], a, f, b)) != want:
        failures.append(f"promq, {what}: {got}, want {want}")
# Nothing is let by for being `topk`: which of several equal series it keeps follows the order a store
# hands them over in, a case keeps its Prometheus's, and another series kept is a difference.
three, other = "x{c=a} 9\nx{c=b} 5\nx{c=c} 5\n", "x{c=a} 9\nx{c=b} 5\nx{c=d} 5\n"
TIES = [(["topk(3, x)"], three, other), (["bottomk(3, x)"], three, other), (["topk(3, x)"], three, "x{c=a} 9\nx{c=c} 5\nx{c=b} 5\n")]
for argv, a, f in TIES:
    if (got := pd.verdict(argv, said(a), said(f), said(a))) != "differs":
        failures.append(f"promq {argv}, another of the equal series: {got}, want differs")
PROMQ_TYPED = [  # a step's input, the promq commands in it
    ("promq 'up'", [["up"]]), ("promq 'sum by (client) (rate(x_total[1m]))' --range 10m --step 30s 2>&1 | head -20; echo ---; kubectl get pods -n media", [["sum by (client) (rate(x_total[1m]))", "--range", "10m", "--step", "30s"]]),
    ('promq "up{job=\\"thumb-api\\"}"; promq \'x{le="10"}\' --range 45m', [['up{job="thumb-api"}'], ['x{le="10"}', "--range", "45m"]]),
    ('promq "rate(x_total[$W])"', []), ("echo promq; promq; kubectl get pods", []), ("for q in up x; do promq $q; done; promq 'up'", [["up"]]),
    ("promq 'histogram_quantile(0.9,\n  sum by (le) (rate(x_bucket[5m])))'", [["histogram_quantile(0.9,\n  sum by (le) (rate(x_bucket[5m])))"]]),
]
with tempfile.TemporaryDirectory() as tmp:
    for text, want in PROMQ_TYPED:
        d = tempfile.mkdtemp(dir=tmp)
        with open(os.path.join(d, "run.json"), "w") as f:
            json.dump({"transcript": {"steps": [{"tool": "bash", "input": text}]}}, f)
        left = []
        if (got := pd.typed_queries([d], left)) != want:
            failures.append(f"what promq was asked in {text!r}: {got}, want {want}")
        if len(left) != text.count("$"):
            failures.append(f"what was left of {text!r}: {left}")
    # Through the command itself: what it returns is what fails a sweep.
    def asked(name, outs):
        path = os.path.join(tmp, name)
        with open(path, "w") as f:
            json.dump([{"argv": [f"m{i}"], "typed": i < 5, "rc": 0, "out": out} for i, out in enumerate(outs)], f)
        return path
    alike = [f"m{i}{{}} {i}\n" for i in range(60)]
    hundred = [f"m{i}{{}} {i}\n" for i in range(100)]
    one_off = alike[:7] + ["m7{} 8\n"] + alike[8:]
    with open(os.path.join(tmp, "known-promq.txt"), "w") as f:
        f.write("# a comment\n^promq m7$\t0 m7\\{\\} 7\\n\t0 m7\\{\\} 8\\n\tfor the test\n")
    with open(os.path.join(tmp, "known-loosely.txt"), "w") as f:  # a line that names part of each answer names neither
        f.write("^promq m7$\tm7\tm7\tfor the test\n")
    known = os.path.join(tmp, "known-promq.txt")
    other_off = alike[:7] + ["m7{} 9\n"] + alike[8:]
    for what, frozen, as_typed, second, known_path, want, shows in (
        ("alike", alike, alike, alike, None, 0, "**differ: 0**"), ("one answer differs", one_off, one_off, alike, None, 1, "**differ: 1**"),
        ("one answer differs and is known", one_off, one_off, alike, known, 0, "(known: for the test)"),
        ("one answer differs, and a line of the known file has a word of it", one_off, one_off, alike, os.path.join(tmp, "known-loosely.txt"), 1, "**differ: 1**"),
        # A line of the known file excuses the difference it describes, not whatever its query happens to answer.
        ("the known query differs in another way", other_off, other_off, alike, known, 1, "**differ: 1**"),
        # Nor an answer the Prometheus itself gave two ways: what is known is known of one answer of its.
        ("the known query differs as known, of a Prometheus that then answered otherwise", one_off, one_off, other_off, known, 1, "**differ: 1**"),
        ("an agent's asking differs from the named one", alike, one_off, alike, None, 1, "answered 59 of the 60 the same"),
        ("an agent's asking differs, of a query that is known for something else", alike, one_off, alike, known, 1, "answered 59 of the 60 the same"),
        ("too few", alike[:9], alike[:9], alike[:9], None, 1, "too few"),
        # One answer the Prometheus changed between its askings is the Prometheus; most of them is a second asking of something else.
        ("the Prometheus changed one of its own answers", alike, alike, one_off, None, 0, "between its two askings: 1"),
        ("the Prometheus changed two of its own answers in sixty", alike, alike, one_off[:9] + ["m9{} 0\n"] + one_off[10:], None, 1, "nothing here was compared"),
        ("the second asking of the Prometheus was refused", alike, alike, ["query failed: no one there\n"] * 60, None, 1, "nothing here was compared"),
        ("the Prometheus changed two of its own answers in a hundred", hundred, hundred, ["m0{} 9\n", "m1{} 9\n"] + hundred[2:], None, 0, "between its two askings: 2"),
        ("the Prometheus changed three of its own answers in a hundred", hundred, hundred, ["m0{} 9\n", "m1{} 9\n", "m2{} 9\n"] + hundred[3:], None, 1, "nothing here was compared"),
        # Fifty are a comparison and forty-nine are not.
        ("fifty", alike[:50], alike[:50], alike[:50], None, 0, "queries: 50 |"), ("forty-nine", alike[:49], alike[:49], alike[:49], None, 1, "too few")):
        paths = [asked("live-1.json", (hundred if len(frozen) > 60 else alike)[:len(frozen)]), asked("frozen.json", frozen), asked("live-2.json", second), asked("as-typed.json", as_typed)]
        with contextlib.redirect_stdout(io.StringIO()) as out:
            got = pd.cmd_compare(*paths, *([known_path] if known_path else []))
        if got != want or shows not in out.getvalue():
            failures.append(f"promq compare, {what}: returned {got}, want {want}, and printed {out.getvalue()[:300]!r}")
    # Three questions look back further than the hour a sweep freezes, on purpose, and of those the
    # frozen answer has to be the Prometheus's with what the case cannot know taken off, said in the
    # words the case's own beginning gives. It is this file's list that says which they are, not the
    # store's saying so: the same words under any other answer are a difference.
    froze = 1791461299434
    case = {"freeze_ms": froze, "from_ms": froze - 3_900_000, "lookback_ms": 300_000, "askew": False}
    began = "frozen case: it holds no samples before 2026-10-08T11:03:19.434Z, and this query looks "
    of_an_instant = lambda by: began + by + " further back than that: its Prometheus may have had more to answer from"
    of_the_window = began + "2h0m0s further back than that: its points before 2026-10-08T11:08:19.434Z may be missing, or come of less than its Prometheus had"
    clock = lambda back: time.strftime("%H:%M:%S", time.gmtime((froze - back * 1000) // 1000))
    steps = [10800 - 600 * i for i in range(19)]  # three hours in steps of ten minutes, as seconds before the freeze
    row = lambda name, backs: name + " " + " ".join(f"{clock(back)}=1" for back in backs) + "\n"
    live_window = row("up{job=a}", steps) + row("up{job=b}", steps[-3:]) + row("up{job=gone}", steps[:5])
    kept_window = row("up{job=a}", steps[-7:]) + row("up{job=b}", steps[-3:])
    under = lambda text, words: text + "(" + words + ")\n"
    back2h, back90m, back3h = ["up offset 2h"], ["up offset 90m"], ["up", "--range", "3h", "--step", "10m"]
    for what, argv, live, frozen, want in (
        ("an instant before the case, answered with nothing and said so", back2h, "up{job=a} 1\n", under("(empty result)\n", of_an_instant("1h0m0s")), "edge"),
        ("the other instant before the case", back90m, "up{job=a} 1\n", under("(empty result)\n", of_an_instant("30m0s")), "edge"),
        ("an instant before the case, of a Prometheus that had nothing there either", back2h, "(empty result)\n", under("(empty result)\n", of_an_instant("1h0m0s")), "edge"),
        ("it answered with nothing and did not say so", back2h, "(empty result)\n", "(empty result)\n", "differs"),
        ("it said so and answered with something", back2h, "up{job=a} 1\n", under("up{job=a} 1\n", of_an_instant("1h0m0s")), "differs"),
        ("it said how far in other words", back2h, "up{job=a} 1\n", under("(empty result)\n", of_an_instant("30m0s")), "differs"),
        ("it said so of another beginning", back2h, "up{job=a} 1\n", under("(empty result)\n", of_an_instant("1h0m0s").replace("11:03:19", "11:03:20")), "differs"),
        ("it said so twice", back2h, "up{job=a} 1\n", under(under("(empty result)\n", of_an_instant("1h0m0s")), of_an_instant("1h0m0s")), "differs"),
        ("it said of an instant what is said of a window", back2h, "up{job=a} 1\n", under("(empty result)\n", of_the_window.replace("2h0m0s", "1h0m0s")), "differs"),
        ("a window, answered with its last hour and said so", back3h, live_window, under(kept_window, of_the_window), "edge"),
        ("a window, with a point from before the case", back3h, live_window, under(row("up{job=a}", steps[-8:]) + row("up{job=b}", steps[-3:]), of_the_window), "differs"),
        ("a window, with a point missing that the case has to have", back3h, live_window, under(row("up{job=a}", steps[-6:]) + row("up{job=b}", steps[-3:]), of_the_window), "differs"),
        ("a window, with a series missing that the case has to have", back3h, live_window, under(row("up{job=a}", steps[-7:]), of_the_window), "differs"),
        ("a window, with a point taken out of the middle", back3h, live_window, under(row("up{job=a}", steps[-7:-4] + steps[-3:]) + row("up{job=b}", steps[-3:]), of_the_window), "differs"),
        ("a window, with a time of day there is none of", back3h, live_window, under(kept_window.replace(clock(3600), "36:08:24", 1), of_the_window), "differs"),
        ("a window, with a line that is no series, as promq ends an answer of too many", back3h, live_window + "(3 more series not shown; narrow the query)\n", under(kept_window + "(3 more series not shown; narrow the query)\n", of_the_window), "differs"),
        ("a window, said above its answer and not under it", back3h, live_window, "(" + of_the_window + ")\n" + kept_window, "differs"),
        # A line is read as a series and its steps, and a name that holds what a step looks like is read where it does not end: it differs, which is the way to be wrong.
        ("a window, of a series whose name holds what a step looks like", back3h, row('up{job="a} 10:00:00=1"}', steps) + row("up{job=b}", steps[-3:]), under(row('up{job="a} 10:00:00=1"}', steps[-7:]) + row("up{job=b}", steps[-3:]), of_the_window), "differs"),
        ("a window, of a series whose name holds a brace and a space", back3h, row('up{job="a} b"}', steps) + row("up{job=b}", steps[-3:]), under(row('up{job="a} b"}', steps[-7:]) + row("up{job=b}", steps[-3:]), of_the_window), "edge"),
        ("a window, with another value", back3h, live_window, under(kept_window.replace(f"{clock(0)}=1\nup{{job=b}}", f"{clock(0)}=2\nup{{job=b}}"), of_the_window), "differs"),
        ("a window, with its series the other way round", back3h, live_window, under(row("up{job=b}", steps[-3:]) + row("up{job=a}", steps[-7:]), of_the_window), "differs"),
        ("a window, with a series the Prometheus does not have", back3h, live_window, under(kept_window + row("up{job=z}", steps[-3:]), of_the_window), "differs"),
        ("a window, with a series twice", back3h, live_window, under(kept_window + row("up{job=b}", steps[-3:]), of_the_window), "differs"),
        ("a window, with a series that ended before the case", back3h, live_window, under(kept_window + row("up{job=gone}", steps[:5]), of_the_window), "differs"),
        ("a window, answered with nothing", back3h, live_window, under("(empty result)\n", of_the_window), "differs"),
        ("a window, said in the words of an instant", back3h, live_window, under(kept_window, of_an_instant("2h0m0s")), "differs"),
        ("a window, whole and nothing said", back3h, live_window, live_window, "differs"),
        # And the words under an answer to any other question excuse nothing.
        ("another question, answered rightly and said to look back", ["up offset 1h"], "up{job=a} 1\n", under("up{job=a} 1\n", of_an_instant("1h0m0s")), "differs"),
        ("another question, answered with less and said to look back", ["up", "--range", "2h"], live_window, under(kept_window, of_the_window), "differs"),
    ):
        if (got := pd.verdict(argv, said(live), said(frozen), said(live), case)) != want:
            failures.append(f"what looks back, {what}: {got}, want {want}")
    # A window that began the day before: a time of day is the last one that is not after the freeze.
    night = dict(case, freeze_ms=froze - froze % 86400000 + 1_800_434, from_ms=froze - froze % 86400000 + 1_800_434 - 3_900_000)
    hands = lambda back: time.strftime("%H:%M:%S", time.gmtime((night["freeze_ms"] - back * 1000) // 1000))
    by_night = lambda backs: "up{job=a} " + " ".join(f"{hands(back)}=1" for back in backs) + "\n"
    of_the_night = pd.remark_for(tuple(back3h), 10800, night)
    for what, kept, want in (("its last hour", steps[-7:], "edge"), ("a point less", steps[-6:], "differs"), ("a point more", steps[-8:], "differs")):
        if (got := pd.verdict(back3h, said(by_night(steps)), said(under(by_night(kept), of_the_night)), said(by_night(steps)), night)) != want:
            failures.append(f"what looks back, a window that began the day before, with {what}: {got}, want {want}")
    looked = under("(empty result)\n", of_an_instant("1h0m0s"))
    for what, got, want in (("with another exit code", pd.verdict(back2h, said("x\n", 1), said(looked), said("x\n", 1), case), "differs"),
                            # A Prometheus that changed its own answer between its two askings: what the case has to answer is held to either, and is no difference of the case's.
                            ("of a Prometheus that changed its answer", pd.verdict(back2h, said("up{job=a} 1\n"), said(looked), said("up{job=a} 2\n"), case), "moved"),
                            ("of a window the Prometheus changed before the case's beginning", pd.verdict(back3h, said(live_window), said(under(kept_window, of_the_window)), said(live_window.replace(f"{clock(10800)}=1", f"{clock(10800)}=0", 1)), case), "moved"),
                            ("of a window the Prometheus changed where the case has it, held to its first answer", pd.verdict(back3h, said(live_window), said(under(kept_window, of_the_window)), said(live_window.replace(f"{clock(0)}=1", f"{clock(0)}=0", 1)), case), "moved"),
                            ("of a window the Prometheus changed where the case has it, held to its second answer", pd.verdict(back3h, said(live_window.replace(f"{clock(0)}=1", f"{clock(0)}=0", 1)), said(under(kept_window, of_the_window)), said(live_window), case), "moved"),
                            ("of a window the Prometheus changed, and wrong by both", pd.verdict(back3h, said(live_window), said(under(row("up{job=a}", steps[-6:]) + row("up{job=b}", steps[-3:]), of_the_window)), said(live_window.replace(f"{clock(0)}=1", f"{clock(0)}=0", 1)), case), "differs"),
                            ("with nothing known of the case", pd.verdict(back2h, said("up{job=a} 1\n"), said(looked), said("up{job=a} 1\n")), "differs"),
                            ("of a case that does not begin where the sweep's freeze puts it", pd.verdict(back2h, said("up{job=a} 1\n"), said(looked), said("up{job=a} 1\n"), dict(case, askew=True)), "differs")):
        if got != want:
            failures.append(f"what looks back, said rightly, {what}: {got}, want {want}")
    # The same of the API, where it is said among the warnings: the request promq makes for the question,
    # its result empty or cut to the last hour, and everything else as the Prometheus sent it.
    document = lambda result, **beside: json.dumps(dict({"status": "success", "data": {"resultType": "matrix" if result and "values" in result[0] else "vector", "result": result}}, **beside), sort_keys=True, indent=1) + "\n"
    at = lambda back: f"#{(froze - back * 1000) / 1000:.3f}"
    one = lambda job, backs: {"metric": {"__name__": "up", "job": job}, "values": [[at(back), "1"] for back in backs]}
    api2h, api3h = ["GET"] + pd.request_of(back2h), ["GET"] + pd.request_of(back3h)
    was = [{"metric": {"__name__": "up", "job": "a"}, "value": [at(0), "1"]}]
    for what, argv, live, frozen, want in (
        ("an instant before the case", api2h, document(was), document([], warnings=[of_an_instant("1h0m0s")]), "edge"),
        ("an instant before the case, said among the infos", api2h, document(was), document([], infos=[of_an_instant("1h0m0s")]), "differs"),
        ("an instant before the case, with something else said as well", api2h, document(was), document([], warnings=["PromQL warning: x", of_an_instant("1h0m0s")]), "differs"),
        ("an instant before the case, with what the Prometheus warned of as well", api2h, document(was, warnings=["PromQL warning: x"]), document([], warnings=["PromQL warning: x", of_an_instant("1h0m0s")]), "edge"),
        ("an instant before the case, answered", api2h, document(was), document(was, warnings=[of_an_instant("1h0m0s")]), "differs"),
        ("an instant before the case, said of another distance", api2h, document(was), document([], warnings=[of_an_instant("30m0s")]), "differs"),
        ("an instant before the case, said of another beginning", api2h, document(was), document([], warnings=[of_an_instant("1h0m0s").replace("11:03:19", "11:03:20")]), "differs"),
        ("an instant before the case, said in the words of a window", api2h, document(was), document([], warnings=[of_the_window]), "differs"),
        ("a window", api3h, document([one("a", steps), one("b", steps[-3:])]), document([one("a", steps[-7:]), one("b", steps[-3:])], warnings=[of_the_window]), "edge"),
        ("a window, with a point from before the case", api3h, document([one("a", steps), one("b", steps[-3:])]), document([one("a", steps[-8:]), one("b", steps[-3:])], warnings=[of_the_window]), "differs"),
        ("a window, with a point missing", api3h, document([one("a", steps), one("b", steps[-3:])]), document([one("a", steps[-6:]), one("b", steps[-3:])], warnings=[of_the_window]), "differs"),
        ("a window, answered as another kind of result", api3h, document([one("a", steps)]), document([one("a", steps[-7:])], warnings=[of_the_window]).replace('"matrix"', '"vector"'), "differs"),
        # What a store could send that a reading of the points alone would let by: it is held as it was sent.
        ("a window, with something more in a series than its name and its points", api3h, document([one("a", steps)]), document([dict(one("a", steps[-7:]), histograms=[])], warnings=[of_the_window]), "differs"),
        ("a window, with its instants a part of a thousandth later", api3h, document([one("a", steps)]), document([{"metric": {"__name__": "up", "job": "a"}, "values": [[at(back) + "4", "1"] for back in steps[-7:]]}], warnings=[of_the_window]), "differs"),
        ("a window, with a value written otherwise", api3h, document([one("a", steps)]), document([{"metric": {"__name__": "up", "job": "a"}, "values": [[at(back), "1.0"] for back in steps[-7:]]}], warnings=[of_the_window]), "differs"),
        ("a window, with a series twice", api3h, document([one("a", steps)]), document([one("a", steps[-7:]), one("a", steps[-7:])], warnings=[of_the_window]), "differs"),
        ("a window, with a series left out that has points in the case", api3h, document([one("a", steps), one("b", steps[-3:])]), document([one("a", steps[-7:])], warnings=[of_the_window]), "differs"),
        ("a window, with something beside its result", api3h, document([one("a", steps)]), document([one("a", steps[-7:])], warnings=[of_the_window], stats={}), "differs"),
        ("an instant before the case, answered with a result that is no list", api2h, document(was), document([], warnings=[of_an_instant("1h0m0s")]).replace('"result": []', '"result": {}'), "differs"),
        ("an instant before the case, answered with no result at all", api2h, document(was), json.dumps({"status": "success", "warnings": [of_an_instant("1h0m0s")]}) + "\n", "differs"),
        ("an instant before the case, answered with data that hold no result", api2h, document(was), json.dumps({"status": "success", "data": [], "warnings": [of_an_instant("1h0m0s")]}) + "\n", "differs"),
        ("a window the Prometheus has nothing in, answered with a result that is no list", api3h, document([]).replace("vector", "matrix"), document([], warnings=[of_the_window]).replace("vector", "matrix").replace('"result": []', '"result": {}'), "differs"),
        ("a window the Prometheus has nothing in, answered with nothing", api3h, document([]).replace("vector", "matrix"), document([], warnings=[of_the_window]).replace("vector", "matrix"), "edge"),
        ("a window, with something in its result that is no series", api3h, document([one("a", steps)]), document([one("a", steps[-7:]), 5], warnings=[of_the_window]).replace('"vector"', '"matrix"'), "differs"),
        ("another request, said to look back", ["GET", "/api/v1/query", "query=up offset 1h", "time=<freeze>"], document(was), document(was, warnings=[of_an_instant("1h0m0s")]), "differs"),
    ):
        if (got := pd.verdict(argv, said(live, 200), said(frozen, 200), said(live, 200), case)) != want:
            failures.append(f"what looks back, of the API, {what}: {got}, want {want}")
    for what, got, want in (("the only warning", pd.unremarked(said(document([], warnings=[of_the_window]), 200)), document([])), ("one of two", pd.unremarked(said(document([], warnings=["PromQL warning: x", of_the_window]), 200)), document([], warnings=["PromQL warning: x"])),
                            ("none", pd.unremarked(said(document([], warnings=["PromQL warning: x"]), 200)), document([], warnings=["PromQL warning: x"])), ("of promq", pd.unremarked(said(looked)), "(empty result)\n"),
                            ("of promq, of a series without a name", pd.unremarked(said(under('{a="b"} 10:00=1\n{a="c"} 10:00=2\n', of_the_window))), '{a="b"} 10:00=1\n{a="c"} 10:00=2\n'),
                            ("beside what it says of its end", pd.unremarked(said(document([], warnings=[of_the_window], infos=["frozen case: it ends at …"]), 200)), document([], infos=["frozen case: it ends at …"])),
                            ("of promq, about its end", pd.unremarked(said("m{} 1\n(frozen case: it ends at …)\n")), "m{} 1\n(frozen case: it ends at …)\n")):
        if got != want:
            failures.append(f"an answer without what it says of its beginning, {what}: {got!r}, want {want!r}")
    # Through the command, with the case's freeze.json: where it ends and begins is read from it, and a
    # case whose metrics do not begin an hour and a lookback before its freeze is one no answer is held to.
    for what, from_ms, frozen2h, want, shows in (("a case that begins where it should", froze - 3_900_000, looked, 0, "are the Prometheus's answer with that much taken off\n\n- `promq 'up offset 2h'`\n  - " + of_an_instant("1h0m0s") + "\n"),
                                                 ("a case that says it begins a second later", froze - 3_899_000, looked.replace("11:03:19.434Z", "11:03:20.434Z"), 1, "**differ: 1**"),
                                                 ("an answer that does not say it looked back", froze - 3_900_000, "(empty result)\n", 1, "**differ: 1**")):
        with open(os.path.join(tmp, "freeze.json"), "w") as f:
            json.dump({"freeze_time": froze / 1000, "metrics": {"from_ms": from_ms, "lookback_delta_ms": 300000}}, f)
        # One of the sixty is refused, by all three alike: spoiled in its words it is worded otherwise, which is not let by.
        rows = lambda outs: [{"argv": [f"m{i}"] if i else back2h, "typed": False, "rc": 1 if i == 59 else 0, "out": "query failed: no\n" if i == 59 else out} for i, out in enumerate(outs)]
        for name, outs in (("live-1.json", ["up{job=a} 1\n"] + alike[1:]), ("frozen.json", [frozen2h] + alike[1:]), ("live-2.json", ["up{job=a} 1\n"] + alike[1:])):
            with open(os.path.join(tmp, name), "w") as f:
                json.dump(rows(outs), f)
        paths = [os.path.join(tmp, name) for name in ("live-1.json", "frozen.json", "live-2.json", "frozen.json")]
        with contextlib.redirect_stdout(io.StringIO()) as out:
            got = pd.cmd_compare(*paths, known, os.path.join(tmp, "freeze.json"))
        if got != want or shows not in out.getvalue():
            failures.append(f"promq compare, {what}: returned {got}, want {want}, and printed {out.getvalue()[:500]!r}")
        if want == 0:  # and what looks back is spoiled like any other answer, and passes none of it
            with contextlib.redirect_stdout(io.StringIO()) as out:
                got = pd.cmd_spoil(*paths[:3], os.path.join(tmp, "freeze.json"))
            if got != 0 or out.getvalue().count("| 0 | 60 |") != 7 or "Unspoiled, 1 of the 60 look back" not in out.getvalue() or "| what looks back: answered with what the Prometheus has, where the case has nothing | 0 | 1 |" not in out.getvalue():
                failures.append(f"spoil of sixty answers, one of which looks back: returned {got} and printed {out.getvalue()!r}")
    # A step that falls where a case holds what an instant looks back before its hour is one whose
    # point the case may have or not; none before that may be there, and every one after has to be.
    fives = [("up", [f"{clock(back)}=1" for back in range(10800, -1, -300)])]
    hour = lambda step: case["freeze_ms"] - (case["freeze_ms"] % 86400000) + sum(int(part) * unit for part, unit in zip(step.partition("=")[0].split(":"), (3600000, 60000, 1000))) + case["freeze_ms"] % 1000
    for what, kept, want in (("the hour", 13, True), ("the hour and the one step before it, at the case's very beginning", 14, True), ("a step from before the case", 15, False), ("less than the hour", 12, False)):
        if pd.kept_rightly(fives, [("up", fives[0][1][-kept:])], hour, case) is not want:
            failures.append(f"of a window in steps of five minutes, {what} kept: want {want}")
    # And the questions that look back are asked in steps none of which falls there, with the hour a
    # sweep freezes and what an instant looks back unless it is told otherwise.
    for question, reach in pd.LOOKS_BACK.items():
        if "--range" in question:
            length, every = (int(pd.seconds(question[question.index(flag) + 1])) for flag in ("--range", "--step"))
            between = [back for back in range(length, -1, -every) if pd.FROZEN_WINDOW < back <= pd.FROZEN_WINDOW + 300]
            if length != reach or between or not any(back > pd.FROZEN_WINDOW + 300 for back in range(length, -1, -every)):
                failures.append(f"the question {list(question)} that looks back has steps {between} where a case may have a point or not, or none before the case")
    # What looks back is spoiled in ways of its own too, which leave what it says as it is: a window, as
    # promq prints it and as the API sends it, and each way that applies to it fails.
    for what, argv, there, here in (("promq", back3h, live_window, under(kept_window, of_the_window)),
                                    ("the API", api3h, document([one("a", steps), one("b", steps[-3:])]), document([one("a", steps[-7:]), one("b", steps[-3:])], warnings=[of_the_window]))):
        rows = lambda outs: [{"argv": argv if not i else ["GET", "/api/v1/query", f"query=m{i}", "time=<freeze>"] if argv[0] == "GET" else [f"m{i}"], "typed": False, "rc": 200 if argv[0] == "GET" else 0, "out": out} for i, out in enumerate(outs)]
        # The second of the sixty is a window too, and whole: it says nothing of looking back, and none of these ways is tried on it.
        whole = there if argv[0] == "GET" else live_window
        for name, outs in (("live-1.json", [there, whole] + alike[2:]), ("frozen.json", [here, whole] + alike[2:]), ("live-2.json", [there, whole] + alike[2:])):
            asked_of = rows(outs)
            if argv[0] == "GET":  # and the third an instant from before the case, of the API: it has no series to change, and is answered after all
                asked_of[2] = {"argv": api2h, "typed": False, "rc": 200, "out": document(was) if name != "frozen.json" else document([], warnings=[of_an_instant("1h0m0s")])}
            with open(os.path.join(tmp, name), "w") as f:
                json.dump(asked_of, f)
        with contextlib.redirect_stdout(io.StringIO()) as out:
            got = pd.cmd_spoil(*(os.path.join(tmp, name) for name in ("live-1.json", "frozen.json", "live-2.json")), os.path.join(tmp, "freeze.json"))
        tried = dict(re.findall(r"^\| what looks back: ([^|]+) \| 0 \| (\d) \|$", out.getvalue(), re.M))
        if argv[0] == "GET":
            if got != 0 or "Unspoiled, 2 of the 60 look back" not in out.getvalue() or len(tried) != 6 or sorted(tried.values()) != ["1", "1", "1", "1", "1", "2"] or tried.get("said of a beginning a second later") != "2":
                failures.append(f"spoil of a window and an instant that look back, of {what}: returned {got} and printed {out.getvalue()!r}")
        elif got != 0 or "Unspoiled, 1 of the 60 look back" not in out.getvalue() or len(tried) != 6 or sorted(tried.values()) != ["0", "1", "1", "1", "1", "1"] or tried.get("answered with what the Prometheus has, where the case has nothing") != "0":
            failures.append(f"spoil of a window that looks back, of {what}: returned {got} and printed {out.getvalue()!r}")
        # What each of those ways makes of the answer, where it is not plain from its name: one point more, and that the Prometheus's.
        more = pd.of_what_looks_back(pd.a_point_more)(said(here), said(there), case)["out"]
        if more != (under(row("up{job=a}", steps[-8:]) + row("up{job=b}", steps[-3:]), of_the_window) if argv[0] != "GET" else document([one("a", steps[-8:]), one("b", steps[-3:])], warnings=[of_the_window])):
            failures.append(f"spoil of a window that looks back, of {what}: the Prometheus's point before its first put back is {more!r}")
        # And a comparison that held such an answer to less would be told so: a point more than the case can have, let by.
        was_held = pd.kept_rightly
        pd.kept_rightly = lambda had, kept, at, case: True
        try:
            with contextlib.redirect_stdout(io.StringIO()) as out:
                got = pd.cmd_spoil(*(os.path.join(tmp, name) for name in ("live-1.json", "frozen.json", "live-2.json")), os.path.join(tmp, "freeze.json"))
        finally:
            pd.kept_rightly = was_held
        if got != 1 or "| what looks back: the Prometheus's point before its first put back | 1 | 1 |" not in out.getvalue():
            failures.append(f"spoil of a window that looks back, of {what}, by a comparison that holds it to nothing: returned {got} and printed {out.getvalue()!r}")
    # What a fourth reader found let by, or failed, or left unheld (test/replay-diff/README.md).
    gone_only = row("up{job=gone}", steps[:5])
    two = [{"metric": {"__name__": "up", "job": "a"}, "value": [at(7200), "1"]}, {"metric": {"__name__": "up", "job": "b"}, "value": [at(7200), "1"]}]
    for what, argv, live, frozen, want in (
        # The warnings beside an answer are a list, and where the case says it begins is one of them.
        ("of the API, warnings that are no list but have the words for a key", api2h, said(document(was), 200), said(document([], warnings={of_an_instant("1h0m0s"): 1}), 200), "differs"),
        ("of the API, a window with warnings that are no list", api3h, said(document([one("a", steps)]), 200), said(document([one("a", steps[-7:])], warnings={of_the_window: 1}), 200), "differs"),
        ("of the API, warnings that are nothing", api2h, said(document(was), 200), said(document([], warnings=None), 200), "differs"),
        # A window of which the case has nothing to have: nothing, and said so, is right; as promq prints it too.
        ("a window the Prometheus has nothing in, as promq prints it", back3h, said("(empty result)\n"), said(under("(empty result)\n", of_the_window)), "edge"),
        ("a window the Prometheus has only a series in that ended before the case", back3h, said(gone_only), said(under("(empty result)\n", of_the_window)), "edge"),
        ("a window the case has to have a series of, answered with nothing", back3h, said(live_window), said(under("(empty result)\n", of_the_window)), "differs"),
        # An instant: nothing under the words, and nothing else — not nothing and then something, not nothing set in.
        ("an instant, answered with nothing and then with something", back2h, said("up{job=a} 1\n"), said("(empty result)\nup{job=a} 1\n(" + of_an_instant("1h0m0s") + ")\n"), "differs"),
        ("an instant, answered with nothing set in a space", back2h, said("up{job=a} 1\n"), said(under(" (empty result)\n", of_an_instant("1h0m0s"))), "differs"),
        # A Prometheus that refused the question answered nothing to hold a case to, whatever the case says in the same way.
        ("an instant the Prometheus refused, and the case answers under the same exit code", back2h, said("query failed: no\n", 1), said(looked, 1), "differs"),
        ("of the API, an instant the Prometheus refused", api2h, said("{}\n", 422), said(document([], warnings=[of_an_instant("1h0m0s")]), 422), "differs"),
        # A series kept with no point at all is no series kept, where the Prometheus has it only from before the case.
        ("of the API, a window with a series kept that has no points", api3h, said(document([one("a", steps), one("gone", steps[:5])]), 200), said(document([one("a", steps[-7:]), one("gone", [])], warnings=[of_the_window]), 200), "differs"),
        # And a Prometheus's answer this does not read — a series with no values — is nothing to hold a case to.
        ("of the API, a window of which the Prometheus has a series that is no list of values", api3h, said(document([one("a", steps), {"metric": {"__name__": "up", "job": "h"}, "histograms": []}]), 200), said(document([one("a", steps[-7:])], warnings=[of_the_window]), 200), "differs"),
    ):
        if (got := pd.verdict(argv, live, frozen, live, case)) != want:
            failures.append(f"what looks back, {what}: {got}, want {want}")
    # A Prometheus that answered alike twice is one that said the same with the same status; with another status it changed its answer.
    if (got := pd.verdict(back2h, said("up{job=a} 1\n"), said(looked), said("up{job=a} 1\n", 1), case)) != "moved":
        failures.append(f"what looks back, of a Prometheus that said the same twice and failed the second time: {got}, want moved")
    # And one of the three askings that got no answer is no answer here either.
    if (got := pd.verdict(back2h, said("(timed out after 60 seconds)\n", 124), said(looked), said("up{job=a} 1\n"), case)) != "unasked":
        failures.append(f"what looks back, of a Prometheus that was not reached the first time: {got}, want unasked")
    # Which question looks back is the whole of it, not how it begins.
    for argv, want in ((back2h, True), (["up offset 2h", "--range", "10m"], False), (api2h, True), (api2h + ["limit=1"], False), (["up offset 2h "], False)):
        if (pd.looking_back(argv) is not None) is not want:
            failures.append(f"{argv} is {'not ' if want else ''}taken for one of the questions that look back")
    # Where a case says it begins: the words themselves, from their beginning. And an instant is written to its second and its thousandths, not to the nearest second.
    for what, answer, want in (("the words after others", said(document([], warnings=["PromQL warning: " + of_the_window]), 200), []), ("of promq, a line that does not end", pd.unremarked(said("x 1\n(" + of_the_window + "\n")), "x 1\n(" + of_the_window + "\n")):
        if (got := pd.said_of_its_beginning(answer) if isinstance(answer, dict) else answer) != want:
            failures.append(f"where an answer says its case begins, {what}: {got!r}, want {want!r}")
    for seconds, want in ((45, "45s"), (60, "1m0s"), (1800, "30m0s"), (3600, "1h0m0s"), (5445, "1h30m45s")):  # how far, as the store writes it
        if (got := pd.go_duration(seconds)) != want:
            failures.append(f"{seconds} seconds, as Go writes a duration: {got}, want {want}")
    if (got := pd.stamp_of(1791464904924)) != time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(1791464904)) + ".924Z":
        failures.append(f"an instant 924 thousandths after a second, as a case writes it: {got}")
    # What each spoiling makes of an answer, where the table of what passes would not show it done otherwise.
    for what, got, want in (
        ("said of a beginning a second later", pd.another_beginning(said(looked), None, case)["out"], looked.replace("11:03:19.434Z", "11:03:20.434Z")),
        ("the words that it looked back put beside it", pd.said_to_look_back(said("up{job=a} 1\n"), None, case)["out"], under("up{job=a} 1\n", of_an_instant("1h0m0s"))),
        ("answered after all, of the API, with all the Prometheus has", pd.answered_after_all(said(document([], warnings=[of_an_instant("1h0m0s")]), 200), said(document(two), 200), case)["out"], document(two, warnings=[of_an_instant("1h0m0s")])),
        ("the last digit of its last value another", pd.of_the_api(pd.last_digit)(said(document(two), 200))["out"], document(two[:1] + [dict(two[1], value=[at(7200), "7"])])),
        # The point put back is of the series it is put back in, whichever comes first on either side: the Prometheus has one first that the case has not.
        ("a point more, where the Prometheus's first series is none of the case's", pd.of_what_looks_back(pd.a_point_more)(said(under(kept_window, of_the_window)), said(gone_only + live_window), case)["out"], under(row("up{job=a}", steps[-8:]) + row("up{job=b}", steps[-3:]), of_the_window)),
        ("a point more, where the case's first series has every point the Prometheus has of it", pd.of_what_looks_back(pd.a_point_more)(said(under(row("up{job=b}", steps[-3:]) + row("up{job=a}", steps[-7:]), of_the_window)), said(live_window), case)["out"], under(row("up{job=b}", steps[-3:]) + row("up{job=a}", steps[-8:]), of_the_window)),
        ("a point more, where the Prometheus has none", pd.of_what_looks_back(pd.a_point_more)(said(under(row("up{job=b}", steps[-3:]), of_the_window)), said(row("up{job=b}", steps[-3:])), case)["out"], under(row("up{job=b}", steps[-3:]), of_the_window)),
    ):
        if got != want:
            failures.append(f"an answer spoiled, {what}: {got!r}, want {want!r}")
    # No answer at all is not an answer, and three of them are not three that agree: a request nothing
    # came back for, a promq that timed out, one that could not reach its endpoint.
    for what, argv, each, want in (("a request with no answer", ["GET", "/api/v1/query", "query=up"], said("no answer: RemoteDisconnected", 0), "unasked"), ("a promq that timed out", ["up"], said("(timed out after 60 seconds)\n", 124), "unasked"),
                                   ("a promq that reached nothing", ["up"], said('query failed: Get "http://127.0.0.1:1/api/v1/query?query=up": dial tcp 127.0.0.1:1: connect: connection refused\n', 1), "unasked"),
                                   ("a promq that was refused", ["up{"], said("query failed: invalid parameter\n", 1), "same"), ("a request that was refused", ["GET", "/api/v1/query"], said("{}\n", 400), "same")):
        if (got := pd.verdict(argv, each, each, each)) != want:
            failures.append(f"three askings alike, each {what}: {got}, want {want}")
    # And one of the three is enough: an asking that got no answer is nothing to hold the other two to.
    for what, a, f, b, want in (("the first asking got no answer", said("(timed out after 60 seconds)\n", 124), said("up{} 1\n"), said("up{} 1\n"), "unasked"),
                                ("the second asking got no answer", said("up{} 1\n"), said("up{} 1\n"), said("(timed out after 60 seconds)\n", 124), "unasked"),
                                ("the frozen store got no answer", said("up{} 1\n"), said("(timed out after 60 seconds)\n", 124), said("up{} 1\n"), "unasked")):
        if (got := pd.verdict(["up"], a, f, b)) != want:
            failures.append(f"three askings, {what}: {got}, want {want}")
    # What looks back and is refused by both in other words is a difference, and no wording: nothing of it is let by.
    if (got := pd.verdict(back2h, said("query failed: x\n", 1), said("query failed: y\n", 1), said("query failed: x\n", 1), case)) != "differs":
        failures.append(f"what looks back, refused by both in other words: {got}, want differs")
    # What a case's freeze.json says of it, as this reads it: its freeze to the thousandth, where its metrics
    # begin, what an instant looks back there, and whether that is where the sweep's freeze has to have put it.
    for what, written, want in (
        ("a freeze whose thousandths a float holds a little short of", {"freeze_time": 1791464904.001, "metrics": {"from_ms": 1791464904001 - 3_900_000}}, {"freeze_ms": 1791464904001, "from_ms": 1791464904001 - 3_900_000, "lookback_ms": 300_000, "askew": False}),
        ("a Prometheus that looks back two minutes", {"freeze_time": 1791464904.5, "metrics": {"from_ms": 1791464904500 - 3_720_000, "lookback_delta_ms": 120_000}}, {"freeze_ms": 1791464904500, "from_ms": 1791464904500 - 3_720_000, "lookback_ms": 120_000, "askew": False}),
        ("a case that begins a second sooner than it should", {"freeze_time": 1791464904.5, "metrics": {"from_ms": 1791464904500 - 3_901_000}}, {"freeze_ms": 1791464904500, "from_ms": 1791464904500 - 3_901_000, "lookback_ms": 300_000, "askew": True}),
        ("a case that begins a second later than it should", {"freeze_time": 1791464904.5, "metrics": {"from_ms": 1791464904500 - 3_899_000}}, {"freeze_ms": 1791464904500, "from_ms": 1791464904500 - 3_899_000, "lookback_ms": 300_000, "askew": True}),
        ("a case with no metrics", {"freeze_time": 1791464904.5}, {"freeze_ms": 1791464904500, "from_ms": None, "lookback_ms": 300_000, "askew": True}),
        ("a case that says its metrics are none", {"freeze_time": 1791464904.5, "metrics": None}, {"freeze_ms": 1791464904500, "from_ms": None, "lookback_ms": 300_000, "askew": True})):
        with open(os.path.join(tmp, "freeze.json"), "w") as f:
            json.dump(written, f)
        if (got := pd.case_of(os.path.join(tmp, "freeze.json"))) != want:
            failures.append(f"what is read of {what}: {got}, want {want}")
    if (got := pd.stamp_of(1767225600004)) != "2026-01-01T00:00:00.004Z":
        failures.append(f"an instant four thousandths after a second, as a case writes it: {got}")
    # Where an answer says its case begins is among its warnings, or in a whole line under what promq
    # printed, and in the words of a beginning: not among the infos, not half a line, not what it says of its end.
    for what, answer, want in (("among the warnings", said(document([], warnings=[of_the_window]), 200), [of_the_window]), ("among the infos", said(document([], infos=[of_the_window]), 200), []),
                               ("what it says of its end", said(document([], warnings=["frozen case: it ends at …"]), 200), []), ("under what promq printed", said(under("(empty result)\n", of_the_window)), [of_the_window]),
                               ("a line that does not end", said("(empty result)\n(" + of_the_window + "\n"), []), ("of promq, what it says of its end", said("m{} 1\n(frozen case: it ends at …)\n"), [])):
        if (got := pd.said_of_its_beginning(answer)) != want:
            failures.append(f"where an answer says its case begins, {what}: {got}, want {want}")
    # And the spoiling that answers after all puts the Prometheus's own answer over the same words.
    for what, there, here, want in (("of promq", "up{job=a} 1\n", looked, under("up{job=a} 1\n", of_an_instant("1h0m0s"))), ("of the API", document(was), document([], warnings=[of_an_instant("1h0m0s")]), document(was, warnings=[of_an_instant("1h0m0s")])),
                                    ("of promq, where the case has something", live_window, under(kept_window, of_the_window), under(kept_window, of_the_window)), ("of promq, where nothing is said of looking back", "up{job=a} 1\n", "(empty result)\n", "(empty result)\n")):
        if (got := pd.answered_after_all(said(here), said(there), case)["out"]) != want:
            failures.append(f"an answer that looks back, answered after all with what the Prometheus has, {what}: {got!r}, want {want!r}")
    # The spoiling that puts those words beside an answer puts them where a store would: among the warnings.
    for what, answer in (("of the API", said(document(was), 200)), ("of promq", said("up{job=a} 1\n"))):
        if len(got := pd.said_of_its_beginning(pd.said_to_look_back(answer, None, case))) != 1 or pd.stamp_of(case["from_ms"]) not in got[0]:
            failures.append(f"the words that it looked back, put beside an answer {what}: it then says {got}")
    # Spoiled through the command, sixty answers of the API: each way that is of the API is tried on every
    # answer that has a result, and on none that has not; a status of 200 is changed like any other; and
    # a refusal spoiled in its words is worded otherwise, which fails a comparison and is not let by.
    series = lambda i: document([{"metric": {"__name__": f"m{i}"}, "value": [at(0), str(i)]}, {"metric": {"__name__": f"m{i}", "x": "y"}, "value": [at(0), "2"]}])
    api_alike = [document([])] + [series(i) for i in range(1, 59)] + [json.dumps({"status": "error", "errorType": "bad_data", "error": "no"}, sort_keys=True, indent=1) + "\n"]
    for name in ("live-1.json", "frozen.json", "live-2.json"):
        with open(os.path.join(tmp, name), "w") as f:
            json.dump([{"argv": ["GET", "/api/v1/query", f"query=m{i}", "time=<freeze>"], "typed": False, "rc": 400 if i == 59 else 200, "out": out} for i, out in enumerate(api_alike)], f)
    with contextlib.redirect_stdout(io.StringIO()) as out:
        got = pd.cmd_spoil(*(os.path.join(tmp, name) for name in ("live-1.json", "frozen.json", "live-2.json")))
    for row in ("| its series the other way round, of the API | 0 | 58 |", "| its last series taken out, of the API | 0 | 58 |", "| the last digit of its last value another, of the API | 0 | 58 |",
                "| answered with another status | 0 | 60 |", "| the words that it looked back put beside it | 0 | 60 |", "| the last digit of its last line another | 0 | 60 |", "Unspoiled, 0 of the 60 look back"):
        if got != 0 or row not in out.getvalue():
            failures.append(f"spoil of sixty answers of the API: returned {got}, and did not print {row!r} in {out.getvalue()!r}")
    paths = [asked("live-1.json", alike), asked("frozen.json", alike), asked("live-2.json", alike), asked("as-typed.json", alike)]
    # A question that got no answer is not one a line of the known file can excuse.
    with open(os.path.join(tmp, "known-unasked.txt"), "w") as f:
        f.write("^promq m7$\t124 \\(timed out after 60 seconds\\)\\n\t124 \\(timed out after 60 seconds\\)\\n\tfor the test\n")
    for name in ("live-1.json", "frozen.json", "live-2.json", "as-typed.json"):
        answers = json.load(open(os.path.join(tmp, name)))
        answers[7].update(rc=124, out="(timed out after 60 seconds)\n")
        with open(os.path.join(tmp, name), "w") as f:
            json.dump(answers, f)
    with contextlib.redirect_stdout(io.StringIO()) as out:
        got = pd.cmd_compare(*paths, os.path.join(tmp, "known-unasked.txt"))
    if got != 1 or "(known: for the test)" in out.getvalue() or "Differing and not excused: 1." not in out.getvalue():
        failures.append(f"promq compare, one query that timed out three times alike and a line of the known file that names it: returned {got}, and printed {out.getvalue()[:400]!r}")
    # And of requests: one nothing came back for is not one the Prometheus answered, with something or with nothing.
    for name in ("live-1.json", "frozen.json", "live-2.json"):
        with open(os.path.join(tmp, "api-" + name), "w") as f:
            json.dump([{"argv": ["GET", "/api/v1/query", f"query=m{i}", "time=<freeze>"], "typed": False, "rc": 0 if i == 7 else 400 if i == 59 else 200, "out": "no answer: RemoteDisconnected" if i == 7 else out} for i, out in enumerate(api_alike)], f)
    with contextlib.redirect_stdout(io.StringIO()) as out:
        got = pd.cmd_compare(*(os.path.join(tmp, "api-" + name) for name in ("live-1.json", "frozen.json", "live-2.json")), "-")
    if got != 1 or "the Prometheus answered 57 with something, 1 with an empty result, and refused 2." not in out.getvalue():
        failures.append(f"compare of sixty requests, one with no answer and one refused: returned {got}, and printed {out.getvalue()[:500]!r}")
    for name in ("live-1.json", "frozen.json", "live-2.json", "as-typed.json"):
        answers = json.load(open(os.path.join(tmp, name)))
        answers[7].update(rc=124, out="(timed out after 60 seconds)\n")
        with open(os.path.join(tmp, name), "w") as f:
            json.dump(answers, f)
    with contextlib.redirect_stdout(io.StringIO()) as out:
        got = pd.cmd_compare(*paths)
    if got != 1 or "got no answer at all from one side or the other: 1 |" not in out.getvalue() or "Differing and not excused: 1." not in out.getvalue():
        failures.append(f"promq compare, one query that timed out three times alike: returned {got}, and printed {out.getvalue()[:400]!r}")

    # The four askings have to be of the same queries, all of them: one short is not zipped away.
    paths = [asked("live-1.json", alike), asked("frozen.json", alike[:59]), asked("live-2.json", alike), asked("as-typed.json", alike[:59])]
    with contextlib.redirect_stdout(io.StringIO()) as out:
        got = pd.cmd_compare(*paths)
    if got != 1 or "not of the same" not in out.getvalue():
        failures.append(f"promq compare, one asking a query short: returned {got}, and printed {out.getvalue()[:120]!r}")
    # Refused by both in other words fails, as differing does; and its excuse is held to the words too.
    def refused(name, words):
        path = os.path.join(tmp, name)
        with open(path, "w") as f:
            json.dump([{"argv": [f"m{i}"], "typed": False, "rc": 0 if i else 1, "out": alike[i] if i else words} for i in range(60)], f)
        return path
    with open(os.path.join(tmp, "known-words.txt"), "w") as f:
        f.write("^promq m0$\t1 query failed: so\\n\t1 query failed: in a newer way\\n\tfor the test\n")
    with open(os.path.join(tmp, "known-other.txt"), "w") as f:
        f.write("^promq m1$\t1 query failed: so\\n\t1 query failed: in a newer way\\n\tfor the test\n")
    with open(os.path.join(tmp, "known-there.txt"), "w") as f:
        f.write("^promq m0$\t1 query failed: as it was\\n\t1 query failed: in a newer way\\n\tfor the test\n")
    with open(os.path.join(tmp, "known-status.txt"), "w") as f:  # the words, and another exit code than both gave
        f.write("^promq m0$\t2 query failed: so\\n\t2 query failed: in a newer way\\n\tfor the test\n")
    for what, frozen_says, known_path, want in (("in other words", "query failed: otherwise\n", None, 1), ("in the words that are known", "query failed: in a newer way\n", os.path.join(tmp, "known-words.txt"), 0),
                                                ("in yet other words than the known ones", "query failed: otherwise\n", os.path.join(tmp, "known-words.txt"), 1),
                                                ("in the words that are known of another query", "query failed: in a newer way\n", os.path.join(tmp, "known-other.txt"), 1),
                                                ("in the words that are known, to a Prometheus that said something else", "query failed: in a newer way\n", os.path.join(tmp, "known-there.txt"), 1),
                                                ("in the words that are known, with a status that is not", "query failed: in a newer way\n", os.path.join(tmp, "known-status.txt"), 1)):
        paths = [refused("live-1.json", "query failed: so\n"), refused("frozen.json", frozen_says), refused("live-2.json", "query failed: so\n"), refused("as-typed.json", frozen_says)]
        with contextlib.redirect_stdout(io.StringIO()) as out:
            got = pd.cmd_compare(*paths, *([known_path] if known_path else []))
        if got != want or "worded differently: 1" not in out.getvalue():
            failures.append(f"promq compare, refused by both {what}: returned {got}, want {want}, and printed {out.getvalue()[:200]!r}")
    # spoil counts what still passes, and a comparison that let a spoiled answer by would fail it.
    paths = [asked("live-1.json", alike), asked("frozen.json", alike), asked("live-2.json", alike)]
    with contextlib.redirect_stdout(io.StringIO()) as out:
        got = pd.cmd_spoil(*paths)
    if got != 0 or out.getvalue().count("| 0 | 60 |") != 7:  # five that change what promq printed, another status, and the words that it looked back
        failures.append(f"spoil of sixty answers that are alike: returned {got} and printed {out.getvalue()!r}")
    for passing in ("same", "order", "moved", "edge"):
        lenient, pd.verdict = pd.verdict, lambda argv, a, f, b, case=None: passing
        with contextlib.redirect_stdout(io.StringIO()):
            got = pd.cmd_spoil(*paths)
        pd.verdict = lenient
        if got != 1:
            failures.append(f"spoil passed a comparison that calls everything {passing}")
    # And of the API it turns the series round, which is the one spoiling a store could send: let by where the order is no one's, and nowhere else.
    api_answer = lambda query, series: {"argv": ["GET", "/api/v1/query", query, "time=<freeze>"], "typed": False, "rc": 200, "out": sent(*series)["out"]}
    sixty = [api_answer(f"query=m{i}", [("a", "1"), ("b", "2")]) for i in range(59)]
    for what, last, want, shows in (("a selector's", api_answer("query=x", [("a", "1"), ("b", "2")]), 0, "| its series the other way round, of the API | 0 | 60 |"),
                                    ("what counts'", api_answer(counted, [("a", "1"), ("b", "2")]), 1, "| its series the other way round, of the API | 1 | 60 |")):
        for name in ("live-1.json", "frozen.json", "live-2.json"):
            with open(os.path.join(tmp, name), "w") as f:
                json.dump(sixty + [last], f)
        with contextlib.redirect_stdout(io.StringIO()) as out:
            got = pd.cmd_spoil(os.path.join(tmp, "live-1.json"), os.path.join(tmp, "frozen.json"), os.path.join(tmp, "live-2.json"))
        if got != want or shows not in out.getvalue():
            failures.append(f"spoil, {what} series the other way round: returned {got}, want {want}, and printed {out.getvalue()!r}")
    # capture asks promq itself, with the instant as promq's own flag, and gives up if most of it fails.
    with open(os.path.join(tmp, "promq"), "w") as f:
        f.write('#!/bin/sh\necho "$PROM_URL $*"\ncase "$1" in fails*) exit 1;; esac\n')
    os.chmod(os.path.join(tmp, "promq"), 0o755)
    with open(os.path.join(tmp, "queries.json"), "w") as f:
        json.dump([{"argv": ["up"], "typed": True}, {"argv": ["x", "--range", "5m"], "typed": False}, {"argv": ["fails"], "typed": False}], f)
    for extra, want in (([], ["http://there up\n", "http://there x --range 5m\n", "http://there fails\n"]), (["--at", "1791428838.5"], ["http://there up --at 1791428838.5\n", "http://there x --range 5m --at 1791428838.5\n", "http://there fails --at 1791428838.5\n"])):
        with contextlib.redirect_stdout(io.StringIO()):
            pd.cmd_capture(os.path.join(tmp, "queries.json"), tmp, "http://there", os.path.join(tmp, "captured.json"), extra)
        got = json.load(open(os.path.join(tmp, "captured.json")))
        if [a["out"] for a in got] != want or [a["rc"] for a in got] != [0, 0, 1] or [a["typed"] for a in got] != [True, False, False]:
            failures.append(f"what capture asked with {extra}: {got}")
    with open(os.path.join(tmp, "queries.json"), "w") as f:
        json.dump([{"argv": ["fails-1"], "typed": False}, {"argv": ["fails-2"], "typed": False}, {"argv": ["up"], "typed": False}], f)
    try:
        with contextlib.redirect_stdout(io.StringIO()):
            pd.cmd_capture(os.path.join(tmp, "queries.json"), tmp, "http://there", os.path.join(tmp, "captured.json"), [])
        failures.append("capture went on with two of three queries failing")
    except SystemExit:
        pass
    # And what an agent with a Prometheus tool of its own asked, which is not a promq in a shell.
    d = tempfile.mkdtemp(dir=tmp)
    call = lambda tool, **asked: {"tool": tool, "input": "Prometheus: something\n" + json.dumps(asked, indent=20)}
    with open(os.path.join(d, "frozen-holmes-1.json"), "w") as f:
        json.dump({"transcript": {"steps": [
            call("execute_prometheus_instant_query", query="sum(up)", timeout=60), call("execute_prometheus_range_query", query="rate(x[1m])", start="2026-10-07T09:24:00Z", end="2026-10-07T09:28:00Z", step=30),
            # As a time is written by more than one hand: with an offset, with a fraction; and a window of one instant.
            call("execute_prometheus_range_query", query="y", start="2026-10-07T08:55:20+00:00", end="2026-10-07T09:55:20+00:00", step=60),
            call("execute_prometheus_range_query", query="z", start="2026-10-07T09:22:45.299958+00:00", end="2026-10-07T09:52:45.299958+00:00"),
            call("execute_prometheus_range_query", query="w", start="2026-10-07T09:24:00Z", end="2026-10-07T09:24:00Z", step=15),
            call("execute_prometheus_range_query", query="x", start="2026-10-07T09:28:00Z", end="2026-10-07T09:24:00Z", step=15), call("execute_prometheus_range_query", query="x", start=None, end=None, step=None),
            call("get_metric_names", match='{__name__=~"thumb.*"}', start=None, end=None), call("get_label_values", label="client", match="x"), call("get_series", match="x"), call("get_series", match=None),
            call("get_metric_metadata", metric="x"), {"tool": "fetch_pod_logs", "input": "Kubernetes: logs\n{}"}, {"tool": "bash", "input": "promq 'up'"},
            {"tool": "get_series", "input": "Prometheus: with nothing after it"}]}}, f)
    want_queries = [["sum(up)"], ["rate(x[1m])", "--range", "240s", "--step", "30s"], ["y", "--range", "3600s", "--step", "60s"], ["z", "--range", "1800s", "--step", "15s"], ["w", "--range", "0s", "--step", "15s"]]
    want_requests = [["/api/v1/label/__name__/values", 'match[]={__name__=~"thumb.*"}', "start=<freeze>-3600", "end=<freeze>"], ["/api/v1/label/client/values", "match[]=x", "start=<freeze>-3600", "end=<freeze>"],
                     ["/api/v1/series", "match[]=x", "start=<freeze>-3600", "end=<freeze>"], ["/api/v1/metadata", "metric=x"]]
    left = []
    if (got := pd.tool_calls([d], left)) != (want_queries, want_requests) or len(left) != 4:  # a window that ends before it begins, one with no times, a search for no series, and a call that is not one
        failures.append(f"what an agent's own Prometheus tool asked: {got}, and what was left: {left}")

# The API, as a client that does not go through promq reads it: the request promq makes for a query,
# with the freeze named where a time goes, and an answer kept as it was sent.
for text, want in (("30m", 1800), ("15s", 15), ("1h30m", 5400), ("500ms", 0.5), ("2d", 172800), ("soon", None), ("", None), ("5", None), ("5m ", None)):
    if (got := pd.seconds(text)) != want:
        failures.append(f"the duration {text!r} is {got} seconds, want {want}")
REQUESTS = [  # promq's arguments, the request
    (["up"], ["/api/v1/query", "query=up", "time=<freeze>"]),
    (["rate(x[1m])", "--range", "10m", "--step", "30s"], ["/api/v1/query_range", "query=rate(x[1m])", "start=<freeze>-600", "end=<freeze>", "step=30s"]),
    (["x", "--range", "1h"], ["/api/v1/query_range", "query=x", "start=<freeze>-3600", "end=<freeze>", "step=15s"]),
    (["x", "--range", "soon"], None),
    (["up", "--range", "30d", "--step", "1s"], ["/api/v1/query_range", "query=up", "start=<freeze>-2592000", "end=<freeze>", "step=1s"]),
    (["up", "--range", "1500ms"], ["/api/v1/query_range", "query=up", "start=<freeze>-1.5", "end=<freeze>", "step=15s"]),
]
for argv, want in REQUESTS:
    if (got := pd.request_of(argv)) != want:
        failures.append(f"the request for promq {argv}: {got}, want {want}")
if (got := pd.shown(["GET", "/api/v1/query", "query=up offset", "time=<freeze>"])) != "GET /api/v1/query?query=up offset&time=<freeze>" or pd.shown(["GET", "/api/v1/labels"]) != "GET /api/v1/labels":
    failures.append(f"a request is shown as {got!r}")
import http.server, threading, urllib.parse
class Echo(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"  # a connection is kept, as a Prometheus keeps one
    came_from = set()
    def do_GET(self):
        url = urllib.parse.urlsplit(self.path)
        Echo.came_from.add(self.client_address)
        code = 400 if url.path.endswith("/refused") else 200
        if url.path.endswith("/gone"):  # no answer: the connection is closed where one should be
            self.close_connection = True
            return
        if url.path.endswith("/garbled"):  # something that is not HTTP at all
            self.wfile.write(b"garbled\r\n\r\n")
            self.close_connection = True
            return
        if url.path.endswith("/once"):  # an answer, and then the connection closed without a word that it would be
            self.close_connection = True
        # Two series in an order no sorting would leave them in, a number written two ways, and a value too long to be cut.
        data = {"result": [{"value": [5.000, "+Inf"], "metric": {}}, {"value": [5, "1e-07"], "metric": {"b": "2", "a": "1"}}], "long": "x" * 20000 if url.path.endswith("/long") else ""}
        if url.path == "/api/v1/metadata":  # what is said of one metric, in the order a map gave it
            data = {"m": [{"type": "gauge", "help": "b", "unit": ""}, {"type": "counter", "help": "z", "unit": ""}, {"type": "gauge", "help": "a", "unit": ""}], "n": [{"type": "gauge", "help": "", "unit": ""}]}
        body = b"not JSON at all" if url.path.endswith("/text") else json.dumps({"status": "success" if code == 200 else "error", "asked": urllib.parse.parse_qsl(url.query),
                                                                               "infos": ["b", "a"], "warnings": ["z", "y"], "data": data}).encode()
        self.send_response(code); self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def log_message(self, *args):
        pass
server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Echo)
threading.Thread(target=server.serve_forever, daemon=True).start()
with tempfile.TemporaryDirectory() as tmp:
    asked = [{"argv": ["GET", "/api/v1/query", "query=sum by (a) (x{b=\"c d\"})", "time=<freeze>"], "typed": True}, {"argv": ["GET", "/api/v1/query_range", "start=<freeze>-90", "end=<freeze>", "step=15s"], "typed": False},
             {"argv": ["GET", "/refused", "match[]=up", "match[]=x"], "typed": False}, {"argv": ["GET", "/text"], "typed": False},
             # The whole second the freeze is in, and a time before it: an instant whose thousandths end in nothing, whatever the freeze's are.
             {"argv": ["GET", "/api/v1/query_range", "start=<second>-60.5", "end=<second>-0.5", "step=<second>"], "typed": False}]
    # No answer at all is kept as none, and when most of what is asked gets none the asking gives up.
    with open(os.path.join(tmp, "one.json"), "w") as f:
        json.dump(asked[:1], f)
    try:
        with contextlib.redirect_stdout(io.StringIO()):
            pd.cmd_fetch(os.path.join(tmp, "one.json"), "http://127.0.0.1:1/", "1791428838.5", os.path.join(tmp, "none.json"))
        failures.append("fetch went on with nothing answering")
    except SystemExit:
        nothing = json.load(open(os.path.join(tmp, "none.json")))[0]
        if nothing["rc"] != 0 or not nothing["out"].startswith("no answer"):
            failures.append(f"no answer was kept as {nothing}")
    with open(os.path.join(tmp, "requests.json"), "w") as f:
        json.dump(asked, f)
    with contextlib.redirect_stdout(io.StringIO()):
        pd.cmd_fetch(os.path.join(tmp, "requests.json"), f"http://127.0.0.1:{server.server_address[1]}/", "1791428838.5", os.path.join(tmp, "out.json"))
    got = json.load(open(os.path.join(tmp, "out.json")))
    asked_for = [json.loads(a["out"]).get("asked") if a["out"].startswith("{") else a["out"] for a in got]
    want = [[["query", 'sum by (a) (x{b="c d"})'], ["time", "1791428838.500"]], [["start", "1791428748.500"], ["end", "1791428838.500"], ["step", "15s"]], [["match[]", "up"], ["match[]", "x"]], "not JSON at all",
            [["start", "1791428777.500"], ["end", "1791428837.500"], ["step", "1791428838.000"]]]
    if asked_for != want or [a["rc"] for a in got] != [200, 200, 400, 200, 200] or [a["typed"] for a in got] != [True, False, False, False, False]:
        failures.append(f"what was fetched: {asked_for} {[a['rc'] for a in got]}, want {want}")
    # Kept as sent: the series in their order, a value as the string it was, a number as it was written — 5 is not 5.0, which is how
    # a time written without its thousandths is told from one with them — and the engine's remarks; an object's keys in one order,
    # and the remarks in one, which is a map's.
    kept = json.loads(got[0]["out"])
    if [r["value"] for r in kept["data"]["result"]] != [["#5.0", "+Inf"], ["#5", "1e-07"]] or kept["infos"] != ["a", "b"] or kept["warnings"] != ["y", "z"] or '"asked"' not in got[0]["out"].split("\n")[1]:
        failures.append(f"an answer was not kept as it was sent: {got[0]['out']!r}")
    # Whole, however long. And a connection that was closed after its last answer is found closed by
    # asking on it, and asked again on another; one that never answers is no answer, and the asking gives
    # up only when most of what it asks gets none.
    def fetched(paths):
        with open(os.path.join(tmp, "some.json"), "w") as f:
            json.dump([{"argv": ["GET", path], "typed": False} for path in paths], f)
        with contextlib.redirect_stdout(io.StringIO()):
            pd.cmd_fetch(os.path.join(tmp, "some.json"), f"http://127.0.0.1:{server.server_address[1]}", "1791428838.5", os.path.join(tmp, "some-out.json"))
        return json.load(open(os.path.join(tmp, "some-out.json")))
    if len(json.loads(fetched(["/long"])[0]["out"])["data"]["long"]) != 20000:
        failures.append("a long answer was not kept whole")
    if [a["rc"] for a in fetched(["/once"] * 40)] != [200] * 40:
        failures.append("an answer was lost to a connection the other end had closed")
    some = fetched(["/a", "/b", "/gone"])
    if [a["rc"] for a in some] != [200, 200, 0] or not some[2]["out"].startswith("no answer: "):
        failures.append(f"one request of three that got no answer was kept as {some[2]}")
    some = fetched(["/a", "/b", "/garbled"])
    if [a["rc"] for a in some] != [200, 200, 0] or not some[2]["out"].startswith("no answer: "):
        failures.append(f"one request of three that was answered with what is not HTTP was kept as {some[2]}")
    try:
        fetched(["/a", "/gone", "/gone"])
        failures.append("fetch went on with two requests of three unanswered")
    except SystemExit:
        pass
    # What a Prometheus says of one metric comes in the order of a map, and is kept in one order; and many
    # requests go over the few connections the workers keep, not over one each — which ran a machine out
    # of ports to connect from, and a third of a sweep's requests were never made.
    Echo.came_from.clear()
    with open(os.path.join(tmp, "many.json"), "w") as f:
        json.dump([{"argv": ["GET", "/api/v1/metadata", f"metric=m{i}"], "typed": False} for i in range(120)], f)
    with contextlib.redirect_stdout(io.StringIO()):
        pd.cmd_fetch(os.path.join(tmp, "many.json"), f"http://127.0.0.1:{server.server_address[1]}", "1791428838.5", os.path.join(tmp, "many-out.json"))
    many = json.load(open(os.path.join(tmp, "many-out.json")))
    helps = [entry["help"] for entry in json.loads(many[7]["out"])["data"]["m"]]
    if helps != ["a", "b", "z"] or [a["rc"] for a in many] != [200] * 120 or not 1 <= len(Echo.came_from) <= 8:
        failures.append(f"120 requests about metadata: one metric's entries in the order {helps}, over {len(Echo.came_from)} connections")
server.shutdown()

# What is asked, of a stand-in for a Prometheus that has two metrics, one of them a histogram's: the
# fixed set about each, the ones about none, what recorded agents asked — marked as theirs whoever
# else asks it — and the requests behind them all; and, through the command itself, what it returns.
class StandIn(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        url = urllib.parse.urlsplit(self.path)
        name = dict(urllib.parse.parse_qsl(url.query)).get("match[]", "up")
        there = {"/api/v1/label/__name__/values": ["lat_bucket", "up"], "/api/v1/labels": ["__name__", "job", "le"],
                 "/api/v1/series": [{"__name__": name, "job": "a"}, {"__name__": name, "job": "b"}],
                 "/api/v1/metadata": {"lat": [{"type": "histogram", "help": "", "unit": ""}], "up": [{"type": "gauge", "help": "", "unit": ""}]}}
        body = json.dumps({"status": "success", "data": there[url.path]} if url.path in there else {"status": "error"}).encode()
        self.send_response(200); self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def log_message(self, *args):
        pass
server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), StandIn)
threading.Thread(target=server.serve_forever, daemon=True).start()
with tempfile.TemporaryDirectory() as tmp:
    runs = os.path.join(tmp, "runs"); os.mkdir(runs)
    with open(os.path.join(runs, "frozen-agent-1.json"), "w") as f:
        json.dump({"transcript": {"steps": [{"tool": "bash", "input": "promq 'up'; promq 'sum(up) by (job)' --range 10m --step 30s; promq \"x{a=\\\"$V\\\"}\""},
                                           {"tool": "get_series", "input": "Prometheus: Get Series\n" + json.dumps({"match": "up"})},
                                           {"tool": "get_label_values", "input": "Prometheus: broken\n{"}]}}, f)
    queries_path, requests_path = os.path.join(tmp, "queries.json"), os.path.join(tmp, "requests.json")
    with contextlib.redirect_stdout(io.StringIO()) as out:
        more = os.path.join(tmp, "more"); os.mkdir(more)  # a second round's runs, and a third's that has none
        with open(os.path.join(more, "frozen-agent-2.json"), "w") as f:
            json.dump({"transcript": {"steps": [{"tool": "execute_prometheus_instant_query", "input": "Prometheus: Query\n" + json.dumps({"query": "sum(lat_bucket)"})},
                                               {"tool": "execute_prometheus_range_query", "input": "Prometheus: Query\n" + json.dumps({"query": "up", "start": "2026-10-07T09:00:00Z", "end": "2026-10-07T09:02:00Z", "step": 20})}]}}, f)
        pd.cmd_queries(f"http://127.0.0.1:{server.server_address[1]}", queries_path, requests_path, ["--old-snapshot", "x", "--runs", runs, "--runs", more, "--runs", os.path.join(tmp, "none")])
    queries, requests = json.load(open(queries_path)), json.load(open(requests_path))
    asked = {tuple(q["argv"]): q["typed"] for q in queries}
    sent_to = {tuple(r["argv"][1:]): r["typed"] for r in requests}
    for argv, typed in ((("up",), True), (("sum(up) by (job)", "--range", "10m", "--step", "30s"), True), (("sum(lat_bucket)",), True), (("up", "--range", "120s", "--step", "20s"), True), (("lat_bucket",), False), (("rate(lat_bucket[1m])",), False), (("sum by (job) (up)",), False),
                        (('up{job="a"}',), False), (("histogram_quantile(0.9, sum by (le) (rate(lat_bucket[5m])))",), False), (("up", "--range", "1h"), False), (("sum(",), False), (("topk(3, lat_bucket)",), False),
                        (("histogram_fraction(0, 0.1, rate(lat_bucket[5m]))",), False), (("stddev_over_time(up[5m])",), False), (("first_over_time(lat_bucket[5m])",), False),
                        # One of each kind of question: a metric's samples and what an instant picks, what is worked out of it, each of its windows, each label; what is about no metric; what fails; and what looks back on purpose.
                        *[((q,), False) for q in ("up[30s]", "timestamp(up)", "count(up)", "up offset 20m", "irate(up[1m])", "increase(up[5m])", "delta(up[2m])", "max_over_time(up[5m])", "avg_over_time(up[10m])", "changes(up[5m])", "resets(up[5m])",
                                                  "quantile_over_time(0.9, up[5m])", "bottomk(2, up)", "sort_desc(up)", "max_over_time(rate(up[1m])[5m:30s])", "max_over_time(up[2m:])", "up[1m:20s]", "round(up, 10)", "deriv(up[5m])", "predict_linear(up[5m], 600)",
                                                  "stdvar_over_time(up[5m])", "idelta(up[1m])", "count without (job) (up)", 'up{job!="a"}', 'up{job=~".+"}', "histogram_quantile(0.5, rate(lat_bucket[1m]))", "histogram_quantile(1.5, rate(lat_bucket[5m]))",
                                                  "histogram_count(rate(lat_bucket[5m]))", '{__name__=~".+"}', "absent(no_such_metric)", "vector(1)", "time()", "up @ end()", "up offset -1m", "rate(up)", "up[5m", "up{job=}", "no_such_function(up)", "sum by (job) (up) by (job)",
                                                  "up offset", "1 +", "histogram_quantile(up)", "up offset 2h", "up offset 90m")],
                        (("up", "--range", "5m"), False), (("up", "--range", "2m", "--step", "5s"), False), (("rate(up[1m])", "--range", "10m", "--step", "30s"), False), (("sum(up)", "--range", "30m", "--step", "1m"), False),
                        (("increase(up[5m])", "--range", "1h", "--step", "5m"), False), (("sum by (job) (rate(up[2m]))", "--range", "5m", "--step", "1m"), False), (("up", "--range", "soon"), False), (("up", "--range", "5m", "--step", "0s"), False),
                        (("up", "--range", "30d", "--step", "1s"), False), (("up", "--range", "3h", "--step", "10m"), False), (("histogram_fraction(0.05, 2, rate(lat_bucket[5m]))", "--range", "10m", "--step", "1m"), False)):
        if asked.get(argv) is not typed:
            failures.append(f"promq {list(argv)} is {'not asked' if argv not in asked else 'asked and marked ' + str(asked[argv])}; want it asked, an agent's: {typed}")
    if ("histogram_quantile(0.9, sum by (le) (rate(up[5m])))",) in asked or ("histogram_fraction(0, 0.1, rate(up[5m]))",) in asked or len(asked) != len(queries) or len(sent_to) != len(requests):
        failures.append("what is asked has a histogram's question about what is none, or the same thing twice")
    whole_window = ("start=<freeze>-3600", "end=<freeze>")
    for request, typed in ((("/api/v1/query", "query=up", "time=<freeze>"), True), (("/api/v1/query_range", "query=sum(up) by (job)", "start=<freeze>-600", "end=<freeze>", "step=30s"), True),
                           (("/api/v1/series", "match[]=up", "start=<freeze>-3600", "end=<freeze>"), True), (("/api/v1/series", "match[]=lat_bucket", "start=<freeze>-3600", "end=<freeze>"), False),
                           (("/api/v1/label/le/values", "start=<freeze>-3600", "end=<freeze>"), False), (("/api/v1/labels",), False), (("/api/v1/query", "query=up", "time=<freeze>", "limit=many"), False),
                           # What kind of metric each is: asked of every name the Prometheus has series of, and of every family it describes.
                           (("/api/v1/metadata",), False), (("/api/v1/metadata", "metric=lat_bucket"), False), (("/api/v1/metadata", "metric=lat"), False), (("/api/v1/metadata", "limit=7"), False),
                           (("/api/v1/metadata", "limit_per_metric=few"), False),
                           # What a client asks wrongly, and what changes an answer besides the question; a path there is none of.
                           (("/api/v1/no_such_endpoint",), False), (("/api/v1/series",) + whole_window, False), (("/api/v1/series", "match[]=up{") + whole_window, False), (("/api/v1/labels", "end=later"), False),
                           (("/api/v1/query", "query=", "time=<freeze>"), False), (("/api/v1/query_range", "query=up", "start=<freeze>", "end=<freeze>-60", "step=15"), False),
                           (("/api/v1/query", 'query={__name__=~".+"}', "time=<freeze>", "limit=2"), False), (("/api/v1/query", "query=up", "time=<freeze>", "lookback_delta=1ms"), False),
                           (("/api/v1/query", "query=up", "time=<freeze>", "timeout=soon"), False), (("/api/v1/labels", "limit=2") + whole_window, False), (("/api/v1/series", 'match[]={__name__=~".+"}', "limit=4") + whole_window, False),
                           (("/api/v1/series", 'match[]={__name__=~".+"}', "limit=-1") + whole_window, False),
                           # An instant other than the freeze, of every metric; and the one listing for less than the whole of what is frozen.
                           (("/api/v1/query", "query=lat_bucket", "time=<freeze>-90"), False), (("/api/v1/query", "query=up", "time=<freeze>-90"), False),
                           # An instant whose thousandths end in nothing, of what a Prometheus writes two ways there: a scalar, a string, a sample.
                           (("/api/v1/query", "query=time()", "time=<second>-0.5"), False), (("/api/v1/query", 'query="a string"', "time=<second>-0.75"), False), (("/api/v1/query", "query=up", "time=<second>-0.5"), False),
                           (("/api/v1/query", "query=scalar(count(up))", "time=<second>-1"), False), (("/api/v1/query_range", "query=time()", "start=<second>-60.5", "end=<second>-0.5", "step=15"), False),
                           # A scalar too large and one too small to write without an exponent, as promq's request and so at the freeze.
                           (("/api/v1/query", "query=1e30", "time=<freeze>"), False), (("/api/v1/query", "query=1e-7", "time=<freeze>"), False), (("/api/v1/query", "query=scalar(vector(1e21))", "time=<freeze>"), False), (("/api/v1/series", 'match[]={job="prometheus"}', "start=<freeze>-60", "end=<freeze>"), False),
                           # The request promq makes for each question that looks back on purpose.
                           (("/api/v1/query", "query=up offset 90m", "time=<freeze>"), False), (("/api/v1/query_range", "query=up", "start=<freeze>-10800", "end=<freeze>", "step=10m"), False)):
        if sent_to.get(request) is not typed:
            failures.append(f"the request {list(request)} is {'not made' if request not in sent_to else 'made and marked ' + str(sent_to[request])}; want it made, an agent's: {typed}")
    # No listing of series for less than the whole window but that one: a case lists what it holds.
    for request in sent_to:
        if request[0] == "/api/v1/series" and any(part.startswith("start=<freeze>") for part in request) and "start=<freeze>-3600" not in request and request != ("/api/v1/series", 'match[]={job="prometheus"}', "start=<freeze>-60", "end=<freeze>"):
            failures.append(f"the request {list(request)} asks for the series of a part of what is frozen, which a case does not answer by")
    if "2 more that an agent asked are not asked" not in out.getvalue() or sum(q["typed"] for q in queries) != 4:
        failures.append(f"what an agent asked and is not asked was not said, or not counted: {out.getvalue()!r}")
    # The command, as the sweep runs it: it has to return what it found, or a sweep could not fail.
    answers = lambda outs: [{"argv": [f"m{i}"], "typed": False, "rc": 0, "out": o} for i, o in enumerate(outs)]
    same = [f"m{i}{{}} {i}\n" for i in range(60)]
    for name, outs in (("live.json", same), ("frozen.json", same), ("other.json", same[:7] + ["m7{} 8\n"] + same[8:])):
        with open(os.path.join(tmp, name), "w") as f:
            json.dump(answers(outs), f)
    tool = os.path.join(os.path.dirname(os.path.abspath(__file__)), "promdiff.py")
    for what, argv, want in (("compare, alike", ["compare", "live.json", "frozen.json", "live.json", "frozen.json"], 0), ("compare, one differing", ["compare", "live.json", "other.json", "live.json", "other.json"], 1),
                             ("compare, of the API, one differing", ["compare", "live.json", "other.json", "live.json", "-"], 1), ("spoil", ["spoil", "live.json", "frozen.json", "live.json"], 0),
                             ("something it does not do", ["confirm", "live.json", "frozen.json"], 1)):
        got = subprocess.run([sys.executable, tool] + [os.path.join(tmp, a) if a.endswith(".json") else a for a in argv], capture_output=True, text=True).returncode
        if got != want:
            failures.append(f"promdiff.py {what}: exit {got}, want {want}")
server.shutdown()
# And held to the same comparison: a status is what an exit code is, and nothing is let by for being topk.
API = [  # what, live, frozen, the verdict
    ("the same", said("{}\n", 200), said("{}\n", 200), "same"), ("another body", said('{"a": 1}\n', 200), said('{"a": 2}\n', 200), "differs"),
    ("both refuse 400, in other words", said('{"error": "x"}\n', 400), said('{"error": "y"}\n', 400), "worded"), ("refused with another status", said('{"error": "x"}\n', 400), said('{"error": "x"}\n', 422), "differs"),
    ("one refuses", said("{}\n", 200), said('{"error": "x"}\n', 400), "differs"), ("no answer at all", said("{}\n", 200), said("no answer: EOF", 0), "unasked"),
]
for what, a, f, want in API:
    if (got := pd.verdict(["GET", "/api/v1/query", "query=topk(1, x)", "time=<freeze>"], a, f, a)) != want:
        failures.append(f"the API, {what}: {got}, want {want}")
# The same series and values in another order is said to be that, and is not a failure: for some of
# what an engine does the order is a map's. Anything else that differs, differs.
ORDER = [  # what, the query, live, frozen, live again, the verdict
    ("the same series the other way round, of what an engine counts in a map", counted, sent(("a", "1"), ("b", "2")), sent(("b", "2"), ("a", "1")), None, "order"),
    ("the same, in brackets", 'query=( histogram_quantile(0.9, x) )', sent(("a", "1"), ("b", "2")), sent(("b", "2"), ("a", "1")), None, "order"),
    ("and the Prometheus's own second answer in a third order", counted, sent(("a", "1"), ("b", "2"), ("c", "3")), sent(("b", "2"), ("a", "1"), ("c", "3")), sent(("c", "3"), ("a", "1"), ("b", "2")), "order"),
    # Of anything else the order is somebody's: the store's, the query's, or the engine's sorting of a window.
    ("the series of a selector the other way round", selected, sent(("a", "1"), ("b", "2")), sent(("b", "2"), ("a", "1")), None, "differs"),
    ("a sorted answer the other way round", "query=sort_desc(x)", sent(("a", "2"), ("b", "1")), sent(("b", "1"), ("a", "2")), None, "differs"),
    ("what counts inside something that sorts", "query=sort(count_values(\"v\", x))", sent(("a", "1"), ("b", "2")), sent(("b", "2"), ("a", "1")), None, "differs"),
    ("what counts, joined to something that sorts", "query=count_values(\"v\", x) or sort_desc(y)", sent(("a", "1"), ("b", "2")), sent(("b", "2"), ("a", "1")), None, "differs"),
    ("what an engine adds up", "query=sum by (c) (x)", sent(("a", "1"), ("b", "2")), sent(("b", "2"), ("a", "1")), None, "differs"),
    ("a bracket in a string of what counts", "query=count_values(\"v)\", x)", sent(("a", "1"), ("b", "2")), sent(("b", "2"), ("a", "1")), None, "order"),
    ("a selector whose two answers from the Prometheus came in two orders", selected, sent(("a", "1"), ("b", "2")), sent(("b", "2"), ("a", "1")), sent(("b", "2"), ("a", "1")), "order"),
    ("the same, and the frozen one in a third", selected, sent(("a", "1"), ("b", "2"), ("c", "3")), sent(("b", "2"), ("a", "1"), ("c", "3")), sent(("c", "3"), ("a", "1"), ("b", "2")), "order"),
    ("another order and another value", counted, sent(("a", "1"), ("b", "2")), sent(("b", "3"), ("a", "1")), None, "differs"),
    ("another order and a series more", counted, sent(("a", "1"), ("b", "2")), sent(("b", "2"), ("a", "1"), ("c", "3")), None, "differs"),
    ("another order and a remark beside it", counted, sent(("a", "1"), ("b", "2")), sent(("b", "2"), ("a", "1"), infos=["x"]), None, "differs"),
    ("another order of what is not a list of series", counted, said('{\n "data": [\n  "a",\n  "b"\n ]\n}\n', 200), said('{\n "data": [\n  "b",\n  "a"\n ]\n}\n', 200), None, "differs"),
]
for what, query, a, f, b, want in ORDER:
    if (got := pd.verdict(["GET", "/api/v1/query", query, "time=<freeze>"], a, f, b or a)) != want:
        failures.append(f"the API, {what}: {got}, want {want}")
# A window's series the engine sorts, whatever is asked for them; what promq prints is put in an order
# before it is compared, and another order of that is a difference; and an answer that is refused is not "in another order".
two, round_ = sent(("a", "1"), ("b", "2")), sent(("b", "2"), ("a", "1"))
for what, argv, a, f, want in (("a window of what counts", ["GET", "/api/v1/query_range", counted, "start=<freeze>-60", "end=<freeze>", "step=15"], two, round_, "differs"),
                               ("promq, of what counts", ['count_values("v", x)'], said(two["out"]), said(round_["out"]), "differs"),
                               ("what counts, and the frozen store answered with another status", ["GET", "/api/v1/query", counted, "time=<freeze>"], two, dict(round_, rc=500), "differs")):
    if (got := pd.verdict(argv, a, f, a)) != want:
        failures.append(f"another order of {what}: {got}, want {want}")
if pd.whole(2592000.0) != "2592000" or pd.whole(1.5) != "1.5" or pd.whole(0.25) != "0.25":
    failures.append("a number of seconds is not written whole")
# Another order is let by of what an engine keeps in a map and of nothing else that looks like it; only
# where every side answered; and an answer the Prometheus changed is one of its two, text and status both.
for query, want in (('count_values("v", x)', "order"), ("histogram_quantile(0.9, x)", "order"), ("histogram_fraction(0, 1, x)", "order"), ("topk(2, x)", "differs"), ("bottomk(2, x)", "differs"),
                    ("sum by (c) (x)", "differs"), ("count by (c) (x)", "differs"), ("max by (c) (x)", "differs"), ("sum(x)", "differs"), ("count(x)", "differs"), ("max(x)", "differs"), ('label_replace(x, "a", "b", "c", "d")', "differs"), ("x", "differs")):
    if (got := pd.verdict(["GET", "/api/v1/query", "query=" + query, "time=<freeze>"], two, round_, two)) != want:
        failures.append(f"another order of {query}: {got}, want {want}")
for what, a, f, b, want in (("another order, where all three were refused", dict(two, rc=400), dict(round_, rc=400), dict(two, rc=400), "worded"),
                            ("the Prometheus changed its words and not its status, and the frozen answer has the first", said("a\n", 200), said("a\n", 200), said("b\n", 200), "moved"),
                            ("the Prometheus changed its status and not its words, and the frozen answer has the second", said("a\n", 200), said("a\n", 503), said("a\n", 503), "moved"),
                            ("the Prometheus changed its status, and the frozen answer has the words with a third", said("a\n", 200), said("a\n", 500), said("a\n", 503), "differs")):
    if (got := pd.verdict(["GET", "/api/v1/query", counted, "time=<freeze>"], a, f, b)) != want:
        failures.append(f"{what}: {got}, want {want}")

# The Prometheus of the metrics fixture, and of the one scenario that has one, is of the version of
# the engine this tool is built with: v0.3NN.P of the module is 3.NN.P of the server. Otherwise what
# a sweep reports is two engines differing, and nothing of a frozen copy. Whoever moves the one
# moves the others.
root = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..")
module = re.search(r"^\s*github\.com/prometheus/prometheus v0\.(\d)(\d+)\.(\d+)\s*$", open(os.path.join(root, "go.mod")).read(), re.M)
images = {image for where in (("test", "replay-diff", "prom"), ("scenarios", "s2-periodic-saturation"))
          for image in re.findall(r"image: (\S*prometheus\S*)", open(os.path.join(root, *where, "prometheus.yaml")).read())}
engine = f"prom/prometheus:v{module.group(1)}.{int(module.group(2))}.{module.group(3)}" if module else None
setup = open(os.path.join(root, "test", "replay-diff", "prom", "setup.sh")).read()
default = re.search(r'IMAGE="\$\{PROM_IMAGE:-(\S+?)\}"', setup)
if images != {engine} or not default or default.group(1) != engine or f'sed "s|{engine}|$IMAGE|g" prometheus.yaml' not in setup:
    failures.append(f"go.mod builds this tool with the engine of {engine}, and the metrics fixture and the scenario run {sorted(images)} (the fixture's setup.sh: {default.group(1) if default else None})")

# The sweep freezes the hour this file takes it to freeze, hands the frozen case's record to both
# comparisons, and fails when either does; and the fixture's Prometheus is given the time it needs.
sweep = open(os.path.join(root, "test", "replay-diff", "sweep.sh")).read()
frozen_for = re.search(r"--metrics-window (\S+?)\)", sweep)
if not frozen_for or pd.seconds(frozen_for.group(1)) != pd.FROZEN_WINDOW:
    failures.append(f"sweep.sh freezes {frozen_for and frozen_for.group(1)}, and promdiff.py takes it to freeze {pd.FROZEN_WINDOW} seconds")
# Each line of it that hands the comparison something, as it is: the Prometheus's two askings and the
# frozen store's between them, what the store says with no instant named, the known differences of
# promq, the case's own record; the freeze given the Prometheus to read; the frozen store asked of
# its freeze by name; and the answers spoiled, beside the reports.
for line in ('    FREEZE=(--metrics-url "$LIVE_PROM" --metrics-window 1h)   #',
             '    prom capture "$D/queries.json" "$OUT/live-bin" "$LIVE_PROM" "$D/promq-live-1.json" --at "$AT" ||',
             '    prom fetch "$D/requests.json" "$LIVE_PROM" "$AT" "$D/api-live-1.json" ||',
             '    prom capture "$D/queries.json" "$FROZEN_BIN" "$FROZEN_PROM" "$D/promq-frozen.json" --at "$AT" ||',
             '    prom capture "$D/queries.json" "$FROZEN_BIN" "$FROZEN_PROM" "$D/promq-as-typed.json" ||',
             '    prom fetch "$D/requests.json" "$FROZEN_PROM" "$AT" "$D/api-frozen.json" ||',
             '    prom capture "$D/queries.json" "$OUT/live-bin" "$LIVE_PROM" "$D/promq-live-2.json" --at "$AT" ||',
             '    prom fetch "$D/requests.json" "$LIVE_PROM" "$AT" "$D/api-live-2.json" ||',
             '    python3 "$HERE/promdiff.py" compare "$D/promq-live-1.json" "$D/promq-frozen.json" "$D/promq-live-2.json" "$D/promq-as-typed.json" "$HERE/known-promq.txt" "$D/frozen/freeze.json" > "$D/report-promq.md" || status=1\n',
             '    python3 "$HERE/promdiff.py" compare "$D/api-live-1.json" "$D/api-frozen.json" "$D/api-live-2.json" - "$HERE/known-promq.txt" "$D/frozen/freeze.json" > "$D/report-api.md" || status=1\n',
             '    { echo "## What promq prints"; echo; python3 "$HERE/promdiff.py" spoil "$D/promq-live-1.json" "$D/promq-frozen.json" "$D/promq-live-2.json" "$D/frozen/freeze.json"\n',
             '      echo; echo "## What the API sends"; echo; python3 "$HERE/promdiff.py" spoil "$D/api-live-1.json" "$D/api-frozen.json" "$D/api-live-2.json" "$D/frozen/freeze.json"; } > "$D/report-spoiled.md" 2>&1 || true\n'):
    if sweep.count("\n" + line) != 1:
        failures.append(f"sweep.sh no longer has the line {line.strip()!r}: what it hands the comparison of a case's metrics is other than this file takes it to be")
# The fixture has what the questions are asked of it for. A past as long as the furthest of them looks
# back. A target that stopped in that past, longer before its end than an instant looks back and well
# inside the hour a sweep freezes: series of the blocks that were never in the head. An external label
# that a series has of its own, with another value, and one that no series has: what a remote read
# adds, and what it adds over. And its setup waits for each of those before it says the fixture is there.
config = open(os.path.join(root, "test", "replay-diff", "prom", "prometheus.yaml")).read()
sys.path.insert(0, os.path.join(root, "test", "replay-diff", "prom", "code"))
import exporter
past = re.search(r'\{name: HISTORY_SECONDS, value: "(\d+)"\}', config)
if not past or int(past.group(1)) < max(pd.LOOKS_BACK.values()):
    failures.append(f"the fixture's past is {past and past.group(1)} seconds, and a question looks back {max(pd.LOOKS_BACK.values())}")
if not exporter.RETIRED or any(target not in exporter.SCRAPED or not 300 < ago <= pd.FROZEN_WINDOW // 2 for target, ago in exporter.RETIRED.items()):
    failures.append(f"the fixture's past has no target that stopped in it where a case would have it and its head would not: {exporter.RETIRED}")
external = re.search(r"external_labels: \{([^}]*)\}", config)
external = dict(pair.split(": ") for pair in external.group(1).split(", ")) if external else {}
their_own = {name: value for added in exporter.SCRAPED.values() for name, value in added.items()}
if not any(name in their_own and their_own[name] != value for name, value in external.items()) or not any(name not in their_own for name in external):
    failures.append(f"the fixture's Prometheus has the external labels {external}: want one a series has of its own with another value, and one no series has")
for waited in ("has 'prometheus_tsdb_blocks_loaded%20%3E%200'", "has 'count_over_time(up%7Binstance%3D~%22exp-d.%2B%22%7D%5B1h%5D)%20%3E%2010' && without 'up%7Binstance%3D~%22exp-d.%2B%22%7D'",
               """has() { local got; got="$(ask "$1")" || return 1; case "$got" in *'"result":[{'*) return 0 ;; esac; return 1; }\n""", """without() { local got; got="$(ask "$1")" || return 1; case "$got" in *'"result":[]'*) return 0 ;; esac; return 1; }\n""",
               "  ask 'sum(' >/dev/null || true\n  ask 'up%20*%20on%20(job)%20up' >/dev/null || true\n  if has ", "has 'prometheus_http_requests_total%7Bcode%3D%22400%22%7D' && has 'prometheus_http_requests_total%7Bcode%3D%22422%22%7D'", "\n  sleep 5\ndone\n"):
    if setup.count(waited) != 1:
        failures.append(f"the metrics fixture's setup.sh no longer has {waited!r}: it says the fixture is there before it is")
# The fixture's head is older at the freeze than the furthest any instant asked of it looks: the
# minute and a half before the freeze that an instant is asked of, and what an instant looks back.
# Then such a question is answered from the head alone, in the head's order, which is what is asked.
aged = re.search(r"time\(\)%20-%20max\(prometheus_tsdb_head_min_time_seconds\)%20%3E%20(\d+)'", setup)
if not aged or int(aged.group(1)) < 90 + 300 + 30 or "for i in $(seq 1 150)" not in setup or '"time=<freeze>-90"' not in inspect.getsource(pd.discovery):
    failures.append(f"the metrics fixture's setup.sh waits for a head {aged and aged.group(1)} seconds old; the instants asked of it look back as far as 390 seconds before the freeze")

print("\n".join(failures) or f"{len(VERDICTS) + len(AGES)} verdicts, {len(EXCUSED)} excuses, {len(ASKED)} commands, {len(TYPED)} typed steps and {len(PROMQ) + len(TIES) + len(PROMQ_TYPED)} of promq and {len(REQUESTS) + len(API) + len(ORDER)} of its API are as they should be")
sys.exit(1 if failures else 0)
