<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/lapilli-lockup-dark.png">
  <img alt="Lapilli" src="assets/lapilli-lockup.png" width="300">
</picture>

**Why won't SSH let me in?** `lapilli ssh` runs ssh once, reads what happened,
looks at your keys and your agent, and tells you the reason in one line, with
the fix.

```
$ lapilli ssh deploy@build.example.com
✗ build.example.com disconnected after ssh offered 3 keys (Too many authentication
  failures).

  Server    build.example.com (203.0.113.10) port 22, OpenSSH_9.6
  Host key  SHA256:Zf3mW5Q2pTn8pJgK1Vbq3sYcXo0dUa7rHk4LmNeFiGw, matches ~/.ssh/known_hosts:4
  User      deploy
  Agent     6 keys

  Keys, in the order ssh tried them:
    1  alice@laptop        ED25519  SHA256:uMXnuhvDtH…  agent  rejected
    2  work                ED25519  SHA256:ZPoW5AXDlz…  agent  rejected
    3  old-mbp             ED25519  SHA256:c1rIPwcN6z…  agent  cut off
    4  yubikey             ED25519  SHA256:4mzuIuqhXP…  agent  not reached
    ...
    7  ~/.ssh/deploy_key   ED25519  SHA256:AOiwZTRg9A…  file   not reached

Why
  ssh offers every key in your agent before the key files you configured, and
  the server allows only a few failed keys per connection (MaxAuthTries, 6
  unless changed). They ran out before ssh got through its list.
  Your configured key ~/.ssh/deploy_key was number 7 in line and never got a
  turn.

Fix
  1. Make ssh offer only that key to this host. In ~/.ssh/config:
       Host build.example.com
         IdentityFile ~/.ssh/deploy_key
         IdentitiesOnly yes
  2. Or check it right away:
       lapilli ssh -i ~/.ssh/deploy_key -o IdentitiesOnly=yes deploy@build.example.com
```

## Install

```sh
go install github.com/lapilli-project/lapilli@latest
```

or take a binary for macOS or Linux from the
[releases](https://github.com/lapilli-project/lapilli/releases). It needs the
OpenSSH client (`ssh`, `ssh-add`), 7.4 or later: it is checked against the
clients of Amazon Linux 2, Rocky Linux 8 and 9, Ubuntu 20.04 to 24.04, Debian
12, Alpine and Fedora, as well as macOS.

## Use

```sh
lapilli ssh user@host                     # any destination ssh accepts, aliases included
lapilli ssh -i ~/.ssh/work -p 2222 host   # with the ssh options you'd use
lapilli ssh git@github.com                # also tells you which GitHub account the key belongs to
lapilli ssh git@github.com:org/repo.git   # a git remote URL works too
lapilli ssh                               # inside a git repository: checks its origin
lapilli ssh --log ci-ssh.log              # explain a saved `ssh -vvv` log, e.g. from CI
lapilli ssh --json user@host              # for scripts and agents
```

Exit status: 0 when the login works, 1 when it doesn't, 2 when lapilli itself
couldn't run.

## What it recognises

Before login

- the host name doesn't resolve; the port is closed, filtered or not SSH
- the server dropped you before the handshake, including OpenSSH 9.8+
  per-source penalties after repeated failures
- no algorithm in common with an old server (ssh-rsa host keys, old key
  exchange or ciphers), with the per-host setting that allows it
- a changed host key (and whether the new one is the service's published key,
  for GitHub, GitLab, Bitbucket and Codeberg); a host you've never connected to
- jump hosts (ProxyJump): which host on the way refused, or couldn't reach the
  next one
- `~/.ssh/config` that ssh refuses to read because of its permissions, or
  stops at: an option this ssh doesn't have (macOS's `UseKeychain` in a config
  copied to Linux), a bad value

Logging in

- every key rejected, with what to check on the server (authorized_keys,
  StrictModes permissions, the sshd log, SELinux labels)
- keys in `~/.ssh` that ssh never tried because of their names
- *Too many authentication failures*: which key was cut off, and the
  `IdentitiesOnly` setting that fixes it
- a key the server accepts but ssh can't use: a passphrase with no agent to
  hold it, permissions too open, an unreadable or PuTTY-format file, an agent
  that refused to sign (1Password, gpg-agent, Secretive, `ssh-add -c`), a
  security key that wasn't touched
- RSA keys an old server can only verify with SHA-1, which OpenSSH 8.8+ won't
  send; DSA keys; key types your own config disables
- a server that doesn't take keys at all, or wants a second factor after the key
- `IdentityFile` pointing at a missing file, or at a `.pub` with no agent
  behind it; a `.pub` that belongs to a different private key
- git hosts: logging in as anything but `git`, keys not registered on the
  account, port 22 blocked (with the port 443 alternative)

If it gets one wrong, an issue with the output of `lapilli ssh --show-log` (look
it over for host names you'd rather not share) is the most useful report there
is: every rule here is pinned to a real `ssh -vvv` log in `testdata/logs`.

## What it does, and doesn't

`lapilli ssh` runs `ssh -vvv` once, with `BatchMode=yes` and options that make
the attempt harmless: no prompts, no remote command (`-N`; `-T` for git hosts,
which answer with a greeting), no forwarding, no connection sharing, no
`LocalCommand`, and nothing written to `known_hosts`. Your keys are offered to
the host you named, exactly as `ssh` would offer them.

On your machine it reads `ssh -G` for the configuration ssh applies, the
headers of key files (type, size, fingerprint, whether a passphrase is set; it
never decrypts a key), and `ssh-add -l` for what your agent holds. Nothing
leaves your machine except the SSH connection itself.

It sees the client side. When the server rejects a key, lapilli can say that it
did and what usually causes it, but not which of those causes it is; the
server's log has that.

## Earlier Lapilli

Up to v0.2.0 (October 2026), Lapilli was a Kubernetes incident recorder with a
Helm chart and a container image. That project stopped and this one took its
name; its code and releases stay at the
[v0.2.0 tag](https://github.com/lapilli-project/lapilli/tree/v0.2.0).

## License

Apache 2.0
