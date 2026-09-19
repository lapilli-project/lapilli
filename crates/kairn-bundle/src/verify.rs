//! Verification — the load-bearing integrity feature (works with or without a signature).
//!
//! The `ieb/v1` verification contract is frozen (see `docs/COMPATIBILITY.md` and
//! `spec/IEB-SPEC.md`). `kairn verify` maps the [`Verdict`] to an exit code: `Ok` 0,
//! `Failed` 1, `Partial` 2, `CannotEvaluate` 3. Only 0 means the bundle passed.
//!
//! A `.ieb` file is **not extracted**: the tar stream is hashed entry by entry, which avoids
//! temp directories, case-insensitive and normalizing filesystems, and link tricks.

use std::collections::BTreeMap;
use std::io::Read;
use std::path::Path;

use crate::hashtree::{
    case_collisions, check_path, sha256_hex, HashTree, MANIFEST_FILE, SIGNATURE_DIR,
};
use crate::manifest::{
    required_files, Manifest, ALG_ECDSA_P256_SHA256, SCHEMA_PREFIX, SCHEMA_VERSION,
};
use crate::seal::{PUBKEY_FILE, SIG_FILE};
use crate::sign::{key_id, verify_b64};
use crate::BundleError;

/// Verifier resource limits. Never below the producer limits (50,000 files / 1 GiB).
pub const VERIFY_MAX_ENTRIES: usize = 100_000;
pub const VERIFY_MAX_BYTES: u64 = 2 << 30;
/// `manifest.json`, `redaction.json` and `signature/*` are read into memory; cap them.
const MAX_SMALL_FILE: u64 = 16 << 20;

