# Failure lab

Reproduces SSH login failures against a local sshd, to capture real `ssh -vvv`
logs for `testdata/logs`. Everything runs as you, on 127.0.0.1, with throwaway
keys generated here. Your own `~/.ssh` and agent are not used.

```sh
./setup.sh                      # keys, five sshd instances on ports 2322-2326
KH="UserKnownHostsFile $PWD/run/known_hosts"
./capture.sh rejected 2322 "$KH" "IdentitiesOnly yes" "IdentityFile $PWD/keys/other"
./agent.sh flood keys/flood1 keys/flood2 keys/flood3 -- too_many 2323 "$KH" "IdentityFile $PWD/keys/good"
./stop.sh
```

| port | server |
|------|--------|
| 2322 | ordinary |
| 2323 | `MaxAuthTries 3` |
| 2324 | `PubkeyAuthentication no` |
| 2325 | accepts only `ssh-rsa` (SHA-1) signatures |
| 2326 | offers only an `ssh-rsa` host key |

`authorized_keys` holds `good`, `enc`, `rsa`, `ecdsa` and `nopub`; `other`,
`enc2` and `flood1`..`flood6` are unknown to the server. `enc` and `enc2` have
the passphrase `pw`.

`agent.sh` starts its own ssh-agent on a short relative socket path and stops if
that agent did not come up, so keys can never land in the agent you use.

Copy a log into `testdata/logs` after replacing local paths and your user name
with `/home/alice/.ssh` and `alice`.
