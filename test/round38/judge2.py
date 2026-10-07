"""Judge 2 of round 38: one model call per blind packet, under the rule `lapilli case packets` prints.

    judge2.py <packets.json> <instructions.txt> <verdicts-out.json> [model] [spending limit in USD]

It sees a packet and the rule, and nothing else: not the key, not the other packets.
"""
import json, re, sys, warnings, logging
warnings.filterwarnings("ignore"); logging.disable(logging.CRITICAL)
import litellm
litellm.suppress_debug_info = True

packets = json.load(open(sys.argv[1]))
rule = open(sys.argv[2]).read().strip()
out_path = sys.argv[3]
model = sys.argv[4] if len(sys.argv) > 4 else "gpt-5.5"
limit = float(sys.argv[5]) if len(sys.argv) > 5 else 3.0

try:
    verdicts = json.load(open(out_path))      # resume: a packet already judged is not judged again
except (OSError, ValueError):
    verdicts = {}
spent = 0.0
for i, p in enumerate(packets, 1):
    if p["id"] in verdicts:
        continue
    if spent >= limit:
        print(f"spending limit of ${limit} reached after {len(verdicts)} of {len(packets)} packets"); break
    prompt = (rule + "\n\nThe packet:\n" + json.dumps(p, ensure_ascii=False, indent=1) +
              "\n\nReply with the JSON object for this one packet and nothing else.")
    for attempt in (1, 2, 3):
        try:
            r = litellm.completion(model=model, messages=[{"role": "user", "content": prompt}], timeout=180, num_retries=2)
            spent += float(litellm.completion_cost(completion_response=r) or 0)
            text = r.choices[0].message.content or ""
            doc = json.loads(re.search(r"\{[\s\S]*\}", text).group(0))
            v = doc.get(p["id"], doc)          # {"<id>": {...}} as the rule asks, or the bare object
            assert v["verdict"] in ("PASS", "FAIL") and len(v["expected"]) == len(p["expected"]) and len(v["must_not"]) == len(p["must_not"])
            verdicts[p["id"]] = {"verdict": v["verdict"], "expected": [bool(x) for x in v["expected"]], "must_not": [bool(x) for x in v["must_not"]], "reason": str(v.get("reason", ""))}
            break
        except Exception as e:                 # a reply that is not in the shape asked for is asked for again, not repaired
            print(f"packet {p['id']} attempt {attempt}: {type(e).__name__}: {str(e)[:120]}")
    else:
        print(f"packet {p['id']}: no usable verdict after three attempts; left unjudged")
    json.dump(verdicts, open(out_path, "w"), indent=1)
    if i % 10 == 0:
        print(f"{i}/{len(packets)} packets, ${spent:.2f}")
print(f"judged {len(verdicts)} of {len(packets)} packets with {model}; ${spent:.2f}")
