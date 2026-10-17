# Changelog

## [Unreleased]

## [0.3.3] - 2026-10-18

Security fixes, from a review of how untrusted input reaches the output and the
ssh command line:

- A value from the log (a host, user or key path) went unquoted into the
  commands lapilli suggests, so a crafted log given to `--log`, or text a
  server sent, could make a suggested command run something else when pasted.
  Every such value is now shell-quoted (`~` and `~user` kept expandable), and
  values that can't be written safely into an ssh config snippet leave the
  snippet out.
- A server's disconnect message or banner may contain newlines, and so lines
  that look like ssh's own. Such lines could make the probe drop its own safety
  options one by one (BatchMode, StrictHostKeyChecking, …) and run ssh again,
  or report a login that didn't happen. Now only ssh's first line can make the
  probe leave out an option, and only `RemoteCommand`, which OpenSSH before 7.6
  lacks; nothing after a disconnect is read; a refusal outweighs an earlier
  "Authenticated to".
- Text from a log or a server is printed with control characters and Unicode
  format characters (escape sequences, bidi overrides) made visible, in the
  answer and with `--show-log`.
- A destination whose host or user name starts with `-` or holds shell
  characters is refused, whether typed or read from a repository's `origin`,
  as OpenSSH 9.6 does (CVE-2023-51385).
- `AddKeysToAgent=no`: with `AddKeysToAgent yes` in your config, a check could
  add a key to your agent.
- `--log` reads only a regular file or standard input, up to 64 MB; a line over
  1 MB is cut instead of ending the parse; the probe keeps at most 8 MB of log.
- Releases are built with the newest Go instead of go.mod's minimum: 0.3.2 was
  built with Go 1.23.12, whose standard library has known vulnerabilities on
  paths lapilli uses (key parsing, URL parsing). go.mod now requires Go 1.26,
  the oldest release still supported; CI runs govulncheck on every change and
  weekly.
- The workflows pin `actions/checkout` and `actions/setup-go` to commit SHAs
  and don't keep the token in `.git`.

## [0.3.2] - 2026-10-18

- Through jump hosts (ProxyJump), `lapilli ssh` answered for the wrong host.
  It stopped at the jump host's login, so a destination that refused the key,
  couldn't be reached or had an unknown host key came out as a success; a jump
  host that refused the key came out as the destination closing the
  connection. The log is now read one connection per host: the answer says
  where ssh stopped ("On the way to db, bastion rejected the only key…"), and a
  jump host that can't reach the next host gets its own answer, with `nc` to
  run from the jump host.
- A jump host's ssh got none of the probe's options, so it could ask for a
  password, and before 8.5 wrote to `known_hosts`. ProxyJump is now run as the
  ProxyCommand ssh would make of it, with those options on every hop, and ssh
  runs without a terminal to ask on.
- `ssh-copy-id` in the answer keeps the port, `-F`, `-o` and the jump host of
  the check.
- A successful login no longer warns that ssh "offered other keys first" when
  those were key files that don't exist.

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
