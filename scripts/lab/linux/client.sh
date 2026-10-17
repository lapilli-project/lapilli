#!/bin/sh
# Runs as alice in a client container: each lab failure, set up in a fresh
# ~/.ssh, then
#   NAME.log        the distribution's "ssh -vvv" output
#   NAME.log.json   lapilli ssh --log NAME.log --json
#   NAME.live.json  lapilli ssh --json, run against the server itself
# into /out/DIR. NAME.wrote says a run changed ~/.ssh/known_hosts.
set -u
OUT="/out/$1"
LAPILLI=/out/lapilli
mkdir -p "$OUT"
rm -f "$OUT"/*
# This container never sees the agent of the machine it runs on; check anyway.
if [ -n "${SSH_AUTH_SOCK:-}" ]; then echo "an agent socket is set; stopping" >&2; exit 1; fi
ssh -V 2> "$OUT/VERSION"

hk="$(cut -d' ' -f1,2 /lab/hostkeys/ed25519.pub)"
hkrsa="$(cut -d' ' -f1,2 /lab/hostkeys/rsa.pub)"
hkother="$(cut -d' ' -f1,2 /lab/hostkeys/other.pub)"

# fresh: an empty ~/.ssh whose known_hosts trusts the lab server, and no agent
fresh() {
  stop_agent
  rm -rf "$HOME/.ssh"
  mkdir -m 700 "$HOME/.ssh"
  for p in 2322 2323 2324 2325; do echo "[lab]:$p $hk"; done > "$HOME/.ssh/known_hosts"
  echo "[lab]:2326 $hkrsa" >> "$HOME/.ssh/known_hosts"
}
# key NAME [AS]: a lab key (and its .pub, if it has one) in ~/.ssh
key() {
  cp "/lab/keys/$1" "$HOME/.ssh/${2:-$1}"
  chmod 600 "$HOME/.ssh/${2:-$1}"
  if [ -f "/lab/keys/$1.pub" ]; then cp "/lab/keys/$1.pub" "$HOME/.ssh/${2:-$1}.pub"; fi
}
# agent [ssh-add args per key ...]: an agent of this container's own
agent() {
  eval "$(ssh-agent -s)" > /dev/null
  for k in "$@"; do ssh-add $k < /dev/null > /dev/null 2>&1; done # no -q before 7.5
}
stop_agent() {
  if [ -n "${SSH_AGENT_PID:-}" ]; then kill "$SSH_AGENT_PID" 2> /dev/null; fi
  unset SSH_AUTH_SOCK SSH_AGENT_PID
}

# run NAME SSH_ARGS... DEST
run() {
  name=$1; shift
  cp "$HOME/.ssh/known_hosts" /tmp/kh 2> /dev/null || : > /tmp/kh
  timeout 40 "$LAPILLI" ssh --json "$@" > "$OUT/$name.live.json" 2> "$OUT/$name.live.err" < /dev/null
  echo $? > "$OUT/$name.live.exit"
  # cksum, not cmp or diff: some images have no diffutils
  if [ "$(cksum < /tmp/kh)" != "$(cksum < "$HOME/.ssh/known_hosts" 2> /dev/null)" ]; then
    cp "$HOME/.ssh/known_hosts" "$OUT/$name.wrote" 2> /dev/null || echo removed > "$OUT/$name.wrote"
  fi
  cp /tmp/kh "$HOME/.ssh/known_hosts"
  timeout 6 ssh -vvv -o BatchMode=yes -o ConnectTimeout=4 -N "$@" > /dev/null 2> "$OUT/$name.log" < /dev/null
  echo $? > "$OUT/$name.exit"
  "$LAPILLI" ssh --log "$OUT/$name.log" --json > "$OUT/$name.log.json" 2> "$OUT/$name.log.err"
}

S="$HOME/.ssh"
fresh; key good id_ed25519;                  run ok_file -p 2322 alice@lab
fresh; key other id_ed25519;                 run rejected -p 2322 alice@lab
fresh; key other; key good
printf 'IdentityFile ~/.ssh/other\nIdentityFile ~/.ssh/good\n' > "$S/config"
                                             run rejected_then_ok -p 2322 alice@lab
fresh;                                       run no_keys -p 2322 alice@lab
fresh; key good work;                        run key_not_default -p 2322 alice@lab
fresh; key enc id_ed25519;                   run enc_accepted -p 2322 alice@lab
fresh; key enc2 id_ed25519;                  run enc_rejected -p 2322 alice@lab
fresh; key nopub id_ed25519;                 run nopub_ok -p 2322 alice@lab
fresh; key good id_ed25519; chmod 644 "$S/id_ed25519"
                                             run bad_perms_good -p 2322 alice@lab
fresh; key other id_ed25519; chmod 644 "$S/id_ed25519"
                                             run bad_perms -p 2322 alice@lab
fresh; key good id_ed25519;                  run pubkey_disabled -p 2324 alice@lab
fresh; key rsa id_rsa;                       run rsa_sha1_only -p 2325 alice@lab
fresh; key good id_ed25519;                  run hostkey_ssh_rsa -p 2326 alice@lab
fresh; key good id_ed25519; echo "[lab]:2322 $hkother" > "$S/known_hosts"
                                             run hostkey_changed -p 2322 alice@lab
fresh; key good id_ed25519; : > "$S/known_hosts"
                                             run hostkey_unknown -p 2322 alice@lab
fresh; key good id_ed25519;                  run conn_refused -p 2329 alice@lab
fresh; key good id_ed25519;                  run dns_fail alice@nosuch.invalid
fresh; key good id_ed25519
agent /lab/keys/flood1 /lab/keys/flood2 /lab/keys/flood3 /lab/keys/flood4 /lab/keys/flood5 /lab/keys/flood6
                                             run too_many -p 2323 alice@lab
fresh; key good
agent /lab/keys/flood1 /lab/keys/flood2 /lab/keys/flood3 /lab/keys/flood4 /lab/keys/flood5 /lab/keys/flood6
printf 'IdentityFile ~/.ssh/good\nIdentitiesOnly yes\n' > "$S/config"
                                             run agent_ok_identitiesonly -p 2323 alice@lab
fresh; agent "-c /lab/keys/good";            run agent_refused -p 2322 alice@lab
fresh; key enc id_ed25519; agent;            run agent_empty_enc -p 2322 alice@lab
fresh; key good id_ed25519;                  run wrong_user -p 2322 nosuchuser@lab
# a config ssh won't use: written on a Mac, a bad value, writable by others
fresh; key good id_ed25519; printf 'Host *\n  AddKeysToAgent yes\n  UseKeychain yes\n' > "$S/config"
                                             run config_unknown_option -p 2322 alice@lab
fresh; key good id_ed25519; printf 'Host lab\n  Port twentytwo\n' > "$S/config"
                                             run config_bad_value alice@lab
fresh; key good id_ed25519; printf 'Host lab\n  Port 2322\n' > "$S/config"; chmod 666 "$S/config"
                                             run config_perms alice@lab
stop_agent
echo "$1: $(cat "$OUT/VERSION")"
