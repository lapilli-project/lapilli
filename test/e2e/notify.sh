#!/usr/bin/env bash
# Incident-notification E2E (docs/design-notify.md, including §"Round 12 changed six more
# things") against a receiver pod that records every POST it gets:
#   routes       — a route with a missing Secret disables itself and says so in a metric,
#                  instead of wedging the pod; a good route reports lapilli_notify_routes ready
#   grouping     — a bad rollout across 5 pods produces exactly ONE message naming 5 captures,
#                  and every member is claimed (an unclaimed member re-announces on a relist)
#   no content   — the default install's message carries no workload string (a canary in the
#                  app's log stream, a canary in its env, and the last log line are all
#                  absent, and neither is the summary sidecar holding the log line) while the
#                  facts an on-call engineer needs are present
#   detail       — `detail: content` shows the changed image tag and still no canary
#   repeat       — the same verdict on the same workload stays quiet for the cooldown, and the
#                  repeat it counted rides on the next message that does go out
#   injection    — `<!channel>` in `alertname` and `<url|label>` in the `pod` label never reach
#                  the channel unescaped (refused by the CRD pattern, or escaped)
#   failure      — receiver down: the capture still reaches Exported, status.notification says
#                  failed with a fixed reason code and its route, kubectl shows the Notify
#                  column, and lapilli_notifications_total{result="failed"} moves
#   rollout      — the controller pod is deleted while a group is still coalescing: the SIGTERM
#                  flush posts it; and when that flush is refused (the Service pointed at
#                  nothing for the moment), the next process posts it instead of losing it
#   cleanup      — the route and the receiver namespace go away, and lapilli_notify_routes with
#                  them ("off" and "broken" must never read the same)
#
# Usage: test/e2e/notify.sh [<lapilli-binary>]   (Lapilli already installed in lapilli-system, as
# run.sh installs it; the binary is optional and only used for one offline bundle check).
set -euo pipefail

ctrl_pod() { # the controller pod that is not terminating
  kubectl -n "$1" get pods -l app.kubernetes.io/name=lapilli \
    -o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{.metadata.name}}{{"\n"}}{{end}}{{end}}' | awk 'NR==1'
}

LAPILLI=${1:-}
KNS=lapilli-system
RELEASE=lapilli                       # the release run.sh installs
NS=notify-e2e                       # receiver *and* the crashlooping workload live here
RX_SVC=notify-receiver
RX_PORT=8080
# The route's `host` is a host[:port] (round 12 fixed the chart regex, which matched the port
# as part of the name), so the receiver is reached on its own port, not through a port-80
# Service — that is the form an in-cluster test endpoint actually has.
RX_HOST="$RX_SVC.$NS.svc.cluster.local:$RX_PORT"
RX_PATH=/hook/platform              # the only part of the URL that comes from the Secret
ROUTE=platform
HOOK_SECRET=lapilli-notify-hook
APP=notify-crash
ACTOR=lapilli-notify-e2e              # server-side apply field manager = the attributed actor
MPORT=18083                         # /metrics port-forward (18081/18082 are run.sh's and kms.sh's)

# The receiver's bytes matter, so it is pinned by digest (round-3 requirement). It only needs
# a stdlib HTTP server.
RECEIVER_IMAGE=python:3.12-alpine@sha256:c4634f578a412db396771b61b064c6e546c9d6414c7fb5b1b05d5871f1885f7b
# The workload is deliberately referenced by *tag*, like diffs.sh and export.sh do, because
# the `detail: content` assertion is about the changed image **tag** text appearing in the
# message; a digest-pinned ref would test a digest instead. (For the record, at the time of
# writing: busybox:1.36 is sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662
# and busybox:1.37 is sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0.)
IMAGE_OLD=busybox:1.36
IMAGE_NEW=busybox:1.37

# Planted strings. None of them may ever appear in a message body.
LOG_CANARY=lapilliNotifyLogCanary7Tw3Jd            # written to the app's stdout
ENV_CANARY=lapilliNotifyEnvCanary5Hq8Zb            # only in the app's env, never printed
LAST_WORDS_TOKEN=lapilliNotifyLastWords2Pk6Vn      # a fragment of the last log line
LAST_LINE="FATAL: $LAST_WORDS_TOKEN cache warmup failed"

# Injection vectors (docs/design-notify.md §"Untrusted strings, escaped").
INJ_RULE='NotifyE2EInject<!channel>'
INJ_POD='<https://evil.example|click>'

# The fixed reason codes status.notification.reason may hold (round 12, item 4). Transport
# text must never reach the object.
REASON_CODES="unreachable timeout endpoint-refused endpoint-error rate-limited-by-endpoint \
endpoint-redirected route-unusable rate-capped claim-failed already-notified"

# Plain HTTP to a cluster-local endpoint needs both halves: the process-wide env var (so a
# chart value alone cannot downgrade a real endpoint) and the route's own opt-in.
ALLOW_HTTP_ENV=LAPILLI_NOTIFY_ALLOW_HTTP
ROUTE_HTTP_KEY=insecureHttp

LAPILLI_DEPLOY=""   # the controller Deployment's real name; the retrieval command must name it
ON_FAIL=""          # a function `fail` runs first, for a step whose evidence `fail` cannot see
step() { echo; echo "==> notify: $*"; }
fail() {
  echo "FAIL (notify): $*"
  if [ -n "$ON_FAIL" ]; then "$ON_FAIL" || true; fi
  kubectl -n $KNS logs "deploy/${LAPILLI_DEPLOY:-lapilli}" --tail=40 || true
  exit 1
}
# UTC wall clock to the millisecond, in the controller log's own format, so a harness action
# can be placed against the pod's lines. python3, because BSD `date` has no %N.
now() { python3 -c 'import datetime; print(datetime.datetime.now(datetime.timezone.utc).strftime("%H:%M:%S.%f")[:-3])'; }

# Read the controller's log as a value, never as the left side of a pipe.
#
# `kubectl logs … | grep -q X` is unsafe twice under `set -o pipefail`. `grep -q` exits the instant
# it matches, so a producer that is still writing takes EPIPE and exits non-zero — and the pipeline
# then "fails" **with the assertion satisfied**. Measured: a 200,000-line producer fails that way
# every time, an 8-line one never does, because the short one's writes all fit in the pipe buffer.
# (kms.sh already carries a note about `pipefail` biting for a different reason.) The second hazard
# is quieter: `deploy/x` resolves through a selector, and `kubectl logs --help` says `--tail`
# "Defaults to -1 with no selector, showing all log lines otherwise 10, if a selector is provided" —
# so an assertion written for a startup line silently stops covering it once the controller has
# logged eleven things. Hence a value, and an explicit tail.
ctl_logs() { kubectl -n $KNS logs "deploy/${LAPILLI_DEPLOY:-lapilli}" --tail="${1:-400}"; }

