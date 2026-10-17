# Test logs

`logs/*.log` are `ssh -vvv` outputs. Files without a prefix were captured from
OpenSSH 10.0p2 against a local sshd set up to fail in each way, using throwaway
keys; paths and the user name were replaced with `/home/alice` and `alice`.

Files starting with `openssh74_`, `openssh80_`, `openssh89_` or `openssh99_`,
and the `config_` files, were captured by `scripts/lab/linux` with the OpenSSH
client of a Linux distribution (7.4: Amazon Linux 2, 8.0: Rocky Linux 8, 8.9:
Ubuntu 22.04, 9.9: Rocky Linux 9 with its crypto policy, 10.0: Alpine 3.22)
against the lab's sshd in another container, as the user `alice`, with
throwaway keys. They are unedited.

Files starting with `synthetic_` were written by hand in the formats of other
OpenSSH versions (7.4, 8.9, 9.6) and of GitHub's server, to test the parser on
messages the lab can't produce. Their keys and fingerprints are made up, except
GitHub's published host key fingerprint.
