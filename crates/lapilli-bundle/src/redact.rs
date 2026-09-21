//! Redaction v1: best-effort removal of credentials from captured API objects **before**
//! they are written into a bundle, so every file downstream only ever sees redacted data.
//!
//! Scope (normative table in `spec/IEB-SPEC.md`): in pod specs and pod templates, env values,
//! args/command (incl. lifecycle/probe exec), probe/lifecycle HTTP header values, and
//! annotations; plus free-text event messages. `kubectl.kubernetes.io/last-applied-configuration`
//! is dropped outright. Container logs are **not** redacted: they are the evidence.
//!
//! Best-effort by design (docs/design-change-diff.md). A missing pattern is fixed by adding a
//! rule plus a test vector; deployments that need a guarantee use [`Mode::Strict`].

use serde::{Deserialize, Serialize};
use serde_json::Value;

pub const POLICY_VERSION: &str = "v1";
pub const REDACTED: &str = "<redacted>";
const LAST_APPLIED: &str = "kubectl.kubernetes.io/last-applied-configuration";

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Mode {
    /// Name and value rules below.
    #[default]
    Default,
    /// Every candidate value is redacted unless its name is in `plaintext`.
    Strict,
    /// Nothing is redacted; recorded in `redaction.json` and flagged by `lapilli verify`.
    Off,
}

#[derive(Clone, Debug, Default)]
pub struct Policy {
    pub mode: Mode,
    /// Names (env names, header names, annotation keys) exempt from `Strict`.
    pub plaintext: Vec<String>,
}

/// What one redaction pass did, for `redaction.json`.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize)]
pub struct Tally {
    /// Number of values replaced by [`REDACTED`].
    pub values: usize,
    /// Fields removed entirely (e.g. the last-applied-configuration annotation).
    pub dropped: Vec<String>,
}

impl Policy {
    /// Redact a Kubernetes object in place: its annotations, and its pod spec or pod template
    /// (Pod, ReplicaSet, Deployment, StatefulSet, DaemonSet, Job all fit one of the two).
    pub fn redact_object(&self, obj: &mut Value) -> Tally {
        let mut t = Tally::default();
        if self.mode == Mode::Off {
            return t;
        }
        if let Some(meta) = obj.get_mut("metadata") {
            self.redact_meta(meta, "metadata", &mut t);
        }
        let Some(spec) = obj.get_mut("spec") else {
            return t;
        };
        if spec.get("containers").is_some_and(Value::is_array) {
            self.redact_pod_spec(spec, &mut t);
        }
        if let Some(template) = spec.get_mut("template") {
            if let Some(meta) = template.get_mut("metadata") {
                self.redact_meta(meta, "spec.template.metadata", &mut t);
            }
            if let Some(pod_spec) = template.get_mut("spec") {
                self.redact_pod_spec(pod_spec, &mut t);
            }
        }
        t
    }

    /// Redact one ConfigMap `data` value under its key. Single-line values use the name
    /// rule on the key; JSON values are redacted per inner key; other multi-line values
    /// (YAML, `.properties`, `.env`, INI) per `key: value` / `key=value` line, with the name
    /// rule applied to each line's key. `binaryData` never goes through here: it is always
    /// fully redacted by the caller.
    pub fn redact_config_value(&self, key: &str, value: &str, t: &mut Tally) -> String {
        if self.mode == Mode::Off {
            return value.to_string();
        }
        if !value.contains('\n') {
            return self.redact_named(key, value, t);
        }
        if self.mode == Mode::Strict && !self.plaintext.iter().any(|p| p == key) {
            t.values += 1;
            return REDACTED.to_string();
        }
        let trimmed = value.trim_start();
        if trimmed.starts_with('{') || trimmed.starts_with('[') {
            if let Ok(mut v) = serde_json::from_str::<Value>(value) {
                self.redact_json_tree(&mut v, t);
                return serde_json::to_string_pretty(&v).unwrap_or_default();
            }
        }
        value
            .split_inclusive('\n')
            .map(|line| self.redact_config_line(line, t))
            .collect()
    }

    fn redact_config_line(&self, line: &str, t: &mut Tally) -> String {
        let body = line.trim_end_matches(['\n', '\r']);
        let eol = &line[body.len()..];
        let indent_len = body.len() - body.trim_start().len();
        let (indent, rest) = body.split_at(indent_len);
        // Comments stay; a `- item` list entry is judged by its content.
        if rest.starts_with('#') || rest.starts_with(';') || rest.is_empty() {
            return line.to_string();
        }
        let sep = match (rest.find(':'), rest.find('=')) {
            (Some(c), Some(e)) => Some(c.min(e)),
            (c, e) => c.or(e),
        };
        let Some(i) = sep.filter(|&i| i > 0 && !rest[..i].contains(char::is_whitespace)) else {
            return format!("{indent}{}{eol}", redact_tokens(rest, false, t));
        };
        // `key: value` keeps the space after the separator.
        let (k, after_sep) = (&rest[..i], &rest[i + 1..]);
        let ws = &after_sep[..after_sep.len() - after_sep.trim_start().len()];
        let v = after_sep.trim_start();
        let unquoted = v.trim_matches(['"', '\'']);
        if unquoted.is_empty() {
            return line.to_string(); // a YAML parent key (`db:`)
        }
        let name = k.trim_start_matches("- ").trim_start_matches("export ");
        let class = classify_name(name);
        let new_v = if class != NameClass::None && !value_allowed(class, unquoted) {
            t.values += 1;
            REDACTED.to_string()
        } else {
            redact_tokens(v, false, t)
        };
        format!("{indent}{k}{}{ws}{new_v}{eol}", &rest[i..i + 1])
    }

