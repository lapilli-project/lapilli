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

/// How a container ended, from `resources/pod.json`'s `lastState.terminated`. Present only
/// when the kubelet reported a terminated instance: a running container that never died has
/// no `Termination` (its restart count lives on [`Summary::restarts`] instead).
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Termination {
    /// `OOMKilled`, `Error`, … as the kubelet reported it.
    pub reason: Option<String>,
    pub exit_code: Option<i64>,
    pub finished_at: Option<String>,
}

/// Whether the crashed instance's own log survived to the bundle.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "kebab-case")]
pub enum LastWords {
    /// The terminated instance's log is in the bundle.
    Captured,
    /// The kubelet had already discarded it when Lapilli asked.
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
    /// How the container's last terminated instance ended, if there was one. `None` means the
    /// pod has no terminated instance at all — not that the bundle failed to read one.
    pub termination: Option<Termination>,
    /// `status.containerStatuses[].restartCount`. Meaningful whether or not the container is
    /// currently terminated: a pod restarting right now is exactly what an on-call engineer
    /// wants to know, so this stands beside `termination` rather than inside it.
    pub restarts: i64,
    pub last_words: LastWords,
    /// The log line the container died on. **Workload content.**
    pub last_line: Option<String>,
    pub change: Option<Change>,
    pub memory: Option<Memory>,
    /// Timeline events collected (`timeline.json`), as a count.
    pub events: usize,
    pub collectors_run: Vec<String>,
    pub collectors_missing: Vec<String>,
    /// `coverage.deferred`: collectors the producer chose not to intend (IEB rule 6). Carried
    /// so that every surface built on a Summary — the postmortem, a notification — can say why
    /// a section is empty by design rather than let 100% coverage stand for "everything".
    #[serde(default)]
    pub collectors_deferred: Vec<String>,
}

/// Longest log line carried in a summary; longer lines are cut with an ellipsis.
pub const MAX_LINE: usize = 300;
/// Longest before/after value carried in a summary.
pub const MAX_VALUE: usize = 100;

