# Test logs

`logs/*.log` are `ssh -vvv` outputs. Files without a prefix were captured from
OpenSSH 10.0p2 against a local sshd set up to fail in each way, using throwaway
keys; paths and the user name were replaced with `/home/alice` and `alice`.

Files starting with `synthetic_` were written by hand in the formats of other
OpenSSH versions (7.4, 8.9, 9.6) and of GitHub's server, to test the parser on
messages the local setup can't produce. Their keys and fingerprints are made up,
except GitHub's published host key fingerprint.
