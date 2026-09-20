//! What a sealed (or staged) bundle says at a glance: the handful of facts an on-call
//! engineer wants in the first ten minutes.
//!
//! Read from the bundle's own files, so it costs no API calls and works on a staging
//! directory before packing as well as on an unpacked bundle. Every field is **structured**
//! (numbers, enum-like reasons, identifiers); rendering is the caller's, and a caller that
//! sends this outside the cluster decides which fields it may carry (docs/design-notify.md).

use std::path::Path;

use serde::{Deserialize, Serialize};
use serde_json::Value;

/// How a container ended, from `resources/pod.json`.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Termination {
    /// `OOMKilled`, `Error`, … as the kubelet reported it.
    pub reason: Option<String>,
    pub exit_code: Option<i64>,
    pub finished_at: Option<String>,
    pub restarts: i64,
}

/// Whether the crashed instance's own log survived to the bundle.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum LastWords {
    /// The terminated instance's log is in the bundle.
    Captured,
    /// The kubelet had already discarded it when Kairn asked.
    Discarded,
    /// No terminated instance, or no log collector.
    #[default]
    None,
}

/// The spec change nearest the alert, from `diffs/index.json`.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Change {
    pub kind: String,
    pub name: String,
    pub revision_from: Option<String>,
    pub revision_to: Option<String>,
    /// Seconds between the change and the alert firing (negative: after it).
    pub seconds_before_alert: Option<i64>,
    /// The field manager credited with the change, if one could be attributed.
    pub actor: Option<String>,
    /// First changed field: path, and the values. **Workload content**: a caller that
    /// sends the summary off-cluster must treat these as sensitive.
    pub field: Option<String>,
    pub before: Option<String>,
    pub after: Option<String>,
}

/// Memory against the container's limit, from `metrics/`.
#[derive(Debug, Clone, Copy, PartialEq, Serialize, Deserialize)]
pub struct Memory {
    pub peak_bytes: f64,
    pub limit_bytes: Option<f64>,
    pub samples: usize,
}

/// A bundle's headline facts. Absent fields mean "the bundle doesn't say", never zero.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct Summary {
    /// The container the facts are about (the bundle's target container).
    pub container: Option<String>,
    pub termination: Option<Termination>,
    pub last_words: LastWords,
    /// The log line the container died on. **Workload content.**
    pub last_line: Option<String>,
    pub change: Option<Change>,
    pub memory: Option<Memory>,
    /// Timeline events collected (`timeline.json`), as a count.
    pub events: usize,
    pub collectors_run: Vec<String>,
    pub collectors_missing: Vec<String>,
}

/// Longest log line carried in a summary; longer lines are cut with an ellipsis.
pub const MAX_LINE: usize = 300;
/// Longest before/after value carried in a summary.
pub const MAX_VALUE: usize = 100;

