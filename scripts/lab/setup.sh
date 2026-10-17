#!/bin/bash
# Local sshd lab: real OpenSSH debug logs for each failure, using throwaway keys only.
# sshd runs as this user, bound to 127.0.0.1, ports 2322-2327.
set -euo pipefail
H="$(cd "$(dirname "$0")" && pwd)"
cd "$H"
mkdir -p run logs
./keys.sh

base() {
  cat <<EOF
ListenAddress 127.0.0.1
PidFile $H/run/sshd-$1.pid
AuthorizedKeysFile $H/authorized_keys
StrictModes no
UsePAM no
PasswordAuthentication yes
KbdInteractiveAuthentication no
LogLevel VERBOSE
PerSourcePenalties no
EOF
}
# 2322: ordinary server
{ base 2322; echo "HostKey $H/hostkeys/ed25519"; } > run/sshd-2322.conf
# 2323: MaxAuthTries 3
{ base 2323; echo "HostKey $H/hostkeys/ed25519"; echo "MaxAuthTries 3"; } > run/sshd-2323.conf
# 2324: no public key auth at all
{ base 2324; echo "HostKey $H/hostkeys/ed25519"; echo "PubkeyAuthentication no"; } > run/sshd-2324.conf
# 2325: an old server that only knows ssh-rsa (SHA-1) signatures
{ base 2325; echo "HostKey $H/hostkeys/ed25519"; echo "PubkeyAcceptedAlgorithms ssh-rsa"; } > run/sshd-2325.conf
# 2326: an old server whose only host key algorithm is ssh-rsa
{ base 2326; echo "HostKey $H/hostkeys/rsa"; echo "HostKeyAlgorithms ssh-rsa"; } > run/sshd-2326.conf

for p in 2322 2323 2324 2325 2326; do
  /usr/sbin/sshd -f "$H/run/sshd-$p.conf" -p "$p" -E "$H/run/sshd-$p.log"
done

# known_hosts that trusts the lab host key on every lab port
{
  for p in 2322 2323 2324 2325; do echo "[127.0.0.1]:$p $(cut -d' ' -f1,2 hostkeys/ed25519.pub)"; done
  echo "[127.0.0.1]:2326 $(cut -d' ' -f1,2 hostkeys/rsa.pub)"
} > run/known_hosts
echo "[127.0.0.1]:2322 $(cut -d' ' -f1,2 hostkeys/other.pub)" > run/known_hosts_changed
: > run/known_hosts_empty
echo "lab up: $(cat run/sshd-*.pid | tr '\n' ' ')"
