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

/// Pack the sealed directory `dir` into a `.ieb` file at `out`.
pub fn pack(dir: &Path, out: &Path) -> Result<(), BundleError> {
    let file = std::fs::File::create(out)?;
    let encoder = zstd::stream::write::Encoder::new(file, ZSTD_LEVEL)
        .map_err(BundleError::Io)?
        .auto_finish();
    let mut tar = tar::Builder::new(encoder);
    // Store paths relative to `dir`; deterministic-enough for a capture artifact.
    tar.append_dir_all(".", dir)?;
    tar.finish()?;
    Ok(())
}

/// Unpack a `.ieb` file into `dest` (which must exist). Rejects entries that would escape
/// `dest` (path traversal / absolute paths) — bundles are untrusted input to `verify`.
pub fn unpack(ieb: &Path, dest: &Path) -> Result<(), BundleError> {
    let file = std::fs::File::open(ieb)?;
    let decoder = zstd::stream::read::Decoder::new(file).map_err(BundleError::Io)?;
    let mut archive = tar::Archive::new(decoder);

    let (mut entries, mut total) = (0usize, 0u64);
    for entry in archive.entries()? {
        let mut entry = entry?;
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
        if entry.header().entry_type().is_symlink() || entry.header().entry_type().is_hard_link() {
            return Err(BundleError::Path(format!(
                "link entry not allowed: {path:?}"
            )));
        }
        let target = dest.join(&path);
        if let Some(parent) = target.parent() {
            std::fs::create_dir_all(parent)?;
        }
        entry.unpack(&target)?;
    }
    Ok(())
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
