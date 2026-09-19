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
            checked += 1;
        }
    }
    assert!(
        checked >= 20,
        "expected the fixture set, found {checked} cases"
    );
    assert!(failures.is_empty(), "{}", failures.join("\n"));
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
