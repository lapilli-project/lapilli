//! `lapilli case …` hands over to `lapilli-case`. These tests stand a shell script in for that
//! binary, so they say nothing about what it does — only that the hand-over is exact: the same
//! arguments, the same exit code, the right copy, and a plain message when there is none.
#![cfg(unix)]

use std::fs;
use std::os::unix::fs::PermissionsExt;
use std::path::Path;
use std::process::Command;

/// A stand-in that prints who it is and what it was given, then exits 7.
fn stand_in(dir: &Path, who: &str) {
    fs::create_dir_all(dir).unwrap();
    let path = dir.join("lapilli-case");
    fs::write(
        &path,
        format!(
            "#!/bin/sh\nprintf '%s:' {who}\nfor a in \"$@\"; do printf ' [%s]' \"$a\"; done\nexit 7\n"
        ),
    )
    .unwrap();
    fs::set_permissions(&path, fs::Permissions::from_mode(0o755)).unwrap();
}

#[test]
fn everything_after_case_is_passed_on_unchanged() {
    let tmp = tempfile::tempdir().unwrap();
    stand_in(tmp.path(), "path");
    let out = Command::new(env!("CARGO_BIN_EXE_lapilli"))
        .args([
            "case",
            "run",
            "cases/a b",
            "--runs",
            "3",
            "--help",
            "-o",
            "--version",
            "--",
            "x",
        ])
        .env("PATH", tmp.path())
        .output()
        .unwrap();
    // `--help` and `--version` belong to lapilli-case here, not to lapilli.
    assert_eq!(
        String::from_utf8_lossy(&out.stdout),
        "path: [run] [cases/a b] [--runs] [3] [--help] [-o] [--version] [--] [x]"
    );
    assert_eq!(out.status.code(), Some(7), "the exit code is the tool's");
}

#[test]
fn bare_case_and_case_help_reach_the_tool() {
    let tmp = tempfile::tempdir().unwrap();
    stand_in(tmp.path(), "path");
    for args in [&["case"][..], &["case", "--help"][..], &["case", "-h"][..]] {
        let out = Command::new(env!("CARGO_BIN_EXE_lapilli"))
            .args(args)
            .env("PATH", tmp.path())
            .output()
            .unwrap();
        let want: String = std::iter::once("path:".to_string())
            .chain(args[1..].iter().map(|a| format!(" [{a}]")))
            .collect();
        assert_eq!(String::from_utf8_lossy(&out.stdout), want, "{args:?}");
        assert_eq!(out.status.code(), Some(7), "{args:?}");
    }
}

#[test]
fn the_copy_beside_lapilli_wins_over_path() {
    let tmp = tempfile::tempdir().unwrap();
    let (installed, elsewhere) = (tmp.path().join("installed"), tmp.path().join("elsewhere"));
    stand_in(&installed, "beside");
    stand_in(&elsewhere, "path");
    let lapilli = installed.join("lapilli");
    fs::copy(env!("CARGO_BIN_EXE_lapilli"), &lapilli).unwrap();
    let out = Command::new(&lapilli)
        .args(["case", "verify"])
        .env("PATH", &elsewhere)
        .output()
        .unwrap();
    assert_eq!(String::from_utf8_lossy(&out.stdout), "beside: [verify]");
}

#[test]
fn a_missing_tool_is_said_plainly() {
    let empty = tempfile::tempdir().unwrap();
    let out = Command::new(env!("CARGO_BIN_EXE_lapilli"))
        .args(["case", "verify", "x"])
        .env("PATH", empty.path())
        .output()
        .unwrap();
    assert_eq!(out.status.code(), Some(127));
    let err = String::from_utf8_lossy(&out.stderr);
    assert!(err.contains("`lapilli-case` was not found"), "{err}");
    assert!(err.contains("go build ./cmd/lapilli-case"), "{err}");
}

#[test]
fn case_is_listed_and_leaves_the_other_commands_alone() {
    let help = Command::new(env!("CARGO_BIN_EXE_lapilli"))
        .arg("--help")
        .output()
        .unwrap();
    let text = String::from_utf8_lossy(&help.stdout);
    assert!(help.status.success());
    for command in ["verify", "postmortem", "unpack", "case"] {
        assert!(
            text.lines()
                .any(|l| l.trim_start().starts_with(&format!("{command} "))),
            "`{command}` is missing from --help:\n{text}"
        );
    }
    // A typo still exits 64, not whatever a helper binary might have said.
    let typo = Command::new(env!("CARGO_BIN_EXE_lapilli"))
        .arg("cases")
        .output()
        .unwrap();
    assert_eq!(typo.status.code(), Some(64));
}
