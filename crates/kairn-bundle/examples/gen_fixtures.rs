//! Generate the golden and negative `ieb/v1` fixtures for a release:
//!
//!   cargo run -p kairn-bundle --example gen_fixtures -- test/fixtures/ieb/<release> test/fixtures/ieb/keys
//!
//! Fixtures are generated once per release and then never modified (docs/COMPATIBILITY.md).
//! Contents and timestamps are fixed; signatures are deterministic (RFC 6979); tar entries
//! are written in path order with mtime 0.

use std::collections::BTreeMap;
use std::fs;
use std::path::{Path, PathBuf};

use kairn_bundle::manifest::{Coverage, IncidentIdentity, Producer, Timing, Trigger, Window};
use kairn_bundle::{seal_dir, SealInput, StaticKeySigner};
use serde_json::json;

type Entries = BTreeMap<String, Vec<u8>>;

fn main() {
    let args: Vec<PathBuf> = std::env::args().skip(1).map(PathBuf::from).collect();
    let (out, keys) = match args.as_slice() {
        [out, keys] => (out.clone(), keys.clone()),
        _ => panic!("usage: gen_fixtures <out_dir> <key_dir>"),
    };
    fs::create_dir_all(&out).unwrap();
    let key = fs::read_to_string(keys.join("fixture.key")).expect("fixture.key (kairn keygen)");
    let signer = StaticKeySigner::from_pkcs8_pem(&key).unwrap();

    let mut expected = Vec::new();
    let ok = sealed(false, None);
    let signed = sealed(false, Some(&signer));
    let key_args = ["--key", "../keys/fixture.pub"];

    emit(
        &out,
        &mut expected,
        "ok-unsigned.ieb",
        &ok,
        0,
        &[],
        "a complete unsigned bundle",
    );
    emit(
        &out,
        &mut expected,
        "ok-signed.ieb",
        &signed,
        0,
        &key_args,
        "signed by the fixture key, verified with --key",
    );
    emit(
        &out,
        &mut expected,
        "ok-signed-unpinned.ieb",
        &signed,
        0,
        &[],
        "signed, no --key: OK but unpinned",
    );
    emit(
        &out,
        &mut expected,
        "partial.ieb",
        &sealed(true, None),
        2,
        &[],
        "an intended collector did not run",
    );
    emit(
        &out,
        &mut expected,
        "fail-wrong-key.ieb",
        &signed,
        1,
        &["--key", "../keys/other.pub"],
        "signed by a different key",
    );
    emit(
        &out,
        &mut expected,
        "fail-key-on-unsigned.ieb",
        &ok,
        1,
        &key_args,
        "--key given, bundle unsigned",
    );

    let mut e = ok.clone();
    e.insert("logs/app-previous.log".into(), b"all good\n".to_vec());
    emit(
        &out,
        &mut expected,
        "fail-modified.ieb",
        &e,
        1,
        &[],
        "a file was modified after sealing",
    );

    let mut e = ok.clone();
    e.remove("logs/app-previous.log");
    emit(
        &out,
        &mut expected,
        "fail-missing.ieb",
        &e,
        1,
        &[],
        "a listed file is missing",
    );

    let mut e = ok.clone();
    e.insert("injected.txt".into(), b"x".to_vec());
    emit(
        &out,
        &mut expected,
        "fail-unlisted.ieb",
        &e,
        1,
        &[],
        "a file not in the hash tree",
    );

    let mut e = signed.clone();
    e.insert("signature/extra.sig".into(), b"x".to_vec());
    emit(
        &out,
        &mut expected,
        "fail-unlisted-signature.ieb",
        &e,
        1,
        &[],
        "an unknown file under signature/",
    );

    let mut e = signed.clone();
    e.retain(|k, _| !k.starts_with("signature/"));
    emit(
        &out,
        &mut expected,
        "fail-stripped-signature.ieb",
        &e,
        1,
        &[],
        "declared signature missing",
    );

    let mut e = ok.clone();
    e.insert(
        "signature/manifest.sig".into(),
        signed["signature/manifest.sig"].clone(),
    );
    emit(
        &out,
        &mut expected,
        "fail-undeclared-signature.ieb",
        &e,
        1,
        &[],
        "signature present but not declared",
    );

    let mut e = ok.clone();
    e.remove("redaction.json");
    reseal_manifest(&mut e, |_| {});
    emit(
        &out,
        &mut expected,
        "fail-no-redaction.ieb",
        &e,
        1,
        &[],
        "redaction.json is required",
    );

    let mut e = ok.clone();
    reseal_manifest(&mut e, |m| {
        m["coverage"]["collectors_run"] = json!(["logs", "logs"]);
        m["coverage"]["collectors_intended"] = json!(["logs", "logs"]);
    });
    emit(
        &out,
        &mut expected,
        "fail-malformed-coverage.ieb",
        &e,
        1,
        &[],
        "duplicate collector names",
    );

    let mut e = ok.clone();
    reseal_manifest(&mut e, |m| {
        m["coverage"]["collectors_run"] = json!(["logs", "events"]);
        m["coverage"]["collectors_intended"] = json!(["logs", "events"]);
    });
    emit(
        &out,
        &mut expected,
        "fail-collector-files.ieb",
        &e,
        1,
        &[],
        "events listed as run without events.json",
    );

    let mut e = ok.clone();
    reseal_manifest(&mut e, |m| m["schema_version"] = json!("kairn.dev/ieb/v9"));
    emit(
        &out,
        &mut expected,
        "cannot-unknown-major.ieb",
        &e,
        3,
        &[],
        "a newer format major",
    );

    let mut e = ok.clone();
    reseal_manifest(&mut e, |m| m["schema_version"] = json!("kairn.dev/ieb/v0"));
    emit(
        &out,
        &mut expected,
        "cannot-v0.ieb",
        &e,
        3,
        &[],
        "the pre-release format",
    );

    // Raw archives the producer could never write.
    raw_ieb(
        &out.join("fail-traversal.ieb"),
        &ok,
        &[("../escape.txt", b"x", tar::EntryType::Regular)],
    );
    expected.push(json!({ "file": "fail-traversal.ieb", "args": [], "exit": 1, "why": "path traversal entry" }));
    raw_ieb(
        &out.join("fail-link.ieb"),
        &ok,
        &[("logs/link", b"", tar::EntryType::Symlink)],
    );
    expected
        .push(json!({ "file": "fail-link.ieb", "args": [], "exit": 1, "why": "symlink entry" }));
    raw_ieb(
        &out.join("fail-duplicate.ieb"),
        &ok,
        &[(
            "logs/app-previous.log",
            b"panic: boom\n",
            tar::EntryType::Regular,
        )],
    );
    expected.push(
        json!({ "file": "fail-duplicate.ieb", "args": [], "exit": 1, "why": "duplicate entry" }),
    );

    let mut e = ok.clone();
    e.insert("logs/App-previous.log".into(), b"x".to_vec());
    reseal_manifest(&mut e, |m| {
        m["hash_tree"]["files"]["logs/App-previous.log"] = json!(sha256("x".as_bytes()));
    });
    fix_root(&mut e);
    emit(
        &out,
        &mut expected,
        "fail-case-collision.ieb",
        &e,
        1,
        &[],
        "paths differ only by case",
    );

    // Round-5 R3 attacks: a second manifest must not be able to hide behind the first.
    let mut no_manifest = ok.clone();
    let real = no_manifest.remove("manifest.json").unwrap();
    let mut evil: serde_json::Value = serde_json::from_slice(&real).unwrap();
    evil["incident"]["id"] = json!("EVIL");
    let evil = serde_json::to_vec(&evil).unwrap();
    raw_ieb(
        &out.join("fail-duplicate-manifest.ieb"),
        &no_manifest,
        &[
            ("manifest.json", &evil, tar::EntryType::Regular),
            ("manifest.json", &real, tar::EntryType::Regular),
        ],
    );
    expected.push(json!({ "file": "fail-duplicate-manifest.ieb", "args": [], "exit": 1, "why": "two manifest.json entries" }));
    raw_ieb(
        &out.join("fail-case-manifest.ieb"),
        &ok,
        &[("Manifest.json", b"{}", tar::EntryType::Regular)],
    );
    expected.push(json!({ "file": "fail-case-manifest.ieb", "args": [], "exit": 1, "why": "manifest.json variant differing by case" }));
    raw_ieb(
        &out.join("fail-signature-file.ieb"),
        &ok,
        &[("signature", b"x", tar::EntryType::Regular)],
    );
    expected.push(json!({ "file": "fail-signature-file.ieb", "args": [], "exit": 1, "why": "signature as a regular file" }));
    raw_ieb(
        &out.join("fail-trailing-slash.ieb"),
        &ok,
        &[("logs/x.log/", b"x", tar::EntryType::Regular)],
    );
    expected.push(json!({ "file": "fail-trailing-slash.ieb", "args": [], "exit": 1, "why": "file entry with a trailing slash" }));

    let mut e = ok.clone();
    let m = String::from_utf8(e["manifest.json"].clone()).unwrap();
    let dup = m.replacen('{', r#"{"schema_version":"kairn.dev/ieb/v9","#, 1);
    e.insert("manifest.json".into(), dup.into_bytes());
    emit(
        &out,
        &mut expected,
        "fail-duplicate-json-key.ieb",
        &e,
        1,
        &[],
        "duplicate member name in manifest.json",
    );

    let mut e = ok.clone();
    reseal_manifest(&mut e, |m| m["schema_version"] = json!("kairn.dev/ieb/v01"));
    emit(
        &out,
        &mut expected,
        "fail-bad-schema-version.ieb",
        &e,
        1,
        &[],
        "not a valid schema_version",
    );

    let mut e = signed.clone();
    e.insert(
        "signature/ext/timestamp.tsr".into(),
        b"reserved for later".to_vec(),
    );
    emit(
        &out,
        &mut expected,
        "ok-signature-extension.ieb",
        &e,
        0,
        &key_args,
        "signature/ext/ is reserved and ignored",
    );
    emit(
        &out,
        &mut expected,
        "ok-compressed-key.ieb",
        &signed,
        0,
        &["--key", "../keys/fixture-compressed.pub"],
        "key_id is over the uncompressed SPKI, whatever form --key uses",
    );

    // Round-5 R4 attacks: tar extension records and file/directory ambiguity.
    raw_ieb(
        &out.join("fail-pax-record.ieb"),
        &ok,
        &[("pax", b"30 size=104857600\n", tar::EntryType::XHeader)],
    );
    expected.push(json!({ "file": "fail-pax-record.ieb", "args": [], "exit": 1, "why": "pax extension record (could override sizes and names)" }));
    raw_ieb(
        &out.join("fail-gnu-longname.ieb"),
        &ok,
        &[(
            "././@LongLink",
            b"logs/index.json\0",
            tar::EntryType::GNULongName,
        )],
    );
    expected.push(json!({ "file": "fail-gnu-longname.ieb", "args": [], "exit": 1, "why": "GNU long-name record" }));
    raw_ieb(
        &out.join("fail-file-and-dir.ieb"),
        &ok,
        &[("logs", b"x", tar::EntryType::Regular)],
    );
    expected.push(json!({ "file": "fail-file-and-dir.ieb", "args": [], "exit": 1, "why": "a path that is both a file and a directory" }));
    raw_ieb(
        &out.join("fail-reserved-prefix.ieb"),
        &ok,
        &[("redaction.json/x", b"x", tar::EntryType::Regular)],
    );
    expected.push(json!({ "file": "fail-reserved-prefix.ieb", "args": [], "exit": 1, "why": "a file inside a file's path" }));

    let mut no_manifest = ok.clone();
    let real = no_manifest.remove("manifest.json").unwrap();
    raw_ieb(
        &out.join("fail-dir-then-manifest.ieb"),
        &no_manifest,
        &[
            ("manifest.json/", b"", tar::EntryType::Directory),
            ("manifest.json", &real, tar::EntryType::Regular),
        ],
    );
    expected.push(json!({ "file": "fail-dir-then-manifest.ieb", "args": [], "exit": 1, "why": "a directory entry with a file's name" }));
    raw_ieb(
        &out.join("fail-sparse.ieb"),
        &ok,
        &[("logs/sparse", b"", tar::EntryType::GNUSparse)],
    );
    expected
        .push(json!({ "file": "fail-sparse.ieb", "args": [], "exit": 1, "why": "sparse entry" }));

    let mut e = ok.clone();
    e.insert("redaction.json".into(), vec![b' '; 17 << 20]);
    emit(
        &out,
        &mut expected,
        "cannot-big-redaction.ieb",
        &e,
        3,
        &[],
        "a file read into memory over the 16 MiB limit",
    );

    // A long path that fits only by splitting into the ustar prefix and name fields.
    let long = format!(
        "diffs/{}/Deployment/{}/0.json",
        "n".repeat(63),
        "d".repeat(63)
    );
    let dir = tempdir();
    for (p, b) in &ok {
        if p != "manifest.json" {
            let f = dir.join(p);
            fs::create_dir_all(f.parent().unwrap()).unwrap();
            fs::write(f, b).unwrap();
        }
    }
    fs::create_dir_all(dir.join(&long).parent().unwrap()).unwrap();
    fs::write(dir.join(&long), b"[]").unwrap();
    reseal_dir(&dir);
    kairn_bundle::pack(&dir, &out.join("ok-long-path.ieb")).unwrap();
    let _ = fs::remove_dir_all(&dir);
    expected.push(json!({ "file": "ok-long-path.ieb", "args": [], "exit": 0, "why": "a 144-byte path in the ustar prefix+name fields" }));

    // Truncated: cut the zstd stream in half.
    let bytes = fs::read(out.join("ok-unsigned.ieb")).unwrap();
    fs::write(out.join("fail-corrupt.ieb"), &bytes[..bytes.len() / 2]).unwrap();
    expected.push(
        json!({ "file": "fail-corrupt.ieb", "args": [], "exit": 1, "why": "truncated archive" }),
    );

    // A header claiming more than the verifier limit (no data needs to exist).
    oversized(&out.join("cannot-oversized.ieb"));
    expected.push(json!({ "file": "cannot-oversized.ieb", "args": [], "exit": 3, "why": "over the verifier limits" }));

    // Cases about the verifier's inputs rather than the bundle's bytes (they reuse bundles
    // above and carry their codes inline).
    let zeros = "0".repeat(64);
    expected.push(
        json!({ "file": "ok-unsigned.ieb", "args": ["--incident", "another-incident"],
        "exit": 1, "codes": ["context"], "why": "bound context: not the expected incident" }),
    );
    expected.push(
        json!({ "file": "ok-unsigned.ieb", "args": ["--expect-sha256", zeros],
        "exit": 1, "codes": ["digest"], "why": "not the expected file (--expect-sha256)" }),
    );
    expected.push(
        json!({ "file": "ok-signed.ieb", "args": ["--key", "../keys/fixture.key"],
        "exit": 3, "codes": ["unreadable"],
        "why": "--key is not a public key: the operator's input, not a signature failure" }),
    );
    expected.push(
        json!({ "file": "ok-signed.ieb", "args": ["--key", "../keys/does-not-exist.pub"],
        "exit": 3, "codes": ["unreadable"], "why": "--key cannot be read" }),
    );

    // The problem codes each fixture must report (`kairn.dev/verify-result/v1`, as a set).
    for case in &mut expected {
        if case.get("codes").is_some() {
            continue;
        }
        let file = case["file"].as_str().unwrap().to_string();
        let codes = CODES
            .iter()
            .find(|(f, _)| *f == file)
            .unwrap_or_else(|| panic!("no expected codes for {file}"))
            .1;
        case["codes"] = json!(codes);
    }
    fs::write(
        out.join("expected.json"),
        serde_json::to_vec_pretty(&expected).unwrap(),
    )
    .unwrap();
    println!("wrote {} fixtures to {}", expected.len(), out.display());
}

/// Expected problem codes per fixture (sorted, deduplicated).
const CODES: &[(&str, &[&str])] = &[
    ("ok-unsigned.ieb", &[]),
    ("ok-signed.ieb", &[]),
    ("ok-signed-unpinned.ieb", &[]),
    ("partial.ieb", &["partial"]),
    ("fail-wrong-key.ieb", &["signature"]),
    ("fail-key-on-unsigned.ieb", &["signature"]),
    ("fail-modified.ieb", &["integrity"]),
    ("fail-missing.ieb", &["integrity"]),
    ("fail-unlisted.ieb", &["integrity"]),
    ("fail-unlisted-signature.ieb", &["integrity"]),
    ("fail-stripped-signature.ieb", &["signature"]),
    ("fail-undeclared-signature.ieb", &["signature"]),
    ("fail-no-redaction.ieb", &["integrity", "manifest"]),
    ("fail-malformed-coverage.ieb", &["manifest"]),
    ("fail-collector-files.ieb", &["manifest"]),
    ("cannot-unknown-major.ieb", &["format-unsupported"]),
    ("cannot-v0.ieb", &["format-unsupported"]),
    ("fail-traversal.ieb", &["structure"]),
    ("fail-link.ieb", &["structure"]),
    ("fail-duplicate.ieb", &["structure"]),
    ("fail-case-collision.ieb", &["integrity", "structure"]),
    ("fail-duplicate-manifest.ieb", &["structure"]),
    ("fail-case-manifest.ieb", &["structure"]),
    ("fail-signature-file.ieb", &["structure"]),
    ("fail-trailing-slash.ieb", &["structure"]),
    ("fail-duplicate-json-key.ieb", &["manifest"]),
    ("fail-bad-schema-version.ieb", &["not-a-bundle"]),
    ("ok-signature-extension.ieb", &["notice"]),
    ("ok-compressed-key.ieb", &[]),
    ("fail-pax-record.ieb", &["structure"]),
    ("fail-gnu-longname.ieb", &["structure"]),
    ("fail-file-and-dir.ieb", &["structure"]),
    ("fail-reserved-prefix.ieb", &["structure"]),
    ("fail-dir-then-manifest.ieb", &["not-a-bundle", "structure"]),
    ("fail-sparse.ieb", &["structure"]),
    ("cannot-big-redaction.ieb", &["limit"]),
    ("ok-long-path.ieb", &[]),
    ("fail-corrupt.ieb", &["not-a-bundle", "structure"]),
    ("cannot-oversized.ieb", &["limit"]),
];

fn emit(
    out: &Path,
    expected: &mut Vec<serde_json::Value>,
    name: &str,
    entries: &Entries,
    exit: i32,
    args: &[&str],
    why: &str,
) {
    write_ieb(&out.join(name), entries);
    expected.push(json!({ "file": name, "args": args, "exit": exit, "why": why }));
}

/// A sealed bundle as path → bytes. `partial`: `metrics` was intended but did not run.
fn sealed(partial: bool, signer: Option<&StaticKeySigner>) -> Entries {
    let dir = tempdir();
    let d = dir.as_path();
    fs::create_dir_all(d.join("logs")).unwrap();
    fs::write(d.join("logs/app-previous.log"), b"panic: boom\n").unwrap();
    fs::write(d.join("logs/index.json"), br#"{"containers":[]}"#).unwrap();
    fs::write(
        d.join("redaction.json"),
        br#"{"policy_version":"v1","mode":"default"}"#,
    )
    .unwrap();
    let mut intended = vec!["logs".to_string()];
    if partial {
        intended.push("metrics".into());
    }
    let input = SealInput {
        incident: IncidentIdentity {
            id: "fixture-incident".into(),
            cluster_id: "fixture-cluster".into(),
            trigger: Trigger {
                rule: "KubePodCrashLooping".into(),
                firing_ts: "2026-09-19T00:00:00Z".into(),
            },
            window: Window {
                start: "2026-09-18T23:55:00Z".into(),
                end: "2026-09-19T00:05:00Z".into(),
            },
        },
        producer: Producer {
            kairn_version: "0.1.0".into(),
            image_digest: "sha256:fixture".into(),
        },
        coverage: Coverage {
            collectors_run: vec!["logs".into()],
            collectors_intended: intended,
        },
        timing: Timing {
            capture_started: "2026-09-19T00:00:01Z".into(),
            sealed_at: "2026-09-19T00:00:02Z".into(),
            capture_to_seal_ms: 1000,
        },
    };
    seal_dir(d, input, signer.map(|s| s as &dyn kairn_bundle::Signer)).unwrap();
    let mut out = Entries::new();
    read_all(d, d, &mut out);
    let _ = fs::remove_dir_all(d);
    out
}

/// Seal an already-populated staging dir (unsigned) the way `sealed` does.
fn reseal_dir(d: &Path) {
    let m: serde_json::Value =
        serde_json::from_slice(&sealed(false, None)["manifest.json"]).unwrap();
    let input = SealInput {
        incident: serde_json::from_value(m["incident"].clone()).unwrap(),
        producer: serde_json::from_value(m["producer"].clone()).unwrap(),
        coverage: serde_json::from_value(m["coverage"].clone()).unwrap(),
        timing: serde_json::from_value(m["timing"].clone()).unwrap(),
    };
    seal_dir(d, input, None).unwrap();
}

/// Rewrite manifest.json (unsigned) after mutating it, keeping its hash tree.
fn reseal_manifest(e: &mut Entries, f: impl FnOnce(&mut serde_json::Value)) {
    let mut m: serde_json::Value = serde_json::from_slice(&e["manifest.json"]).unwrap();
    f(&mut m);
    e.insert("manifest.json".into(), serde_json::to_vec(&m).unwrap());
}

/// Recompute the root after editing hash_tree.files (so only the targeted rule fails).
fn fix_root(e: &mut Entries) {
    let mut m: serde_json::Value = serde_json::from_slice(&e["manifest.json"]).unwrap();
    let files: BTreeMap<String, String> =
        serde_json::from_value(m["hash_tree"]["files"].clone()).unwrap();
    m["hash_tree"]["root"] = json!(kairn_bundle::HashTree::compute_root(&files));
    e.insert("manifest.json".into(), serde_json::to_vec(&m).unwrap());
}

fn sha256(b: &[u8]) -> String {
    use sha2::{Digest, Sha256};
    Sha256::digest(b)
        .iter()
        .map(|x| format!("{x:02x}"))
        .collect()
}

fn header(path: &str, size: u64, kind: tar::EntryType) -> tar::Header {
    let mut h = tar::Header::new_gnu();
    h.set_entry_type(kind);
    h.set_size(size);
    h.set_mode(0o644);
    h.set_mtime(0);
    h.set_uid(0);
    h.set_gid(0);
    // Bypass the tar crate's own path checks for the deliberately hostile fixtures.
    let name = &mut h.as_old_mut().name;
    name[..path.len()].copy_from_slice(path.as_bytes());
    if kind == tar::EntryType::Symlink {
        h.set_link_name("/etc/passwd").unwrap();
    }
    h.set_cksum();
    h
}

fn write_ieb(path: &Path, entries: &Entries) {
    raw_ieb(path, entries, &[]);
}

fn raw_ieb(path: &Path, entries: &Entries, extra: &[(&str, &[u8], tar::EntryType)]) {
    let file = fs::File::create(path).unwrap();
    let enc = zstd::stream::write::Encoder::new(file, 3)
        .unwrap()
        .auto_finish();
    let mut tar = tar::Builder::new(enc);
    for (p, bytes) in entries {
        tar.append(
            &header(p, bytes.len() as u64, tar::EntryType::Regular),
            bytes.as_slice(),
        )
        .unwrap();
    }
    for (p, bytes, kind) in extra {
        tar.append(&header(p, bytes.len() as u64, *kind), *bytes)
            .unwrap();
    }
    tar.finish().unwrap();
}

fn oversized(path: &Path) {
    let file = fs::File::create(path).unwrap();
    let enc = zstd::stream::write::Encoder::new(file, 3)
        .unwrap()
        .auto_finish();
    let mut tar = tar::Builder::new(enc);
    let h = header("huge.bin", 3 << 30, tar::EntryType::Regular);
    // Header only: the verifier must refuse before reading any data.
    tar.get_mut().write_all_header(&h);
}

trait WriteHeader {
    fn write_all_header(&mut self, h: &tar::Header);
}
impl<W: std::io::Write> WriteHeader for W {
    fn write_all_header(&mut self, h: &tar::Header) {
        self.write_all(h.as_bytes()).unwrap();
    }
}

fn read_all(root: &Path, dir: &Path, out: &mut Entries) {
    for entry in fs::read_dir(dir).unwrap() {
        let p = entry.unwrap().path();
        if p.is_dir() {
            read_all(root, &p, out);
        } else {
            let rel = p.strip_prefix(root).unwrap().to_str().unwrap().to_string();
            out.insert(rel, fs::read(&p).unwrap());
        }
    }
}

fn tempdir() -> PathBuf {
    use std::sync::atomic::{AtomicUsize, Ordering};
    static N: AtomicUsize = AtomicUsize::new(0);
    let n = N.fetch_add(1, Ordering::Relaxed);
    let d = std::env::temp_dir().join(format!("kairn-fixture-{}-{n}", std::process::id()));
    let _ = fs::remove_dir_all(&d);
    fs::create_dir_all(&d).unwrap();
    d
}
