//! Content hash tree over the files of an Incident Evidence Bundle.
//!
//! Design decision (see `spec/IEB-SPEC.md`): we hash file **contents** individually — never
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

    /// Build a hash tree by walking `dir`, hashing every regular file except the manifest
    /// and the signature directory (which are not part of the signed content set).
    pub fn from_dir(dir: &Path) -> Result<Self, BundleError> {
        let mut files = BTreeMap::new();
        collect(dir, dir, &mut files)?;
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

fn collect(root: &Path, dir: &Path, out: &mut BTreeMap<String, String>) -> Result<(), BundleError> {
    for entry in std::fs::read_dir(dir)? {
        let entry = entry?;
        let path = entry.path();
        let rel = path
            .strip_prefix(root)
            .map_err(|e| BundleError::Path(e.to_string()))?
            .to_string_lossy()
            .replace('\\', "/");
        // Exclude the manifest and the signature dir from the content set.
        if rel == MANIFEST_FILE
            || rel == SIGNATURE_DIR
            || rel.starts_with(&format!("{SIGNATURE_DIR}/"))
        {
            continue;
        }
        let ft = entry.file_type()?;
        if ft.is_dir() {
            collect(root, &path, out)?;
        } else if ft.is_file() {
            let bytes = std::fs::read(&path)?;
            out.insert(rel, hex(Sha256::digest(&bytes).as_ref()));
        }
        // symlinks and other types are intentionally skipped (bundles forbid symlinks).
    }
    Ok(())
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
