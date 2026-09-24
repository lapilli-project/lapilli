//! `lapilli mcp` over stdio, driven by a client that speaks the wire protocol itself —
//! JSON-RPC 2.0, one frame per line — with no dependency beyond serde_json. What an agent
//! receives is asserted, not exit codes: the tool inventory, the verify-result document, the
//! object body out of a sealed bundle, the refusals. And every line the server writes to stdout
//! must be a frame, which is the "nothing but MCP on stdout" rule checked for free.
//!
//! The released fixtures cover verify and the refusals; `find_bundles` and `read_file` on a
//! real capture need a bundle with `incident.target`, `resources/pod.json` and a diff, which
//! no fixture has, so one is built here with the same sealer the controller uses.
#![cfg(feature = "mcp")]

use std::io::{BufRead, BufReader, Write};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};

use serde_json::{json, Value};

struct Client {
    child: Child,
    stdout: BufReader<std::process::ChildStdout>,
    next_id: u64,
}

impl Client {
    fn spawn(root: &Path, extra: &[&str]) -> Self {
        let mut child = Command::new(env!("CARGO_BIN_EXE_lapilli"))
            .arg("mcp")
            .arg("--root")
            .arg(root)
            .args(extra)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .spawn()
            .expect("spawn lapilli mcp");
        let stdout = BufReader::new(child.stdout.take().unwrap());
        let mut c = Client {
            child,
            stdout,
            next_id: 1,
        };
        let init = c.request(
            "initialize",
            json!({
                "protocolVersion": "2025-06-18",
                "capabilities": {},
                "clientInfo": { "name": "lapilli-test", "version": "0" }
            }),
        );
        assert!(
            init["result"]["capabilities"]["tools"].is_object(),
            "server did not advertise tools: {init}"
        );
        c.notify("notifications/initialized", json!({}));
        c
    }

    fn send(&mut self, v: Value) {
        let stdin = self.child.stdin.as_mut().unwrap();
        stdin.write_all(v.to_string().as_bytes()).unwrap();
        stdin.write_all(b"\n").unwrap();
        stdin.flush().unwrap();
    }

    fn notify(&mut self, method: &str, params: Value) {
        self.send(json!({ "jsonrpc": "2.0", "method": method, "params": params }));
    }

    /// Send a request and read frames until its response arrives. Every frame read must parse
    /// as JSON-RPC: a stray log line here is exactly the transport-killing bug this guards.
    fn request(&mut self, method: &str, params: Value) -> Value {
        let id = self.next_id;
        self.next_id += 1;
        self.send(json!({ "jsonrpc": "2.0", "id": id, "method": method, "params": params }));
        loop {
            let mut line = String::new();
            let n = self.stdout.read_line(&mut line).expect("read stdout");
            assert!(n > 0, "server closed stdout before answering {method}");
            let frame: Value = serde_json::from_str(line.trim_end())
                .unwrap_or_else(|e| panic!("stdout carried a non-frame line ({e}): {line:?}"));
            assert_eq!(frame["jsonrpc"], "2.0", "not a JSON-RPC frame: {frame}");
            if frame["id"] == json!(id) {
                return frame;
            }
        }
    }

    /// Call a tool; return the parsed JSON the tool put in its text content, or the error text.
    fn call(&mut self, name: &str, args: Value) -> Result<Value, String> {
        let r = self.request("tools/call", json!({ "name": name, "arguments": args }));
        if let Some(err) = r.get("error") {
            return Err(err["message"].as_str().unwrap_or("").to_string());
        }
        let res = &r["result"];
        let text = res["content"][0]["text"].as_str().unwrap_or("").to_string();
        if res["isError"].as_bool().unwrap_or(false) {
            return Err(text);
        }
        Ok(serde_json::from_str(&text).unwrap_or(Value::String(text)))
    }
}

impl Drop for Client {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

fn fixtures() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../test/fixtures/ieb/v0.1.0")
}

