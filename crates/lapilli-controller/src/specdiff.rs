//! Pure pod-template diffing for `diffs/` (normalization v1, see [`docs/design-change-diff.md`](https://github.com/lapilli-project/lapilli/blob/main/docs/design-change-diff.md)).
//!
//! Changes are detected on the raw templates, but every value that leaves this module is
//! read from a **redacted** copy at the same path, so a raw credential is only ever compared,
//! never emitted. Redaction replaces strings in place, so paths line up between the two.

use serde_json::{json, Map, Value};

/// Which list fields are matched by merge key rather than position, and by which key(s).
fn merge_keys(field: &str) -> Option<&'static [&'static str]> {
    Some(match field {
        "containers"
        | "initContainers"
        | "ephemeralContainers"
        | "env"
        | "volumes"
        | "imagePullSecrets" => &["name"],
        "volumeMounts" | "volumeDevices" => &["mountPath"],
        "ports" => &["containerPort", "protocol"],
        _ => return None,
    })
}

/// Normalization v1: the pod template minus what differs between revisions by construction
/// (the hash labels the controllers stamp on each ReplicaSet / ControllerRevision).
pub fn normalize(template: &Value) -> Value {
    let mut t = template.clone();
    if let Some(labels) = t
        .get_mut("metadata")
        .and_then(|m| m.get_mut("labels"))
        .and_then(Value::as_object_mut)
    {
        labels.remove("pod-template-hash");
        labels.remove("controller-revision-hash");
    }
    if let Some(meta) = t.get_mut("metadata").and_then(Value::as_object_mut) {
        meta.remove("creationTimestamp");
    }
    t
}

#[derive(Debug, Clone, PartialEq)]
pub struct Change {
    pub op: &'static str, // add | remove | replace
    pub display: String,
    pub path_before: Option<String>,
    pub path_after: Option<String>,
}

/// Diff two normalized templates. Paths are relative to the template; `display` drops the
/// leading `spec.` (it is always the pod spec) so it reads `containers[name=app].env[...]`.
pub fn diff(before: &Value, after: &Value) -> Vec<Change> {
    let mut out = Vec::new();
    walk(
        before,
        after,
        &Path::default(),
        &Path::default(),
        None,
        &mut out,
    );
    out
}

#[derive(Clone, Default)]
struct Path {
    display: Vec<String>,
    pointer: Vec<String>,
}

impl Path {
    fn key(&self, k: &str) -> Path {
        let mut p = self.clone();
        p.display.push(k.to_string());
        p.pointer.push(k.replace('~', "~0").replace('/', "~1"));
        p
    }
    fn elem(&self, display: String, index: usize) -> Path {
        let mut p = self.clone();
        if let Some(last) = p.display.last_mut() {
            last.push_str(&display);
        } else {
            p.display.push(display);
        }
        p.pointer.push(index.to_string());
        p
    }
    fn display(&self) -> String {
        let mut parts = self.display.as_slice();
        if parts.first().map(String::as_str) == Some("spec") && parts.len() > 1 {
            parts = &parts[1..];
        }
        parts.join(".")
    }
    fn pointer(&self) -> String {
        self.pointer.iter().map(|p| format!("/{p}")).collect()
    }
}

fn walk(
    before: &Value,
    after: &Value,
    pb: &Path,
    pa: &Path,
    field: Option<&str>,
    out: &mut Vec<Change>,
) {
    match (before, after) {
        (Value::Object(b), Value::Object(a)) => {
            let mut keys: Vec<&String> = b.keys().chain(a.keys()).collect();
            keys.sort();
            keys.dedup();
            for k in keys {
                match (b.get(k), a.get(k)) {
                    (Some(bv), Some(av)) => walk(bv, av, &pb.key(k), &pa.key(k), Some(k), out),
                    (Some(_), None) => out.push(Change {
                        op: "remove",
                        display: pb.key(k).display(),
                        path_before: Some(pb.key(k).pointer()),
                        path_after: None,
                    }),
                    (None, Some(_)) => out.push(Change {
                        op: "add",
                        display: pa.key(k).display(),
                        path_before: None,
                        path_after: Some(pa.key(k).pointer()),
                    }),
                    (None, None) => {}
                }
            }
        }
        (Value::Array(b), Value::Array(a)) => {
            match field
                .and_then(merge_keys)
                .filter(|keys| unique_keys(b, keys) && unique_keys(a, keys))
            {
                Some(keys) => walk_keyed(b, a, keys, pb, pa, out),
                None => walk_positional(b, a, pb, pa, out),
            }
        }
        (b, a) if b != a => out.push(Change {
            op: "replace",
            display: pa.display(),
            path_before: Some(pb.pointer()),
            path_after: Some(pa.pointer()),
        }),
        _ => {}
    }
}

