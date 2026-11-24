# shellcheck shell=bash
# Other images than the pinned ones for the kind-based end-to-end tests
# (run.sh, service.sh), whose images are in Kubernetes manifests, not in
# shell variables: PROMETHEUS_IMAGE and GRAFANA_IMAGE, when set and not
# empty, replace the manifest's Prometheus and Grafana image lines. The
# cluster pulls the image, so a tag is enough.

# images_check: an error, and a non-zero status, for a value that could
# break out of the YAML scalar. Call it on its own line, before a pipe.
images_check() {
  local v
  for v in PROMETHEUS_IMAGE GRAFANA_IMAGE; do
    [ -n "${!v:-}" ] || continue
    if ! [[ ${!v} =~ ^[A-Za-z0-9][A-Za-z0-9._/:@-]*$ ]]; then
      echo "$v is not an image reference: '${!v}'" >&2
      return 1
    fi
  done
}

# manifest FILE: FILE with the images given, one line to stderr for each
# one replaced; FILE as it is when none is given.
manifest() {
  images_check || return 1
  awk '
    function put(name, pat) {
      if (ENVIRON[name] == "" || $0 !~ pat) return 0
      match($0, /^[ \t]*image:[ \t]+/)
      print substr($0, 1, RLENGTH) ENVIRON[name]
      print name "=" ENVIRON[name] > "/dev/stderr"
      return 1
    }
    put("PROMETHEUS_IMAGE", "^[ \t]*image:[ \t]+prom/prometheus[:@]") { next }
    put("GRAFANA_IMAGE", "^[ \t]*image:[ \t]+grafana/grafana[:@]") { next }
    { print }
  ' "$1"
}