/// A sealed crash-loop capture with a target, an object body and a diff — the shape the
/// controller produces and the fixtures do not carry.
fn captured_bundle(root: &Path) -> PathBuf {
    use lapilli_bundle::manifest::{
        Coverage, IncidentIdentity, Producer, Target, Timing, Trigger, Window,
    };
    let stage = tempfile::tempdir().unwrap();
    let d = stage.path();
    std::fs::create_dir_all(d.join("resources")).unwrap();
    std::fs::create_dir_all(d.join("diffs/shop/Deployment/checkout")).unwrap();
    std::fs::create_dir_all(d.join("logs")).unwrap();
    std::fs::write(
        d.join("resources/pod.json"),
        json!({
            "apiVersion": "v1", "kind": "Pod",
            "metadata": { "name": "checkout-5c8f4588f5-abcde", "namespace": "shop" },
            "spec": { "containers": [ { "name": "app", "image": "shop/checkout:v1.4.3",
                "env": [ { "name": "CACHE_WARMUP", "value": "eager" },
                         { "name": "DB_PASSWORD", "value": "[redacted]" } ] } ] },
            "status": { "containerStatuses": [ { "name": "app", "restartCount": 3,
                "lastState": { "terminated": { "reason": "Error", "exitCode": 1,
                    "finishedAt": "2026-09-24T03:12:00Z" } } } ] }
        })
        .to_string(),
    )
    .unwrap();
    std::fs::write(d.join("changes.json"), b"{}").unwrap();
    std::fs::write(
        d.join("diffs/index.json"),
        json!({ "normalization": "v1",
            "expected": [ { "namespace": "shop", "kind": "Deployment", "name": "checkout" } ],
            "entries": [ { "namespace": "shop", "kind": "Deployment", "name": "checkout",
                "status": "ok", "source": "replicaset-history",
                "before": { "revision": "1" }, "after": { "revision": "2" },
                "changed_at": "2026-09-24T03:11:51Z", "seconds_relative_to_firing": -9,
                "after_firing": false, "in_range": true, "actor": "demo-deployer",
                "actor_kind": "fieldManager (client-asserted)", "kind_of_change": "spec",
                "summary": ["containers[name=app].env[name=CACHE_WARMUP].value: lazy → eager"],
                "file": "diffs/shop/Deployment/checkout/0.json" } ] })
        .to_string(),
    )
    .unwrap();
    std::fs::write(
        d.join("diffs/shop/Deployment/checkout/0.json"),
        // The producer's shape (`diffs.rs`): a JSON array of changes with `display`.
        json!([ { "display": "containers[name=app].env[name=CACHE_WARMUP].value",
            "path_after": "/spec/template/spec/containers/0/env/0/value",
            "before": "lazy", "after": "eager", "changed": true } ])
        .to_string(),
    )
    .unwrap();
    std::fs::write(
        d.join("logs/app-previous.log"),
        b"FATAL: cache warmup failed\n",
    )
    .unwrap();
    std::fs::write(
        d.join("logs/index.json"),
        json!({ "containers": [ { "container": "app", "instances": [
            { "instance": "previous", "file": "logs/app-previous.log" } ] } ] })
        .to_string(),
    )
    .unwrap();
    std::fs::write(
        d.join("redaction.json"),
        br#"{"policy_version":"v1","mode":"default"}"#,
    )
    .unwrap();
    lapilli_bundle::seal_dir(
        d,
        lapilli_bundle::SealInput {
            incident: IncidentIdentity {
                id: "kind-lapilli-0123456789abcdef".into(),
                cluster_id: "kind-lapilli".into(),
                trigger: Trigger {
                    rule: "KubePodCrashLooping".into(),
                    firing_ts: "2026-09-24T03:12:00Z".into(),
                },
                window: Window {
                    start: "2026-09-24T03:07:00Z".into(),
                    end: "2026-09-24T03:17:00Z".into(),
                },
                target: Some(Target {
                    namespace: "shop".into(),
                    pod: "checkout-5c8f4588f5-abcde".into(),
                }),
            },
            producer: Producer {
                version: "test".into(),
                image_digest: "none".into(),
            },
            coverage: Coverage {
                collectors_run: vec!["logs".into(), "resources".into(), "changes".into()],
                collectors_intended: vec!["logs".into(), "resources".into(), "changes".into()],
                deferred: vec!["events".into(), "metrics".into()],
            },
            timing: Timing {
                capture_started: "2026-09-24T03:12:01Z".into(),
                sealed_at: "2026-09-24T03:12:02Z".into(),
                capture_to_seal_ms: 1000,
            },
        },
        None,
    )
    .unwrap();
    let out = root.join("kind-lapilli-0123456789abcdef.ieb");
    lapilli_bundle::pack(d, &out).unwrap();
    out
}