    /// Redact free text (e.g. an event message) with the token rules. Best-effort even in
    /// `strict`: arbitrary prose can hide a secret in a shape no rule matches (see
    /// spec/IEB-SPEC.md). `strict` does tighten it — an unknown `name=value` or
    /// `name: value` token has its value redacted.
    pub fn redact_text(&self, text: &str, t: &mut Tally) -> String {
        if self.mode == Mode::Off {
            return text.to_string();
        }
        redact_tokens(text, self.mode == Mode::Strict, t)
    }

    fn redact_meta(&self, meta: &mut Value, at: &str, t: &mut Tally) {
        let Some(ann) = meta.get_mut("annotations").and_then(Value::as_object_mut) else {
            return;
        };
        if ann.remove(LAST_APPLIED).is_some() {
            t.dropped.push(format!("{at}.annotations[{LAST_APPLIED}]"));
        }
        for (key, value) in ann.iter_mut() {
            // Kubernetes' own annotations (revision numbers, restartedAt) are never secrets
            // and are what readers need to follow a rollout.
            if is_k8s_annotation(key) {
                continue;
            }
            if let Value::String(s) = value {
                *s = self.redact_named(key, s, t);
            }
        }
    }

    fn redact_pod_spec(&self, spec: &mut Value, t: &mut Tally) {
        for list in ["initContainers", "containers", "ephemeralContainers"] {
            let Some(containers) = spec.get_mut(list).and_then(Value::as_array_mut) else {
                continue;
            };
            for c in containers {
                self.redact_container(c, t);
            }
        }
    }

    fn redact_container(&self, c: &mut Value, t: &mut Tally) {
        if let Some(env) = c.get_mut("env").and_then(Value::as_array_mut) {
            for e in env {
                let name = e["name"].as_str().unwrap_or_default().to_string();
                if let Some(Value::String(v)) = e.get_mut("value") {
                    *v = self.redact_named(&name, v, t);
                }
            }
        }
        for field in ["command", "args"] {
            if let Some(argv) = c.get_mut(field) {
                self.redact_argv(argv, t);
            }
        }
        if let Some(lifecycle) = c.get_mut("lifecycle") {
            for hook in ["postStart", "preStop"] {
                if let Some(h) = lifecycle.get_mut(hook) {
                    self.redact_handler(h, t);
                }
            }
        }
        for probe in ["livenessProbe", "readinessProbe", "startupProbe"] {
            if let Some(h) = c.get_mut(probe) {
                self.redact_handler(h, t);
            }
        }
    }

    /// A probe or lifecycle handler: `exec.command[]` and `httpGet.httpHeaders[]`.
    fn redact_handler(&self, h: &mut Value, t: &mut Tally) {
        if let Some(cmd) = h.get_mut("exec").and_then(|e| e.get_mut("command")) {
            self.redact_argv(cmd, t);
        }
        if let Some(headers) = h
            .get_mut("httpGet")
            .and_then(|g| g.get_mut("httpHeaders"))
            .and_then(Value::as_array_mut)
        {
            for hd in headers {
                let name = hd["name"].as_str().unwrap_or_default().to_string();
                if let Some(Value::String(v)) = hd.get_mut("value") {
                    *v = self.redact_named(&name, v, t);
                }
            }
        }
    }

    /// argv: each element is tokenized (a `sh -c` script is one element), and a
    /// `-flag value` pair spanning two elements is handled too.
    fn redact_argv(&self, argv: &mut Value, t: &mut Tally) {
        let Some(items) = argv.as_array_mut() else {
            return;
        };
        let mut pending: Option<NameClass> = None;
        for item in items {
            let Value::String(s) = item else { continue };
            if let Some(class) = pending.take() {
                if !value_allowed(class, s) {
                    *s = REDACTED.to_string();
                    t.values += 1;
                    continue;
                }
            }
            if let Some(flag) = s
                .strip_prefix('-')
                .filter(|f| !f.contains('=') && !f.contains(' '))
            {
                let class = classify_name(flag.trim_start_matches('-'));
                if class != NameClass::None {
                    pending = Some(class);
                }
            }
            *s = redact_tokens(s, self.mode == Mode::Strict, t);
        }
    }

