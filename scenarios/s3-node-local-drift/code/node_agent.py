import os, time, hashlib
NODE = os.environ.get("NODE_NAME", "?"); TUNING = "/host/etc/ledger-tuning/tuning.conf"
def ts(): return time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())
while True:
    try:
        raw = open(TUNING, "rb").read(); conf = raw.decode().strip().replace("\n", ";"); sha = hashlib.sha256(raw).hexdigest()[:12]
    except Exception:
        conf, sha = "absent", "-"
    load = open("/proc/loadavg").read().split()[0]
    print(f'{ts()} node-facts node={NODE} kernel={os.uname().release} load1={load} ledger_tuning_sha256={sha} ledger_tuning="{conf}"', flush=True)
    time.sleep(20)
