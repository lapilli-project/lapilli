//! Content hash tree over the files of an Incident Evidence Bundle.
//!
//! Design decision (see [`spec/IEB-SPEC.md`](https://github.com/lapilli-project/lapilli/blob/main/spec/IEB-SPEC.md)): we hash file **contents** individually — never
//! the tar byte-stream — and the root is a hash over the entries **sorted by path**. This
//! makes tar ordering, timestamps, and zstd settings irrelevant to the hash tree, so no
//! canonical-tar or JSON-canonicalization machinery is needed.

use std::collections::BTreeMap;
use std::path::Path;

use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use crate::BundleError;

/// The name of the manifest file inside a bundle. It is excluded from the hash tree
/// (the manifest *contains* the tree) and is the payload that gets signed.
pub const MANIFEST_FILE: &str = "manifest.json";
/// Directory holding the optional detached signature; excluded from the hash tree.
pub const SIGNATURE_DIR: &str = "signature";

/// A content hash tree: `path -> sha256(hex)` for every bundle file, plus a `root`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct HashTree {
    /// Map of bundle-relative path -> lowercase hex SHA-256 of the file's contents.
    /// A `BTreeMap` keeps entries sorted by path, which also fixes the root computation.
    pub files: BTreeMap<String, String>,
    /// Lowercase hex SHA-256 over the sorted `"{path}:{hash}\n"` entries.
    pub root: String,
}

impl HashTree {
    /// Compute the root from the current `files` map (entries are already path-sorted).
    pub fn compute_root(files: &BTreeMap<String, String>) -> String {
        let mut hasher = Sha256::new();
        for (path, hash) in files {
            hasher.update(path.as_bytes());
            hasher.update(b":");
            hasher.update(hash.as_bytes());
            hasher.update(b"\n");
        }
        hex(hasher.finalize().as_ref())
    }

    /// Build a hash tree by walking `dir` (the producer side), hashing every regular file
    /// except the manifest and `signature/`. Fails if any path breaks the `ieb/v1` path
    /// rules or the bundle exceeds the producer limits, so a bad bundle is never sealed.
    pub fn from_dir(dir: &Path) -> Result<Self, BundleError> {
        let mut files = BTreeMap::new();
        let mut problems = Vec::new();
        let mut bytes = 0u64;
        collect(dir, dir, &mut files, &mut problems, &mut bytes)?;
        if bytes > PRODUCER_MAX_BYTES {
            return Err(BundleError::Path(format!(
                "bundle is {bytes} bytes; the limit is {PRODUCER_MAX_BYTES}"
            )));
        }
        problems.extend(case_collisions(files.keys()));
        if let Some(p) = problems.first() {
            return Err(BundleError::Path(p.clone()));
        }
        if files.len() > PRODUCER_MAX_FILES {
            return Err(BundleError::Path(format!(
                "bundle has {} files; the limit is {PRODUCER_MAX_FILES}",
                files.len()
            )));
        }
        let root = Self::compute_root(&files);
        Ok(Self { files, root })
    }

    /// Recompute the tree over `dir` and return whether it matches `self` exactly.
    /// Returns the set of mismatching / missing / extra paths for diagnostics.
    pub fn verify_against_dir(&self, dir: &Path) -> Result<Vec<String>, BundleError> {
        let recomputed = Self::from_dir(dir)?;
        let mut problems = Vec::new();
        for (path, hash) in &self.files {
            match recomputed.files.get(path) {
                Some(h) if h == hash => {}
                Some(_) => problems.push(format!("modified: {path}")),
                None => problems.push(format!("missing: {path}")),
            }
        }
        for path in recomputed.files.keys() {
            if !self.files.contains_key(path) {
                problems.push(format!("unexpected: {path}"));
            }
        }
        if recomputed.root != self.root {
            problems.push(format!(
                "root mismatch: {} != {}",
                recomputed.root, self.root
            ));
        }
        Ok(problems)
    }
}

/// Producer limits (IEB-SPEC): a verifier's defaults are never below these.
pub const PRODUCER_MAX_FILES: usize = 50_000;
pub const PRODUCER_MAX_BYTES: u64 = 1 << 30;

/// The `ieb/v1` path rule: relative, `/`-separated segments of `[A-Za-z0-9._-]`, none empty,
/// `.` or `..`. (Case-insensitive uniqueness is checked across the whole set.)
pub fn check_path(path: &str) -> Result<(), String> {
    if path.is_empty() {
        return Err("empty path".into());
    }
    if !ustar_representable(path) {
        return Err(format!(
            "invalid path {path:?}: too long for a plain ustar header (≤ 256 bytes: name ≤ 100, \
             or prefix ≤ 155 + '/' + name ≤ 100 after a split at a '/')"
        ));
    }
    for seg in path.split('/') {
        if seg.is_empty() || seg == "." || seg == ".." {
            return Err(format!("invalid path {path:?}: empty, '.' or '..' segment"));
        }
        if !seg
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'_' | b'-'))
        {
            return Err(format!(
                "invalid path {path:?}: only [A-Za-z0-9._-] allowed in a segment"
            ));
        }
    }
    Ok(())
}

