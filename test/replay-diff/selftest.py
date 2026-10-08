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
    # `--since=0s` is no window: kubectl prints the tail, and nothing is not an answer to it.
    ("the last no seconds of a log, answered with nothing", ["logs", "p", "--tail=3", "--since=0s"], answer("a1\na2\na3\n"), answer(""), answer("a4\na5\na6\n"), "differs"),
    ("the last no seconds of a log, answered with its tail", ["logs", "p", "--tail=3", "--since=0s"], answer("a1\na2\na3\n"), answer("a2\na3\na4\n"), answer("a3\na4\na5\n"), "same"),
    ("the last five seconds of a log, in which nothing was written", ["logs", "p", "--since=5s"], answer("a1\n"), answer(""), answer("a9\n"), "same"),
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
import contextlib, io, json, subprocess, tempfile
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
    one_off = alike[:7] + ["m7{} 8\n"] + alike[8:]
    with open(os.path.join(tmp, "known-promq.txt"), "w") as f:
        f.write("# a comment\n^promq m7$\t^m7\\{\\} 7$\t^m7\\{\\} 8$\tfor the test\n")
    known = os.path.join(tmp, "known-promq.txt")
    other_off = alike[:7] + ["m7{} 9\n"] + alike[8:]
    for what, frozen, as_typed, second, known_path, want, shows in (
        ("alike", alike, alike, alike, None, 0, "**differ: 0**"), ("one answer differs", one_off, one_off, alike, None, 1, "**differ: 1**"),
        ("one answer differs and is known", one_off, one_off, alike, known, 0, "(known: for the test)"),
        # A line of the known file excuses the difference it describes, not whatever its query happens to answer.
        ("the known query differs in another way", other_off, other_off, alike, known, 1, "**differ: 1**"),
        ("an agent's asking differs from the named one", alike, one_off, alike, None, 1, "answered 59 of the 60 the same"),
        ("an agent's asking differs, of a query that is known for something else", alike, one_off, alike, known, 1, "answered 59 of the 60 the same"),
        ("too few", alike[:9], alike[:9], alike[:9], None, 1, "too few"),
        # One answer the Prometheus changed between its askings is the Prometheus; most of them is a second asking of something else.
        ("the Prometheus changed one of its own answers", alike, alike, one_off, None, 0, "between its two askings: 1"),
        ("the Prometheus changed two of its own answers in sixty", alike, alike, one_off[:9] + ["m9{} 0\n"] + one_off[10:], None, 1, "nothing here was compared"),
        ("the second asking of the Prometheus was refused", alike, alike, ["query failed: no one there\n"] * 60, None, 1, "nothing here was compared")):
        paths = [asked("live-1.json", alike[:len(frozen)]), asked("frozen.json", frozen), asked("live-2.json", second), asked("as-typed.json", as_typed)]
        with contextlib.redirect_stdout(io.StringIO()) as out:
            got = pd.cmd_compare(*paths, *([known_path] if known_path else []))
        if got != want or shows not in out.getvalue():
            failures.append(f"promq compare, {what}: returned {got}, want {want}, and printed {out.getvalue()[:300]!r}")
    # Refused by both in other words fails, as differing does; and its excuse is held to the words too.
    def refused(name, words):
        path = os.path.join(tmp, name)
        with open(path, "w") as f:
            json.dump([{"argv": [f"m{i}"], "typed": False, "rc": 0 if i else 1, "out": alike[i] if i else words} for i in range(60)], f)
        return path
    with open(os.path.join(tmp, "known-words.txt"), "w") as f:
        f.write("^promq m0$\t^query failed: so$\t^query failed: in a newer way$\tfor the test\n")
    with open(os.path.join(tmp, "known-other.txt"), "w") as f:
        f.write("^promq m1$\t^query failed: so$\t^query failed: in a newer way$\tfor the test\n")
    with open(os.path.join(tmp, "known-there.txt"), "w") as f:
        f.write("^promq m0$\t^query failed: as it was$\t^query failed: in a newer way$\tfor the test\n")
    for what, frozen_says, known_path, want in (("in other words", "query failed: otherwise\n", None, 1), ("in the words that are known", "query failed: in a newer way\n", os.path.join(tmp, "known-words.txt"), 0),
                                                ("in yet other words than the known ones", "query failed: otherwise\n", os.path.join(tmp, "known-words.txt"), 1),
                                                ("in the words that are known of another query", "query failed: in a newer way\n", os.path.join(tmp, "known-other.txt"), 1),
                                                ("in the words that are known, to a Prometheus that said something else", "query failed: in a newer way\n", os.path.join(tmp, "known-there.txt"), 1)):
        paths = [refused("live-1.json", "query failed: so\n"), refused("frozen.json", frozen_says), refused("live-2.json", "query failed: so\n"), refused("as-typed.json", frozen_says)]
        with contextlib.redirect_stdout(io.StringIO()) as out:
            got = pd.cmd_compare(*paths, *([known_path] if known_path else []))
        if got != want or "worded differently: 1" not in out.getvalue():
            failures.append(f"promq compare, refused by both {what}: returned {got}, want {want}, and printed {out.getvalue()[:200]!r}")
    # spoil counts what still passes, and a comparison that let a spoiled answer by would fail it.
    paths = [asked("live-1.json", alike), asked("frozen.json", alike), asked("live-2.json", alike)]
    with contextlib.redirect_stdout(io.StringIO()) as out:
        got = pd.cmd_spoil(*paths)
    if got != 0 or out.getvalue().count("| 0 | 60 |") != 5:
        failures.append(f"spoil of sixty answers that are alike: returned {got} and printed {out.getvalue()!r}")
    lenient, pd.verdict = pd.verdict, lambda argv, a, f, b: "same"
    with contextlib.redirect_stdout(io.StringIO()):
        got = pd.cmd_spoil(*paths)
    pd.verdict = lenient
    if got != 1:
        failures.append("spoil passed a comparison that calls everything the same")
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
    want_requests = [["/api/v1/label/__name__/values", 'match[]={__name__=~"thumb.*"}', "start=<freeze>-1800", "end=<freeze>"], ["/api/v1/label/client/values", "match[]=x", "start=<freeze>-1800", "end=<freeze>"],
                     ["/api/v1/series", "match[]=x", "start=<freeze>-1800", "end=<freeze>"], ["/api/v1/metadata", "metric=x"]]
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
    def do_GET(self):
        url = urllib.parse.urlsplit(self.path)
        code = 400 if url.path.endswith("/refused") else 200
        body = b"not JSON at all" if url.path.endswith("/text") else json.dumps({"status": "success" if code == 200 else "error", "asked": urllib.parse.parse_qsl(url.query),
                                                                               "infos": ["b", "a"], "warnings": ["only one"],
                                                                               "data": {"result": [{"value": [5, "1e-07"], "metric": {"b": "2", "a": "1"}}, {"value": [5.000, "+Inf"], "metric": {}}]}}).encode()
        self.send_response(code); self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def log_message(self, *args):
        pass
