# Security

Report a vulnerability privately through
[GitHub Security Advisories](https://github.com/lapilli-project/lapilli/security/advisories/new),
not in a public issue.

`lapilli ssh` runs ssh with your keys against the host you name, and reads your
key files and your agent. Of particular interest:

- a way to make it connect anywhere other than the destination it was given,
  run a remote command, forward anything, or write to `known_hosts`;
- a way for a server, a crafted `ssh -vvv` log, a repository's `origin`, or a
  file in `~/.ssh` to make it run a command, write a file, print private key
  material, send escape sequences to your terminal, or suggest a command that
  does more than it says;
- anything that makes it decrypt a key or ask for a passphrase.

Only the latest release gets fixes. There is one maintainer, so fixes are best
effort.
