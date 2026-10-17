# A distribution with its own OpenSSH client (and sshd, for the server) and a
# user alice. run.sh builds one per image: --build-arg IMAGE=ubuntu:22.04.
ARG IMAGE=debian:trixie
FROM ${IMAGE}
ARG SERVER=
RUN set -e; \
  if command -v apt-get >/dev/null; then \
    apt-get update -q; \
    DEBIAN_FRONTEND=noninteractive apt-get install -yq --no-install-recommends openssh-client ${SERVER:+openssh-server}; \
    rm -rf /var/lib/apt/lists/*; \
  elif command -v apk >/dev/null; then \
    apk add --no-cache openssh-client openssh-keygen ${SERVER:+openssh-server}; \
  elif command -v dnf >/dev/null; then \
    dnf install -yq openssh-clients shadow-utils ${SERVER:+openssh-server}; dnf clean all; \
  else \
    yum install -yq openssh-clients shadow-utils ${SERVER:+openssh-server}; yum clean all; \
  fi; \
  if command -v useradd >/dev/null; then useradd -m -s /bin/sh alice; else adduser -D -s /bin/sh alice; fi; \
  sed -i 's/^alice:!*:/alice:*:/' /etc/shadow
