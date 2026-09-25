//! Pack a sealed bundle directory into a single portable `.ieb` file (tar + zstd) and
//! unpack it again. This is the "one portable file you own" — the headline artifact.
//!
//! Packing happens *after* sealing, so the tar byte-stream is never what the hash tree
//! covers (contents are hashed individually — see `hashtree`). Order/timestamps in the tar
//! are therefore irrelevant to verification.

use std::path::Path;

use crate::BundleError;

/// zstd compression level for `.ieb` files. Evidence bundles favor a decent ratio over
/// speed, but not the slowest tiers; 10 is a good middle ground.
const ZSTD_LEVEL: i32 = 10;

/// Unpack limits for untrusted bundles. Real bundles are kilobytes to a few megabytes.
pub const MAX_ENTRIES: usize = 100_000;
pub const MAX_UNPACKED_BYTES: u64 = 2 << 30;

/// Pack the sealed directory `dir` into a `.ieb` file at `out`: plain ustar entries (no pax
/// or GNU long-name records, which `ieb/v1` forbids), files only, in path order, mtime 0.
pub fn pack(dir: &Path, out: &Path) -> Result<(), BundleError> {
    let mut files = Vec::new();
    list_files(dir, dir, &mut files)?;
    files.sort();
    let file = std::fs::File::create(out)?;
    let encoder = zstd::stream::write::Encoder::new(file, ZSTD_LEVEL)
        .map_err(BundleError::Io)?
        .auto_finish();
    let mut tar = tar::Builder::new(encoder);
    for rel in files {
        let bytes = std::fs::read(dir.join(&rel))?;
        let mut header = tar::Header::new_ustar();
        header
            .set_path(&rel)
            .map_err(|e| BundleError::Path(format!("{rel}: {e}")))?;
        header.set_entry_type(tar::EntryType::Regular);
        header.set_size(bytes.len() as u64);
        header.set_mode(0o644);
        header.set_mtime(0);
        header.set_cksum();
        tar.append(&header, bytes.as_slice())?;
    }
    tar.finish()?;
    Ok(())
}

fn list_files(root: &Path, dir: &Path, out: &mut Vec<String>) -> Result<(), BundleError> {
    for entry in std::fs::read_dir(dir)? {
        let entry = entry?;
        let path = entry.path();
        if entry.file_type()?.is_dir() {
            list_files(root, &path, out)?;
        } else {
            let rel = path
                .strip_prefix(root)
                .map_err(|e| BundleError::Path(e.to_string()))?
                .to_str()
                .ok_or_else(|| BundleError::Path(format!("non-UTF-8 name: {}", path.display())))?
                .replace('\\', "/");
            out.push(rel);
        }
    }
    Ok(())
}

