import socket, time, os, random, threading
NAME = os.environ.get("APP_NAME", "client"); POOL = int(os.environ.get("POOL_SIZE", "2")); RECYCLE = float(os.environ.get("POOL_RECYCLE_SECONDS", "15"))
OP = os.environ.get("OP_NAME", "GET cart"); HOST = ("cache", 6379)
def ts(): return time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())
def worker(i):
    conn = None; born = 0; ok = 0
    while True:
        try:
            if conn is None or time.time() - born > RECYCLE:
                if conn:
                    try: conn.close()
                    except Exception: pass
                conn = socket.create_connection(HOST, timeout=2.0); conn.settimeout(2.0); born = time.time()
            t0 = time.time(); conn.sendall(b"PING\r\n"); data = conn.recv(64)
            if not data: raise ConnectionError("closed by peer")
            ok += 1
            if ok % 20 == 0: print(f"{ts()} INFO {NAME} served request id={random.randint(10000,99999)} cache_ms={int((time.time()-t0)*1000)}", flush=True)
        except Exception as e:
            print(f"{ts()} ERROR {NAME} cache {OP}:{random.randint(1000,9999)} timed out after 2.0s (cache:6379) [{type(e).__name__}]", flush=True)
            try: conn.close()
            except Exception: pass
            conn = None
        time.sleep(0.5)
print(f"{ts()} INFO {NAME} starting pool_size={POOL} pool_recycle_seconds={RECYCLE}", flush=True)
for i in range(POOL): threading.Thread(target=worker, args=(i,), daemon=True).start()
while True: time.sleep(3600)