TMP=$(mktemp -d)
PF=""
LOGF=""   # a `kubectl logs -f` following a pod that is about to be deleted
cleanup() {
  [ -n "$PF" ] && kill "$PF" 2>/dev/null || true
  [ -n "$LOGF" ] && kill "$LOGF" 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT

CAPTURES=""   # every IncidentCapture this script creates, deleted in the cleanup step
MSGS=0        # how many POSTs the receiver is expected to hold at this point
M=""          # the last message's file prefix, set by next_msg

# The Deployment the chart named (`<release>-lapilli`, collapsed to `lapilli` for a release called
# lapilli). The message's retrieval command must name this one, so never hardcode it.
LAPILLI_DEPLOY=$(kubectl -n $KNS get deploy -l app.kubernetes.io/name=lapilli \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
[ -n "$LAPILLI_DEPLOY" ] || { echo "FAIL (notify): no lapilli Deployment in $KNS"; exit 1; }
EXEC_PREFIX="kubectl -n $KNS exec deploy/$LAPILLI_DEPLOY -c controller --"

# ---------------------------------------------------------------------------- receiver -----

step "receiver pod that records every POST (pinned $RECEIVER_IMAGE)"
# Serves: POST <any path> -> remembers {path, headers, body}; GET /requests -> the whole log
# as JSON; GET /count -> the number of POSTs; GET /healthz -> readiness.
cat > "$TMP/receiver.py" <<'PY'
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

REQUESTS = []


class Receiver(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):  # the interesting lines are printed below
        pass

    def _send(self, code, payload=b"", ctype="application/json"):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        if payload:
            self.wfile.write(payload)

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length) if length else b""
        REQUESTS.append({
            "path": self.path,
            "headers": {k.lower(): v for k, v in self.headers.items()},
            "body": body.decode("utf-8", "replace"),
        })
        print("POST %s (%d bytes) -> #%d" % (self.path, len(body), len(REQUESTS)), flush=True)
        self._send(200, b'{"ok":true}')

    def do_GET(self):
        if self.path.startswith("/requests"):
            self._send(200, json.dumps({"count": len(REQUESTS), "requests": REQUESTS}).encode())
        elif self.path.startswith("/count"):
            self._send(200, str(len(REQUESTS)).encode(), "text/plain")
        elif self.path.startswith("/healthz"):
            self._send(200, b"ok", "text/plain")
        else:
            self._send(404, b"{}")


ThreadingHTTPServer(("", 8080), Receiver).serve_forever()
PY
kubectl create namespace $NS >/dev/null 2>&1 || true
kubectl -n $NS create configmap notify-receiver-code --from-file=receiver.py="$TMP/receiver.py" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl apply -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: { name: $RX_SVC, namespace: $NS }
spec:
  replicas: 1
  selector: { matchLabels: { app: $RX_SVC } }
  template:
    metadata: { labels: { app: $RX_SVC } }
    spec:
      terminationGracePeriodSeconds: 1
      containers:
        - name: receiver
          image: $RECEIVER_IMAGE
          command: ["python3", "-u", "/app/receiver.py"]
          volumeMounts: [{ name: code, mountPath: /app }]
          readinessProbe: { httpGet: { path: /healthz, port: $RX_PORT } }
      volumes:
        - name: code
          configMap: { name: notify-receiver-code }
---
apiVersion: v1
kind: Service
metadata: { name: $RX_SVC, namespace: $NS }
spec:
  selector: { app: $RX_SVC }
  ports: [{ name: http, port: $RX_PORT, targetPort: $RX_PORT }]
EOF
kubectl -n $NS rollout status deploy/$RX_SVC --timeout=180s >/dev/null

# Read the receiver through the API server's service proxy (no port-forward to race with).
rx_get() { kubectl get --raw "/api/v1/namespaces/$NS/services/$RX_SVC:http/proxy$1"; }
rx_count() { # number of POSTs so far, or -1 when the receiver can't be reached
  local n
  n=$(rx_get /count 2>/dev/null) || n=""
  case "$n" in '' | *[!0-9]*) echo -1 ;; *) echo "$n" ;; esac
}
rx_save() { rx_get /requests > "$1" 2>/dev/null || fail "could not read the receiver's request log"; }
rx_wait() { # want, tries (2 s apart) → waits until count >= want
  local n=-1
  for _ in $(seq 1 "$2"); do
    n=$(rx_count)
    [ "$n" -ge "$1" ] && return 0
    sleep 2
  done
  fail "only $n POST(s) arrived, wanted $1 (the controller never sent, or never reached the receiver)"
}
rx_hold() { # want, seconds → the count must stay exactly `want` for that long
  local n
  for _ in $(seq 1 $(( $2 / 5 ))); do
    n=$(rx_count)
    [ "$n" = "$1" ] || fail "the receiver has $n POST(s), wanted exactly $1 (one message per incident?)"
    sleep 5
  done
}

# Message N (0-based) out of a saved request log → $TMP/msg-N.raw (the body as sent),
# $TMP/msg-N.flat (every *string value* in it, joined) and $TMP/msg-N.path. Presence is
# asserted against the flattened values, because the exact block nesting is the renderer's
# business; absence is asserted against the raw bytes.
msg() { # requests-file, index → prints the prefix of the three files
  local out="$TMP/msg-$2"
  python3 - "$1" "$2" "$out" <<'PY'
import json, sys
src, index, out = sys.argv[1], int(sys.argv[2]), sys.argv[3]
reqs = json.load(open(src))["requests"]
if index >= len(reqs):
    print("no message #%d (only %d)" % (index, len(reqs)), file=sys.stderr)
    sys.exit(1)
req = reqs[index]
open(out + ".raw", "w").write(req["body"])
open(out + ".path", "w").write(req["path"])
values = []


def walk(v):
    if isinstance(v, str):
        values.append(v)
    elif isinstance(v, bool) or isinstance(v, (int, float)):
        values.append(str(v))
    elif isinstance(v, dict):
        for x in v.values():
            walk(x)
    elif isinstance(v, list):
        for x in v:
            walk(x)


try:
    walk(json.loads(req["body"]))
except Exception:                      # not JSON: treat the whole body as one value
    values.append(req["body"])
open(out + ".flat", "w").write(" ".join(values).replace("\n", " ") + "\n")
PY
  echo "$out"
}
show() { echo "--- POST $(cat "$1.path" 2>/dev/null) ---"; cat "$1.raw"; echo; echo "--- flattened ---"; cat "$1.flat"; }
has() { # msg-prefix, ERE, what
  grep -Eqi -- "$2" "$1.flat" || { show "$1"; fail "$3"; }
  echo "  ok: $3"
}
hasF() { # msg-prefix, fixed string, what
  grep -Fq -- "$2" "$1.flat" || { show "$1"; fail "$3"; }
  echo "  ok: $3"
}
hasnt() { # msg-prefix, fixed string, what
  if grep -Fq -- "$2" "$1.raw"; then show "$1"; fail "$3"; fi
  echo "  ok: $3"
}
next_msg() { # settle-seconds → wait for one more POST, hold the count, load it into $M
  MSGS=$((MSGS + 1))
  rx_wait "$MSGS" 90
  rx_hold "$MSGS" "$1"
  rx_save "$TMP/req-$MSGS.json"
  M=$(msg "$TMP/req-$MSGS.json" "$((MSGS - 1))") || fail "message #$MSGS has no body"
}
no_new_msg() { rx_hold "$MSGS" "$1"; }   # seconds during which nothing new may arrive