/// Read a file an index file *named*, refusing anything that is not a plain relative path inside the
/// bundle.
///
/// The name comes from JSON **content** (`logs/index.json`, `diffs/index.json`), and verification
/// only hashes content — it never constrains it. `Path::join` replaces the base outright for an
/// absolute path and happily walks out of it for `../`, so a bundle Lapilli did not produce could name
/// `/etc/passwd` here. That was unreachable while this only ever ran on the controller's own staging
/// directory; it stops being unreachable the moment anything reads a bundle from elsewhere, which is
/// the whole point of a portable bundle. `check_path` is the same validator `pack::unpack` and
/// `verify` use, so the three agree on what a bundle path is.
fn read_inner(dir: &Path, named: &str) -> Option<Vec<u8>> {
    crate::hashtree::check_path(named).ok()?;
    std::fs::read(dir.join(named)).ok()
}

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
        let restarts = status
            .as_ref()
            .and_then(|cs| cs["restartCount"].as_i64())
            .unwrap_or(0);
        // Only a real `lastState.terminated` block makes a termination: a container that is
        // merely running (or waiting) must not be reported as having died.
        let termination = status
            .as_ref()
            .map(|cs| &cs["lastState"]["terminated"])
            .filter(|t| t.is_object())
            .map(|t| Termination {
                reason: t["reason"].as_str().map(str::to_string),
                exit_code: t["exitCode"].as_i64(),
                finished_at: t["finishedAt"].as_str().map(str::to_string),
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
                    .and_then(|f| read_inner(dir, f))
                    .and_then(|b| String::from_utf8(b).ok())
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
                    e["seconds_relative_to_firing"]
                        .as_i64()
                        .map_or(i64::MAX, |s| s.abs())
                })?;
            let detail = e["file"]
                .as_str()
                .and_then(|f| serde_json::from_slice::<Value>(&read_inner(dir, f)?).ok());
            // A diff detail file is a JSON **array** of changes (`docs/design-change-diff.md`,
            // `spec/IEB-SPEC.md` §diffs), not `{"changes": [...]}`. Reading it the wrong way
            // left `field`/`before`/`after` permanently empty, which is the whole of what
            // `detail: content` promises to add — so content mode added nothing at all. The
            // object form is still accepted in case a producer wraps it.
            let first = detail.as_ref().and_then(|d| {
                let changes = d.as_array().or_else(|| d["changes"].as_array())?;
                changes
                    .iter()
                    .find(|c| c["changed"].as_bool().unwrap_or(true))
                    .cloned()
            });
            Some(Change {
                kind: e["kind"].as_str().unwrap_or("?").to_string(),
                name: e["name"].as_str().unwrap_or("?").to_string(),
                // The producer's own names (`diffs.rs`): the revisions live inside `before`/`after`,
                // and the timing is `seconds_relative_to_firing`. This reader used to look for
                // `revision_from`, `revision_to` and `seconds_before_alert`, which **no producer
                // ever wrote** — so every notification said "changed" with no revision and no
                // timing, and the "nearest the alert" selector above, keying every entry to
                // `i64::MAX`, silently returned the first entry instead.
                revision_from: revision(&e["before"]["revision"]),
                revision_to: revision(&e["after"]["revision"]),
                // Sign flip, not a rename: `seconds_relative_to_firing` is `changed_at - firing`,
                // so a change BEFORE the alert is negative there and positive here. Renaming the
                // field without this would have inverted the timing in every message — "94s after
                // the alert" for a change 94s before it.
                seconds_before_alert: e["seconds_relative_to_firing"].as_i64().map(|s| -s),
                actor: e["actor"].as_str().map(str::to_string),
                field: first.as_ref().and_then(|c| {
                    // `display` is the readable path (`containers[name=app].image`);
                    // `path_after` is the JSON pointer, and `path` is accepted for a producer
                    // that writes neither.
                    c["display"]
                        .as_str()
                        .or_else(|| c["path_after"].as_str())
                        .or_else(|| c["path"].as_str())
                        .map(str::to_string)
                }),
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
        let (run, missing, deferred) = coverage(read("manifest.json").as_ref());

        Summary {
            container,
            termination,
            restarts,
            last_words,
            last_line,
            change,
            memory,
            events,
            collectors_run: run,
            collectors_missing: missing,
            collectors_deferred: deferred,
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

    /// Everything except the log line. Free-text redaction is heuristic and bundles do not
    /// redact logs at all, so this is the most a caller may send off-cluster — `detail:
    /// content`'s ceiling (docs/design-notify.md).
    pub fn without_log_line(&self) -> Self {
        let mut s = self.clone();
        s.last_line = None;
        s
    }

    /// True when the summary says nothing worth sending.
    ///
    /// A nonzero restart count counts as something: a running pod that has restarted is news
    /// on its own, even with no terminated instance, no change and no metrics.
    pub fn is_empty(&self) -> bool {
        self.termination.is_none()
            && self.restarts == 0
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
/// `(run, missing, deferred)` from the manifest's coverage. `deferred` absent or `null` is empty.
fn coverage(manifest: Option<&Value>) -> (Vec<String>, Vec<String>, Vec<String>) {
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
        return (Vec::new(), Vec::new(), Vec::new());
    };
    let run = list(&m["coverage"]["collectors_run"]);
    let missing = list(&m["coverage"]["collectors_intended"])
        .into_iter()
        .filter(|c| !run.contains(c))
        .collect();
    let deferred = list(&m["coverage"]["deferred"]);
    (run, missing, deferred)
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
                // The producer's shape, copied from a real `diffs/index.json` (`diffs.rs`): the
                // revisions are nested under `before`/`after`, and the timing is
                // `seconds_relative_to_firing`, **negative before the alert**. The earlier version
                // of this fixture invented `revision_from`/`revision_to`/`seconds_before_alert` —
                // the same names the reader looked for — so the test agreed with the bug instead of
                // catching it. Generation and verification that share a blind spot agree.
                // The far entry is FIRST on purpose. With it second, "picked the nearest change"
                // and "picked the first entry" are indistinguishable — and the shipped selector did
                // the latter, so the test has to be able to tell them apart.
                json!({ "entries": [
                    { "status": "ok", "kind": "Deployment", "name": "checkout", "file": "diffs/old.json",
                      "before": { "revision": "0", "object": "ReplicaSet/checkout-old" },
                      "after": { "revision": "1", "object": "ReplicaSet/checkout-abc" },
                      "seconds_relative_to_firing": -3600 },
                    { "status": "ok", "kind": "Deployment", "name": "checkout", "file": "diffs/d.json",
                      "before": { "revision": "1", "object": "ReplicaSet/checkout-abc" },
                      "after": { "revision": "2", "object": "ReplicaSet/checkout-def" },
                      "seconds_relative_to_firing": -94, "after_firing": false,
                      "actor": "argocd-application-controller" }
                ] })
                .to_string(),
            ),
            (
                "diffs/d.json",
                json!([
                    { "display": "containers[name=app].image",
                      "path_after": "/spec/template/spec/containers/0/image",
                      "before": "shop/checkout:v1.4.2", "after": "shop/checkout:v1.4.3",
                      "changed": true }
                ])
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

    /// A bundle Lapilli did not produce can name anything it likes in its index files, because
    /// verification hashes content and does not constrain it. `Path::join` would follow an absolute
    /// path out of the bundle entirely, and `../` out of it relatively. This is unreachable while the
    /// only caller is the controller reading its own staging directory, and it stops being
    /// unreachable the moment anything reads a bundle from elsewhere.
    #[test]
    fn an_index_that_names_a_file_outside_the_bundle_reads_nothing() {
        let outside = tempfile::tempdir().unwrap();
        let secret = outside.path().join("secret.txt");
        std::fs::write(&secret, "SHOULD NEVER BE READ").unwrap();

        for named in [
            secret.to_string_lossy().to_string(), // absolute: join() replaces the base
            "../secret.txt".to_string(),          // relative escape
            "logs/../../secret.txt".to_string(),  // escape after a legitimate-looking prefix
        ] {
            let dir = tempfile::tempdir().unwrap();
            std::fs::create_dir_all(dir.path().join("logs")).unwrap();
            // A sibling of the bundle dir, so `../secret.txt` resolves to something that exists.
            std::fs::write(dir.path().join("../secret.txt"), "SHOULD NEVER BE READ").ok();
            // The producer's shape: `containers[]`, each with `instances[]`. An earlier version of
            // this fixture put `instances` at the top level, so the reader never reached the `file`
            // at all and the test passed with the path check removed — a vacuous test that mutation
            // caught.
            std::fs::write(
                dir.path().join("logs/index.json"),
                json!({ "containers": [{ "container": "app", "instances": [
                    { "which": "previous", "state": "terminated", "file": named,
                      "finished_at": "2026-09-20T01:00:05Z" }
                ] }] })
                .to_string(),
            )
            .unwrap();

            let s = Summary::from_dir(dir.path(), Some("app"));
            assert_eq!(
                s.last_line, None,
                "an index naming {named:?} must read nothing, not a file outside the bundle"
            );
            assert_eq!(
                s.last_words,
                LastWords::Captured,
                "the instance IS terminated, so the status is `captured`; what must not happen is \
                 reading the file it names"
            );
        }
    }

    #[test]
    fn reads_the_headline_facts() {
        let dir = oomkill_bundle();
        let s = Summary::from_dir(dir.path(), None);
        assert_eq!(s.container.as_deref(), Some("app"));
        let t = s.termination.clone().unwrap();
        assert_eq!(t.reason.as_deref(), Some("OOMKilled"));
        assert_eq!(t.exit_code, Some(137));
        assert_eq!(s.restarts, 2);
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
    fn the_changed_field_and_its_values_are_read_from_the_diff_file() {
        // The E2E caught this: a diff detail file is a JSON **array** of changes, and this was
        // reading `{"changes": [...]}`, so `field`/`before`/`after` were always empty — which is
        // the entirety of what a notification's `detail: content` mode adds. The fixtures
        // encoded the wrong shape too, which is why nothing failed. Assert the values, not just
        // that they are dropped again by `facts_only()`.
        let s = Summary::from_dir(oomkill_bundle().path(), None);
        let c = s.change.clone().expect("the diff resolved");
        assert_eq!(c.field.as_deref(), Some("containers[name=app].image"));
        assert_eq!(c.before.as_deref(), Some("shop/checkout:v1.4.2"));
        assert_eq!(c.after.as_deref(), Some("shop/checkout:v1.4.3"));
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
                    "file": "diffs/c.json", "seconds_relative_to_firing": -10 }] })
                .to_string(),
            ),
            (
                "diffs/c.json",
                json!([{ "display": "data.MODE", "before": long, "after": {"a": 1, "b": 2},
                          "changed": true }])
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

    /// A pod that is running now and never terminated. Alerts on live pods
    /// (`KubeContainerWaiting`, memory/CPU) land here, so it must not be dressed up as a crash.
    #[test]
    fn a_running_container_has_no_termination() {
        let pod = |restarts: i64| {
            stage(&[(
                "resources/pod.json",
                json!({
                    "spec": { "containers": [{ "name": "app" }] },
                    "status": { "containerStatuses": [{ "name": "app",
                        "restartCount": restarts,
                        "state": { "running": { "startedAt": "2026-09-20T01:00:00Z" } } }] }
                })
                .to_string(),
            )])
        };

        // No `lastState.terminated`: nothing died, so there is nothing to report as a death.
        let s = Summary::from_dir(pod(3).path(), None);
        assert_eq!(s.container.as_deref(), Some("app"));
        assert!(
            s.termination.is_none(),
            "a running container was reported as terminated: {:?}",
            s.termination
        );
        // The restart count survives the move off `Termination` — it is the live pod's news.
        assert_eq!(s.restarts, 3);
        // …and it is worth sending: three restarts on a pod that is up right now is exactly
        // what an on-call engineer needs, so the "say nothing" gate must not swallow it.
        assert!(!s.is_empty());

        // A quiet running pod, though, really does say nothing: no termination, no restarts,
        // no change, no metrics. This is the case the caller's gate exists for, and before the
        // fix it was unreachable for any pod that merely had a container status.
        let quiet = Summary::from_dir(pod(0).path(), None);
        assert!(quiet.termination.is_none() && quiet.restarts == 0);
        assert!(quiet.is_empty());
    }

    /// The content/facts split is a deny-list over a clone, so a field added to `Summary`
    /// later is carried into a chat channel by default. This pin makes that addition fail
    /// here first. Same spirit as the controller's `every_documented_series_is_emitted`.
    #[test]
    fn every_summary_field_is_classified_fact_or_content() {
        // Fully populated on purpose: a field skipped when empty still has to show up.
        let full = Summary {
            container: Some("app".into()),
            termination: Some(Termination {
                reason: Some("OOMKilled".into()),
                exit_code: Some(137),
                finished_at: Some("2026-09-20T01:00:05Z".into()),
            }),
            restarts: 2,
            last_words: LastWords::Captured,
            last_line: Some("fatal: out of memory".into()),
            change: Some(Change {
                kind: "Deployment".into(),
                name: "checkout".into(),
                revision_from: Some("1".into()),
                revision_to: Some("2".into()),
                seconds_before_alert: Some(94),
                actor: Some("argocd-application-controller".into()),
                field: Some("spec.template.spec.containers[app].image".into()),
                before: Some("shop/checkout:v1.4.2".into()),
                after: Some("shop/checkout:v1.4.3".into()),
            }),
            memory: Some(Memory {
                peak_bytes: 64_200_000.0,
                limit_bytes: Some(67_108_864.0),
                samples: 2,
            }),
            events: 2,
            collectors_run: vec!["logs".into()],
            collectors_missing: vec!["metrics".into()],
            collectors_deferred: vec!["events".into()],
        };
        let Value::Object(map) = serde_json::to_value(&full).unwrap() else {
            panic!("a Summary must serialize as a JSON object");
        };
        let fields: std::collections::BTreeSet<&str> = map.keys().map(String::as_str).collect();

        // Every top-level field of `Summary`, each classified:
        //   fact    — Kubernetes/Lapilli's own observation; safe for any channel.
        //   content — taken from the workload; `facts_only()` must clear it.
        let classified = [
            // fact: the target container's name.
            "container",
            // fact: kubelet-reported reason, exit code and finish time.
            "termination",
            // fact: `restartCount`.
            "restarts",
            // fact: whether the dead instance's log survived to the bundle.
            "last_words",
            // CONTENT: a log line from the workload. Cleared by facts_only() AND
            // without_log_line().
            "last_line",
            // fact, except its `field`/`before`/`after` — those are CONTENT (spec paths and
            // values out of the workload) and facts_only() clears them.
            "change",
            // fact: peak and limit bytes.
            "memory",
            // fact: a count of timeline events.
            "events",
            // fact: Lapilli's own collector names.
            "collectors_run",
            // fact: Lapilli's own collector names.
            "collectors_missing",
            // fact: Lapilli's own collector names, declared by the producer as not intended
            // (IEB rule 6). Nothing in it comes out of the workload.
            "collectors_deferred",
        ];
        let expected: std::collections::BTreeSet<&str> = classified.iter().copied().collect();

        const HOWTO: &str = "Summary's top-level fields changed. The content/facts split \
            (facts_only(), without_log_line()) is a DENY-list, so a new field reaches a chat \
            channel by default. Classify the field as `fact` or `content`: if it is content \
            (anything read out of the workload — logs, env, spec values, annotations), clear it \
            in facts_only() (and in without_log_line() if it is a log line), then add it to \
            this test's `classified` list with a comment saying which it is.";
        assert_eq!(fields, expected, "{HOWTO}");
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

    #[test]
    fn the_sidecar_round_trips_through_serde() {
        // `enqueue_notification` reads `<incident>.summary.json` back and falls through to
        // `Summary::default()` — which `is_empty()` calls empty, so the capture is never
        // announced — if deserialization fails for any reason. A field added without a
        // `serde(default)` is exactly the kind of thing that would do that silently.
        let s = Summary {
            container: Some("app".into()),
            termination: Some(Termination {
                reason: Some("Error".into()),
                exit_code: Some(42),
                finished_at: Some("2026-09-20T04:03:00Z".into()),
            }),
            restarts: 3,
            last_words: LastWords::Captured,
            last_line: None,
            change: None,
            memory: None,
            events: 4,
            collectors_run: vec!["logs".into()],
            collectors_missing: vec![],
            collectors_deferred: vec!["events".into()],
        };
        let bytes = serde_json::to_vec_pretty(&s).unwrap();
        let back: Summary = serde_json::from_slice(&bytes).expect("the sidecar must read back");
        assert_eq!(back, s);
        assert!(
            !back.is_empty(),
            "a crashlooping pod's summary must not read as empty"
        );
    }
}