#[test]
fn the_five_tools_are_listed_with_schemas() {
    let mut c = Client::spawn(&fixtures(), &[]);
    let r = c.request("tools/list", json!({}));
    let names: Vec<&str> = r["result"]["tools"]
        .as_array()
        .unwrap()
        .iter()
        .map(|t| t["name"].as_str().unwrap())
        .collect();
    for want in [
        "find_bundles",
        "verify",
        "summary",
        "read_file",
        "postmortem",
    ] {
        assert!(names.contains(&want), "missing {want}: {names:?}");
    }
    let read = r["result"]["tools"]
        .as_array()
        .unwrap()
        .iter()
        .find(|t| t["name"] == "read_file")
        .unwrap();
    assert!(
        read["inputSchema"]["properties"]["file"].is_object(),
        "read_file must declare its `file` argument: {read}"
    );
}

#[test]
fn verify_returns_the_result_document_verdict_first() {
    let mut c = Client::spawn(&fixtures(), &[]);
    let d = c
        .call("verify", json!({ "path": "ok-deferred.ieb" }))
        .unwrap();
    assert_eq!(d["schema"], "lapilli.dev/verify-result/v1");
    assert_eq!(d["verdict"], "OK");
    assert_eq!(d["bundle"]["deferred"], json!(["events", "metrics"]));
    assert_eq!(
        c.call("verify", json!({ "path": "partial.ieb" })).unwrap()["verdict"],
        "PARTIAL"
    );
    assert_eq!(
        c.call("verify", json!({ "path": "fail-modified.ieb" }))
            .unwrap()["verdict"],
        "FAILED"
    );
}

#[test]
fn only_ieb_files_under_the_root_are_served() {
    let mut c = Client::spawn(&fixtures(), &[]);
    let e = c
        .call("verify", json!({ "path": "../keys/fixture.pub" }))
        .unwrap_err();
    assert!(e.contains("outside") || e.contains("only .ieb"), "{e}");
    let e = c
        .call("verify", json!({ "path": "/etc/hosts" }))
        .unwrap_err();
    assert!(e.contains("outside") || e.contains("only .ieb"), "{e}");
    let e = c
        .call("verify", json!({ "path": "expected.json" }))
        .unwrap_err();
    assert!(e.contains("only .ieb"), "{e}");
    let e = c
        .call("verify", json!({ "path": "missing.ieb" }))
        .unwrap_err();
    assert!(!e.is_empty());
}

#[test]
fn read_file_serves_hash_tree_names_from_verified_bundles_only() {
    let mut c = Client::spawn(&fixtures(), &[]);
    let d = c
        .call(
            "read_file",
            json!({ "path": "ok-unsigned.ieb", "file": "logs/index.json" }),
        )
        .unwrap();
    assert_eq!(d["verdict"], "OK");
    assert_eq!(d["untrusted"], false);
    assert!(d["content"]["containers"].is_array(), "{d}");
    assert!(d["sha256"].is_string());
    // A log file: refused without --allow-logs.
    let e = c
        .call(
            "read_file",
            json!({ "path": "ok-unsigned.ieb", "file": "logs/app-previous.log" }),
        )
        .unwrap_err();
    assert!(e.contains("--allow-logs"), "{e}");
    // Names the tree does not list, and names that try to leave the bundle.
    let e = c
        .call(
            "read_file",
            json!({ "path": "ok-unsigned.ieb", "file": "nope.json" }),
        )
        .unwrap_err();
    assert!(e.contains("hash tree"), "{e}");
    assert!(c
        .call(
            "read_file",
            json!({ "path": "ok-unsigned.ieb", "file": "../../etc/passwd" })
        )
        .is_err());
    // A FAILED bundle serves nothing as evidence.
    let e = c
        .call(
            "read_file",
            json!({ "path": "fail-modified.ieb", "file": "logs/index.json" }),
        )
        .unwrap_err();
    assert!(e.contains("FAILED"), "{e}");
}

#[test]
fn logs_are_served_only_with_allow_logs_and_flagged_untrusted() {
    let mut c = Client::spawn(&fixtures(), &["--allow-logs"]);
    let d = c
        .call(
            "read_file",
            json!({ "path": "ok-unsigned.ieb", "file": "logs/app-previous.log" }),
        )
        .unwrap();
    assert_eq!(d["untrusted"], true);
    assert_eq!(d["redacted_at_capture"], false);
    assert!(d["content"].as_str().unwrap().contains("panic: boom"));
}