server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Echo)
threading.Thread(target=server.serve_forever, daemon=True).start()
with tempfile.TemporaryDirectory() as tmp:
    asked = [{"argv": ["GET", "/api/v1/query", "query=sum by (a) (x{b=\"c d\"})", "time=<freeze>"], "typed": True}, {"argv": ["GET", "/api/v1/query_range", "start=<freeze>-90", "end=<freeze>", "step=15s"], "typed": False},
             {"argv": ["GET", "/refused", "match[]=up", "match[]=x"], "typed": False}, {"argv": ["GET", "/text"], "typed": False}]
    with open(os.path.join(tmp, "requests.json"), "w") as f:
        json.dump(asked, f)
    with contextlib.redirect_stdout(io.StringIO()):
        pd.cmd_fetch(os.path.join(tmp, "requests.json"), f"http://127.0.0.1:{server.server_address[1]}/", "1791428838.5", os.path.join(tmp, "out.json"))
    got = json.load(open(os.path.join(tmp, "out.json")))
    sent = [json.loads(a["out"]).get("asked") if a["out"].startswith("{") else a["out"] for a in got]
    want = [[["query", 'sum by (a) (x{b="c d"})'], ["time", "1791428838.500"]], [["start", "1791428748.500"], ["end", "1791428838.500"], ["step", "15s"]], [["match[]", "up"], ["match[]", "x"]], "not JSON at all"]
    if sent != want or [a["rc"] for a in got] != [200, 200, 400, 200] or [a["typed"] for a in got] != [True, False, False, False]:
        failures.append(f"what was fetched: {sent} {[a['rc'] for a in got]}, want {want}")
    # Kept as sent: the series in their order, a value as the string it was, a number as it was written — 5 is not 5.0, which is how
    # a time written without its thousandths is told from one with them — and the engine's remarks; an object's keys in one order,
    # and the remarks in one, which is a map's.
    kept = json.loads(got[0]["out"])
    if [r["value"] for r in kept["data"]["result"]] != [["#5", "1e-07"], ["#5.0", "+Inf"]] or kept["infos"] != ["a", "b"] or kept["warnings"] != ["only one"] or '"asked"' not in got[0]["out"].split("\n")[1]:
        failures.append(f"an answer was not kept as it was sent: {got[0]['out']!r}")
server.shutdown()

