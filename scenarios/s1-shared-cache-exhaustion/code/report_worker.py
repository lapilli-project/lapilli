import socket, time, os, random
MODE = os.environ.get("CACHE_CONN_MODE", "pooled"); INTERVAL = float(os.environ.get("TASK_INTERVAL_SECONDS", "2")); HOST = ("cache", 6379)
def ts(): return time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())
class CacheClient:
    def __init__(self):
        self._shared = None
        self._held = []          # connections kept "for reuse" by later tasks
    def get(self):
        if MODE == "pooled":
            if self._shared is None: self._shared = socket.create_connection(HOST, timeout=3.0)
            return self._shared
        conn = socket.create_connection(HOST, timeout=3.0)   # per-task: a fresh connection for every task
        self._held.append(conn)
        return conn
cache = CacheClient(); n = 0
print(f"{ts()} INFO report-worker starting cache_conn_mode={MODE} task_interval_seconds={INTERVAL}", flush=True)
while True:
    n += 1
    try:
        c = cache.get(); c.settimeout(3.0); t0 = time.time(); c.sendall(b"PING\r\n")
        if not c.recv(64): raise ConnectionError("closed")
        if n % 5 == 0: print(f"{ts()} INFO report task {n} done rows={random.randint(100,900)} cache_ms={int((time.time()-t0)*1000)}", flush=True)
    except Exception as e:
        print(f"{ts()} WARN report task {n} slow: cache did not answer within 3.0s [{type(e).__name__}]", flush=True)
        if MODE != "pooled" and cache._held:
            try: cache._held.pop().close()
            except Exception: pass
    time.sleep(INTERVAL)