/// Unpack a `.ieb` file into `dest` (which must exist). Rejects entries that would escape
/// `dest` (path traversal / absolute paths) — bundles are untrusted input to `verify`.
pub fn unpack(ieb: &Path, dest: &Path) -> Result<(), BundleError> {
    let file = std::fs::File::open(ieb)?;
    let decoder = zstd::stream::read::Decoder::new(file).map_err(BundleError::Io)?;
    let mut archive = tar::Archive::new(decoder);
    // Same stance as `verify`: no pax or GNU long-name records (see IEB-SPEC rule 1).

    let (mut entries, mut total) = (0usize, 0u64);
    let mut seen = std::collections::HashSet::new();
    for entry in archive.entries()?.raw(true) {
        let mut entry = entry?;
        let kind = entry.header().entry_type();
        if kind.is_pax_global_extensions()
            || kind.is_pax_local_extensions()
            || kind.is_gnu_longname()
            || kind.is_gnu_longlink()
        {
            return Err(BundleError::Path(
                "pax or GNU extension records are not allowed in ieb/v1".into(),
            ));
        }
        // Decompression-bomb / resource limits: bundles are untrusted input.
        entries += 1;
        total = total.saturating_add(entry.header().size()?);
        if entries > MAX_ENTRIES || total > MAX_UNPACKED_BYTES {
            return Err(BundleError::Path(format!(
                "bundle exceeds unpack limits ({MAX_ENTRIES} entries / {} MiB)",
                MAX_UNPACKED_BYTES >> 20
            )));
        }
        let path = entry.path()?.into_owned();
        // Reject absolute paths, `..`, and symlinks — untrusted archive hardening.
        if path.is_absolute()
            || path
                .components()
                .any(|c| c == std::path::Component::ParentDir)
        {
            return Err(BundleError::Path(format!(
                "unsafe path in bundle: {path:?}"
            )));
        }
        if let Some(p) = path.to_str().map(|p| p.trim_start_matches("./")) {
            let p = p.trim_end_matches('/');
            if !p.is_empty() && p != "." && !entry.header().entry_type().is_dir() {
                let rule = p
                    .strip_prefix("signature/")
                    .map_or_else(|| crate::hashtree::check_path(p), |_| Ok(()));
                rule.map_err(BundleError::Path)?;
                if !seen.insert(p.to_ascii_lowercase()) {
                    return Err(BundleError::Path(format!(
                        "duplicate or case-colliding entry: {p}"
                    )));
                }
            }
        } else {
            return Err(BundleError::Path("non-UTF-8 entry name".into()));
        }
        // Same entry types as `verify` (IEB-SPEC rule 1): regular files and directories.
        // Links, fifos, devices and sparse entries are refused rather than created.
        if !(kind.is_file() || kind.is_contiguous() || kind.is_dir()) {
            return Err(BundleError::Path(format!(
                "link or special entry not allowed: {path:?}"
            )));
        }
        let target = dest.join(&path);
        // The header's mode is not honoured: a directory entry with mode 0644 or a file with
        // mode 0000 used to unpack as such, and the unpacked directory then verified as
        // "Permission denied" (exit 3) while the same bytes as a `.ieb` verified OK — reachable
        // through `lapilli mcp`, which stages with `unpack` after `verify` said OK (found in the
        // triage of the independent review, 2026-09-25). Files are 0644 and directories 0755,
        // whatever the archive says; ownership was never applied (the tar crate's default),
        // mtime still is, and carries no meaning (rule 1).
        if kind.is_dir() {
            std::fs::create_dir_all(&target)?;
            set_mode(&target, 0o755)?;
            continue;
        }
        if let Some(parent) = target.parent() {
            std::fs::create_dir_all(parent)?;
        }
        entry.unpack(&target)?;
        set_mode(&target, 0o644)?;
    }
    Ok(())
}

#[cfg(unix)]
fn set_mode(path: &Path, mode: u32) -> std::io::Result<()> {
    use std::os::unix::fs::PermissionsExt;
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(mode))
}

#[cfg(not(unix))]
fn set_mode(_path: &Path, _mode: u32) -> std::io::Result<()> {
    Ok(())
}

