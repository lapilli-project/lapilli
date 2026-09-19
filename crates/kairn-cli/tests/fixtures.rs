//! Every released bundle fixture must keep verifying with its recorded exit code
//! (docs/COMPATIBILITY.md). Fixtures live in `test/fixtures/ieb/<release>/expected.json` and
//! are never modified after their release ships; a failure here blocks the release.

use std::path::PathBuf;
use std::process::Command;

#[test]
fn every_released_fixture_keeps_its_verdict() {
    let root = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../test/fixtures/ieb");
    let mut checked = 0;
    let mut failures = Vec::new();
    for release in std::fs::read_dir(&root).expect("test/fixtures/ieb") {
        let dir = release.unwrap().path();
        let expected = dir.join("expected.json");
        if !expected.exists() {
            continue; // keys/
        }
        let cases: Vec<serde_json::Value> =
            serde_json::from_slice(&std::fs::read(&expected).unwrap()).unwrap();
        for case in cases {
            let file = case["file"].as_str().unwrap();
            let args: Vec<&str> = case["args"]
                .as_array()
                .unwrap()
                .iter()
                .map(|a| a.as_str().unwrap())
                .collect();
            let want = case["exit"].as_i64().unwrap() as i32;
            let out = Command::new(env!("CARGO_BIN_EXE_kairn"))
                .current_dir(&dir)
                .arg("verify")
                .arg(file)
                .args(&args)
                .output()
                .unwrap();
            let got = out.status.code().unwrap_or(-1);
            if got != want {
                failures.push(format!(
                    "{}/{file}: exit {got}, want {want} ({})\n{}{}",
                    dir.file_name().unwrap().to_string_lossy(),
                    case["why"],
                    String::from_utf8_lossy(&out.stdout),
                    String::from_utf8_lossy(&out.stderr)
                ));
            }
            // The same case through `--output json` (kairn.dev/verify-result/v1).
            let out = Command::new(env!("CARGO_BIN_EXE_kairn"))
                .current_dir(&dir)
                .arg("verify")
                .arg(file)
                .args(&args)
                .args(["--output", "json"])
                .output()
                .unwrap();
            let name = format!(
                "{}/{file} --output json",
                dir.file_name().unwrap().to_string_lossy()
            );
            match check_result_document(&out, want) {
                Err(e) => failures.push(format!("{name}: {e}")),
                Ok(codes) => {
                    if let Some(expected) = case["codes"].as_array() {
                        let expected: Vec<&str> =
                            expected.iter().map(|c| c.as_str().unwrap()).collect();
                        if codes != expected {
                            failures.push(format!(
                                "{name}: problem codes {codes:?}, want {expected:?}"
                            ));
                        }
                    }
                }
            }
            checked += 1;
        }
    }
    assert!(
        checked >= 20,
        "expected the fixture set, found {checked} cases"
    );
    assert!(failures.is_empty(), "{}", failures.join("\n"));
}

/// The closed set of problem codes in `kairn.dev/verify-result/v1` (docs/COMPATIBILITY.md
/// §2). Adding one is allowed in a minor release; removing or renaming one is not.
const CODES: &[&str] = &[
    "unreadable",
    "limit",
    "format-unsupported",
    "not-a-bundle",
    "structure",
    "manifest",
    "integrity",
    "context",
    "signature",
    "partial",
    "notice",
    "custody",
    "digest",
];