/// What the caller asserts the bundle should be, plus the key it trusts.
#[derive(Debug, Default, Clone)]
pub struct VerifyOptions {
    /// If set, must equal `manifest.incident.cluster_id` (fail-closed on mismatch).
    pub expected_cluster: Option<String>,
    /// If set, must equal `manifest.incident.id` (fail-closed on mismatch).
    pub expected_incident: Option<String>,
    /// SPKI PEM public key obtained **out of band**. When set, the bundle must declare and
    /// carry a signature by this key. Keys inside the bundle are never trusted.
    pub trusted_key_pem: Option<String>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Verdict {
    Ok,
    Partial,
    Failed,
    /// Unknown format major, pre-release format, or over the resource limits.
    CannotEvaluate,
}

impl Verdict {
    pub fn exit_code(self) -> i32 {
        match self {
            Verdict::Ok => 0,
            Verdict::Failed => 1,
            Verdict::Partial => 2,
            Verdict::CannotEvaluate => 3,
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SignatureStatus {
    /// Declared unsigned, and no signature present.
    Absent,
    /// Verifies against the caller's `--key`: integrity + producer authenticity.
    Trusted,
    /// Signed and self-consistent, but no `--key`: says nothing about who sealed it.
    Unpinned,
    /// Declared/present signature that does not check out.
    Invalid,
}

#[derive(Debug, Clone)]
pub struct VerifyReport {
    pub verdict: Verdict,
    /// `schema_version` as found (read before anything is verified).
    pub format: Option<String>,
    /// `producer.kairn_version`, for bug reports.
    pub producer: Option<String>,
    pub hash_ok: bool,
    pub context_ok: bool,
    pub coverage_score: f64,
    pub partial: bool,
    pub signature: SignatureStatus,
    /// Human-readable problems (tampered/missing/extra files, context mismatch, ...).
    pub problems: Vec<String>,
    /// `mode` from `redaction.json`; an unknown mode is reported as `off` (fail safe).
    pub redaction_mode: Option<String>,
}

impl VerifyReport {
    fn cannot_evaluate(format: Option<String>, reason: String) -> Self {
        Self::bare(Verdict::CannotEvaluate, format, reason)
    }
    fn failed(format: Option<String>, reason: String) -> Self {
        Self::bare(Verdict::Failed, format, reason)
    }
    fn bare(verdict: Verdict, format: Option<String>, reason: String) -> Self {
        Self {
            verdict,
            format,
            producer: None,
            hash_ok: false,
            context_ok: false,
            coverage_score: 0.0,
            partial: false,
            signature: SignatureStatus::Absent,
            problems: vec![reason],
            redaction_mode: None,
        }
    }
}

/// Verify a `.ieb` file (streamed, never extracted) or an unpacked directory. `Err` only
/// when the input can't be read at all (not found, permission denied): exit 3.
pub fn verify_bundle(path: &Path, opts: &VerifyOptions) -> Result<VerifyReport, BundleError> {
    let contents = if path.is_dir() {
        read_dir(path)?
    } else {
        read_ieb(path)?
    };
    Ok(match contents {
        Ok(c) => evaluate(c, opts),
        Err(limit) => VerifyReport::cannot_evaluate(None, limit),
    })
}

/// Verify an unpacked bundle directory.
pub fn verify_bundle_dir(dir: &Path, opts: &VerifyOptions) -> Result<VerifyReport, BundleError> {
    Ok(match read_dir(dir)? {
        Ok(c) => evaluate(c, opts),
        Err(limit) => VerifyReport::cannot_evaluate(None, limit),
    })
}

/// Everything verification needs, independent of the container.
#[derive(Default)]
struct Contents {
    manifest: Option<Vec<u8>>,
    /// path → sha256 of every file outside `manifest.json` and `signature/`.
    files: BTreeMap<String, String>,
    /// file name under `signature/` → bytes.
    signature: BTreeMap<String, Vec<u8>>,
    redaction: Option<Vec<u8>>,
    /// Files under `signature/ext/` (not checked by ieb/v1).
    extensions: Vec<String>,
    /// Structural problems found while reading (corrupt archive, links, bad paths, …).
    problems: Vec<String>,
    all_paths: std::collections::BTreeSet<String>,
    folded: BTreeMap<String, String>,
    /// Case-folded proper ancestors of every path.
    dirs: std::collections::BTreeSet<String>,
    count: usize,
    bytes: u64,
}

impl Contents {
    /// Count one file against the verifier limits (same limits in both modes).
    fn over_limits(&mut self, size: u64) -> Option<String> {
        self.count += 1;
        self.bytes = self.bytes.saturating_add(size);
        (self.count > VERIFY_MAX_ENTRIES || self.bytes > VERIFY_MAX_BYTES).then(|| {
            format!(
                "bundle exceeds the verifier limits ({VERIFY_MAX_ENTRIES} entries / {} MiB)",
                VERIFY_MAX_BYTES >> 20
            )
        })
    }
}

impl Contents {
    fn add(&mut self, path: String, bytes: Vec<u8>) {
        let hash = sha256_hex(&bytes);
        self.add_hashed(path, hash, Some(bytes));
    }

    /// Register a directory entry: it must not name (or contain) a file.
    fn add_dir(&mut self, dir: &str) {
        let folded = dir.to_ascii_lowercase();
        if self.folded.contains_key(&folded) {
            self.problems
                .push(format!("{dir} is both a directory entry and a file"));
            return;
        }
        self.dirs.insert(folded);
    }

    /// Register one file. `bytes` is kept only for the few files read into memory.
    fn add_hashed(&mut self, path: String, hash: String, bytes: Option<Vec<u8>>) {
        // Duplicates and case collisions are checked over **every** path, including
        // manifest.json and signature/*, so a second manifest can't hide behind the first.
        if !self.all_paths.insert(path.clone()) {
            self.problems.push(format!("duplicate entry: {path}"));
            return;
        }
        // No path may also be a directory of another (`logs` and `logs/index.json`): such a
        // bundle can't be unpacked, so its two forms would get different verdicts.
        let folded = path.to_ascii_lowercase();
        if self.dirs.contains(&folded) {
            self.problems.push(format!(
                "{path} is both a file and a directory of other paths"
            ));
            return;
        }
        let mut ancestor = folded.as_str();
        while let Some(i) = ancestor.rfind('/') {
            ancestor = &ancestor[..i];
            if self.folded.contains_key(ancestor) {
                self.problems
                    .push(format!("{path} is inside {ancestor}, which is a file"));
                return;
            }
            self.dirs.insert(ancestor.to_string());
        }
        if let Some(prev) = self.folded.insert(path.to_ascii_lowercase(), path.clone()) {
            self.problems
                .push(format!("paths differ only by case: {prev} / {path}"));
            return;
        }
        let first = path.split('/').next().unwrap_or_default();
        let reserved_variant = (path.eq_ignore_ascii_case(MANIFEST_FILE) && path != MANIFEST_FILE)
            || (first.eq_ignore_ascii_case(SIGNATURE_DIR) && first != SIGNATURE_DIR)
            || path == SIGNATURE_DIR;
        if reserved_variant {
            self.problems.push(format!(
                "reserved name used with different case or type: {path}"
            ));
            return;
        }
        if path == MANIFEST_FILE {
            self.manifest = bytes;
            return;
        }
        if let Some(name) = path.strip_prefix(&format!("{SIGNATURE_DIR}/")) {
            if let Some(ext) = name.strip_prefix("ext/") {
                // Reserved for later signature-adjacent artifacts (sigstore bundle, RFC 3161
                // token): ignored by ieb/v1 verifiers, but they must obey the path rules.
                if let Err(e) = check_path(ext) {
                    self.problems.push(e);
                } else {
                    self.extensions.push(path.clone());
                }
                return;
            }
            self.signature
                .insert(name.to_string(), bytes.unwrap_or_default());
            return;
        }
        if let Err(e) = check_path(&path) {
            self.problems.push(e);
        }
        if path == "redaction.json" {
            self.redaction = bytes;
        }
        self.files.insert(path, hash);
    }
}

/// Stream a `.ieb`: `Ok(Err(reason))` when over the limits (exit 3); a corrupt archive is a
/// structural problem (exit 1), not an error.
fn read_ieb(path: &Path) -> Result<Result<Contents, String>, BundleError> {
    let file = std::fs::File::open(path)?;
    let mut c = Contents::default();
    let decoder = match zstd::stream::read::Decoder::new(file) {
        Ok(d) => d,
        Err(e) => {
            c.problems.push(format!("archive is corrupt: {e}"));
            return Ok(Ok(c));
        }
    };
    let mut archive = tar::Archive::new(decoder);
    // Raw: the tar crate must not interpret pax or GNU long-name records for us (they can
    // override sizes and names, or make it buffer a huge name). ieb/v1 forbids them.
    let entries = match archive.entries() {
        Ok(e) => e.raw(true),
        Err(e) => {
            c.problems.push(format!("archive is corrupt: {e}"));
            return Ok(Ok(c));
        }
    };
    for entry in entries {
        let mut entry = match entry {
            Ok(e) => e,
            Err(e) => {
                c.problems.push(format!("archive is corrupt: {e}"));
                break;
            }
        };
        let kind = entry.header().entry_type();
        if kind.is_pax_global_extensions()
            || kind.is_pax_local_extensions()
            || kind.is_gnu_longname()
            || kind.is_gnu_longlink()
        {
            // Skipped without reading the payload.
            c.problems
                .push("pax or GNU extension records are not allowed in ieb/v1".into());
            continue;
        }
        let raw = entry.path_bytes().into_owned();
        let Ok(name) = String::from_utf8(raw) else {
            c.problems.push("non-UTF-8 entry name".into());
            continue;
        };
        let name = name.strip_prefix("./").unwrap_or(&name).to_string();
        // Type before size: a special entry is FAILED, whatever size it claims.
        if kind.is_dir() {
            // Directories carry no content, but must not collide with a file path (the
            // unpacked form could then not be created, and would get another verdict).
            let dir = name.trim_end_matches('/');
            if !dir.is_empty() {
                c.add_dir(dir);
            }
            continue;
        }
        if !(kind.is_file() || kind.is_contiguous()) {
            c.problems
                .push(format!("link or special entry not allowed: {name}"));
            continue;
        }
        let size = entry.header().size().unwrap_or(u64::MAX);
        if let Some(limit) = c.over_limits(size) {
            return Ok(Err(limit));
        }
        if name.is_empty()
            || name.ends_with('/')
            || name.starts_with('/')
            || name.split('/').any(|s| s == ".." || s == ".")
        {
            c.problems.push(format!("unsafe path in bundle: {name:?}"));
            continue;
        }
        let small = name == MANIFEST_FILE
            || name == "redaction.json"
            || name.starts_with(&format!("{SIGNATURE_DIR}/"));
        if small {
            if size > MAX_SMALL_FILE {
                return Ok(Err(small_limit(&name)));
            }
            let mut bytes = Vec::new();
            if let Err(e) = entry.read_to_end(&mut bytes) {
                c.problems.push(format!("archive is corrupt: {e}"));
                break;
            }
            c.add(name, bytes);
        } else {
            // Hash while streaming: large log files are never held in memory.
            let mut hasher = <sha2::Sha256 as sha2::Digest>::new();
            if let Err(e) = std::io::copy(&mut entry, &mut HashWriter(&mut hasher)) {
                c.problems.push(format!("archive is corrupt: {e}"));
                break;
            }
            let digest = sha2::Digest::finalize(hasher);
            let hash: String = digest.iter().map(|b| format!("{b:02x}")).collect();
            c.add_hashed(name, hash, None);
        }
    }
    Ok(Ok(c))
}

fn small_limit(name: &str) -> String {
    format!(
        "{name} exceeds the verifier limit for files read into memory ({} MiB)",
        MAX_SMALL_FILE >> 20
    )
}

struct HashWriter<'a>(&'a mut sha2::Sha256);
impl std::io::Write for HashWriter<'_> {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        sha2::Digest::update(self.0, buf);
        Ok(buf.len())
    }
    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

/// Walk an unpacked directory (for bundles a human unpacked, and for tests). Same limits
/// as the stream mode, so both modes give the same verdict.
fn read_dir(dir: &Path) -> Result<Result<Contents, String>, BundleError> {
    let mut c = Contents::default();
    if let Err(limit) = walk(dir, dir, &mut c)? {
        return Ok(Err(limit));
    }
    Ok(Ok(c))
}

fn walk(root: &Path, dir: &Path, c: &mut Contents) -> Result<Result<(), String>, BundleError> {
    for entry in std::fs::read_dir(dir)? {
        let entry = entry?;
        let path = entry.path();
        let rel = path
            .strip_prefix(root)
            .map_err(|e| BundleError::Path(e.to_string()))?;
        let Some(rel) = rel.to_str().map(|r| r.replace('\\', "/")) else {
            c.problems
                .push(format!("non-UTF-8 file name: {}", path.display()));
            continue;
        };
        let ft = entry.file_type()?;
        if ft.is_dir() {
            if let Err(limit) = walk(root, &path, c)? {
                return Ok(Err(limit));
            }
        } else if ft.is_file() {
            let size = entry.metadata()?.len();
            if let Some(limit) = c.over_limits(size) {
                return Ok(Err(limit));
            }
            let small = rel == MANIFEST_FILE
                || rel == "redaction.json"
                || rel.starts_with(&format!("{SIGNATURE_DIR}/"));
            if small && size > MAX_SMALL_FILE {
                return Ok(Err(small_limit(&rel)));
            }
            c.add(rel, std::fs::read(&path)?);
        } else {
            c.problems
                .push(format!("link or special file not allowed: {rel}"));
        }
    }
    Ok(Ok(()))
}

/// The `ieb/v1` rules. Dispatch on `schema_version` happens first; that field is
/// necessarily read before anything is verified.
fn evaluate(c: Contents, opts: &VerifyOptions) -> VerifyReport {
    let Some(manifest_bytes) = c.manifest.clone() else {
        let why = c
            .problems
            .first()
            .cloned()
            .unwrap_or_else(|| "manifest.json is missing: not a Kairn bundle".into());
        return VerifyReport::failed(None, why);
    };
    // Duplicate member names are FAILED: otherwise one reader could see the first value and
    // another enforce the last (signed bytes showing one hash, enforcing another).
    if let Err(e) = serde_json::from_slice::<NoDuplicateKeys>(&manifest_bytes) {
        return VerifyReport::failed(None, format!("manifest.json is not valid: {e}"));
    }
    let head: serde_json::Value = match serde_json::from_slice(&manifest_bytes) {
        Ok(v) => v,
        Err(e) => return VerifyReport::failed(None, format!("manifest.json is not JSON: {e}")),
    };
    let format = head["schema_version"].as_str().map(str::to_string);
    let major = format
        .as_deref()
        .and_then(|v| v.strip_prefix(SCHEMA_PREFIX))
        .and_then(|m| m.strip_prefix('v'))
        .filter(|n| !n.is_empty() && n.bytes().all(|b| b.is_ascii_digit()))
        .filter(|n| n == &"0" || !n.starts_with('0'));
    match (format.as_deref(), major) {
        (Some(SCHEMA_VERSION), _) => {}
        (_, Some("0")) => {
            return VerifyReport::cannot_evaluate(
                format.clone(),
                "format v0 is a pre-release development format; no release supports it".into(),
            )
        }
        (Some(v), Some(_)) => {
            return VerifyReport::cannot_evaluate(
                format.clone(),
                format!("format {v} is not known to this kairn (it reads ieb/v1); upgrade kairn"),
            )
        }
        _ => {
            return VerifyReport::failed(
                format.clone(),
                "manifest.json has no valid Kairn schema_version: not a Kairn bundle".into(),
            )
        }
    }
    let manifest: Manifest = match serde_json::from_slice(&manifest_bytes) {
        Ok(m) => m,
        Err(e) => return VerifyReport::failed(format, format!("malformed v1 manifest: {e}")),
    };
    v1(c, manifest, &manifest_bytes, format, opts)
}

/// Deserializes any JSON, failing on a duplicate member name at any depth.
struct NoDuplicateKeys;

impl<'de> serde::Deserialize<'de> for NoDuplicateKeys {
    fn deserialize<D: serde::Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        struct V;
        impl<'de> serde::de::Visitor<'de> for V {
            type Value = NoDuplicateKeys;
            fn expecting(&self, f: &mut std::fmt::Formatter) -> std::fmt::Result {
                f.write_str("any JSON value")
            }
            fn visit_bool<E>(self, _: bool) -> Result<Self::Value, E> {
                Ok(NoDuplicateKeys)
            }
            fn visit_i64<E>(self, _: i64) -> Result<Self::Value, E> {
                Ok(NoDuplicateKeys)
            }
            fn visit_u64<E>(self, _: u64) -> Result<Self::Value, E> {
                Ok(NoDuplicateKeys)
            }
            fn visit_f64<E>(self, _: f64) -> Result<Self::Value, E> {
                Ok(NoDuplicateKeys)
            }
            fn visit_str<E>(self, _: &str) -> Result<Self::Value, E> {
                Ok(NoDuplicateKeys)
            }
            fn visit_unit<E>(self) -> Result<Self::Value, E> {
                Ok(NoDuplicateKeys)
            }
            fn visit_seq<A: serde::de::SeqAccess<'de>>(
                self,
                mut seq: A,
            ) -> Result<Self::Value, A::Error> {
                while seq.next_element::<NoDuplicateKeys>()?.is_some() {}
                Ok(NoDuplicateKeys)
            }
            fn visit_map<A: serde::de::MapAccess<'de>>(
                self,
                mut map: A,
            ) -> Result<Self::Value, A::Error> {
                let mut seen = std::collections::HashSet::new();
                while let Some(key) = map.next_key::<String>()? {
                    if !seen.insert(key.clone()) {
                        return Err(serde::de::Error::custom(format!(
                            "duplicate member name {key:?}"
                        )));
                    }
                    map.next_value::<NoDuplicateKeys>()?;
                }
                Ok(NoDuplicateKeys)
            }
        }
        d.deserialize_any(V)
    }
}

