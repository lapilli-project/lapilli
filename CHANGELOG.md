# Changelog

## [Unreleased]

## [0.3.1] - 2026-10-18

- `lapilli ssh` works with OpenSSH 7.4 and 7.5 (RHEL and CentOS 7, Amazon
  Linux 2). It passed `RemoteCommand`, which those versions don't know, and ssh
  stopped before connecting; an option ssh rejects is now left out.
- On OpenSSH before 8.5, checking a host no longer adds its address to
  `~/.ssh/known_hosts` (`CheckHostIP=no`).
- A configuration ssh refuses is explained instead of ending in "ssh -G
  failed": an option this ssh doesn't have (such as macOS's `UseKeychain` in a
  config copied to Linux), a bad value, or a config file others can write to.
- When agent keys use up the server's tries, the answer names the key file in
  `~/.ssh` that never got a turn, and how to check it.
- `scripts/lab/linux` runs every lab failure with the OpenSSH client of nine
  Linux distributions (7.4 to 10.x) in Docker, live and from the saved log.

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