fn key_of(v: &Value, keys: &[&str]) -> Option<String> {
    let parts: Option<Vec<String>> = keys
        .iter()
        .map(|k| match &v[*k] {
            Value::String(s) => Some(s.clone()),
            Value::Number(n) => Some(n.to_string()),
            // `protocol` defaults to TCP when absent.
            Value::Null if *k == "protocol" => Some("TCP".into()),
            _ => None,
        })
        .collect();
    parts.map(|p| p.join("\u{0}"))
}

/// Duplicate keys (the API allows duplicate env names) → fall back to positional.
fn unique_keys(items: &[Value], keys: &[&str]) -> bool {
    let mut seen = std::collections::HashSet::new();
    items
        .iter()
        .all(|i| key_of(i, keys).is_some_and(|k| seen.insert(k)))
}

/// `[name=app]`, `[containerPort=8080,protocol=TCP]`; `\ ] = ,` escaped in values.
fn key_display(v: &Value, keys: &[&str]) -> String {
    let esc = |s: &str| {
        s.replace('\\', "\\\\")
            .replace(']', "\\]")
            .replace('=', "\\=")
            .replace(',', "\\,")
    };
    let parts: Vec<String> = keys
        .iter()
        .map(|k| {
            let raw = match &v[*k] {
                Value::String(s) => s.clone(),
                Value::Null => "TCP".into(),
                other => other.to_string(),
            };
            format!("{k}={}", esc(&raw))
        })
        .collect();
    format!("[{}]", parts.join(","))
}

fn walk_keyed(
    b: &[Value],
    a: &[Value],
    keys: &[&str],
    pb: &Path,
    pa: &Path,
    out: &mut Vec<Change>,
) {
    let index = |items: &[Value]| -> Vec<(String, usize)> {
        items
            .iter()
            .enumerate()
            .filter_map(|(i, v)| key_of(v, keys).map(|k| (k, i)))
            .collect()
    };
    let (bi, ai) = (index(b), index(a));
    let find = |idx: &[(String, usize)], k: &str| idx.iter().find(|(kk, _)| kk == k).map(|x| x.1);
    for (k, i) in &bi {
        let disp = key_display(&b[*i], keys);
        match find(&ai, k) {
            Some(j) => walk(
                &b[*i],
                &a[j],
                &pb.elem(disp.clone(), *i),
                &pa.elem(disp, j),
                None,
                out,
            ),
            None => out.push(Change {
                op: "remove",
                display: pb.elem(disp, *i).display(),
                path_before: Some(pb.elem(String::new(), *i).pointer()),
                path_after: None,
            }),
        }
    }
    for (k, j) in &ai {
        if find(&bi, k).is_none() {
            let disp = key_display(&a[*j], keys);
            out.push(Change {
                op: "add",
                display: pa.elem(disp, *j).display(),
                path_before: None,
                path_after: Some(pa.elem(String::new(), *j).pointer()),
            });
        }
    }
}

fn walk_positional(b: &[Value], a: &[Value], pb: &Path, pa: &Path, out: &mut Vec<Change>) {
    for i in 0..b.len().max(a.len()) {
        let (sb, sa) = (pb.elem(format!("[{i}]"), i), pa.elem(format!("[{i}]"), i));
        match (b.get(i), a.get(i)) {
            (Some(bv), Some(av)) => walk(bv, av, &sb, &sa, None, out),
            (Some(_), None) => out.push(Change {
                op: "remove",
                display: sb.display(),
                path_before: Some(sb.pointer()),
                path_after: None,
            }),
            (None, Some(_)) => out.push(Change {
                op: "add",
                display: sa.display(),
                path_before: None,
                path_after: Some(sa.pointer()),
            }),
            (None, None) => {}
        }
    }
}

/// The change list as written to `diffs/<ns>/<Kind>/<name>/<n>.json`, with values taken from
/// the redacted templates (pointers are into the template; the file prefixes them with
/// `/spec/template`).
pub fn render(changes: &[Change], before_red: &Value, after_red: &Value) -> Vec<Value> {
    changes
        .iter()
        .map(|c| {
            let mut m = Map::new();
            m.insert("op".into(), json!(c.op));
            m.insert("display".into(), json!(c.display));
            if let Some(p) = &c.path_before {
                m.insert("path_before".into(), json!(format!("/spec/template{p}")));
                m.insert(
                    "before".into(),
                    before_red.pointer(p).cloned().unwrap_or(Value::Null),
                );
            }
            if let Some(p) = &c.path_after {
                m.insert("path_after".into(), json!(format!("/spec/template{p}")));
                m.insert(
                    "after".into(),
                    after_red.pointer(p).cloned().unwrap_or(Value::Null),
                );
            }
            m.insert("changed".into(), json!(true));
            Value::Object(m)
        })
        .collect()
}

