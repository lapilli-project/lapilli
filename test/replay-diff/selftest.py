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
    ("an event's count outside them", ["describe", "pod", "x"], answer("Warning Unhealthy 5s (x10 over 2m) kubelet\n"), answer("Warning Unhealthy 3s (x40 over 2m) kubelet\n"), answer("Warning Unhealthy 9s (x12 over 2m) kubelet\n"), "differs"),
    ("a table with other columns", ["get", "pods", "-o", "wide"], answer("NAME READY AGE IP NODE\na 1/1 5m 10.0.0.1 w\n"), answer("NAME READY AGE\na 1/1 5m\n"), answer("NAME READY AGE IP NODE\na 1/1 5m 10.0.0.1 w\n"), "differs"),
    ("an empty answer", ["get", "events", "--sort-by=.lastTimestamp"], answer("LAST SEEN TYPE\n5m Normal\n"), answer("", "No resources found in shop namespace.\n"), answer("LAST SEEN TYPE\n5m Normal\n"), "differs"),
    ("another order", ["get", "pods"], answer("H\nb\na\n"), answer("H\na\nb\n"), answer("H\nb\na\n"), "differs"),
    ("another order under --sort-by, where equal keys may stand either way", ["get", "events", "--sort-by=.lastTimestamp"], answer("H\nb\na\n"), answer("H\na\nb\n"), answer("H\nb\na\n"), "order"),
    ("the cluster itself moved", ["get", "events"], answer("H\ne1\n"), answer("H\ne1\ne2\ne3\n"), answer("H\ne1\ne2\ne3\ne4\n"), "moved"),
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
]
EXCUSED = [  # the command, what compare said of it, whether known.txt lets it by
    (["explain", "pods"], "exit codes live 0, frozen 1, live 0: Error from server (NotFound)", True),
    (["cluster-info"], "live: Kubernetes control plane is running at https://a | frozen: Kubernetes control plane is running at http://b", True),
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
]

# An age may differ by as long as lay between two askings, which is written beside each answer.
slow = lambda out, at: dict(answer(out), at=at)
VERDICTS += [
    ("describe, asked a minute apart on a slow machine", ["describe", "pod", "x"], slow("Age: 60s\n", 0), slow("Age: 2m\n", 60), slow("Age: 3m\n", 120), "same"),
    ("describe, asked two seconds apart and a minute older", ["describe", "pod", "x"], slow("Age: 60s\n", 0), slow("Age: 2m\n", 2), slow("Age: 64s\n", 4), "differs"),
]

known = rd.load_known(os.path.join(os.path.dirname(os.path.abspath(__file__)), "known.txt"))
failures = []
for what, argv, before, frozen, after, want in VERDICTS:
    if (got := rd.verdict(argv, before, frozen, after)[0]) != want:
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
print("\n".join(failures) or f"{len(VERDICTS)} verdicts, {len(EXCUSED)} excuses and {len(ASKED)} commands are as they should be")
sys.exit(1 if failures else 0)
