---
layout: default
title: Why won't SSH let me in?
description: "lapilli ssh runs ssh once, reads what happened, looks at your keys and your agent, and tells you why the login fails, in one line, with the fix."
---

# Why won't SSH let me in?

`lapilli ssh` runs ssh once, reads what happened, looks at your keys and your
agent, and tells you the reason in one line, with the fix.

```
$ lapilli ssh git@github.com
✗ GitHub rejected the only key ssh offered: it isn't registered on GitHub.

  Server    github.com (20.200.245.247) port 22
  Host key  SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU, matches ~/.ssh/known_hosts:3
  User      git
  Agent     0 keys

  Keys, in the order ssh tried them:
    1  ~/.ssh/id_rsa  RSA-2048  SHA256:Q3bN8xYkP2…  file   rejected

Why
  The key reached GitHub and was turned down. GitHub accepts a key that is
  added to an account or to a repository as a deploy key; this one is neither.

Fix
  1. If you already added a key to GitHub, it may be one ssh didn't try:
       lapilli ssh -i ~/.ssh/work_ed25519 git@github.com
  2. Copy the public key:
       pbcopy < ~/.ssh/id_rsa.pub
  3. Add it at https://github.com/settings/ssh/new, then check again:
       lapilli ssh git@github.com
```

## Install

```sh
go install github.com/lapilli-project/lapilli@latest
```

or take a binary for macOS or Linux from the
[releases](https://github.com/lapilli-project/lapilli/releases). It needs the
OpenSSH client.

## Use

```sh
lapilli ssh user@host                     # any destination ssh accepts, aliases included
lapilli ssh -i ~/.ssh/work -p 2222 host   # with the ssh options you'd use
lapilli ssh git@github.com:org/repo.git   # a git remote URL works too
lapilli ssh                               # inside a git repository: checks its origin
lapilli ssh --log ci-ssh.log              # explain a saved `ssh -vvv` log
lapilli ssh --json user@host              # for scripts and agents
```

## What it knows

- **Before login:** a name that doesn't resolve; a port that is closed,
  filtered or not SSH; a server that drops you before the handshake, including
  OpenSSH's per-source penalties; no algorithm in common with an old server; a
  changed host key, and whether the new one is GitHub's, GitLab's, Bitbucket's
  or Codeberg's published key.
- **Logging in:** every key rejected; keys in `~/.ssh` that ssh never tried;
  *Too many authentication failures* and the key that was cut off; a key the
  server accepts but ssh can't use (a passphrase with no agent, permissions too
  open, an agent that refused to sign, a security key nobody touched); RSA keys
  an old server can only check with SHA-1; a server that wants a second factor.
- **Git hosts:** the wrong user, a key that isn't on the account, port 22
  blocked and the port 443 way around it.

## What it won't do

It runs ssh with `BatchMode=yes`: no prompts, no remote command, no forwarding,
no connection sharing, nothing written to `known_hosts`. It reads key file
headers but never decrypts a key. Nothing leaves your machine except the SSH
connection itself.

It sees the client side. When a server rejects a key, it can tell you that it
did and what usually causes it; only the server's log can say which cause it
was.