    /// A value under a name (env var, header, annotation key).
    fn redact_named(&self, name: &str, value: &str, t: &mut Tally) -> String {
        if self.mode == Mode::Strict {
            if self.plaintext.iter().any(|p| p == name) {
                return value.to_string();
            }
            t.values += 1;
            return REDACTED.to_string();
        }
        let class = classify_name(name);
        if class != NameClass::None && !value_allowed(class, value) {
            t.values += 1;
            return REDACTED.to_string();
        }
        // JSON-valued annotations and config: redact per inner key.
        let trimmed = value.trim_start();
        if trimmed.starts_with('{') || trimmed.starts_with('[') {
            if let Ok(mut v) = serde_json::from_str::<Value>(value) {
                let before = t.values;
                self.redact_json_tree(&mut v, t);
                if t.values > before {
                    return v.to_string();
                }
                return value.to_string();
            }
        }
        redact_tokens(value, false, t)
    }

    fn redact_json_tree(&self, v: &mut Value, t: &mut Tally) {
        match v {
            Value::Object(map) => {
                for (k, child) in map.iter_mut() {
                    match child {
                        Value::String(s) => *s = self.redact_named(k, s, t),
                        other => self.redact_json_tree(other, t),
                    }
                }
            }
            Value::Array(items) => {
                for item in items {
                    if let Value::String(s) = item {
                        *s = redact_tokens(s, false, t);
                    } else {
                        self.redact_json_tree(item, t);
                    }
                }
            }
            _ => {}
        }
    }
}

// --- names -------------------------------------------------------------------------------

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum NameClass {
    None,
    /// `key`, `token`, `auth`, `cert`: often not secret (`CACHE_KEY_PREFIX`,
    /// `TOKEN_TTL_SECONDS`), so plainly non-secret values stay visible.
    Weak,
    /// `password`, `secret`, `credential`, …: redacted unless boolean or duration.
    Strong,
}

const STRONG: &[&str] = &[
    "password",
    "passwd",
    "pass",
    "pwd",
    "passphrase",
    "secret",
    "secrets",
    "credential",
    "credentials",
    "private",
    "privatekey",
    "apikey",
    "dsn",
    "authorization",
    "bearer",
    "connectionstring",
];
const WEAK: &[&str] = &["key", "token", "auth", "cert", "signature", "sig"];
/// Adjacent token pairs that are strong even though each token alone is weak or neutral.
const STRONG_PAIRS: &[(&str, &str)] = &[
    ("api", "key"),
    ("access", "key"),
    ("private", "key"),
    ("client", "secret"),
    ("connection", "string"),
    ("access", "token"),
    ("refresh", "token"),
    ("auth", "token"),
];

/// Split a name on separators and camelCase boundaries: `DB_PASSWORD`, `db.password`,
/// `dbPassword`, `--db-password` all yield `[db, password]`.
fn name_tokens(name: &str) -> Vec<String> {
    let mut tokens = Vec::new();
    let mut cur = String::new();
    let mut prev_lower = false;
    for ch in name.chars() {
        if !ch.is_ascii_alphanumeric() {
            if !cur.is_empty() {
                tokens.push(std::mem::take(&mut cur));
            }
            prev_lower = false;
            continue;
        }
        if ch.is_ascii_uppercase() && prev_lower && !cur.is_empty() {
            tokens.push(std::mem::take(&mut cur));
        }
        prev_lower = ch.is_ascii_lowercase() || ch.is_ascii_digit();
        cur.push(ch.to_ascii_lowercase());
    }
    if !cur.is_empty() {
        tokens.push(cur);
    }
    tokens
}

fn classify_name(name: &str) -> NameClass {
    let tokens = name_tokens(name);
    // Glued names (`PGPASSWORD`, `DBSECRET`) are one token; long strong words also count
    // as a prefix or suffix. Short ones (`pass`, `key`) don't, or `bypass`/`monkey` match.
    let glued = |t: &String| {
        ["password", "passwd", "secret", "credential", "passphrase"]
            .iter()
            .any(|w| t.len() > w.len() && (t.ends_with(w) || t.starts_with(w)))
    };
    if tokens
        .iter()
        .any(|t| STRONG.contains(&t.as_str()) || glued(t))
        || tokens
            .windows(2)
            .any(|w| STRONG_PAIRS.contains(&(w[0].as_str(), w[1].as_str())))
    {
        NameClass::Strong
    } else if tokens.iter().any(|t| WEAK.contains(&t.as_str())) {
        NameClass::Weak
    } else {
        NameClass::None
    }
}

/// Whether a value under a secret-looking name may stay visible.
fn value_allowed(class: NameClass, value: &str) -> bool {
    let v = value.trim();
    let boolean = matches!(
        v.to_ascii_lowercase().as_str(),
        "true" | "false" | "yes" | "no" | "on" | "off" | "enabled" | "disabled"
    );
    match class {
        NameClass::None => true,
        NameClass::Strong => boolean || is_duration(v) || v.is_empty(),
        NameClass::Weak => {
            boolean
                || v.is_empty()
                || is_duration(v)
                || v.parse::<i64>().is_ok()
                || (v.len() <= 12
                    && v.bytes().all(|b| {
                        b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'_' || b == b'-'
                    })
                    && !looks_secret(v))
                || (v.starts_with('/') && !v.contains(char::is_whitespace))
                || (v.contains("://") && url_is_clean(v))
        }
    }
}