impl Summary {
    /// Read a bundle (or staging) directory. Missing or malformed files are skipped: a
    /// summary is best-effort and never fails a capture.
    pub fn from_dir(dir: &Path, container: Option<&str>) -> Self {
        let read = |rel: &str| -> Option<Value> {
            serde_json::from_slice(&std::fs::read(dir.join(rel)).ok()?).ok()
        };
        let pod = read("resources/pod.json");
        // The container the caller names, else the one the pod spec lists first.
        let container = container.map(str::to_string).or_else(|| {
            pod.as_ref()?["spec"]["containers"][0]["name"]
                .as_str()
                .map(str::to_string)
        });
        let status = pod.as_ref().and_then(|p| {
            p["status"]["containerStatuses"]
                .as_array()?
                .iter()
                .find(|c| container.as_deref().is_none_or(|n| c["name"] == n))
                .cloned()
        });
        let termination = status.as_ref().map(|cs| {
            let t = &cs["lastState"]["terminated"];
            Termination {
                reason: t["reason"].as_str().map(str::to_string),
                exit_code: t["exitCode"].as_i64(),
                finished_at: t["finishedAt"].as_str().map(str::to_string),
                restarts: cs["restartCount"].as_i64().unwrap_or(0),
            }
        });

        let instances: Vec<Value> = read("logs/index.json")
            .and_then(|i| {
                let containers = i["containers"].as_array()?.clone();
                let entry = containers
                    .iter()
                    .find(|c| container.as_deref().is_none_or(|n| c["container"] == n))
                    .or_else(|| containers.first())?;
                entry["instances"].as_array().cloned()
            })
            .unwrap_or_default();
        // The crash's last words live in whichever terminated instance ended last: in a fast
        // crash loop that is often "current", not "previous".
        let newest_terminated = instances
            .iter()
            .filter(|i| i["state"] == "terminated")
            .max_by_key(|i| i["finished_at"].as_str().unwrap_or("").to_string());
        let (last_words, last_line) = match newest_terminated {
            None => (LastWords::None, None),
            Some(i) if i["unavailable"].is_string() => (LastWords::Discarded, None),
            Some(i) => {
                let line = i["file"]
                    .as_str()
                    .and_then(|f| std::fs::read_to_string(dir.join(f)).ok())
                    .and_then(|text| {
                        text.lines()
                            .rev()
                            .find(|l| !l.trim().is_empty())
                            .map(|l| truncate(l, MAX_LINE))
                    });
                (LastWords::Captured, line)
            }
        };

        let change = read("diffs/index.json").as_ref().and_then(|i| {
            let entries = i["entries"].as_array()?;
            // The change nearest the alert: entries carry their distance already.
            let e = entries
                .iter()
                .filter(|e| e["status"] == "ok" && e["file"].is_string())
                .min_by_key(|e| {
                    e["seconds_before_alert"]
                        .as_i64()
                        .map_or(i64::MAX, |s| s.abs())
                })?;
            let detail = e["file"].as_str().and_then(|f| {
                serde_json::from_slice::<Value>(&std::fs::read(dir.join(f)).ok()?).ok()
            });
            let first = detail
                .as_ref()
                .and_then(|d| d["changes"].as_array()?.first().cloned());
            Some(Change {
                kind: e["kind"].as_str().unwrap_or("?").to_string(),
                name: e["name"].as_str().unwrap_or("?").to_string(),
                revision_from: revision(&e["revision_from"]),
                revision_to: revision(&e["revision_to"]),
                seconds_before_alert: e["seconds_before_alert"].as_i64(),
                actor: e["actor"].as_str().map(str::to_string),
                field: first
                    .as_ref()
                    .and_then(|c| c["path"].as_str().map(str::to_string)),
                before: first
                    .as_ref()
                    .and_then(|c| value_text(&c["before"]).map(|v| truncate(&v, MAX_VALUE))),
                after: first
                    .as_ref()
                    .and_then(|c| value_text(&c["after"]).map(|v| truncate(&v, MAX_VALUE))),
            })
        });

        let memory = read("metrics/memory_working_set_bytes.json")
            .and_then(|m| peak(&m))
            .map(|(peak_bytes, samples)| Memory {
                peak_bytes,
                limit_bytes: pod
                    .as_ref()
                    .and_then(|p| memory_limit(p, container.as_deref())),
                samples,
            });

        let events = read("timeline.json")
            .and_then(|t| Some(t.as_array()?.len()))
            .unwrap_or(0);
        let (run, missing) = coverage(read("manifest.json").as_ref());

        Summary {
            container,
            termination,
            last_words,
            last_line,
            change,
            memory,
            events,
            collectors_run: run,
            collectors_missing: missing,
        }
    }

    /// Everything except the two workload-content fields (`last_line`, and the change's
    /// field and values): what may go to a chat channel by default.
    pub fn facts_only(&self) -> Self {
        let mut facts = self.clone();
        facts.last_line = None;
        if let Some(change) = &mut facts.change {
            change.field = None;
            change.before = None;
            change.after = None;
        }
        facts
    }

    /// True when the summary says nothing worth sending.
    pub fn is_empty(&self) -> bool {
        self.termination.is_none()
            && self.change.is_none()
            && self.memory.is_none()
            && self.last_words == LastWords::None
    }
}

fn revision(v: &Value) -> Option<String> {
    v.as_str()
        .map(str::to_string)
        .or_else(|| v.as_i64().map(|n| n.to_string()))
}

/// A changed value as one short string; objects and arrays are described, not dumped.
fn value_text(v: &Value) -> Option<String> {
    match v {
        Value::Null => None,
        Value::String(s) => Some(s.clone()),
        Value::Bool(b) => Some(b.to_string()),
        Value::Number(n) => Some(n.to_string()),
        Value::Array(a) => Some(format!("[{} items]", a.len())),
        Value::Object(o) => Some(format!("{{{} fields}}", o.len())),
    }
}

fn truncate(s: &str, max: usize) -> String {
    let s = s.trim();
    if s.chars().count() <= max {
        return s.to_string();
    }
    let cut: String = s.chars().take(max).collect();
    format!("{cut}…")
}

/// Peak value over every series in a Prometheus range response, and the sample count.
fn peak(result: &Value) -> Option<(f64, usize)> {
    let series = result["data"]["result"].as_array()?;
    let mut top = None;
    let mut samples = 0;
    for s in series {
        let values = s["values"].as_array()?;
        samples = samples.max(values.len());
        for v in values {
            let y: f64 = v[1].as_str()?.parse().ok()?;
            if top.is_none_or(|t| y > t) {
                top = Some(y);
            }
        }
    }
    top.map(|t| (t, samples))
}