# Every Slack text object is `plain_text` except exactly one `mrkdwn` block: the fenced
# retrieval command, whose every value is admin-set or pattern-constrained (`safe_commands`
# enforces it in the renderer, and falls back to plain_text if it ever does not hold).
check_shape() { # msg-prefix
  python3 - "$1.raw" <<'PY' || { show "$1"; fail "the message's Block Kit envelope is not what the design allows"; }
import json, sys
body = json.load(open(sys.argv[1]))
assert isinstance(body.get("text"), str) and body["text"], "no fallback text: %r" % body.get("text")
blocks = body["blocks"]
kinds = [b["type"] for b in blocks]
assert kinds[:4] == ["header", "section", "context", "section"], "unexpected block order: %s" % kinds
# A 5th context block appears only when the route's rate cap was already spent, which this
# test never provokes.
assert len(blocks) == 4, "%d blocks (a rate-cap notice?): %s" % (len(blocks), kinds)
texts = []


def walk(v):
    if isinstance(v, dict):
        if v.get("type") in ("plain_text", "mrkdwn") and isinstance(v.get("text"), str):
            texts.append(v)
        for x in v.values():
            walk(x)
    elif isinstance(v, list):
        for x in v:
            walk(x)


walk(blocks)
assert len(texts) >= 4, "only %d text objects" % len(texts)
mrkdwn = [t for t in texts if t["type"] == "mrkdwn"]
assert len(mrkdwn) == 1, "%d mrkdwn text objects, want exactly one (the command block): %s" % (
    len(mrkdwn), [t["type"] for t in texts])
fenced = mrkdwn[0]["text"]
assert fenced.startswith("```") and fenced.rstrip().endswith("```"), \
    "the mrkdwn block is not fenced: %r" % fenced[:120]
assert "lapilli" in fenced, "the fenced block is not the retrieval command: %r" % fenced[:120]
print("  ok: %d text objects, all plain_text except the one fenced mrkdwn command block"
      % len(texts))
PY
}

# The group message's own structure: the header names the workload and the verdict, the context
# line carries the first three ids plus "+N more", and the command block covers EVERY capture.
check_group() { # msg-prefix, namespace, owner, reason, rule, exec-prefix, ids…
  python3 - "$@" <<'PY' || { show "$1"; fail "the group message does not say what the design requires"; }
import json, sys
prefix, namespace, owner, reason, rule, execpfx = sys.argv[1:7]
ids = sys.argv[7:]
body = json.load(open(prefix + ".raw"))
blocks = body["blocks"]
want_header = "%s/%s \u00b7 %s \u00d7%d" % (namespace, owner, reason, len(ids))
head = blocks[0]["text"]["text"]
assert head == want_header, "header is %r, want %r" % (head, want_header)
ctx = blocks[2]["elements"][0]["text"]
assert ctx.startswith("\U0001f4cb lapilli evidence \u00b7 alert " + rule), "context line: %r" % ctx
present = [i for i in ids if i in ctx]
assert len(present) == 3, \
    "%d of %d ids in the context line, want the first three: %r" % (len(present), len(ids), ctx)
assert "+%d more" % (len(ids) - 3) in ctx, 'no "+N more": %r' % ctx
assert "~ asserted by the client, not observed" in ctx, "no tilde note: %r" % ctx
cmd = blocks[3]["text"]["text"]
for i in ids:                      # round 12: the leader's bundle alone left ids nobody could act on
    assert i in cmd, "%s is not in the command block: %r" % (i, cmd)
assert "for id in " in cmd and "done" in cmd, "not a loop over every capture: %r" % cmd
assert execpfx in cmd, "%r is not in the command block: %r" % (execpfx, cmd)
assert "/usr/local/bin/lapilli cat-bundle" in cmd, "no absolute cat-bundle path: %r" % cmd
assert "lapilli verify " in cmd, "no verify command: %r" % cmd
print("  ok: header %r" % head)
print("  ok: context line names the rule, the first 3 of %d ids and \"+%d more\", with the "
      "tilde note" % (len(ids), len(ids) - 3))
print("  ok: command block loops over all %d captures, naming deploy/%s"
      % (len(ids), execpfx.split("deploy/")[1].split()[0]))
PY
}

# ------------------------------------------------------------------------------- metrics ---

scrape() { # → /metrics on stdout (empty if it could not be read)
  local out=""
  kubectl -n $KNS port-forward "deploy/$LAPILLI_DEPLOY" $MPORT:8081 >/dev/null 2>&1 &
  PF=$!
  for _ in $(seq 1 30); do curl -sf "localhost:$MPORT/metrics" >/dev/null 2>&1 && break; sleep 1; done
  out=$(curl -sf "localhost:$MPORT/metrics" || true)
  kill "$PF" 2>/dev/null || true
  wait "$PF" 2>/dev/null || true
  PF=""
  printf '%s\n' "$out"
}
metrics_now() { # sets $METRICS
  METRICS=$(scrape)
  [ -n "$METRICS" ] || fail "/metrics is not served"
}

# ------------------------------------------------------------------------------- route -----

route_json() { # detail, pathSecret name → the notify.routes JSON for --set-json
  local http=""
  if [ -n "$ROUTE_HTTP_KEY" ]; then http=",\"$ROUTE_HTTP_KEY\":true"; fi
  printf '[{"name":"%s","host":"%s","pathSecret":"%s","format":"slack","detail":"%s","maxPerWindow":10%s}]' \
    "$ROUTE" "$RX_HOST" "$2" "$1" "$http"
}
install_route() { # detail, pathSecret name
  local envjson='[]'
  if [ -n "$ALLOW_HTTP_ENV" ]; then envjson="[{\"name\":\"$ALLOW_HTTP_ENV\",\"value\":\"true\"}]"; fi
  # profile.notifyRoute is the chart's own wiring; the chart refuses a name no route defines.
  helm upgrade $RELEASE charts/lapilli -n $KNS --reuse-values \
    --set-json "notify.routes=$(route_json "$1" "$2")" --set-json "extraEnv=$envjson" \
    --set "profile.notifyRoute=$ROUTE" \
    `# the dispatcher's own tracing, so a message that never goes can be told apart from` \
    `# one that went and was refused` \
    `# --set splits on commas, so a RUST_LOG filter has to go in as JSON` \
    --set-json 'logLevel="info,lapilli_controller::notify=debug,kube=warn"' \
    --wait --timeout 180s >/dev/null \
    || fail "helm upgrade with notify.routes failed (detail=$1 pathSecret=$2)"
  # A CaptureProfile may only *name* a route, and that is how the capture finds one.
  [ "$(kubectl -n $KNS get captureprofile default -o jsonpath='{.spec.notify.route}')" = "$ROUTE" ] \
    || fail "the default CaptureProfile does not name the route (profile.notifyRoute)"
  LAPILLI_DEPLOY=$(kubectl -n $KNS get deploy -l app.kubernetes.io/name=lapilli \
    -o jsonpath='{.items[0].metadata.name}')
  CTRL=$(ctrl_pod "$KNS")
  echo "  route $ROUTE -> http://$RX_HOST$RX_PATH (detail: $1, pathSecret: $2)"
}

step "negative: a route whose Secret does not exist disables itself, it does not wedge the pod"
# The Secret volume is `optional: true` (round 12): one replica with Recreate means a mistyped
# Secret would otherwise leave the cluster with no evidence recorder at all. `helm --wait`
# returning is itself half the assertion — the pod became Ready without the Secret.
install_route facts "$HOOK_SECRET-does-not-exist"
metrics_now
grep -qxF 'lapilli_notify_routes{state="error"} 1' <<<"$METRICS" \
  || { echo "$METRICS" | grep -E '^lapilli_notify_routes' || echo "(no lapilli_notify_routes series)"; \
       fail "a route with no Secret is not reported as an error"; }
grep -qxF 'lapilli_notify_routes{state="ready"} 0' <<<"$METRICS" \
  || fail "a route with no Secret still counts as ready"