/// One human line per change (`containers[name=app].env[name=X].value: lazy → eager`),
/// at most `max` lines plus a "+N more" line.
pub fn summary(rendered: &[Value], max: usize) -> Vec<String> {
    let short = |v: &Value| -> String {
        let s = match v {
            Value::String(s) => s.clone(),
            other => other.to_string(),
        };
        if s.chars().count() > 60 {
            format!("{}…", s.chars().take(59).collect::<String>())
        } else {
            s
        }
    };
    let mut lines: Vec<String> = rendered
        .iter()
        .take(max)
        .map(|c| {
            let d = c["display"].as_str().unwrap_or("?");
            match c["op"].as_str() {
                Some("add") if c["probable_default"] == true => {
                    format!("{d}: + {} (probably a new API default)", short(&c["after"]))
                }
                Some("add") => format!("{d}: + {}", short(&c["after"])),
                Some("remove") => format!("{d}: − {}", short(&c["before"])),
                _ => format!("{d}: {} → {}", short(&c["before"]), short(&c["after"])),
            }
        })
        .collect();
    if rendered.len() > max {
        lines.push(format!("(+{} more)", rendered.len() - max));
    }
    lines
}

/// ControllerRevision data is stored raw and never re-defaulted, so after an API server
/// upgrade a newer revision can carry defaults the older one lacks. An `add` whose value is
/// the Kubernetes default for that field is flagged `probable_default` (not dropped).
pub fn mark_probable_defaults(rendered: &mut [Value]) {
    const DEFAULTS: &[(&str, &str)] = &[
        ("terminationMessagePath", "\"/dev/termination-log\""),
        ("terminationMessagePolicy", "\"File\""),
        ("imagePullPolicy", "\"IfNotPresent\""),
        ("dnsPolicy", "\"ClusterFirst\""),
        ("restartPolicy", "\"Always\""),
        ("schedulerName", "\"default-scheduler\""),
        ("securityContext", "{}"),
        ("enableServiceLinks", "true"),
        ("terminationGracePeriodSeconds", "30"),
        ("protocol", "\"TCP\""),
        ("timeoutSeconds", "1"),
        ("periodSeconds", "10"),
        ("successThreshold", "1"),
        ("failureThreshold", "3"),
    ];
    for c in rendered.iter_mut().filter(|c| c["op"] == "add") {
        let field = c["display"]
            .as_str()
            .and_then(|d| d.rsplit('.').next())
            .unwrap_or_default()
            .to_string();
        let value = c["after"].to_string();
        if DEFAULTS.iter().any(|(f, v)| *f == field && *v == value) {
            c["probable_default"] = json!(true);
        }
    }
}