fn is_duration(v: &str) -> bool {
    // Go/Kubernetes durations: one or more <number><unit>, e.g. 30s, 1h30m, 1.5h.
    let mut rest = v;
    if rest.is_empty() {
        return false;
    }
    while !rest.is_empty() {
        let num_len = rest
            .find(|c: char| !(c.is_ascii_digit() || c == '.'))
            .unwrap_or(rest.len());
        if num_len == 0 {
            return false;
        }
        rest = &rest[num_len..];
        let unit = ["ns", "us", "µs", "ms", "s", "m", "h", "d"]
            .iter()
            .filter(|u| rest.starts_with(**u))
            .max_by_key(|u| u.len());
        match unit {
            Some(u) => rest = &rest[u.len()..],
            None => return false,
        }
    }
    true
}

fn is_k8s_annotation(key: &str) -> bool {
    let domain = key.split('/').next().unwrap_or_default();
    key.contains('/')
        && (domain == "kubernetes.io"
            || domain.ends_with(".kubernetes.io")
            || domain == "k8s.io"
            || domain.ends_with(".k8s.io"))
}

// --- values ------------------------------------------------------------------------------

/// The value rule: does this token look like a credential regardless of its name?
fn looks_secret(v: &str) -> bool {
    const PREFIXES: &[&str] = &[
        "ghp_",
        "gho_",
        "ghu_",
        "ghs_",
        "ghr_",
        "github_pat_",
        "glpat-",
        "xoxa-",
        "xoxb-",
        "xoxp-",
        "xoxr-",
        "xoxs-",
        "sk-",
        "sk_live_",
        "sk_test_",
        "rk_live_",
        "AIza",
        "-----BEGIN",
    ];
    if PREFIXES
        .iter()
        .any(|p| v.starts_with(p) && v.len() > p.len() + 8)
    {
        return true;
    }
    if (v.starts_with("AKIA") || v.starts_with("ASIA"))
        && v.len() == 20
        && v.bytes()
            .all(|b| b.is_ascii_uppercase() || b.is_ascii_digit())
    {
        return true;
    }
    if is_jwt(v) {
        return true;
    }
    // Canonical UUIDs are ids (tenants, requests, objects), not credentials, far more often
    // than not. A UUID used as a key is still caught by its name (`API_KEY`, `*_SECRET`).
    if is_uuid(v) {
        return false;
    }
    let hex = v.bytes().all(|b| b.is_ascii_hexdigit());
    if hex && v.len() >= 32 && entropy(v) >= 3.0 {
        return true;
    }
    let b64 = v
        .bytes()
        .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'+' | b'/' | b'=' | b'-' | b'_'));
    b64 && v.len() >= 24 && entropy(v) >= 4.0 && !v.contains("//")
}

fn is_uuid(v: &str) -> bool {
    let parts: Vec<&str> = v.split('-').collect();
    parts.iter().map(|p| p.len()).eq([8, 4, 4, 4, 12])
        && parts
            .iter()
            .all(|p| p.bytes().all(|b| b.is_ascii_hexdigit()))
}

fn is_jwt(v: &str) -> bool {
    let parts: Vec<&str> = v.split('.').collect();
    parts.len() == 3
        && parts[0].starts_with("eyJ")
        && parts[1].starts_with("eyJ")
        && parts.iter().all(|p| {
            p.bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
        })
}

/// Shannon entropy in bits per character.
fn entropy(s: &str) -> f64 {
    let mut counts = [0usize; 256];
    for b in s.bytes() {
        counts[b as usize] += 1;
    }
    let n = s.len() as f64;
    counts
        .iter()
        .filter(|&&c| c > 0)
        .map(|&c| {
            let p = c as f64 / n;
            -p * p.log2()
        })
        .sum()
}

/// Query parameter names whose value is always a credential.
const CRED_PARAMS: &[&str] = &[
    "password",
    "pwd",
    "passwd",
    "pass",
    "token",
    "access_token",
    "secret",
    "sig",
    "signature",
    "x-amz-signature",
    "x-amz-credential",
    "x-amz-security-token",
    "api_key",
    "apikey",
    "key",
];

fn url_is_clean(url: &str) -> bool {
    let mut t = Tally::default();
    redact_url(url, &mut t) == url
}