# This assertion failed once (2026-09-21) while being satisfied: the `fail` handler's own log dump,
# taken a moment later, contained the exact line the assertion said was missing. The cause is NOT
# established. `kubectl logs … | grep -q` was the shape, and it has two known hazards — `grep -q`
# exits on its match, so a still-writing producer takes EPIPE and `pipefail` fails a satisfied
# assertion; and `deploy/x` goes through a selector, where kubectl's `--tail` defaults to 10 lines
# instead of all of them. Neither explains this one: the pod had 8 log lines, which a shell producer
# writes into the pipe buffer without ever blocking (measured), and 8 < 10. So the shape is fixed
# because it is unsafe, not because it is the proven cause — and the failure now prints what it read,
# so the next occurrence settles it instead of being theorised about.
DISABLED_LOGS=$(ctl_logs)
grep -q "notification route disabled" <<<"$DISABLED_LOGS" || {
  echo "--- what this assertion read (${#DISABLED_LOGS} bytes) ---"
  printf '%s\n' "$DISABLED_LOGS"
  echo "--- pods in $KNS ---"
  kubectl -n $KNS get pods -o wide || true
  fail "the disabled route was not logged"
}
echo "  ok: pod Ready, route disabled, lapilli_notify_routes{state=\"error\"}=1"

step "admin defines the route; only the path segment comes from the Secret"
kubectl -n $KNS create secret generic $HOOK_SECRET --from-literal=path=$RX_PATH \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null
# maxPerWindow stays at the design's default 10; this script sends at most 5 messages, so the
# rate cap never fires here (it is unit-tested, and check_shape refuses a cap notice).
install_route facts "$HOOK_SECRET"
metrics_now
grep -qxF 'lapilli_notify_routes{state="ready"} 1' <<<"$METRICS" \
  || { echo "$METRICS" | grep -E '^lapilli_notify_routes' || echo "(no lapilli_notify_routes series)"; \
       fail "the loaded route is not reported as ready"; }
grep -qxF 'lapilli_notify_routes{state="error"} 0' <<<"$METRICS" || fail "a route is still in error"
echo "  ok: lapilli_notify_routes ready=1 error=0"

# ---------------------------------------------------------------------------- workload -----

step "a bad rollout: 5 replicas, only the image tag changed ($IMAGE_OLD -> $IMAGE_NEW)"
# Recreate, so all five pods really belong to the new revision (a RollingUpdate stalls when
# the new pods never become ready, and the group would then be a mix of revisions).
app() { # image
  kubectl apply --server-side --field-manager=$ACTOR -f - >/dev/null <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: { name: $APP, namespace: $NS }
spec:
  replicas: 5
  revisionHistoryLimit: 5
  strategy: { type: Recreate }
  selector: { matchLabels: { app: $APP } }
  template:
    metadata: { labels: { app: $APP } }
    spec:
      terminationGracePeriodSeconds: 1
      containers:
        - name: app
          image: $1
          command:
            - sh
            - -c
            - >-
              echo "boot notify-e2e";
              echo "log-canary=$LOG_CANARY";
              sleep 1;
              echo "$LAST_LINE";
              exit 42
          env:
            - { name: NOTIFY_E2E_CANARY, value: "$ENV_CANARY" }
EOF
}
crashing_pods() { # [image] — pods of $APP that have restarted at least once
  # The image filter is not cosmetic. With `strategy: Recreate`, applying a new template leaves
  # the *previous* revision's pods in place for a moment, and those are already crashlooping —
  # so a caller that just rolled the workload would count them, return immediately, and then
  # find nothing once Recreate deletes them. Naming the image pins the answer to the revision
  # the caller means.
  kubectl -n $NS get pods -l app=$APP -o json | IMAGE="${1:-}" python3 -c '
import json, os, sys
want = os.environ.get("IMAGE") or ""
for p in json.load(sys.stdin)["items"]:
    if p["metadata"].get("deletionTimestamp"):
        continue
    containers = (p.get("spec") or {}).get("containers") or []
    if want and (not containers or containers[0].get("image") != want):
        continue
    st = (p.get("status") or {}).get("containerStatuses") or []
    if st and st[0].get("restartCount", 0) >= 1:
        print(p["metadata"]["name"])'
}
wait_crashing() { # how many pods must be crashlooping, tries (2 s apart), [image]
  local n=0
  for _ in $(seq 1 "$2"); do
    n=$(crashing_pods "${3:-}" | wc -l | tr -d ' ')
    [ "$n" -ge "$1" ] && return 0
    sleep 2
  done
  kubectl -n $NS get pods -l app=$APP
  fail "only $n of $1 pods reached CrashLoopBackOff"
}
app "$IMAGE_OLD"
wait_crashing 5 90 "$IMAGE_OLD"
app "$IMAGE_NEW"      # revision 1 -> 2, attributed to $ACTOR
wait_crashing 5 120 "$IMAGE_NEW"
PODS=$(crashing_pods "$IMAGE_NEW" | head -5)
[ "$(echo "$PODS" | wc -l | tr -d ' ')" = 5 ] || fail "expected 5 crashlooping pods, got: $PODS"
echo "  5 pods crashlooping on revision 2:"; echo "$PODS" | sed 's/^/    /'