/// Only the restart annotation changed: a restart, not a config change — though a restart is
/// exactly when out-of-band changes (in-place ConfigMap edits, mutable tags) take effect.
pub fn is_restart_only(changes: &[Change]) -> bool {
    !changes.is_empty()
        && changes
            .iter()
            .all(|c| c.display.contains("kubectl.kubernetes.io/restartedAt"))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn template(env: Value, image: &str) -> Value {
        json!({
            "metadata": { "labels": { "app": "checkout", "pod-template-hash": "abc" } },
            "spec": { "containers": [
                { "name": "app", "image": image, "env": env,
                  "ports": [{ "containerPort": 8080 }] }
            ] }
        })
    }

    #[test]
    fn env_change_is_addressed_by_merge_key() {
        let b = normalize(&template(
            json!([{ "name": "A", "value": "1" }, { "name": "CACHE_WARMUP", "value": "lazy" }]),
            "app:1",
        ));
        let a = normalize(&template(
            // Reordered + one value changed: only the value change may show.
            json!([{ "name": "CACHE_WARMUP", "value": "eager" }, { "name": "A", "value": "1" }]),
            "app:1",
        ));
        let changes = diff(&b, &a);
        assert_eq!(changes.len(), 1, "{changes:?}");
        assert_eq!(
            changes[0].display,
            "containers[name=app].env[name=CACHE_WARMUP].value"
        );
        assert_eq!(
            changes[0].path_before.as_deref(),
            Some("/spec/containers/0/env/1/value")
        );
        assert_eq!(
            changes[0].path_after.as_deref(),
            Some("/spec/containers/0/env/0/value")
        );
        let rendered = render(&changes, &b, &a);
        assert_eq!(rendered[0]["before"], "lazy");
        assert_eq!(rendered[0]["after"], "eager");
        assert_eq!(
            rendered[0]["path_after"],
            "/spec/template/spec/containers/0/env/0/value"
        );
        assert_eq!(
            summary(&rendered, 5),
            ["containers[name=app].env[name=CACHE_WARMUP].value: lazy → eager"]
        );
    }

    #[test]
    fn hash_label_is_not_a_change_but_image_is() {
        let b = normalize(&template(json!([]), "app:1"));
        let mut a = template(json!([]), "app:2");
        a["metadata"]["labels"]["pod-template-hash"] = json!("def");
        let changes = diff(&b, &normalize(&a));
        assert_eq!(changes.len(), 1);
        assert_eq!(changes[0].display, "containers[name=app].image");
    }

    #[test]
    fn add_and_remove_have_one_sided_pointers() {
        let b = normalize(&template(json!([{ "name": "OLD", "value": "x" }]), "app:1"));
        let a = normalize(&template(json!([{ "name": "NEW", "value": "y" }]), "app:1"));
        let r = render(&diff(&b, &a), &b, &a);
        let remove = r.iter().find(|c| c["op"] == "remove").unwrap();
        let add = r.iter().find(|c| c["op"] == "add").unwrap();
        assert!(remove.get("path_after").is_none() && remove["before"]["name"] == "OLD");
        assert!(add.get("path_before").is_none() && add["after"]["name"] == "NEW");
        assert_eq!(add["display"], "containers[name=app].env[name=NEW]");
    }

    #[test]
    fn values_come_from_the_redacted_copy() {
        let b = template(
            json!([{ "name": "DB_PASSWORD", "value": "old-secret" }]),
            "app:1",
        );
        let a = template(
            json!([{ "name": "DB_PASSWORD", "value": "new-secret" }]),
            "app:1",
        );
        let changes = diff(&normalize(&b), &normalize(&a));
        assert_eq!(changes.len(), 1);
        let red = |t: &Value| {
            let mut w = json!({ "spec": { "template": t } });
            lapilli_bundle::redact::Policy::default().redact_object(&mut w);
            w["spec"]["template"].clone()
        };
        let r = render(&changes, &red(&b), &red(&a));
        let out = serde_json::to_string(&r).unwrap();
        assert!(!out.contains("secret\""), "{out}");
        assert_eq!(r[0]["before"], "<redacted>");
        assert_eq!(r[0]["changed"], true);
    }

    #[test]
    fn duplicate_env_names_fall_back_to_positional() {
        let b = normalize(&template(
            json!([{ "name": "X", "value": "1" }, { "name": "X", "value": "2" }]),
            "app:1",
        ));
        let a = normalize(&template(
            json!([{ "name": "X", "value": "1" }, { "name": "X", "value": "3" }]),
            "app:1",
        ));
        let changes = diff(&b, &a);
        assert_eq!(changes[0].display, "containers[name=app].env[1].value");
    }

    #[test]
    fn composite_keys_and_escaping() {
        let v = json!({ "containerPort": 8080 });
        assert_eq!(
            key_display(&v, &["containerPort", "protocol"]),
            "[containerPort=8080,protocol=TCP]"
        );
        let v = json!({ "mountPath": "/a]b=c" });
        assert_eq!(key_display(&v, &["mountPath"]), "[mountPath=/a\\]b\\=c]");
    }

    #[test]
    fn new_defaults_are_flagged_not_dropped() {
        let mut r = vec![
            json!({ "op": "add", "display": "containers[name=app].terminationMessagePolicy", "after": "File" }),
            json!({ "op": "add", "display": "containers[name=app].env[name=X]", "after": { "name": "X" } }),
        ];
        mark_probable_defaults(&mut r);
        assert_eq!(r[0]["probable_default"], true);
        assert!(r[1].get("probable_default").is_none());
    }

    #[test]
    fn restart_only_detection() {
        let mut b = template(json!([]), "app:1");
        b["metadata"]["annotations"] = json!({ "kubectl.kubernetes.io/restartedAt": "t1" });
        let mut a = b.clone();
        a["metadata"]["annotations"]["kubectl.kubernetes.io/restartedAt"] = json!("t2");
        assert!(is_restart_only(&diff(&normalize(&b), &normalize(&a))));
    }
}
