import os, time, threading
from http.server import ThreadingHTTPServer, BaseHTTPRequestHandler
TUNING = "/etc/ledger-tuning/tuning.conf"; NEEDED = 200; state = {"ok": True}
def ts(): return time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())
def read_limit():
    try:
        for line in open(TUNING):
            k, _, v = line.strip().partition("=")
            if k == "max_open_files": return int(v)
    except Exception: pass
    return 65536
def loop():
    n = 0
    while True:
        limit = read_limit(); n += 1
        if limit < NEEDED:
            state["ok"] = False
            print(f"{ts()} ERROR ledger-api accept failed: [Errno 24] Too many open files", flush=True)
        else:
            state["ok"] = True
            if n % 6 == 0: print(f"{ts()} INFO ledger-api posted batch entries={NEEDED} version={os.environ.get('APP_VERSION')}", flush=True)
        time.sleep(10)
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        ok = state["ok"]; self.send_response(200 if ok else 503); self.end_headers(); self.wfile.write(b"ok" if ok else b"degraded")
print(f"{ts()} INFO ledger-api starting version={os.environ.get('APP_VERSION')}", flush=True)
threading.Thread(target=loop, daemon=True).start()
ThreadingHTTPServer(("0.0.0.0", 8080), H).serve_forever()