# Fire one Alertmanager alert from inside the controller pod (which holds the webhook token),
# the way run.sh and diffs.sh do. These set globals instead of printing: `fail` inside a
# command substitution would only kill the subshell and the script would sail on.
IC=""         # the IncidentCapture the last alert produced ("" = the webhook refused it)
FIRE_RC=0     # `lapilli post-alert`'s exit code
FIRE_OUT=""   # its response (or error), on one line
fire() { # rule, pod → sets IC / FIRE_RC / FIRE_OUT; never fails the script
  local resp
  set +e
  resp=$(printf '{"alerts":[{"status":"firing","labels":{"alertname":"%s","namespace":"%s","pod":"%s"}}]}' "$1" "$NS" "$2" |
    kubectl -n $KNS exec -i "$CTRL" -c controller -- /usr/local/bin/lapilli post-alert 2>&1)
  FIRE_RC=$?
  set -e
  FIRE_OUT=${resp//$'\n'/ }
  IC=""
  if [ "$FIRE_RC" = 0 ]; then
    IC=$(printf '%s' "$resp" | python3 -c 'import json,sys; print(json.load(sys.stdin)["captures"][0])' 2>/dev/null || true)
  fi
}
fire_ok() { # rule, pod → sets IC and remembers it for the cleanup; fails if refused
  fire "$1" "$2"
  [ -n "$IC" ] || fail "the webhook refused rule=$1 pod=$2: $FIRE_OUT"
  CAPTURES="$CAPTURES $IC"
}
phase() { kubectl -n $KNS get incidentcapture "$1" -o jsonpath='{.status.phase}' 2>/dev/null; }
wait_exported() { # capture, tries (2 s apart)
  for _ in $(seq 1 "$2"); do
    [ "$(phase "$1")" = Exported ] && return 0
    [ "$(phase "$1")" = Failed ] && fail "$1 failed: $(kubectl -n $KNS get incidentcapture "$1" -o jsonpath='{.status.message}')"
    sleep 2
  done
  fail "$1 never exported (phase $(phase "$1"))"
}
incident_of() { kubectl -n $KNS get incidentcapture "$1" -o jsonpath='{.spec.incidentId}'; }

# --------------------------------------------- one message per incident, facts only --------

step "one alert per pod (5 alerts, one rule) → exactly ONE message"
GROUP=""
for pod in $PODS; do
  fire_ok NotifyE2EGroup "$pod"
  GROUP="$GROUP $IC"
done
[ "$(echo $GROUP | wc -w | tr -d ' ')" = 5 ] || fail "5 alerts produced: $GROUP"
for ic in $GROUP; do wait_exported "$ic" 90; done
IDS=""
for ic in $GROUP; do IDS="$IDS $(incident_of "$ic")"; done
echo "  5 captures Exported:$IDS"
# The coalescing window is 30 s, reset by each new capture and capped at 2 minutes, so the
# message lands within ~2.5 minutes of the last seal; then nothing more may arrive, for long
# enough to cover a second window plus the retry budget.
next_msg 75
[ "$(cat "$M.path")" = "$RX_PATH" ] \
  || fail "the POST went to $(cat "$M.path"), not the Secret's path $RX_PATH"
echo "  ok: exactly one POST, to the Secret's path $RX_PATH"

step "the group message's shape and what it names"
check_shape "$M"
# shellcheck disable=SC2086
check_group "$M" "$NS" "Deployment/$APP" Error NotifyE2EGroup "$EXEC_PREFIX" $IDS

step "the default install's message carries no workload content"
hasnt "$M" "$LOG_CANARY" "the canary planted in the app's log stream is absent"
hasnt "$M" "$ENV_CANARY" "the canary planted in the app's env is absent"
hasnt "$M" "$LAST_LINE" "the app's last log line is absent"
hasnt "$M" "$LAST_WORDS_TOKEN" "not even a fragment of the last log line"
hasnt "$M" "$IMAGE_NEW" "with detail: facts, the changed image is not in the message"
hasnt "$M" "image" "with detail: facts, not even the changed field's name"

step "…while the facts are all there"
hasF "$M" "Error (exit 42)" "the termination reason and exit code"
has  "$M" '(^| )restart [1-9]' "the restart count"
has  "$M" '(all 5 pods: Error|[1-9] of 5 pods: Error)' "how many pods reported that reason"
# Either status is correct, and which one you get is not under this test's control: the previous
# instance's log comes back about 7 times in 10 on kind (round 35 §3). `notify.rs:348-349` is the
# only place that decides, and the property worth gating is that the message reports *which* —
# never that it stays silent about the headline evidence. Asserting only the Captured branch made
# this a latent 3-in-10 failure of a required check.
has "$M" "last log line (is in the bundle|already discarded by the kubelet)" \
  "whether the last words survived (the status, not the line)"
# The revisions and the timing ARE asserted here, and the reason this comment exists is that they
# were not, for a wrong reason. An earlier note here recorded them as null and blamed the diffs
# collector — "a Deployment whose pods never become Ready stays progressing, so the collector
# cannot establish the previous revision". That was read through a broken reader. `diffs.rs` writes
# the revisions nested under `before`/`after` and the timing as `seconds_relative_to_firing`; the
# summary reader looked for `revision_from`, `revision_to` and `seconds_before_alert`, which no
# producer has ever written. So the facts were in every bundle and nothing could read them, and the
# E2E documented the symptom as a limitation of the collector that produced them correctly.
has  "$M" "Deployment/$APP rev [0-9]+ → [0-9]+ changed" "the rollout, with both revisions"
has  "$M" "[0-9]+s before the alert" "when the change landed, and on the right side of the alert"
hasF "$M" "by ~$ACTOR" "the actor, marked as client-asserted with ~"
hasF "$M" "~ asserted by the client, not observed" "what the ~ means, said once"
# NOTE(implementer): `PARTIAL: … did not run` leads the verdict whenever a collector is
# intended and did not run (the metrics collector on a pod with no memory limit, say). It is
# correct either way here, so it is deliberately not asserted.

step "the claim is taken for EVERY member, and the summary sidecar holds no log line"
# Round 12: claiming only the leader left the other members unclaimed, and each re-announced
# the incident on its next reconcile — a watch relist was enough. Same node path kms.sh uses.
node_ls() { docker exec lapilli-control-plane sh -c "ls /var/local-path-provisioner/*/ 2>/dev/null" | sort -u; }
node_cat() { docker exec lapilli-control-plane sh -c "cat /var/local-path-provisioner/*/$1 2>/dev/null"; }
LISTING=$(node_ls || true)
[ -n "$LISTING" ] || fail "could not list the bundle PVC on the kind node"
CLAIMS=0
for id in $IDS; do
  grep -qxF "$id.summary.json" <<<"$LISTING" \
    || { echo "$LISTING" | head -40; fail "no $id.summary.json next to the bundles"; }
  if grep -qxF "$id.notified" <<<"$LISTING"; then CLAIMS=$((CLAIMS + 1)); fi
done
[ "$CLAIMS" = 5 ] || fail "$CLAIMS of 5 members claimed: an unclaimed member re-announces the incident"
echo "  ok: 5 summary.json sidecars, and all 5 members claimed"
# Round 12, item 6: the sidecar sits outside the hash tree and the signature, nothing prunes
# it, so a `.ieb` deleted for retention must not leave the container's last words behind it.
for id in $IDS; do
  SIDECAR=$(node_cat "$id.summary.json" || true)
  [ -n "$SIDECAR" ] || fail "could not read $id.summary.json from the node"
  for leak in "$LAST_LINE" "$LAST_WORDS_TOKEN" "$LOG_CANARY"; do
    case "$SIDECAR" in
      *"$leak"*) printf '%s\n' "$SIDECAR" | head -c 2000
                 fail "$id.summary.json carries workload text ($leak)" ;;
    esac
  done
done
echo "  ok: no summary.json sidecar carries the log line (they outlive the bundle, unsigned)"

# ------------------------------------------------------------------ detail: content --------

step "detail: content shows the changed image tag — and still no canary"
install_route content "$HOOK_SECRET"
CPOD=$(echo "$PODS" | sed -n 1p)
fire_ok NotifyE2EContent "$CPOD"
wait_exported "$IC" 90
next_msg 45
check_shape "$M"
hasF "$M" "image" "the changed field is named"
has  "$M" "image: busybox:1\.3[67] → busybox:1\.3[67]" "the changed image tag, before → after"
hasF "$M" " · pod $CPOD" "a single-pod message names the pod"
hasnt "$M" "$LOG_CANARY" "detail: content still carries no log canary"
hasnt "$M" "$ENV_CANARY" "detail: content still carries no env canary"
hasnt "$M" "$LAST_LINE" "detail: content still never carries the last log line"
hasnt "$M" "$LAST_WORDS_TOKEN" "not even a fragment of it"

# ------------------------------------------------------- repeat suppression ----------------
# From here on there is no helm upgrade: the cooldown lives in the dispatcher's memory, so a
# controller restart is a fresh start and would (correctly) announce the incident again.

step "the same verdict on the same workload is counted, not posted again"
RPOD_A=$(echo "$PODS" | sed -n 2p)
RPOD_B=$(echo "$PODS" | sed -n 3p)
fire_ok NotifyE2ERepeat "$RPOD_A"
wait_exported "$IC" 90
next_msg 20                       # the incident is announced once
echo "  announced: $(head -c 120 "$M.flat")…"
fire_ok NotifyE2ERepeat "$RPOD_B" # same rule, same workload → same group, same verdict
REPEAT_IC=$IC
wait_exported "$REPEAT_IC" 90
# One coalescing window (30 s, up to 120 s) plus the retry budget: a message would be here.
no_new_msg 150
echo "  ok: the second firing of the same verdict produced no POST (the cooldown is 30 minutes)"
# A counted repeat is now claimed and reported (`state: repeat`, `reason: in-cooldown`), which
# this asserts — that was a real defect this script found: without the claim the capture
# re-enqueued on every reconcile, and once the group's rollout moved on it no longer matched the
# cooldown entry and would have been announced as if it were news.
grep -qx repeat <<<"$(kubectl -n $KNS get incidentcapture "$REPEAT_IC" \
  -o jsonpath='{.status.notification.state}')" \
  || fail "a counted repeat must be recorded as state=repeat on the capture"
