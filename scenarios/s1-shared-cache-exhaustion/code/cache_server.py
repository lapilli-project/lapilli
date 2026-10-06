import socket, threading, time, os, collections
MAX = int(os.environ.get("MAX_ACTIVE", "40"))
lock = threading.Lock(); active = {}; waiting = collections.deque(); sem = threading.BoundedSemaphore(MAX)
def handle(conn):
    try:
        f = conn.makefile("rwb", buffering=0)
        while True:
            line = f.readline()
            if not line: break
            f.write(b"+PONG\r\n")
    except Exception: pass
    finally:
        with lock: active.pop(conn, None)
        try: conn.close()
        except Exception: pass
        sem.release()
def acceptor():
    s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); s.bind(("0.0.0.0", 6379)); s.listen(512)
    while True:
        conn, addr = s.accept()
        with lock:
            waiting.append((conn, addr[0]))
            while len(waiting) > 150:            # oldest unserved connection is dropped
                old, _ = waiting.popleft()
                try: old.close()
                except Exception: pass
def dispatcher():
    while True:
        with lock: has = bool(waiting)
        if not has: time.sleep(0.05); continue
        if sem.acquire(timeout=0.2):
            with lock:
                conn, ip = waiting.popleft(); active[conn] = ip
            threading.Thread(target=handle, args=(conn,), daemon=True).start()
def reporter():
    while True:
        time.sleep(10)
        with lock:
            peers = collections.Counter(active.values()); q = len(waiting)
        tbl = ",".join(f"{ip}:{n}" for ip, n in peers.most_common())
        print(f"{time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())} conn-table active={sum(peers.values())}/{MAX} queued={q} peers={tbl or '-'}", flush=True)
print(f"cache-server starting log_level={os.environ.get('LOG_LEVEL','info')} max_active={MAX}", flush=True)
for t in (acceptor, dispatcher, reporter): threading.Thread(target=t, daemon=True).start()
while True: time.sleep(3600)