/// Redact the userinfo password, credential query parameters, and secret-looking path
/// segments of a URL; everything else (scheme, host, path) stays readable.
fn redact_url(url: &str, t: &mut Tally) -> String {
    let Some(sep) = url.find("://") else {
        return url.to_string();
    };
    let (scheme, rest) = url.split_at(sep + 3);
    let (authority_and_path, query) = match rest.find('?') {
        Some(q) => (&rest[..q], Some(&rest[q + 1..])),
        None => (rest, None),
    };
    let (authority, path) = match authority_and_path.find('/') {
        Some(p) => authority_and_path.split_at(p),
        None => (authority_and_path, ""),
    };
    let authority = match authority.rfind('@') {
        Some(at) => {
            let userinfo = &authority[..at];
            let user = userinfo.split(':').next().unwrap_or_default();
            t.values += 1;
            if userinfo.contains(':') {
                format!("{user}:{REDACTED}{}", &authority[at..])
            } else {
                format!("{REDACTED}{}", &authority[at..])
            }
        }
        None => authority.to_string(),
    };
    let path: String = path
        .split('/')
        .map(|seg| {
            if looks_secret(seg) {
                t.values += 1;
                REDACTED.to_string()
            } else {
                seg.to_string()
            }
        })
        .collect::<Vec<_>>()
        .join("/");
    let mut out = format!("{scheme}{authority}{path}");
    if let Some(q) = query {
        let (q, frag) = match q.find('#') {
            Some(h) => (&q[..h], &q[h..]),
            None => (q, ""),
        };
        let params: Vec<String> = q
            .split('&')
            .map(|kv| match kv.split_once('=') {
                Some((k, v))
                    if CRED_PARAMS.contains(&k.to_ascii_lowercase().as_str())
                        || classify_name(k) == NameClass::Strong
                        || looks_secret(v) =>
                {
                    t.values += 1;
                    format!("{k}={REDACTED}")
                }
                _ => kv.to_string(),
            })
            .collect();
        out.push('?');
        out.push_str(&params.join("&"));
        out.push_str(frag);
    }
    out
}

/// Tokenize free text / argv elements on whitespace and `;`, and redact each token:
/// `--name=value`, `-Dname=value`, `NAME=value` (incl. after `export`) use the name rule on
/// `name`; URLs use [`redact_url`]; anything else uses the value rule. Separators and
/// surrounding quotes are preserved, so the text keeps its shape.
fn redact_tokens(text: &str, strict: bool, t: &mut Tally) -> String {
    let mut out = String::with_capacity(text.len());
    let mut token = String::new();
    let mut pending = Pending::None;
    for ch in text.chars().chain(std::iter::once('\n')) {
        if ch.is_whitespace() || ch == ';' {
            if !token.is_empty() {
                let (redacted, next) = redact_in_sequence(&token, pending, strict, t);
                out.push_str(&redacted);
                pending = next;
                token.clear();
            }
            out.push(ch);
        } else {
            token.push(ch);
        }
    }
    out.pop(); // the sentinel newline
    out
}

/// What the previous token says about this one (`--password <value>`, `-u user:pass`).
#[derive(Clone, Copy)]
enum Pending {
    None,
    Named(NameClass),
    UserColonPass,
}

fn redact_in_sequence(
    token: &str,
    pending: Pending,
    strict: bool,
    t: &mut Tally,
) -> (String, Pending) {
    match pending {
        Pending::Named(class) if !value_allowed(class, token.trim_matches(['"', '\''])) => {
            // `Authorization: Basic <credential>`: the scheme names what follows, so keep it
            // and redact the token after it instead.
            if matches!(token, "Basic" | "Bearer" | "Digest" | "Token") {
                return (token.to_string(), Pending::Named(class));
            }
            t.values += 1;
            return (REDACTED.to_string(), Pending::None);
        }
        Pending::UserColonPass => {
            if let Some((user, _)) = token.split_once(':') {
                t.values += 1;
                return (format!("{user}:{REDACTED}"), Pending::None);
            }
        }
        _ => {}
    }
    let next = match token.strip_prefix('-') {
        Some(flag) if !flag.contains('=') => {
            let name = flag.trim_start_matches('-');
            if matches!(name, "u" | "user") {
                Pending::UserColonPass
            } else {
                match classify_name(name) {
                    NameClass::None => Pending::None,
                    class => Pending::Named(class),
                }
            }
        }
        // `Authorization: Basic …`, `token: abc` — a header-style name whose value is the
        // next token (a scheme word like `Basic` or `Bearer` is kept, its credential is not).
        _ => match token.strip_suffix(':').map(classify_name) {
            Some(NameClass::None) | None => Pending::None,
            Some(class) => Pending::Named(class),
        },
    };
    (redact_token(token, strict, t), next)
}