# Deleted here rather than in cleanup so the next step's rollout starts from a clean slate.
kubectl -n $KNS delete incidentcapture "$REPEAT_IC" >/dev/null

step "the counted repeat rides on the next message for that workload"
# A new rollout is news, so the same group key posts again — carrying the repeat it swallowed.
app "$IMAGE_OLD"                  # revision 2 -> 3, image busybox:1.37 -> busybox:1.36
wait_crashing 5 150 "$IMAGE_OLD"
RPOD_C=$(crashing_pods "$IMAGE_OLD" | head -1)
fire_ok NotifyE2ERepeat "$RPOD_C"
wait_exported "$IC" 120
next_msg 30
check_shape "$M"
# The rollback reuses revision 1's ReplicaSet, which Kubernetes then renumbers, so assert that
# a *new* revision is reported rather than betting on the number.
# Not `rev 2 → N`: this workload's revisions are unknown (see the NOTE above), so what makes
# this posting news is the *changed value*, which the cooldown key fingerprints. That fallback
# is the point of the assertion — without it a rollback during a crashloop would stay silent
# for the whole 30-minute cooldown.
has  "$M" "Deployment/$APP rev [0-9]+ → [0-9]+ changed" "a new rollout — which is why this posted"
has  "$M" '×[0-9]+ more since [0-9]{2}:[0-9]{2} UTC' "the swallowed repeat(s), with a since time"
echo "  reported: $(grep -Eo '×[0-9]+ more since [0-9]{2}:[0-9]{2} UTC' "$M.flat" | head -1)"

# ---------------------------------------------------------------- injection ----------------

step "injection: <!channel> in alertname and <url|label> in the pod label"
BEFORE=$(rx_count)
[ "$BEFORE" -ge 0 ] || fail "the receiver is unreachable before the injection scenario"
fire "$INJ_RULE" "$RPOD_C"
if [ -n "$IC" ]; then
  echo "  the webhook ACCEPTED the crafted alertname ($IC) — the message must escape it"
  CAPTURES="$CAPTURES $IC"
else
  # Expected: the CRD's own pattern on spec.trigger.rule is ^[^<>&]{1,200}$, so the API
  # refuses the object and the webhook answers non-200.
  echo "  the crafted alertname was REFUSED before any capture existed (rc=$FIRE_RC): $FIRE_OUT"
fi
fire NotifyE2EInjectPod "$INJ_POD"
if [ -n "$IC" ]; then
  echo "  the webhook ACCEPTED the crafted pod label ($IC) — the message must escape it"
  CAPTURES="$CAPTURES $IC"
else
  echo "  the crafted pod label was REFUSED (rc=$FIRE_RC): $FIRE_OUT"
fi
# NOTE(implementer): a capture for a pod that does not exist produces an empty summary, and an
# empty summary is never announced, so the crafted `pod` label usually yields no message at all
# rather than an escaped one. Kubernetes will not let a real pod carry `<`, so the escaping
# itself is only reachable in notify.rs's unit test (`strings_an_alert_author_chose_cannot_…`),
# which crafts key.rule and key.owner directly. What this scenario proves end to end is the
# invariant: whatever the input, no request body carries the raw vectors.
sleep 90
NOW=$(rx_count)
[ "$NOW" -ge 0 ] || fail "the receiver went away during the injection scenario"
if [ "$NOW" -gt "$BEFORE" ]; then
  echo "  $((NOW - BEFORE)) message(s) arrived for the crafted input; checking the escaping"
  rx_save "$TMP/req-inject.json"
  i=$BEFORE
  while [ "$i" -lt "$NOW" ]; do
    MI=$(msg "$TMP/req-inject.json" "$i") || fail "could not read message #$i"
    check_shape "$MI"
    hasnt "$MI" '<!channel>' "no unescaped <!channel> in message #$i"
    hasnt "$MI" '<https://evil.example|click>' "no unescaped <url|label> in message #$i"
    # Escaped, not silently dropped: if the crafted text is carried, it is carried as entities.
    if grep -Fq '!channel' "$MI.raw"; then
      grep -Fq '&lt;!channel&gt;' "$MI.raw" \
        || { show "$MI"; fail "message #$i carries the crafted rule but not as &lt;!channel&gt;"; }
      echo "  ok: the crafted rule appears as &lt;!channel&gt;"
    fi
    if grep -Fq 'evil.example' "$MI.raw"; then
      grep -Fq '&lt;https://evil.example|click&gt;' "$MI.raw" \
        || { show "$MI"; fail "message #$i carries the crafted pod but not as &lt;…&gt;"; }
      echo "  ok: the crafted pod label appears escaped, not as a link"
    fi
    i=$((i + 1))
  done
  MSGS=$NOW
  echo "  outcome: the crafted input was rendered, escaped (not refused)"
else
  echo "  outcome: no message at all for the crafted input"
fi
# Whatever happened, the whole request log must be free of the raw vectors — including the
# `<url|label>` form, which only a `mrkdwn` block could act on.
rx_save "$TMP/req-all.json"
python3 - "$TMP/req-all.json" '<!channel>' '<!here>' '<https://' <<'PY' || fail "an injection vector reached the receiver unescaped"
import json, sys
bad = []
for n, r in enumerate(json.load(open(sys.argv[1]))["requests"]):
    for needle in sys.argv[2:]:
        if needle in r["body"]:
            bad.append((n, needle, r["body"][:2000]))
for n, needle, body in bad:
    print("message #%d contains %r:\n%s" % (n, needle, body))
sys.exit(1 if bad else 0)
PY
echo "  ok: no request body contains a raw <!channel>, <!here> or <https://…|…>"

# --------------------------------------------------------- receiver down ------------------

step "receiver down: the capture still exports, the notification is recorded as failed"
kubectl -n $NS scale deploy/$RX_SVC --replicas=0 >/dev/null
for _ in $(seq 1 60); do
  [ -z "$(kubectl -n $NS get pods -l app=$RX_SVC -o name 2>/dev/null)" ] && break
  sleep 2
done
[ -z "$(kubectl -n $NS get pods -l app=$RX_SVC -o name 2>/dev/null)" ] || fail "the receiver did not go away"
fire_ok NotifyE2EDown "$RPOD_C"
DOWN=$IC
wait_exported "$DOWN" 120
echo "  ok: the capture reached Exported with the receiver gone"
NSTATE=""
# 5 s timeout + 2 retries over ~10 s, after a coalescing window of up to 30 s.
for _ in $(seq 1 90); do
  NSTATE=$(kubectl -n $KNS get incidentcapture "$DOWN" -o jsonpath='{.status.notification.state}' 2>/dev/null || true)
  [ -n "$NSTATE" ] && [ "$NSTATE" != pending ] && break
  sleep 2
done
[ "$NSTATE" = failed ] \
  || fail "status.notification.state is '${NSTATE:-<empty>}', want failed: $(kubectl -n $KNS get incidentcapture "$DOWN" -o jsonpath='{.status.notification}')"
NREASON=$(kubectl -n $KNS get incidentcapture "$DOWN" -o jsonpath='{.status.notification.reason}')
NAT=$(kubectl -n $KNS get incidentcapture "$DOWN" -o jsonpath='{.status.notification.at}')
NROUTE=$(kubectl -n $KNS get incidentcapture "$DOWN" -o jsonpath='{.status.notification.route}')
[ -n "$NAT" ] || fail "status.notification.at is empty"
[ "$NROUTE" = "$ROUTE" ] || fail "status.notification.route is '$NROUTE', want $ROUTE"
# A fixed code, never transport text: a 4xx body can quote the request that was sent.
case "$NREASON" in *' '*) fail "status.notification.reason '$NREASON' is prose, not a code" ;; esac
case " $REASON_CODES " in
  *" $NREASON "*) ;;
  *) fail "status.notification.reason '$NREASON' is not one of the fixed codes: $REASON_CODES" ;;
