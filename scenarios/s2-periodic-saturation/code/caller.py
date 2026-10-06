import time, os, threading, urllib.request, random
NAME = os.environ["APP_NAME"]; RPS = float(os.environ.get("STEADY_RPS", "2"))
EVERY = float(os.environ.get("BATCH_INTERVAL_SECONDS", "0")); DUR = float(os.environ.get("BATCH_DURATION_SECONDS", "0")); CONC = int(os.environ.get("BATCH_CONCURRENCY", "0"))
URL = f"http://thumb-api:8080/render?client={NAME}"
def ts(): return time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())
def call(quiet=False):
    try:
        with urllib.request.urlopen(URL, timeout=2.5) as r: r.read(); return True
    except Exception as e:
        if not quiet: print(f"{ts()} ERROR {NAME} thumbnail request failed: {getattr(e, 'code', type(e).__name__)}", flush=True)
        return False
def steady():
    n = 0
    while True:
        n += 1
        if call() and n % 40 == 0: print(f"{ts()} INFO {NAME} rendered page id={random.randint(1000,9999)}", flush=True)
        time.sleep(1.0 / RPS)
def burst_loop():
    time.sleep(EVERY / 2)
    while True:
        end = time.time() + DUR
        def w():
            while time.time() < end: call(quiet=True)
        ths = [threading.Thread(target=w, daemon=True) for _ in range(CONC)]
        [t.start() for t in ths]; [t.join() for t in ths]
        time.sleep(max(1.0, EVERY - DUR))
print(f"{ts()} INFO {NAME} starting", flush=True)
if RPS > 0: threading.Thread(target=steady, daemon=True).start()
if EVERY > 0 and CONC > 0: threading.Thread(target=burst_loop, daemon=True).start()
while True: time.sleep(3600)