fn memory_limit(pod: &Value, container: Option<&str>) -> Option<f64> {
    let c = pod["spec"]["containers"]
        .as_array()?
        .iter()
        .find(|c| container.is_none_or(|n| c["name"] == n))?;
    let text = c["resources"]["limits"]["memory"].as_str()?;
    quantity_bytes(text)
}

/// `64Mi`, `1Gi`, `512M`, `1000000` → bytes.
pub fn quantity_bytes(text: &str) -> Option<f64> {
    let digits: String = text
        .chars()
        .take_while(|c| c.is_ascii_digit() || *c == '.')
        .collect();
    let n: f64 = digits.parse().ok()?;
    let factor = match text[digits.len()..].trim() {
        "" => 1.0,
        "k" => 1e3,
        "M" => 1e6,
        "G" => 1e9,
        "T" => 1e12,
        "Ki" => 1024.0,
        "Mi" => 1024.0 * 1024.0,
        "Gi" => 1024.0 * 1024.0 * 1024.0,
        "Ti" => 1024.0_f64.powi(4),
        _ => return None,
    };
    Some(n * factor)
}

/// Collectors that ran, and intended ones that didn't, from the manifest's coverage.
fn coverage(manifest: Option<&Value>) -> (Vec<String>, Vec<String>) {
    let list = |v: &Value| -> Vec<String> {
        v.as_array()
            .map(|a| {
                a.iter()
                    .filter_map(|s| s.as_str().map(str::to_string))
                    .collect()
            })
            .unwrap_or_default()
    };
    let Some(m) = manifest else {
        return (Vec::new(), Vec::new());
    };
    let run = list(&m["coverage"]["collectors_run"]);
    let missing = list(&m["coverage"]["collectors_intended"])
        .into_iter()
        .filter(|c| !run.contains(c))
        .collect();
    (run, missing)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn stage(files: &[(&str, String)]) -> tempfile::TempDir {
        let dir = tempfile::tempdir().unwrap();
        for (rel, content) in files {
            let path = dir.path().join(rel);
            std::fs::create_dir_all(path.parent().unwrap()).unwrap();
            std::fs::write(path, content).unwrap();
        }
        dir
    }

    fn oomkill_bundle() -> tempfile::TempDir {
        stage(&[
            (
                "resources/pod.json",
                json!({
                    "spec": { "containers": [{ "name": "app",
                        "resources": { "limits": { "memory": "64Mi" } } }] },
                    "status": { "containerStatuses": [{ "name": "app", "restartCount": 2,
                        "lastState": { "terminated": { "reason": "OOMKilled", "exitCode": 137,
                            "finishedAt": "2026-09-20T01:00:05Z" } } }] }
                })
                .to_string(),
            ),
            (
                "logs/index.json",
                json!({ "containers": [{ "container": "app", "instances": [
                    { "which": "previous", "state": "terminated", "unavailable": "already discarded",
                      "finished_at": "2026-09-20T00:50:00Z" },
                    { "which": "current", "state": "terminated", "file": "logs/app-current.log",
                      "finished_at": "2026-09-20T01:00:05Z" }
                ] }] })
                .to_string(),
            ),
            (
                "logs/app-current.log",
                "allocating\nstill allocating\nfatal: out of memory writing DB_PASSWORD=****\n\n"
                    .to_string(),
            ),
            (
                "diffs/index.json",
                json!({ "entries": [
                    { "status": "ok", "kind": "Deployment", "name": "checkout", "file": "diffs/d.json",
                      "revision_from": 1, "revision_to": 2, "seconds_before_alert": 94,
                      "actor": "argocd-application-controller" },
                    { "status": "ok", "kind": "Deployment", "name": "checkout", "file": "diffs/old.json",
                      "seconds_before_alert": 3600 }
                ] })
                .to_string(),
            ),
            (
                "diffs/d.json",
                json!({ "changes": [
                    { "path": "spec.template.spec.containers[app].image",
                      "before": "shop/checkout:v1.4.2", "after": "shop/checkout:v1.4.3" }
                ] })
                .to_string(),
            ),
            (
                "metrics/memory_working_set_bytes.json",
                json!({ "data": { "result": [{ "values": [
                    ["1758330000", "20000000"], ["1758330005", "64200000"]
                ] }] } })
                .to_string(),
            ),
            ("timeline.json", json!([{ "reason": "Pulled" }, { "reason": "BackOff" }]).to_string()),
            (
                "manifest.json",
                json!({ "coverage": { "collectors_run": ["logs", "resources"],
                    "collectors_intended": ["logs", "resources", "metrics"] } })
                .to_string(),
            ),
        ])
    }

    #[test]
    fn reads_the_headline_facts() {
        let dir = oomkill_bundle();
        let s = Summary::from_dir(dir.path(), None);
        assert_eq!(s.container.as_deref(), Some("app"));
        let t = s.termination.clone().unwrap();
        assert_eq!(t.reason.as_deref(), Some("OOMKilled"));
        assert_eq!((t.exit_code, t.restarts), (Some(137), 2));
        // The newest terminated instance's log, its last non-empty line.
        assert_eq!(s.last_words, LastWords::Captured);
        assert!(s
            .last_line
            .as_deref()
            .unwrap()
            .starts_with("fatal: out of memory"));
        // The change nearest the alert wins over the hour-old one.
        let c = s.change.clone().unwrap();
        assert_eq!(
            (c.kind.as_str(), c.name.as_str()),
            ("Deployment", "checkout")
        );
        assert_eq!(c.seconds_before_alert, Some(94));
        assert_eq!(c.revision_from.as_deref(), Some("1"));
        assert_eq!(c.actor.as_deref(), Some("argocd-application-controller"));
        assert_eq!(c.after.as_deref(), Some("shop/checkout:v1.4.3"));
        let m = s.memory.unwrap();
        assert_eq!((m.peak_bytes, m.samples), (64_200_000.0, 2));
        assert_eq!(m.limit_bytes, Some(64.0 * 1024.0 * 1024.0));
        assert_eq!(s.events, 2);
        assert_eq!(s.collectors_missing, ["metrics"]);
        assert!(!s.is_empty());
    }

    #[test]
    fn facts_only_drops_workload_content() {
        let facts = Summary::from_dir(oomkill_bundle().path(), None).facts_only();
        assert!(facts.last_line.is_none());
        let c = facts.change.clone().unwrap();
        assert!(c.field.is_none() && c.before.is_none() && c.after.is_none());
        // The facts that decide the next action stay.
        assert_eq!(c.seconds_before_alert, Some(94));
        assert_eq!(c.actor.as_deref(), Some("argocd-application-controller"));
        assert_eq!(facts.last_words, LastWords::Captured);
        // Nothing taken from the workload survives anywhere in the facts.
        let text = serde_json::to_string(&facts).unwrap();
        for leak in ["DB_PASSWORD", "out of memory", "v1.4.3", "v1.4.2"] {
            assert!(!text.contains(leak), "{leak} leaked into facts: {text}");
        }
    }

    #[test]
    fn discarded_logs_are_reported_as_such() {
        let dir = stage(&[(
            "logs/index.json",
            json!({ "containers": [{ "container": "app", "instances": [
                { "which": "previous", "state": "terminated", "unavailable": "gone",
                  "finished_at": "2026-09-20T01:00:00Z" }
            ] }] })
            .to_string(),
        )]);
        let s = Summary::from_dir(dir.path(), Some("app"));
        assert_eq!(s.last_words, LastWords::Discarded);
        assert!(s.last_line.is_none());
    }

    #[test]
    fn long_lines_and_values_are_cut() {
        let long = "x".repeat(MAX_LINE + 50);
        let dir = stage(&[
            (
                "logs/index.json",
                json!({ "containers": [{ "container": "app", "instances": [
                    { "state": "terminated", "file": "logs/a.log", "finished_at": "2026-09-20T01:00:00Z" }
                ] }] })
                .to_string(),
            ),
            ("logs/a.log", long.clone()),
            (
                "diffs/index.json",
                json!({ "entries": [{ "status": "ok", "kind": "ConfigMap", "name": "cfg",
                    "file": "diffs/c.json", "seconds_before_alert": 10 }] })
                .to_string(),
            ),
            (
                "diffs/c.json",
                json!({ "changes": [{ "path": "data.MODE", "before": long, "after": {"a": 1, "b": 2} }] })
                    .to_string(),
            ),
        ]);
        let s = Summary::from_dir(dir.path(), None);
        assert_eq!(s.last_line.as_ref().unwrap().chars().count(), MAX_LINE + 1);
        let c = s.change.unwrap();
        assert_eq!(c.before.as_ref().unwrap().chars().count(), MAX_VALUE + 1);
        // A structure is described, never dumped.
        assert_eq!(c.after.as_deref(), Some("{2 fields}"));
    }

    #[test]
    fn an_empty_directory_says_nothing() {
        let dir = tempfile::tempdir().unwrap();
        let s = Summary::from_dir(dir.path(), None);
        assert!(s.is_empty());
        assert_eq!(s.last_words, LastWords::None);
    }

    #[test]
    fn quantities() {
        assert_eq!(quantity_bytes("64Mi"), Some(67_108_864.0));
        assert_eq!(quantity_bytes("1Gi"), Some(1_073_741_824.0));
        assert_eq!(quantity_bytes("512M"), Some(512e6));
        assert_eq!(quantity_bytes("1000"), Some(1000.0));
        assert_eq!(quantity_bytes("garbage"), None);
    }
}