fn v1(
    c: Contents,
    manifest: Manifest,
    manifest_bytes: &[u8],
    format: Option<String>,
    opts: &VerifyOptions,
) -> VerifyReport {
    let mut problems = c.problems.clone();
    let mut structural_ok = problems.is_empty();

    // 1) Hash tree: every file listed and matching, nothing unlisted, root consistent, and
    //    the listed paths themselves obey the path rules.
    let tree = &manifest.hash_tree;
    let mut tree_problems = Vec::new();
    for (path, hash) in &tree.files {
        if let Err(e) = check_path(path) {
            tree_problems.push(format!("manifest lists {e}"));
        }
        match c.files.get(path) {
            Some(h) if h == hash => {}
            Some(_) => tree_problems.push(format!("modified: {path}")),
            None => tree_problems.push(format!("missing: {path}")),
        }
    }
    for path in c.files.keys() {
        if !tree.files.contains_key(path) {
            tree_problems.push(format!("unexpected: {path}"));
        }
    }
    tree_problems.extend(case_collisions(tree.files.keys()));
    let root = HashTree::compute_root(&tree.files);
    if root != tree.root {
        tree_problems.push(format!("root mismatch: {root} != {}", tree.root));
    }
    for name in c.signature.keys() {
        if name != SIG_FILE && name != PUBKEY_FILE {
            tree_problems.push(format!("unexpected: {SIGNATURE_DIR}/{name}"));
        }
    }
    // Structural problems found while reading (links, traversal, duplicates, bad paths,
    // corruption) mean the file set itself can't be trusted.
    let hash_ok = tree_problems.is_empty() && c.problems.is_empty();
    for ext in &c.extensions {
        problems.push(format!(
            "note: {ext} is a signature extension this kairn does not check"
        ));
    }
    problems.extend(tree_problems);

    // 2) Coverage: well-formed sets, and each collector that ran wrote its files.
    let cov = &manifest.coverage;
    let dupes = |v: &[String]| {
        let mut s = std::collections::BTreeSet::new();
        v.iter().any(|x| !s.insert(x))
    };
    if dupes(&cov.collectors_run) || dupes(&cov.collectors_intended) {
        structural_ok = false;
        problems.push("malformed coverage: duplicate collector names".into());
    }
    if let Some(extra) = cov
        .collectors_run
        .iter()
        .find(|r| !cov.collectors_intended.contains(r))
    {
        structural_ok = false;
        problems.push(format!(
            "malformed coverage: {extra} ran but was not intended"
        ));
    }
    for collector in &cov.collectors_run {
        for f in required_files(collector) {
            if !tree.files.contains_key(*f) {
                structural_ok = false;
                problems.push(format!(
                    "collector {collector} is listed as run but {f} is not in the bundle"
                ));
            }
        }
    }
    let partial = cov.is_partial();
    if partial {
        let missing: Vec<&String> = cov
            .collectors_intended
            .iter()
            .filter(|c| !cov.collectors_run.contains(c))
            .collect();
        problems.push(format!("PARTIAL capture: did not run: {missing:?}"));
    }

    // 3) Redaction record: required; an unknown mode is treated as `off`.
    let redaction_mode = match &c.redaction {
        None => {
            structural_ok = false;
            problems.push("redaction.json is missing (required in ieb/v1)".into());
            None
        }
        Some(b) => {
            let mode = serde_json::from_slice::<serde_json::Value>(b)
                .ok()
                .and_then(|v| v["mode"].as_str().map(str::to_string));
            Some(match mode.as_deref() {
                Some("default") | Some("strict") | Some("off") => mode.unwrap(),
                _ => "off".to_string(),
            })
        }
    };

    // 4) Bound context (fail closed).
    let mut context_ok = true;
    if let Some(exp) = &opts.expected_cluster {
        if exp != &manifest.incident.cluster_id {
            context_ok = false;
            problems.push(format!(
                "cluster mismatch: expected {exp}, bundle {}",
                manifest.incident.cluster_id
            ));
        }
    }
    if let Some(exp) = &opts.expected_incident {
        if exp != &manifest.incident.id {
            context_ok = false;
            problems.push(format!(
                "incident mismatch: expected {exp}, bundle {}",
                manifest.incident.id
            ));
        }
    }

    // 5) Signature. Authenticity only from `--key`; the declaration catches accidental loss.
    let sig = c
        .signature
        .get(SIG_FILE)
        .map(|b| String::from_utf8_lossy(b).to_string());
    let embedded_pub = c
        .signature
        .get(PUBKEY_FILE)
        .map(|b| String::from_utf8_lossy(b).to_string());
    let decl = manifest.signing.as_ref();
    let known_alg = decl.is_some_and(|d| d.alg == ALG_ECDSA_P256_SHA256);
    let signature = match (decl, &sig, &opts.trusted_key_pem) {
        (Some(_), None, _) => {
            problems.push("the manifest declares a signature, but it is missing".into());
            SignatureStatus::Invalid
        }
        (None, Some(_), _) => {
            problems.push("a signature is present but the manifest does not declare one".into());
            SignatureStatus::Invalid
        }
        (None, None, Some(_)) => {
            problems.push("a trusted key was given but the bundle is unsigned".into());
            SignatureStatus::Absent
        }
        (None, None, None) => SignatureStatus::Absent,
        (Some(d), Some(_), Some(_)) if !known_alg => {
            problems.push(format!(
                "signing algorithm {} is not known to this kairn",
                d.alg
            ));
            SignatureStatus::Invalid
        }
        (Some(d), Some(sig), Some(key)) => match key_id(key) {
            Err(e) => {
                problems.push(format!("--key is not a usable public key: {e}"));
                SignatureStatus::Invalid
            }
            Ok(kid) if kid != d.key_id => {
                problems.push(format!(
                    "signed by a different key (bundle key_id {}, --key {kid})",
                    d.key_id
                ));
                SignatureStatus::Invalid
            }
            Ok(_) => match verify_b64(key, manifest_bytes, sig) {
                Ok(()) => SignatureStatus::Trusted,
                Err(e) => {
                    problems.push(format!("not signed by the trusted key ({e})"));
                    SignatureStatus::Invalid
                }
            },
        },
        (Some(_), Some(_), None) if !known_alg => SignatureStatus::Unpinned,
        (Some(d), Some(sig), None) => match &embedded_pub {
            // Self-consistency only: catches corruption, proves nothing about the signer.
            Some(pem) => match key_id(pem) {
                Ok(kid) if kid == d.key_id => match verify_b64(pem, manifest_bytes, sig) {
                    Ok(()) => SignatureStatus::Unpinned,
                    Err(e) => {
                        problems.push(format!("signature invalid: {e}"));
                        SignatureStatus::Invalid
                    }
                },
                _ => {
                    problems.push("embedded public key does not match the declared key_id".into());
                    SignatureStatus::Invalid
                }
            },
            None => SignatureStatus::Unpinned,
        },
    };
    let sig_failed = signature == SignatureStatus::Invalid
        || (opts.trusted_key_pem.is_some() && signature != SignatureStatus::Trusted);

    let verdict = if !hash_ok || !structural_ok || !context_ok || sig_failed {
        Verdict::Failed
    } else if partial {
        Verdict::Partial
    } else {
        Verdict::Ok
    };
    VerifyReport {
        verdict,
        format,
        producer: Some(manifest.producer.kairn_version.clone()),
        hash_ok,
        context_ok,
        coverage_score: cov.score(),
        partial,
        signature,
        problems,
        redaction_mode,
    }
}
