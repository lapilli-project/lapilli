# Changelog

## [Unreleased]

## [0.3.0] - 2026-10-17

Lapilli is rebuilt from scratch as a tool that tells you why an SSH login fails.
Nothing carries over from 0.2.0: the Kubernetes incident recorder, `lapilli
verify`, the bundle format, the Helm chart, the container image and `lapilli
case` are gone. Their code and releases stay at
[v0.2.0](https://github.com/lapilli-project/lapilli/tree/v0.2.0).

- `lapilli ssh [ssh options] [user@]host` runs ssh once with `BatchMode`, reads
  its debug log, the configuration `ssh -G` reports, your key files' headers and
  your agent, and says in one line why the login fails, with the fix.
- A git remote URL works as the destination, and with no destination inside a
  repository it checks `origin`. GitHub, GitLab, Bitbucket, Codeberg and
  SourceHut are recognised.
- `--log FILE` explains a saved `ssh -vvv` log; `--json` prints the report for
  scripts.
- Written in Go with the standard library only. Released for macOS and Linux,
  amd64 and arm64.