esac
echo "  ok: status.notification = {state: failed, reason: $NREASON, route: $NROUTE, at: $NAT}"
# The printer column, so an operator sees it without -o yaml.
GET_OUT=$(kubectl -n $KNS get incidentcapture "$DOWN")
grep -q NOTIFY <<<"$(printf '%s\n' "$GET_OUT" | sed -n 1p)" \
  || { printf '%s\n' "$GET_OUT"; fail "no NOTIFY printer column (was crds.json regenerated?)"; }
grep -q failed <<<"$(printf '%s\n' "$GET_OUT" | sed -n 2p)" \
  || { printf '%s\n' "$GET_OUT"; fail "the NOTIFY column does not show failed"; }
echo "  ok: kubectl get incidentcapture shows NOTIFY=failed"
if [ -n "$LAPILLI" ]; then
  BP=$(kubectl -n $KNS get incidentcapture "$DOWN" -o jsonpath='{.status.bundlePath}')
  CID=$(kubectl -n $KNS get incidentcapture "$DOWN" -o jsonpath='{.spec.clusterId}')
  DID=$(incident_of "$DOWN")
  kubectl -n $KNS exec "$(ctrl_pod "$KNS")" -c controller -- \
    /usr/local/bin/lapilli cat-bundle "$BP" > "$TMP/down.ieb" || fail "no bundle for $DOWN"
  "$LAPILLI" verify "$TMP/down.ieb" --cluster "$CID" --incident "$DID" >/dev/null \
    || fail "a failed notification damaged the bundle"
  echo "  ok: the bundle still verifies (notification never touches the capture)"
fi

step "metrics: lapilli_notifications_total counts the send, the repeat and the failure"
metrics_now
grep -qE '^lapilli_notifications_total\{result="failed"\} [1-9]' <<<"$METRICS" \
  || { echo "$METRICS" | grep -E '^lapilli_notifications_total' || echo "(no lapilli_notifications_total series)"; \
       fail "lapilli_notifications_total{result=\"failed\"} did not move"; }
grep -qE '^lapilli_notifications_total\{result="sent"\} [1-9]' <<<"$METRICS" \
  || { echo "$METRICS" | grep -E '^lapilli_notifications_total'; fail "no notification counted as sent"; }
grep -qE '^lapilli_notifications_total\{result="repeat"\} [1-9]' <<<"$METRICS" \
  || { echo "$METRICS" | grep -E '^lapilli_notifications_total'; fail "the suppressed repeat was not counted"; }
grep -qxF 'lapilli_notify_routes{state="ready"} 1' <<<"$METRICS" \
  || fail "lapilli_notify_routes no longer reports the route as ready"
echo "  ok: $(echo "$METRICS" | grep -E '^lapilli_notifications_total\{result="(sent|repeat|failed)"\}' | tr '\n' ' ')"

# --------------------------------------------------------- rollout -------------------------

step "a rollout does not lose a group that is still coalescing"
# Last of the behavioural steps on purpose: deleting the controller pod resets every counter, so
# it has to come after the metrics assertions. The receiver-down step left the receiver at 0.
kubectl -n $NS scale deploy/$RX_SVC --replicas=1 >/dev/null
kubectl -n $NS rollout status deploy/$RX_SVC --timeout=180s >/dev/null
# The receiver keeps its request log in memory, so scaling it to 0 and back gives a FRESH one:
# resync the expected-message baseline or every later index is off by what the old pod saw.
for _ in $(seq 1 60); do
  [ "$(rx_count)" -ge 0 ] && break
  sleep 2
done
[ "$(rx_count)" -ge 0 ] || fail "the restarted receiver never became reachable"
MSGS=$(rx_count)
# Kubernetes sends SIGTERM on every rollout, and a group is claimed before it is posted, so a
# group abandoned on the way out would be marked notified and never announced. Fire, then delete
# the pod INSIDE the coalescing window (30 s) and check the message still arrives.
ROLL_POD=$(crashing_pods "$IMAGE_OLD" | head -1)
fire_ok NotifyE2EDrain "$ROLL_POD"
DRAIN_IC=$IC
wait_exported "$DRAIN_IC" 120
BEFORE=$(rx_count)
CTRL=$(ctrl_pod $KNS)
# The pod about to die is the only witness to the flush, and its log goes with it: `fail`
# reads `deploy/…`, which resolves to the *replacement* pod. When this step failed in CI
# (2026-09-26, kind 1.37) the printed log was the new pod's startup, which said nothing about
# why the message never came. So follow the old pod's log from before the delete, and on
# failure print it together with the two things that outlive the pod: the drained capture's
# `status.notification` (the dispatcher writes the outcome there, `failed`/`unreachable` if
# the flush POST was refused) and the receiver's own request log.
kubectl -n $KNS logs -f "$CTRL" -c controller --tail=0 > "$TMP/ctrl-terminated.log" 2>&1 &
LOGF=$!
rollout_diagnostics() {
  echo "--- the terminated controller pod ($CTRL), from the delete on ---"
  sleep 3   # the follower may still be receiving the last lines
  kill "$LOGF" 2>/dev/null || true; wait "$LOGF" 2>/dev/null || true; LOGF=""
  cat "$TMP/ctrl-terminated.log"
  echo "--- the drained capture $DRAIN_IC ---"
  kubectl -n $KNS get incidentcapture "$DRAIN_IC" \
    -o jsonpath='created={.metadata.creationTimestamp} phase={.status.phase} notification={.status.notification}{"\n"}' || true
  echo "--- the receiver's log ---"
  kubectl -n $NS logs "deploy/$RX_SVC" --tail=20 || true
  echo "--- the replacement controller pod ($(ctrl_pod $KNS)) ---"
}
ON_FAIL=rollout_diagnostics
echo "  $(now) deleting $CTRL with $DRAIN_IC exported and $BEFORE POST(s) at the receiver"
kubectl -n $KNS delete pod "$CTRL" --wait=false >/dev/null
# The flush is bounded at 10 s and the window is 30 s, so the message must beat the window.
for _ in $(seq 1 30); do
  [ "$(rx_count)" -gt "$BEFORE" ] && break
  sleep 2
done
[ "$(rx_count)" -gt "$BEFORE" ] \
  || fail "the group was lost when the controller was terminated (SIGTERM flush)"
ON_FAIL=""
echo "  $(now) the message arrived"
MSGS=$((MSGS + 1))
rx_save "$TMP/req-$MSGS.json"
M=$(msg "$TMP/req-$MSGS.json" "$((MSGS - 1))") || fail "the drained message has no body"
hasF "$M" "$APP" "the drained message names the workload"
# Say which path delivered it, so a pass can be read too: the flush, or a replay of a flush
# that failed (the controller hands a group it could not post to its successor).
kill "$LOGF" 2>/dev/null || true; wait "$LOGF" 2>/dev/null || true; LOGF=""
grep -E "shutting down|flushing notification groups|notification (sent|failed)|handed to the next" \
  "$TMP/ctrl-terminated.log" | sed 's/^/    old pod: /' || true
echo "  ok: the coalescing group was flushed on SIGTERM, not abandoned"
# Wait on the Deployment, not on a pod label: the pod that was just deleted is still listed for
# a moment, and `kubectl wait` picks it and then fails when it disappears.
kubectl -n $KNS rollout status "deploy/$(kubectl -n $KNS get deploy \
  -l app.kubernetes.io/name=lapilli -o jsonpath='{.items[0].metadata.name}')" --timeout=180s >/dev/null