/// Check one `--output json` run against the v1 contract; returns its sorted problem codes.
fn check_result_document(
    out: &std::process::Output,
    want_exit: i32,
) -> Result<Vec<String>, String> {
    use serde_json::Value;
    let exit = out.status.code().unwrap_or(-1);
    if exit != want_exit {
        return Err(format!("exit {exit}, want {want_exit}"));
    }
    if !out.stderr.is_empty() {
        return Err(format!(
            "stderr not empty: {}",
            String::from_utf8_lossy(&out.stderr)
        ));
    }
    let doc: Value = serde_json::from_slice(&out.stdout)
        .map_err(|e| format!("stdout is not one JSON document: {e}"))?;
    let field = |v: &Value, path: &str| -> Result<Value, String> {
        v.pointer(path).cloned().ok_or(format!("missing {path}"))
    };
    if field(&doc, "/schema")? != "kairn.dev/verify-result/v1" {
        return Err("wrong schema".into());
    }
    if field(&doc, "/exit_code")? != exit {
        return Err("exit_code differs from the process exit".into());
    }
    let verdict = field(&doc, "/verdict")?;
    let want_verdict = match exit {
        0 => "OK",
        1 => "FAILED",
        2 => "PARTIAL",
        3 => "CANNOT_EVALUATE",
        _ => return Err(format!("unexpected exit {exit}")),
    };
    if verdict != want_verdict {
        return Err(format!("verdict {verdict} for exit {exit}"));
    }
    // Every member is always present, with its type (null only where the spec allows).
    #[derive(Clone, Copy)]
    enum T {
        Str,
        Int,
        Num,
        Bool,
        Obj,
        Arr,
    }
    let typed = |path: &str, t: T, nullable: bool| -> Result<Value, String> {
        let v = field(&doc, path)?;
        let ok = match (&v, t) {
            (Value::Null, _) => nullable,
            (Value::String(_), T::Str) | (Value::Bool(_), T::Bool) => true,
            (Value::Object(_), T::Obj) | (Value::Array(_), T::Arr) => true,
            (Value::Number(n), T::Int) => n.is_u64(),
            (Value::Number(_), T::Num) => true,
            _ => false,
        };
        ok.then_some(v.clone())
            .ok_or(format!("{path} has the wrong type: {v}"))
    };
    typed("/schema", T::Str, false)?;
    typed("/kairn_version", T::Str, false)?;
    typed("/verdict", T::Str, false)?;
    typed("/exit_code", T::Int, false)?;
    typed("/input/type", T::Str, false)?;
    typed("/input/location", T::Str, false)?;
    typed("/input/size", T::Int, true)?;
    let sha = typed("/input/sha256", T::Str, true)?;
    if let Some(h) = sha.as_str() {
        if h.len() != 64
            || !h
                .bytes()
                .all(|b| b.is_ascii_hexdigit() && !b.is_ascii_uppercase())
        {
            return Err(format!("input.sha256 is not lowercase hex: {h}"));
        }
    }
    typed("/input/version_id", T::Str, true)?;
    if !typed("/input/history", T::Obj, true)?.is_null() {
        typed("/input/history/state", T::Str, false)?;
        typed("/input/history/versions", T::Int, true)?;
        typed("/input/history/delete_markers", T::Int, true)?;
        typed("/input/history/fetched_is_latest", T::Bool, true)?;
        typed("/input/history/truncated", T::Bool, true)?;
        typed("/input/history/reason", T::Str, true)?;
    }
    typed("/expected/cluster", T::Str, true)?;
    typed("/expected/incident", T::Str, true)?;
    typed("/expected/source", T::Str, false)?;
    if !typed("/bundle", T::Obj, true)?.is_null() {
        typed("/bundle/format", T::Str, true)?;
        let producer = typed("/bundle/producer_version", T::Str, true)?;
        typed("/bundle/cluster_id", T::Str, true)?;
        typed("/bundle/incident_id", T::Str, true)?;
        typed("/bundle/redaction_mode", T::Str, true)?;
        // The checks are null exactly when the format's rules did not run.
        let checks = [
            typed("/bundle/hash_ok", T::Bool, true)?,
            typed("/bundle/context_ok", T::Bool, true)?,
            typed("/bundle/coverage_score", T::Num, true)?,
            typed("/bundle/partial", T::Bool, true)?,
            typed("/bundle/signature", T::Str, true)?,
        ];
        if checks.iter().any(|c| c.is_null() != producer.is_null()) {
            return Err("bundle checks must be null exactly when the rules did not run".into());
        }
    }
    typed("/problems", T::Arr, false)?;
    let mut codes = Vec::new();
    for p in doc["problems"]
        .as_array()
        .ok_or("problems is not an array")?
    {
        let code = p["code"].as_str().ok_or("problem without a code")?;
        if !CODES.contains(&code) {
            return Err(format!("unknown problem code {code}"));
        }
        p["message"].as_str().ok_or("problem without a message")?;
        codes.push(code.to_string());
    }
    codes.sort();
    codes.dedup();
    Ok(codes)
}

#[test]
fn unreadable_input_is_still_a_result_document() {
    let out = Command::new(env!("CARGO_BIN_EXE_kairn"))
        .args(["verify", "/nonexistent/bundle.ieb", "--output", "json"])
        .output()
        .unwrap();
    assert_eq!(check_result_document(&out, 3).unwrap(), vec!["unreadable"]);
    let doc: serde_json::Value = serde_json::from_slice(&out.stdout).unwrap();
    assert!(doc["bundle"].is_null());
}

#[test]
fn usage_errors_do_not_look_like_partial() {
    let out = Command::new(env!("CARGO_BIN_EXE_kairn"))
        .args(["verify", "--kye", "x"])
        .output()
        .unwrap();
    assert_eq!(out.status.code(), Some(64));
}

#[test]
fn unreadable_input_cannot_be_evaluated() {
    let out = Command::new(env!("CARGO_BIN_EXE_kairn"))
        .args(["verify", "/nonexistent/bundle.ieb"])
        .output()
        .unwrap();
    assert_eq!(out.status.code(), Some(3));
}