/// `ieb/v1` archives are plain ustar (no pax or GNU long-name records), so every path must
/// fit the ustar `name` (100) and `prefix` (155) fields, split at a `/`.
pub fn ustar_representable(path: &str) -> bool {
    if path.len() <= 100 {
        return true;
    }
    path.len() <= 256
        && path
            .match_indices('/')
            .any(|(i, _)| i <= 155 && path.len() - i - 1 <= 100)
}

/// Paths that differ only by case would collapse into one file on case-insensitive
/// filesystems (macOS, Windows), so a v1 bundle may not contain them.
pub fn case_collisions<'a>(paths: impl Iterator<Item = &'a String>) -> Vec<String> {
    let mut seen: BTreeMap<String, &String> = BTreeMap::new();
    let mut out = Vec::new();
    for p in paths {
        if let Some(prev) = seen.insert(p.to_ascii_lowercase(), p) {
            out.push(format!("paths differ only by case: {prev} / {p}"));
        }
    }
    out
}

fn collect(
    root: &Path,
    dir: &Path,
    out: &mut BTreeMap<String, String>,
    problems: &mut Vec<String>,
    total: &mut u64,
) -> Result<(), BundleError> {
    for entry in std::fs::read_dir(dir)? {
        let entry = entry?;
        let path = entry.path();
        let Some(rel) = path
            .strip_prefix(root)
            .map_err(|e| BundleError::Path(e.to_string()))?
            .to_str()
            .map(|r| r.replace('\\', "/"))
        else {
            problems.push(format!("non-UTF-8 file name: {}", path.display()));
            continue;
        };
        // Exclude the manifest and the signature dir from the content set.
        if rel == MANIFEST_FILE
            || rel == SIGNATURE_DIR
            || rel.starts_with(&format!("{SIGNATURE_DIR}/"))
        {
            continue;
        }
        let ft = entry.file_type()?;
        if ft.is_dir() {
            collect(root, &path, out, problems, total)?;
        } else if ft.is_file() {
            if let Err(e) = check_path(&rel) {
                problems.push(e);
                continue;
            }
            let (hash, len) = sha256_file(&path, u64::MAX)?;
            *total += len;
            out.insert(rel, hash);
        } else {
            problems.push(format!("not a regular file: {rel}"));
        }
    }
    Ok(())
}

pub(crate) fn sha256_hex(bytes: &[u8]) -> String {
    hex(Sha256::digest(bytes).as_ref())
}

/// Lowercase hex of a finished hasher.
pub(crate) fn hex_digest(hasher: Sha256) -> String {
    hex(hasher.finalize().as_ref())
}

/// SHA-256 of at most the first `limit` bytes of a file, streamed through a fixed buffer, and
/// how many bytes were hashed. Neither the producer nor the verifier holds a file in memory
/// to hash it: a 256 MiB log costs 64 KiB.
pub(crate) fn sha256_file(path: &Path, limit: u64) -> Result<(String, u64), BundleError> {
    use std::io::{BufRead, Read};
    let file = std::fs::File::open(path)?;
    let mut reader = std::io::BufReader::with_capacity(64 << 10, file.take(limit));
    let mut hasher = Sha256::new();
    let mut len = 0u64;
    loop {
        let buf = reader.fill_buf()?;
        if buf.is_empty() {
            break;
        }
        hasher.update(buf);
        len += buf.len() as u64;
        let n = buf.len();
        reader.consume(n);
    }
    Ok((hex_digest(hasher), len))
}

fn hex(bytes: &[u8]) -> String {
    let mut s = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        s.push_str(&format!("{b:02x}"));
    }
    s
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;

    #[test]
    fn root_is_stable_and_order_independent() {
        let mut a = BTreeMap::new();
        a.insert("b.txt".to_string(), "22".to_string());
        a.insert("a.txt".to_string(), "11".to_string());
        let r1 = HashTree::compute_root(&a);
        // BTreeMap already sorts, so inserting in any order yields the same root.
        let mut b = BTreeMap::new();
        b.insert("a.txt".to_string(), "11".to_string());
        b.insert("b.txt".to_string(), "22".to_string());
        assert_eq!(r1, HashTree::compute_root(&b));
    }

    #[test]
    fn detects_tampering() {
        let dir = tempfile::tempdir().unwrap();
        fs::write(dir.path().join("a.txt"), b"hello").unwrap();
        fs::create_dir(dir.path().join("logs")).unwrap();
        fs::write(dir.path().join("logs/x.log"), b"boom").unwrap();

        let tree = HashTree::from_dir(dir.path()).unwrap();
        assert!(tree.verify_against_dir(dir.path()).unwrap().is_empty());

        // Tamper one byte -> verification must surface it.
        fs::write(dir.path().join("a.txt"), b"hellO").unwrap();
        let problems = tree.verify_against_dir(dir.path()).unwrap();
        assert!(
            problems.iter().any(|p| p.contains("modified: a.txt")),
            "{problems:?}"
        );
    }
}
