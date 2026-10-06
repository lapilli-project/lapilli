import threading, time, os, collections
from http.server import ThreadingHTTPServer, BaseHTTPRequestHandler
from urllib.parse import urlparse, parse_qs
WORKERS = int(os.environ.get("RENDER_WORKERS", "4")); sem = threading.Semaphore(WORKERS)
lock = threading.Lock(); reqs = collections.Counter(); inflight = 0; rejected_logged = 0
def ts(): return time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())
class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.0"
    def log_message(self, *a): pass
    def do_GET(self):
        global inflight, rejected_logged
        u = urlparse(self.path)
        if u.path == "/metrics":
            with lock:
                lines = ["# TYPE thumb_requests_total counter"] + [f'thumb_requests_total{{client="{c}",code="{k}"}} {v}' for (c, k), v in sorted(reqs.items())]
                lines += ["# TYPE thumb_inflight gauge", f"thumb_inflight {inflight}", "# TYPE thumb_render_workers gauge", f"thumb_render_workers {WORKERS}"]
            body = ("\n".join(lines) + "\n").encode(); self.send_response(200); self.send_header("Content-Type", "text/plain; version=0.0.4"); self.end_headers(); self.wfile.write(body); return
        if u.path == "/healthz": self.send_response(200); self.end_headers(); self.wfile.write(b"ok"); return
        client = (parse_qs(u.query).get("client") or ["unknown"])[0]
        with lock: inflight += 1
        got = sem.acquire(timeout=1.0)
        try:
            if not got:
                with lock:
                    reqs[(client, "503")] += 1; rejected_logged += 1; n = rejected_logged
                if n % 25 == 1: print(f"{ts()} WARN render rejected: no free render worker within 1.0s (workers={WORKERS})", flush=True)
                self.send_response(503); self.end_headers(); self.wfile.write(b"busy"); return
            time.sleep(0.15)
            with lock: reqs[(client, "200")] += 1
            self.send_response(200); self.end_headers(); self.wfile.write(b"thumb")
        finally:
            if got: sem.release()
            with lock: inflight -= 1
print(f"{ts()} INFO thumb-api starting render_workers={WORKERS} cache_size={os.environ.get('THUMB_CACHE_SIZE','256')}", flush=True)
srv = ThreadingHTTPServer(("0.0.0.0", 8080), H); srv.daemon_threads = True; srv.serve_forever()
