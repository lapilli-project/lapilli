#!/bin/sh
# Runs as root in the lab's server container: the five sshd setups of
# ../setup.sh, listening on every address. sshd's own logs go to stdout.
set -eu
mkdir -p /etc/lab /run/sshd /var/log/lab
cp /lab/hostkeys/ed25519 /lab/hostkeys/rsa /etc/lab/
chmod 600 /etc/lab/ed25519 /etc/lab/rsa
cp /lab/authorized_keys /etc/lab/authorized_keys
chmod 644 /etc/lab/authorized_keys

base() {
  cat <<EOF
Port $1
HostKey /etc/lab/$2
PidFile /run/sshd-$1.pid
AuthorizedKeysFile /etc/lab/authorized_keys
StrictModes no
UsePAM no
PasswordAuthentication yes
KbdInteractiveAuthentication no
LogLevel VERBOSE
PerSourcePenalties no
EOF
}
{ base 2322 ed25519; } > /etc/lab/2322.conf
{ base 2323 ed25519; echo "MaxAuthTries 3"; } > /etc/lab/2323.conf
{ base 2324 ed25519; echo "PubkeyAuthentication no"; } > /etc/lab/2324.conf
{ base 2325 ed25519; echo "PubkeyAcceptedAlgorithms ssh-rsa"; } > /etc/lab/2325.conf
{ base 2326 rsa; echo "HostKeyAlgorithms ssh-rsa"; } > /etc/lab/2326.conf

for p in 2322 2323 2324 2325 2326; do
  /usr/sbin/sshd -f /etc/lab/$p.conf -E /var/log/lab/$p.log
done
echo "lab sshd up: $(/usr/sbin/sshd -V 2>&1 | head -1)"
exec tail -F /var/log/lab/2322.log /var/log/lab/2323.log /var/log/lab/2324.log /var/log/lab/2325.log /var/log/lab/2326.log