/// Read only `manifest.json` out of a `.ieb`, streaming, without unpacking anything. For a
/// lookup over many bundles (`lapilli mcp find_bundles`): the identity, the window and the
/// target are all in the manifest, and a bundle is small, but a directory of them is not.
///
/// Untrusted input, like `unpack`: the same entry-type refusals and the same size ceiling
/// apply, and the manifest is bounded by the verifier's small-file cap. This reads bytes; it
/// verifies nothing. `verify` is the authority on whether they are what they claim.
pub fn read_manifest(ieb: &Path) -> Result<crate::Manifest, BundleError> {
    const MAX_MANIFEST: u64 = 16 << 20;
    let file = std::fs::File::open(ieb)?;
    let decoder = zstd::stream::read::Decoder::new(file).map_err(BundleError::Io)?;
    let mut archive = tar::Archive::new(decoder);
    let (mut entries, mut total) = (0usize, 0u64);
    for entry in archive.entries()?.raw(true) {
        let mut entry = entry?;
        let kind = entry.header().entry_type();
        if kind.is_pax_global_extensions()
            || kind.is_pax_local_extensions()
            || kind.is_gnu_longname()
            || kind.is_gnu_longlink()
        {
            return Err(BundleError::Path(
                "pax or GNU extension records are not allowed in ieb/v1".into(),
            ));
        }
        entries += 1;
        total = total.saturating_add(entry.header().size()?);
        if entries > MAX_ENTRIES || total > MAX_UNPACKED_BYTES {
            return Err(BundleError::Path("bundle exceeds unpack limits".into()));
        }
        let path = entry.path()?.into_owned();
        let name = path
            .to_str()
            .map(|p| p.trim_start_matches("./"))
            .unwrap_or("");
        if name == "manifest.json" {
            if entry.header().size()? > MAX_MANIFEST {
                return Err(BundleError::Path("manifest.json is over 16 MiB".into()));
            }
            let mut bytes = Vec::new();
            std::io::Read::read_to_end(&mut entry, &mut bytes)?;
            return Ok(serde_json::from_slice(&bytes)?);
        }
    }
    Err(BundleError::Path("no manifest.json in the bundle".into()))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;
    use std::io::Read;

    #[test]
    fn oversized_archives_are_refused() {
        let src = tempfile::tempdir().unwrap();
        let ieb = src.path().join("bomb.ieb");
        // A header claiming more than the limit (the data itself never needs to exist).
        let file = std::fs::File::create(&ieb).unwrap();
        let enc = zstd::stream::write::Encoder::new(file, 1)
            .unwrap()
            .auto_finish();
        let mut tar = tar::Builder::new(enc);
        let mut h = tar::Header::new_gnu();
        h.set_size(MAX_UNPACKED_BYTES + 1);
        h.set_mode(0o644);
        h.set_cksum();
        let _ = tar.append_data(&mut h, "big", std::io::repeat(0).take(0));
        drop(tar);
        let dest = tempfile::tempdir().unwrap();
        let err = unpack(&ieb, dest.path()).unwrap_err().to_string();
        assert!(err.contains("unpack limits"), "{err}");
    }

    /// Found in the triage of an independent review (2026-09-25): a directory entry with mode
    /// 0644 or a file with mode 0000 unpacked as such, and the unpacked directory then verified
    /// exit 3 ("Permission denied") while the same bytes as a `.ieb` verified OK. `unpack` now
    /// sets 0644 / 0755 itself, and both forms get the same verdict.
    #[test]
    fn unpack_ignores_header_modes() {
        use crate::verify::testutil::{files_of, ieb, sealed_dir};
        use crate::{verify_bundle, verify_bundle_dir, Verdict, VerifyOptions};
        use std::os::unix::fs::PermissionsExt;

        let files = files_of(sealed_dir().path());
        let mut entries = vec![("logs/", b"".as_slice(), tar::EntryType::Directory, 0o644)];
        for (p, b) in &files {
            let mode = if p == "logs/index.json" { 0o000 } else { 0o644 };
            entries.push((p.as_str(), b.as_slice(), tar::EntryType::Regular, mode));
        }
        let src = tempfile::tempdir().unwrap();
        let path = src.path().join("modes.ieb");
        fs::write(&path, ieb(&entries)).unwrap();
        let opts = VerifyOptions::default();
        let packed = verify_bundle(&path, &opts).unwrap();
        assert_eq!(packed.verdict, Verdict::Ok, "{:?}", packed.problems);

        let dest = tempfile::tempdir().unwrap();
        unpack(&path, dest.path()).unwrap();
        let unpacked = verify_bundle_dir(dest.path(), &opts).unwrap();
        assert_eq!(unpacked.verdict, packed.verdict, "{:?}", unpacked.problems);
        let mode = |p: &str| {
            fs::metadata(dest.path().join(p))
                .unwrap()
                .permissions()
                .mode()
                & 0o777
        };
        assert_eq!(mode("logs"), 0o755);
        assert_eq!(mode("logs/index.json"), 0o644);
        assert_eq!(mode("manifest.json"), 0o644);
    }

    /// The same entry types as `verify`: a fifo (or any other special entry) is refused, not
    /// created in `dest`.
    #[test]
    fn unpack_refuses_special_entries() {
        use crate::verify::testutil::ieb;
        let src = tempfile::tempdir().unwrap();
        let path = src.path().join("fifo.ieb");
        fs::write(
            &path,
            ieb(&[("logs/pipe", b"", tar::EntryType::Fifo, 0o644)]),
        )
        .unwrap();
        let dest = tempfile::tempdir().unwrap();
        let err = unpack(&path, dest.path()).unwrap_err().to_string();
        assert!(err.contains("special entry not allowed"), "{err}");
        assert!(!dest.path().join("logs/pipe").exists());
    }

    #[test]
    fn pack_then_unpack_roundtrip() {
        let src = tempfile::tempdir().unwrap();
        fs::create_dir(src.path().join("logs")).unwrap();
        fs::write(src.path().join("logs/a.log"), b"hello").unwrap();
        fs::write(src.path().join("manifest.json"), b"{}").unwrap();

        let ieb = src.path().join("out.ieb");
        pack(src.path(), &ieb).unwrap();
        assert!(ieb.exists());

        let dest = tempfile::tempdir().unwrap();
        unpack(&ieb, dest.path()).unwrap();
        assert_eq!(fs::read(dest.path().join("logs/a.log")).unwrap(), b"hello");
        assert_eq!(fs::read(dest.path().join("manifest.json")).unwrap(), b"{}");
    }
}
