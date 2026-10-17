#!/bin/bash
# Throwaway keys for the lab, made once: host keys, client keys, and the
# authorized_keys the lab servers read. Used by setup.sh and linux/run.sh.
set -euo pipefail
H="$(cd "$(dirname "$0")" && pwd)"
cd "$H"

if [ ! -f keys/good ]; then
mkdir -p keys hostkeys
ssh-keygen -q -t ed25519 -N '' -C lab-host -f hostkeys/ed25519
ssh-keygen -q -t rsa -b 3072 -N '' -C lab-host-rsa -f hostkeys/rsa
ssh-keygen -q -t ed25519 -N '' -C lab-host-other -f hostkeys/other

ssh-keygen -q -t ed25519 -N '' -C good -f keys/good
ssh-keygen -q -t ed25519 -N '' -C other -f keys/other
ssh-keygen -q -t ed25519 -N 'pw' -C enc -f keys/enc
ssh-keygen -q -t ed25519 -N 'pw' -C enc2 -f keys/enc2
ssh-keygen -q -t rsa -b 3072 -N '' -C rsa -f keys/rsa
ssh-keygen -q -t ecdsa -b 256 -N '' -C ecdsa -f keys/ecdsa
for i in 1 2 3 4 5 6; do ssh-keygen -q -t ed25519 -N '' -C "flood$i" -f "keys/flood$i"; done
# a private key with no .pub beside it
ssh-keygen -q -t ed25519 -N '' -C nopub -f keys/nopub && rm keys/nopub.pub
chmod 600 keys/* hostkeys/*
fi

cat keys/good.pub keys/enc.pub keys/rsa.pub keys/ecdsa.pub > authorized_keys
ssh-keygen -y -f keys/nopub >> authorized_keys
# a server that knows only "other", so one hop of a jump can refuse what the other takes
cp keys/other.pub authorized_keys_other