CTRL=$(ctrl_pod $KNS)

step "a rollout whose flush is refused: the group is handed to the next process, not lost"
# The flush above gets ONE attempt inside 3 s, the group is claimed before it, and the next
# process files every capture created before it started as history. So a receiver unreachable
# for exactly that moment lost the message for good — which is what the step above did on CI
# (2026-09-26, kind 1.37): the receiver had been scaled 0→1 seconds earlier. Make the moment
# certain instead of hoping for it: point the Service at nothing while the pod is terminated,
# put it back once the flush has failed, and require the next process to post the group.
# This also covers the other way the same group was lost, found while reproducing: after a pod
# *deletion* the replacement starts before the old pod's SIGTERM (measured +0.56 s vs +1.33 s),
# files the capture as history and claims it, and the old pod's flush used to read that claim as
# an announcement. Whichever pod claims first, the old pod must hand the group over and the new
# pod must post it — its periodic hand-off scan (30 s) is what makes the 60 s wait enough.
ROLL_POD=$(crashing_pods "$IMAGE_OLD" | head -1)
fire_ok NotifyE2EHandoff "$ROLL_POD"
DRAIN_IC=$IC
wait_exported "$DRAIN_IC" 120
BEFORE=$(rx_count)
CTRL=$(ctrl_pod $KNS)
kubectl -n $KNS logs -f "$CTRL" -c controller --tail=0 > "$TMP/ctrl-terminated.log" 2>&1 &
LOGF=$!
ON_FAIL=rollout_diagnostics
kubectl -n $NS patch svc $RX_SVC -p '{"spec":{"selector":{"app":"nobody"}}}' >/dev/null
# Patching the selector is not the same as the route being unreachable: kube-proxy has to write
# the change, and until it does the endpoint still answers. This step failed in CI with the flush
# SUCCEEDING 675 ms after the patch and ~18 ms after SIGTERM, which left no hand-off to find and
# read as a product defect. So wait for the break to be OBSERVED before deleting the pod —
# `rx_count` returns -1 exactly when the receiver cannot be reached — which is what the comment
# above means by making it certain instead of hoping for it.
for _ in $(seq 1 60); do
  [ "$(rx_count)" = "-1" ] && break
  sleep 0.5
done
[ "$(rx_count)" = "-1" ] \
  || fail "the receiver is still reachable 30 s after pointing its Service at nobody; this scenario cannot set up its own precondition"
echo "  $(now) receiver unreachable confirmed; deleting $CTRL with $DRAIN_IC exported"
kubectl -n $KNS delete pod "$CTRL" --wait=false >/dev/null
# Restore the Service only once the old pod's flush has been REFUSED — SIGTERM has landed
# anywhere from 0.4 s to 1.3 s after the delete here, and restoring early would let the flush
# succeed and prove nothing. The refused attempt fails at once, so this is a short wait.
#
# `notification sent` is deliberately NOT a reason to stop waiting. It used to be, and that is
# the bug above: a successful flush broke this loop, the Service was restored, and the hand-off
# assertion below then failed for a group that was never refused in the first place.
for _ in $(seq 1 60); do
  grep -qE "notification failed|could not post" "$TMP/ctrl-terminated.log" && break
  sleep 0.5
done
grep -qE "notification failed|could not post" "$TMP/ctrl-terminated.log" \
  || { cat "$TMP/ctrl-terminated.log"; fail "the old pod's shutdown flush was never refused, so the hand-off path was not exercised; the precondition broke, not the product"; }
kubectl -n $NS patch svc $RX_SVC -p "{\"spec\":{\"selector\":{\"app\":\"$RX_SVC\"}}}" >/dev/null
echo "  $(now) receiver Service restored"
for _ in $(seq 1 30); do
  [ "$(rx_count)" -gt "$BEFORE" ] && break
  sleep 2
done
[ "$(rx_count)" -gt "$BEFORE" ] \
  || fail "the group whose flush was refused was lost instead of being handed to the next process"
ON_FAIL=""
echo "  $(now) the message arrived"
MSGS=$((MSGS + 1))
rx_save "$TMP/req-$MSGS.json"
M=$(msg "$TMP/req-$MSGS.json" "$((MSGS - 1))") || fail "the handed-over message has no body"
hasF "$M" "$APP" "the handed-over message names the workload"
kill "$LOGF" 2>/dev/null || true; wait "$LOGF" 2>/dev/null || true; LOGF=""
# The path it took, both halves: the old pod's flush failed and handed over, the new pod re-sent.
grep -E "notification failed|handed to the next" "$TMP/ctrl-terminated.log" | sed 's/^/    old pod: /' || true
grep -q "handed to the next process" "$TMP/ctrl-terminated.log" \
  || { cat "$TMP/ctrl-terminated.log"; fail "the old pod did not hand the group over (was its flush refused at all?)"; }
kubectl -n $KNS rollout status "deploy/$LAPILLI_DEPLOY" --timeout=180s >/dev/null
CTRL=$(ctrl_pod $KNS)
ctl_logs 200 | grep -q "re-sending a notification the previous process could not post" \
  || fail "the new pod did not report re-sending the handed-over group"
NSTATE=$(kubectl -n $KNS get incidentcapture "$DRAIN_IC" -o jsonpath='{.status.notification.state}')
[ "$NSTATE" = sent ] \
  || fail "status.notification.state is '$NSTATE' after the replay, want sent"
echo "  ok: the refused flush was handed over and posted by the next process; status ends as sent"

# -------------------------------------------------------------------- cleanup -------------

step "cleanup: the route, the receiver and the workload go away again"
if [ -n "$CAPTURES" ]; then
  # --ignore-not-found: the repeat scenario deletes its own capture as soon as it has asserted.
  # shellcheck disable=SC2086
  kubectl -n $KNS delete incidentcapture --ignore-not-found $CAPTURES >/dev/null
fi
helm upgrade $RELEASE charts/lapilli -n $KNS --reuse-values \
  --set-json 'notify.routes=[]' --set-json 'extraEnv=[]' --set profile.notifyRoute= \
  --wait --timeout 180s >/dev/null
kubectl -n $KNS delete secret $HOOK_SECRET >/dev/null
kubectl delete namespace $NS --wait=false >/dev/null
LAPILLI_DEPLOY=$(kubectl -n $KNS get deploy -l app.kubernetes.io/name=lapilli -o jsonpath='{.items[0].metadata.name}')
[ -z "$(kubectl -n $KNS get captureprofile default -o jsonpath='{.spec.notify.route}' 2>/dev/null)" ] \
  || fail "the default profile still names a route"
helm get values $RELEASE -n $KNS -o json | python3 -c '
import json, sys
routes = (json.load(sys.stdin).get("notify") or {}).get("routes") or []
sys.exit(0 if routes == [] else 1)' || fail "notify.routes is not empty after the cleanup"
# "Notification is off" and "notification is broken" must never be the same reading, so the
# gauge is absent entirely when no route is configured.
metrics_now
if grep -qE '^lapilli_notify_routes' <<<"$METRICS"; then
  echo "$METRICS" | grep -E '^lapilli_notify_routes'
  fail "lapilli_notify_routes is still exported with no route configured"
fi
grep -qE '^lapilli_notifications_total' <<<"$METRICS" \
  || fail "lapilli_notifications_total disappeared (it is always exported)"
echo "  ok: no route, no hook Secret, namespace $NS deleting, lapilli_notify_routes absent"

echo; echo "notify scenarios OK"
