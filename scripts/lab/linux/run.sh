#!/bin/bash
# run.sh [IMAGE ...]: the lab failures, seen by the OpenSSH client of each Linux
# distribution. One container runs a current sshd with the lab's setups; each
# distribution's container runs client.sh against it, with its own ssh and a
# lapilli built for Linux. Results go to out/<image>/; a table is printed.
#
# Containers get no access to this machine's ~/.ssh or ssh-agent.
set -euo pipefail
H="$(cd "$(dirname "$0")" && pwd)"
LAB="$(dirname "$H")"
ROOT="$(cd "$LAB/../.." && pwd)"

IMAGES=(amazonlinux:2 rockylinux/rockylinux:8 ubuntu:20.04 rockylinux/rockylinux:9
  ubuntu:22.04 debian:12 ubuntu:24.04 alpine:3.22 fedora:42)
if [ $# -gt 0 ]; then IMAGES=("$@"); fi

"$LAB/keys.sh"
mkdir -p "$H/out"
arch="$(docker version -f '{{.Server.Arch}}')"
(cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o "$H/out/lapilli" .)

docker network inspect lapilli-lab > /dev/null 2>&1 || docker network create lapilli-lab > /dev/null
docker build -q -t lapilli-lab:server --build-arg IMAGE=debian:trixie --build-arg SERVER=1 "$H" > /dev/null
docker rm -f lapilli-lab-sshd > /dev/null 2>&1 || true
docker run -d --name lapilli-lab-sshd --network lapilli-lab --network-alias lab --network-alias jump \
  -v "$LAB:/lab:ro" lapilli-lab:server sh /lab/linux/server.sh > /dev/null
trap 'docker rm -f lapilli-lab-sshd > /dev/null 2>&1' EXIT
for _ in $(seq 20); do docker logs lapilli-lab-sshd 2>&1 | grep -q 'lab sshd up' && break; sleep 0.5; done
docker logs lapilli-lab-sshd 2>&1 | head -1

for img in "${IMAGES[@]}"; do
  tag="lapilli-lab:$(echo "$img" | tr '/:' '--')"
  dir="$(echo "$img" | sed 's#.*/##; s/:/-/')"
  docker build -q -t "$tag" --build-arg IMAGE="$img" "$H" > /dev/null
  docker run --rm --network lapilli-lab --user alice -e ONLY="${ONLY:-*}" -v "$LAB:/lab:ro" -v "$H/out:/out" \
    "$tag" sh /lab/linux/client.sh "$dir"
done

"$H/table.sh" "${IMAGES[@]}"