#[test]
fn an_alert_finds_its_capture_and_reads_the_object_body_and_the_diff() {
    let root = tempfile::tempdir().unwrap();
    captured_bundle(root.path());
    // A second bundle without a target, to prove the filter is on the manifest, not the name.
    std::fs::copy(
        fixtures().join("ok-unsigned.ieb"),
        root.path().join("other.ieb"),
    )
    .unwrap();
    let mut c = Client::spawn(root.path(), &[]);

    // What the alert carries: namespace, a pod prefix, a window around the firing time.
    let f = c
        .call(
            "find_bundles",
            json!({ "namespace": "shop", "pod": "checkout-*",
                    "since": "2026-09-24T03:00:00Z", "until": "2026-09-24T03:30:00Z" }),
        )
        .unwrap();
    assert_eq!(f["scanned"], 2, "{f}");
    assert_eq!(f["matched"], 1, "{f}");
    let b = &f["bundles"][0];
    assert_eq!(b["incident_id"], "kind-lapilli-0123456789abcdef");
    assert_eq!(b["target"]["pod"], "checkout-5c8f4588f5-abcde");
    assert_eq!(b["deferred"], json!(["events", "metrics"]));
    let path = b["path"].as_str().unwrap().to_string();

    // Wrong namespace, wrong window: nothing.
    let none = c
        .call("find_bundles", json!({ "namespace": "payments" }))
        .unwrap();
    assert_eq!(none["matched"], 0);
    let none = c
        .call(
            "find_bundles",
            json!({ "namespace": "shop", "until": "2026-09-24T02:00:00Z" }),
        )
        .unwrap();
    assert_eq!(none["matched"], 0);

    // The object body, with the value the diff is about — and the redaction the capture did.
    let pod = c
        .call(
            "read_file",
            json!({ "path": path, "file": "resources/pod.json" }),
        )
        .unwrap();
    assert_eq!(pod["redacted_at_capture"], true);
    let env = &pod["content"]["spec"]["containers"][0]["env"];
    assert_eq!(env[0]["value"], "eager");
    assert_eq!(env[1]["value"], "[redacted]");
    // The diff.
    let diff = c
        .call(
            "read_file",
            json!({ "path": path, "file": "diffs/index.json" }),
        )
        .unwrap();
    assert!(diff["content"]["entries"][0]["summary"][0]
        .as_str()
        .unwrap()
        .contains("CACHE_WARMUP"));
    // The summary and the postmortem, without the log line.
    let s = c.call("summary", json!({ "path": path })).unwrap();
    assert_eq!(s["verdict"], "OK");
    assert_eq!(
        s["summary"]["change"]["field"]
            .as_str()
            .map(|f| f.contains("CACHE_WARMUP")),
        Some(true)
    );
    assert!(s["summary"]["last_line"].is_null(), "{s}");
    let p = c.call("postmortem", json!({ "path": path })).unwrap();
    assert_eq!(p["verdict"], "OK");
    let md = p["markdown"].as_str().unwrap();
    assert!(md.contains("revision 1 → 2"), "{md}");
    assert!(
        !md.contains("FATAL: cache warmup failed"),
        "the log line leaked"
    );
}

/// A producer writes `…10.354989292+00:00`; a caller writes `…10Z`. Compared as text those
/// sort the wrong way at equal seconds, which the first version did; the window is parsed.
#[test]
fn the_time_window_is_compared_as_instants_not_strings() {
    let root = tempfile::tempdir().unwrap();
    captured_bundle(root.path()); // fires at 2026-09-24T03:12:00Z
    let mut c = Client::spawn(root.path(), &[]);
    let hit = |c: &mut Client, since: &str| {
        c.call("find_bundles", json!({ "since": since })).unwrap()["matched"]
            .as_u64()
            .unwrap()
    };
    assert_eq!(
        hit(&mut c, "2026-09-24T03:12:00.000+00:00"),
        1,
        "equal instant, other spelling"
    );
    assert_eq!(
        hit(&mut c, "2026-09-24T03:12:00.001+00:00"),
        0,
        "one millisecond later"
    );
    assert_eq!(
        hit(&mut c, "2026-09-24T12:12:00+09:00"),
        1,
        "same instant in another offset"
    );
    let e = c
        .call("find_bundles", json!({ "since": "yesterday" }))
        .unwrap_err();
    assert!(e.contains("RFC 3339"), "{e}");
}