fn redact_token(raw: &str, strict: bool, t: &mut Tally) -> String {
    if raw.is_empty() {
        return String::new();
    }
    // Keep surrounding quotes/parens out of the analysis but in the output.
    let start = raw
        .find(|c: char| !matches!(c, '"' | '\'' | '(' | '`'))
        .unwrap_or(raw.len());
    let end = raw
        .rfind(|c: char| !matches!(c, '"' | '\'' | ')' | '`' | ','))
        .map_or(start, |i| i + 1)
        .max(start);
    let (pre, core, post) = (&raw[..start], &raw[start..end], &raw[end..]);
    if core.is_empty() {
        return raw.to_string();
    }

    let is_url = match (core.find("://"), core.find('=')) {
        (Some(u), Some(e)) => u < e,
        (Some(_), None) => true,
        _ => false,
    };
    if is_url {
        return format!("{pre}{}{post}", redact_url(core, t));
    }
    if let Some(eq) = core.find('=').filter(|&i| i > 0) {
        let (lhs, value) = (&core[..eq], &core[eq + 1..]);
        let name = lhs.trim_start_matches('-');
        let name = name
            .strip_prefix('D')
            .filter(|_| lhs.starts_with("-D"))
            .unwrap_or(name);
        let class = classify_name(name);
        let redact_value = if class != NameClass::None {
            !value_allowed(class, value)
        } else {
            strict
        };
        let new_value = if redact_value && !value.is_empty() {
            t.values += 1;
            REDACTED.to_string()
        } else if value.contains("://") {
            redact_url(value, t)
        } else if looks_secret(value) {
            t.values += 1;
            REDACTED.to_string()
        } else {
            value.to_string()
        };
        return format!("{pre}{lhs}={new_value}{post}");
    }
    // `DBPassword:"hunter2"`, `token: abc` — the same rules as `name=value`. Only a name that
    // classifies as a secret (or strict) redacts, so `12:30` and `http/1.1` stay readable.
    if let Some(colon) = core.find(':').filter(|&i| i > 0) {
        let (name, value) = (
            &core[..colon],
            core[colon + 1..].trim_start_matches(['"', '\'']),
        );
        let value = value.trim_end_matches(['"', '\'', ',', '}', ')']);
        let class = classify_name(
            name.trim_start_matches(['{', ',', '.'])
                .rsplit(['.', '{'])
                .next()
                .unwrap_or(name),
        );
        if !value.is_empty() && (class != NameClass::None || strict) && !value_allowed(class, value)
        {
            t.values += 1;
            let kept = &core[..colon + 1];
            return format!("{pre}{kept}{REDACTED}{post}");
        }
    }
    if looks_secret(core) {
        t.values += 1;
        return format!("{pre}{REDACTED}{post}");
    }
    raw.to_string()
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    const CANARY: &str = "lapilliCanary9f3c2b7e";

    fn policy() -> Policy {
        Policy::default()
    }

    /// A pod carrying a planted credential in every candidate path of the normative table,
    /// plus values that must stay readable.
    fn pod() -> Value {
        json!({
          "metadata": { "name": "checkout-7f8b6b66f8-7txr5",
            "uid": "3f1e2d4c-5b6a-7980-a1b2-c3d4e5f60718",
            "annotations": {
              "kubectl.kubernetes.io/last-applied-configuration": format!("{{\"env\":\"{CANARY}\"}}"),
              "deployment.kubernetes.io/revision": "2",
              "vault.example.com/config": format!("{{\"db\":{{\"password\":\"{CANARY}\"}},\"pool\":10}}"),
              "example.com/owner": "team-checkout"
            } },
          "spec": {
            "containers": [{
              "name": "app",
              "image": "123456789012.dkr.ecr.ap-northeast-2.amazonaws.com/checkout@sha256:9b2f5c0e8e3a4d1b7c6f5e4d3c2b1a0f9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b",
              "command": ["sh", "-c", format!("export PGPASSWORD={CANARY} && psql -h db")],
              "args": ["--db-password", CANARY, format!("--api-token={CANARY}"), "-Xmx2g", "--port=8080"],
              "env": [
                { "name": "CACHE_WARMUP", "value": "eager" },
                { "name": "DB_PASSWORD", "value": "hunter2" },
                { "name": "JAVA_OPTS", "value": format!("-Xmx1g -Dspring.datasource.password={CANARY} -XX:+UseG1GC") },
                { "name": "SPRING_DATASOURCE_URL", "value": format!("jdbc:postgresql://db.shop.svc:5432/app?user=app&password={CANARY}") },
                { "name": "UPSTREAM", "value": format!("https://svc:{CANARY}@api.example.com/v1") },
                { "name": "AUTH_SERVICE_URL", "value": "http://auth.shop.svc.cluster.local:8080" },
                { "name": "AUTH_ENABLED", "value": "true" },
                { "name": "TOKEN_TTL_SECONDS", "value": "300" },
                { "name": "PUBLIC_KEY_PATH", "value": "/etc/tls/tls.crt" },
                { "name": "CACHE_KEY_PREFIX", "value": "checkout" },
                { "name": "GITHUB", "value": "ghp_16C7e42F292c6912E7710c838347Ae178B4a" },
                { "name": "SESSION", "value": "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.dGVzdHNpZ25hdHVyZQ" },
                { "name": "HEX_BLOB", "value": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" },
                { "name": "FROM_SECRET", "valueFrom": { "secretKeyRef": { "name": "db", "key": "password" } } }
              ],
              "livenessProbe": { "httpGet": { "path": "/healthz", "port": 8080,
                "httpHeaders": [ { "name": "Authorization", "value": format!("Bearer {CANARY}") },
                                 { "name": "X-Request-Source", "value": "kubelet" } ] } },
              "lifecycle": { "preStop": { "exec": { "command": ["sh", "-c", format!("curl -u admin:{CANARY} http://x/drain")] } } }
            }]
          }
        })
    }

    #[test]
    fn canary_never_survives_and_useful_values_do() {
        let mut p = pod();
        let t = policy().redact_object(&mut p);
        let out = p.to_string();
        assert!(!out.contains(CANARY), "canary leaked: {out}");
        assert!(
            !out.contains("hunter2"),
            "short password under a strong name leaked"
        );
        assert!(!out.contains("ghp_16C7e42F"), "GitHub token leaked");
        assert!(!out.contains("eyJzdWIi"), "JWT leaked");
        assert!(!out.contains("9f86d081884c7d65"), "hex key leaked");
        assert!(t.values >= 12, "{t:?}");
        assert_eq!(
            t.dropped,
            vec!["metadata.annotations[kubectl.kubernetes.io/last-applied-configuration]"]
        );

        // Negative vectors: must stay readable.
        let env = |n: &str| {
            p["spec"]["containers"][0]["env"]
                .as_array()
                .unwrap()
                .iter()
                .find(|e| e["name"] == n)
                .unwrap()["value"]
                .clone()
        };
        assert_eq!(env("CACHE_WARMUP"), "eager");
        assert_eq!(
            env("AUTH_SERVICE_URL"),
            "http://auth.shop.svc.cluster.local:8080"
        );
        assert_eq!(env("AUTH_ENABLED"), "true");
        assert_eq!(env("TOKEN_TTL_SECONDS"), "300");
        assert_eq!(env("PUBLIC_KEY_PATH"), "/etc/tls/tls.crt");
        assert_eq!(env("CACHE_KEY_PREFIX"), "checkout");
        assert_eq!(
            env("SPRING_DATASOURCE_URL"),
            "jdbc:postgresql://db.shop.svc:5432/app?user=app&password=<redacted>"
        );
        assert_eq!(env("UPSTREAM"), "https://svc:<redacted>@api.example.com/v1");
        assert_eq!(
            env("JAVA_OPTS"),
            "-Xmx1g -Dspring.datasource.password=<redacted> -XX:+UseG1GC"
        );
        assert!(
            out.contains("sha256:9b2f5c0e8e3a"),
            "image digest must stay"
        );
        assert!(out.contains("3f1e2d4c-5b6a-7980"), "uid must stay");
        assert!(out.contains("\"--port=8080\""));
        assert!(out.contains("\"-Xmx2g\""));
        assert!(out.contains("kubelet"));
        assert!(out.contains("\"deployment.kubernetes.io/revision\":\"2\""));
        assert!(out.contains("team-checkout"));
        assert!(
            out.contains("\\\"pool\\\":10"),
            "JSON annotation keeps non-secret keys"
        );
        assert!(
            out.contains("secretKeyRef"),
            "valueFrom references are not values"
        );
        // `-flag value` spanning two argv elements.
        assert_eq!(p["spec"]["containers"][0]["args"][1], REDACTED);
        // The object keeps its shape: no nulls inserted for absent probes/hooks.
        assert!(!out.contains("null"), "{out}");
    }

    /// Free text is best-effort; these vectors record what v1 does catch — and, honestly,
    /// what it does not (docs/design-notify.md leans on this).
    #[test]
    fn free_text_rules_and_their_limits() {
        let mut t = Tally::default();
        let default = Policy::default();
        let strict = Policy {
            mode: Mode::Strict,
            ..Policy::default()
        };
        // A secret-looking NAME, whatever the separator, in both modes.
        for policy in [&default, &strict] {
            let out = policy.redact_text(
                &format!("panic: config.Config{{DBPassword:\"{CANARY}\", Host:\"db\"}}"),
                &mut t,
            );
            assert!(!out.contains(CANARY), "{out}");
            assert!(out.contains("Host:\"db\""), "{out}");
            let out = policy.redact_text(
                &format!("upstream 401: Authorization: Basic {CANARY}"),
                &mut t,
            );
            assert!(!out.contains(CANARY), "{out}");
        }
        // strict also redacts an unknown name's value; default keeps it readable.
        let line = "reconcile failed: shard=eu-west-1 attempt=3";
        assert!(default
            .redact_text(line, &mut t)
            .contains("shard=eu-west-1"));
        assert!(!strict.redact_text(line, &mut t).contains("eu-west-1"));
        // Readability that must survive in both modes.
        for policy in [&default, &strict] {
            let out = policy.redact_text("12:30:05 GET /healthz http/1.1 200", &mut t);
            assert_eq!(out, "12:30:05 GET /healthz http/1.1 200");
        }
        // Known limit, stated rather than hidden: prose with no name and no separator.
        let out = default.redact_text(&format!("the new password is {CANARY} (rotate it)"), &mut t);
        assert!(out.contains(CANARY), "documented limit changed: {out}");
    }

    #[test]
    fn strict_redacts_everything_but_the_plaintext_list() {
        let mut p = pod();
        let policy = Policy {
            mode: Mode::Strict,
            plaintext: vec!["CACHE_WARMUP".into()],
        };
        policy.redact_object(&mut p);
        let env = p["spec"]["containers"][0]["env"]
            .as_array()
            .unwrap()
            .clone();
        for e in env {
            if e["value"].is_null() {
                continue;
            }
            let expected = if e["name"] == "CACHE_WARMUP" {
                "eager"
            } else {
                REDACTED
            };
            assert_eq!(e["value"], expected, "{}", e["name"]);
        }
    }

    #[test]
    fn off_changes_nothing() {
        let mut p = pod();
        let before = p.clone();
        let t = Policy {
            mode: Mode::Off,
            plaintext: vec![],
        }
        .redact_object(&mut p);
        assert_eq!(p, before);
        assert_eq!(t, Tally::default());
    }

    #[test]
    fn names_classify_by_token_not_substring() {
        for strong in [
            "DB_PASSWORD",
            "dbPassword",
            "spring.datasource.password",
            "CLIENT_SECRET",
            "apiKey",
            "AWS_ACCESS_KEY_ID",
            "Authorization",
        ] {
            assert_eq!(classify_name(strong), NameClass::Strong, "{strong}");
        }
        for weak in [
            "CACHE_KEY_PREFIX",
            "TOKEN_TTL_SECONDS",
            "AUTH_ENABLED",
            "TLS_CERT_PATH",
        ] {
            assert_eq!(classify_name(weak), NameClass::Weak, "{weak}");
        }
        assert_eq!(classify_name("PGPASSWORD"), NameClass::Strong);
        assert_eq!(classify_name("MYSQL_PWD"), NameClass::Strong);
        for none in [
            "BYPASS_CACHE",
            "AUTHOR",
            "MONKEY",
            "CACHE_WARMUP",
            "COMPASS_URL",
        ] {
            assert_eq!(classify_name(none), NameClass::None, "{none}");
        }
    }

    #[test]
    fn value_rule_thresholds() {
        // 64-hex and 40-char base64 are caught; ordinary long strings are not.
        assert!(looks_secret(
            "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
        ));
        assert!(looks_secret("wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"));
        assert!(looks_secret("AKIAIOSFODNN7EXAMPLE"));
        assert!(!looks_secret("postgres-primary.shop.svc.cluster.local"));
        assert!(!looks_secret("checkout-7f8b6b66f8-7txr5"));
        assert!(!looks_secret("3f1e2d4c-5b6a-7980-a1b2-c3d4e5f60718"));
        assert!(!looks_secret("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"));
    }

    #[test]
    fn free_text_event_messages() {
        let mut t = Tally::default();
        let msg = format!("Error: failed to pull with token={CANARY}: unauthorized");
        let out = policy().redact_text(&msg, &mut t);
        assert!(!out.contains(CANARY), "{out}");
        assert!(out.starts_with("Error: failed to pull with token=<redacted>"));
        assert_eq!(
            policy().redact_text("Back-off restarting failed container app", &mut t),
            "Back-off restarting failed container app"
        );
    }

    #[test]
    fn config_files_are_redacted_per_line() {
        let p = policy();
        let mut t = Tally::default();
        let yaml = format!("server:\n  port: 8080\n  mode: eager\ndb:\n  url: jdbc:postgresql://db:5432/app\n  password: {CANARY}\n# password: in a comment stays\n");
        let out = p.redact_config_value("application.yaml", &yaml, &mut t);
        assert!(!out.contains(CANARY), "{out}");
        assert!(
            out.contains("  port: 8080\n") && out.contains("  mode: eager\n"),
            "{out}"
        );
        assert!(out.contains("  password: <redacted>\n"), "{out}");
        assert!(out.contains("db:\n"));

        let props = format!("cache.mode=eager\nspring.datasource.password={CANARY}\n");
        let out = p.redact_config_value("app.properties", &props, &mut t);
        assert_eq!(
            out,
            "cache.mode=eager\nspring.datasource.password=<redacted>\n"
        );

        let json = format!("{{\n  \"api\": {{ \"token\": \"{CANARY}\" }},\n  \"retries\": 3\n}}\n");
        let out = p.redact_config_value("settings.json", &json, &mut t);
        assert!(
            !out.contains(CANARY) && out.contains("\"retries\": 3"),
            "{out}"
        );

        assert_eq!(p.redact_config_value("MODE", "eager", &mut t), "eager");
        assert_eq!(
            p.redact_config_value("DB_PASSWORD", "hunter2", &mut t),
            REDACTED
        );
    }

    #[test]
    fn durations_parse() {
        for d in ["30s", "1h30m", "1.5h", "500ms", "2d"] {
            assert!(is_duration(d), "{d}");
        }
        for d in ["", "s", "30", "hunter2", "3x"] {
            assert!(!is_duration(d), "{d}");
        }
    }
}