# What is asked, of a stand-in for a Prometheus that has two metrics, one of them a histogram's: the
# fixed set about each, the ones about none, what recorded agents asked — marked as theirs whoever
# else asks it — and the requests behind them all; and, through the command itself, what it returns.
class StandIn(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        url = urllib.parse.urlsplit(self.path)
        name = dict(urllib.parse.parse_qsl(url.query)).get("match[]", "up")
        there = {"/api/v1/label/__name__/values": ["lat_bucket", "up"], "/api/v1/labels": ["__name__", "job", "le"],
                 "/api/v1/series": [{"__name__": name, "job": "a"}, {"__name__": name, "job": "b"}]}
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
        pd.cmd_queries(f"http://127.0.0.1:{server.server_address[1]}", queries_path, requests_path, ["--old-snapshot", "x", "--runs", runs, "--runs", os.path.join(tmp, "none")])
    queries, requests = json.load(open(queries_path)), json.load(open(requests_path))
    asked = {tuple(q["argv"]): q["typed"] for q in queries}
    sent_to = {tuple(r["argv"][1:]): r["typed"] for r in requests}
    for argv, typed in ((("up",), True), (("sum(up) by (job)", "--range", "10m", "--step", "30s"), True), (("lat_bucket",), False), (("rate(lat_bucket[1m])",), False), (("sum by (job) (up)",), False),
                        (('up{job="a"}',), False), (("histogram_quantile(0.9, sum by (le) (rate(lat_bucket[5m])))",), False), (("up", "--range", "1h"), False), (("sum(",), False), (("topk(3, lat_bucket)",), False)):
        if asked.get(argv) is not typed:
            failures.append(f"promq {list(argv)} is {'not asked' if argv not in asked else 'asked and marked ' + str(asked[argv])}; want it asked, an agent's: {typed}")
    if ("histogram_quantile(0.9, sum by (le) (rate(up[5m])))",) in asked or len(asked) != len(queries) or len(sent_to) != len(requests):
        failures.append("what is asked has a histogram's question about what is none, or the same thing twice")
    for request, typed in ((("/api/v1/query", "query=up", "time=<freeze>"), True), (("/api/v1/query_range", "query=sum(up) by (job)", "start=<freeze>-600", "end=<freeze>", "step=30s"), True),
                           (("/api/v1/series", "match[]=up", "start=<freeze>-1800", "end=<freeze>"), True), (("/api/v1/series", "match[]=lat_bucket", "start=<freeze>-1800", "end=<freeze>"), False),
                           (("/api/v1/label/le/values", "start=<freeze>-1800", "end=<freeze>"), False), (("/api/v1/labels",), False), (("/api/v1/query", "query=up", "time=<freeze>", "limit=many"), False)):
        if sent_to.get(request) is not typed:
            failures.append(f"the request {list(request)} is {'not made' if request not in sent_to else 'made and marked ' + str(sent_to[request])}; want it made, an agent's: {typed}")
    if "2 more that an agent asked are not asked" not in out.getvalue() or sum(q["typed"] for q in queries) != 2:
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
    ("one refuses", said("{}\n", 200), said('{"error": "x"}\n', 400), "differs"), ("no answer at all", said("{}\n", 200), said("no answer: EOF", 0), "differs"),
]
for what, a, f, want in API:
    if (got := pd.verdict(["GET", "/api/v1/query", "query=topk(1, x)", "time=<freeze>"], a, f, a)) != want:
        failures.append(f"the API, {what}: {got}, want {want}")
# The same series and values in another order is said to be that, and is not a failure: for some of
# what an engine does the order is a map's. Anything else that differs, differs.
sent = lambda *series, **more: said(json.dumps(dict({"status": "success", "data": {"resultType": "vector", "result": [{"metric": {"c": c}, "value": [5.0, v]} for c, v in series]}}, **more), sort_keys=True, indent=1) + "\n", 200)
counted, selected = 'query=count_values("v", x)', "query=x"
ORDER = [  # what, the query, live, frozen, live again, the verdict
    ("the same series the other way round, of what an engine counts in a map", counted, sent(("a", "1"), ("b", "2")), sent(("b", "2"), ("a", "1")), None, "order"),
    ("the same, in brackets", 'query=( histogram_quantile(0.9, x) )', sent(("a", "1"), ("b", "2")), sent(("b", "2"), ("a", "1")), None, "order"),
    ("and the Prometheus's own second answer in a third order", counted, sent(("a", "1"), ("b", "2"), ("c", "3")), sent(("b", "2"), ("a", "1"), ("c", "3")), sent(("c", "3"), ("a", "1"), ("b", "2")), "order"),
    # Of anything else the order is somebody's: the store's, the query's, or the engine's sorting of a window.
    ("the series of a selector the other way round", selected, sent(("a", "1"), ("b", "2")), sent(("b", "2"), ("a", "1")), None, "differs"),
    ("a sorted answer the other way round", "query=sort_desc(x)", sent(("a", "2"), ("b", "1")), sent(("b", "1"), ("a", "2")), None, "differs"),
    ("what counts inside something that sorts", "query=sort(count_values(\"v\", x))", sent(("a", "1"), ("b", "2")), sent(("b", "2"), ("a", "1")), None, "differs"),
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
if pd.whole(2592000.0) != "2592000" or pd.whole(1.5) != "1.5" or pd.whole(0.25) != "0.25":
    failures.append("a number of seconds is not written whole")

print("\n".join(failures) or f"{len(VERDICTS) + len(AGES)} verdicts, {len(EXCUSED)} excuses, {len(ASKED)} commands, {len(TYPED)} typed steps and {len(PROMQ) + len(TIES) + len(PROMQ_TYPED)} of promq and {len(REQUESTS) + len(API) + len(ORDER)} of its API are as they should be")
sys.exit(1 if failures else 0)
