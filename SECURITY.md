# Security

Report a vulnerability privately through
[GitHub Security Advisories](https://github.com/lapilli-project/lapilli/security/advisories/new),
not in a public issue.

`lapilli verdict` reads rule, incident and firing files, and queries the
Prometheus-compatible API it is given, with the token or Google credentials it
is given. It only reads. Of particular interest:

- a way to make it send a query, a token or a credential anywhere other than
  the API it was given;
- a way for a rule file, an incident file, a firing file or an API answer to
  make it write a file, run a command, or send escape sequences to your
  terminal;
- a rule or answer that makes it use memory or time far out of proportion.

Only the latest release gets fixes. There is one maintainer, so fixes are best
effort.
