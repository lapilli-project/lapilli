{{/* The rules history (serve.rulesHistory, docs/design/history.md): the
containers that clone the repository and write the versions of the rules
before lapilli serve starts, and, following the branch (no
rules.configMaps), those that fetch it and add new versions beside it.

The clone, with the token, is in a volume of its own (rules-history-repo),
which only the containers that fetch write: those that run render and
render.prepare on what the repository holds have it read-only, and have no
token, so nothing they run can change what the next fetch runs with. */}}

{{/* Whether the history is followed: "true", or nothing. */}}
{{- define "lapilli.historyFollows" -}}
{{- if and (eq .Values.mode "serve") .Values.serve.rulesHistory.repository (not .Values.rules.configMaps) }}true{{ end -}}
{{- end -}}

{{/* The names of the containers that run git or render. */}}
{{- define "lapilli.historyContainers" -}}
rules-history-clone rules-history{{ if include "lapilli.historyFollows" . }} rules-history-fetch rules-history-follow{{ end }}
{{- end -}}

{{/* The image that writes the versions: one with helm and kustomize for
render, else the one with git. */}}
{{- define "lapilli.historyImage" -}}
{{- $h := .Values.serve.rulesHistory -}}
{{- if $h.render }}{{ $h.renderImage }}{{ else }}{{ $h.image }}{{ end -}}
{{- end -}}

{{/* since, in the shell: serve.rulesHistory.since, or the backfill and a
day more before now. */}}
{{- define "lapilli.historySince" -}}
since=${SINCE:-}
if [ -z "$since" ]; then
  case $BACKFILL in
    *d) s=$(( ${BACKFILL%d} * 86400 )) ;;
    *h) s=$(( ${BACKFILL%h} * 3600 )) ;;
    *) echo "serve.rulesHistory.since: give it, as serve.backfill $BACKFILL isn't in days or hours" >&2; exit 1 ;;
  esac
  since=$(date -u -d "@$(( $(date +%s) - s - 86400 ))" +%Y-%m-%dT%H:%M:%SZ)
fi
{{- end -}}

{{/* The script that clones the repository, or with EVERY fetches it that
often, with the token. */}}
{{- define "lapilli.historyFetch" -}}
set -eu
mkdir -p "$HOME"
if [ -n "${TOKEN_FILE:-}" ]; then
  # The token goes to the repository's host alone, whatever
  # else asks: a redirect, a submodule.
  host=$(printf '%s' "$REPOSITORY" | sed -n 's#^https://\([^/@]*\)/.*#\1#p')
  if [ -n "$host" ]; then
    # shellcheck disable=SC2016 # git runs the helper, which reads the file
    git config --global "credential.https://$host.helper" '!f() { echo username=x-access-token; printf "password=%s\n" "$(cat "$TOKEN_FILE")"; }; f'
  else
    echo "serve.rulesHistory.tokenSecret: the repository isn't https://HOST/..., so the token isn't given" >&2
  fi
fi
# Every object, so the versions are written without the
# token; following, the branch's new commits as they come.
fetch() {
  if [ -e /repo/cloned ]; then
    b=${BRANCH:-$(git -C /repo/clone symbolic-ref --short HEAD)}
    git -C /repo/clone fetch --quiet --update-head-ok origin "+refs/heads/$b:refs/heads/$b"
    return
  fi
  rm -rf /repo/clone
  set -- clone --quiet --no-checkout --single-branch
  [ -z "$BRANCH" ] || set -- "$@" --branch "$BRANCH"
  git "$@" -- "$REPOSITORY" /repo/clone && touch /repo/cloned
}
if [ "$EVERY" -eq 0 ]; then
  # A history that can't be fetched leaves nothing: serve
  # starts with today's rules, and the pod isn't held up by git.
  rm -rf /repo/clone /repo/cloned
  fetch || echo "couldn't clone the repository: serve starts without the history" >&2
  rm -rf "$HOME"
  exit 0
fi
while :; do
  sleep "$EVERY"
  fetch || echo "couldn't fetch the repository: trying again in ${EVERY}s" >&2
done
{{- end -}}

{{/* The environment of the containers that write versions. */}}
{{- define "lapilli.historyEnv" -}}
{{- $h := .Values.serve.rulesHistory -}}
- name: PATHS
  value: {{ required "serve.rulesHistory.paths: the rule files or directories in the repository" $h.paths | quote }}
- name: SINCE
  value: {{ $h.since | quote }}
- name: RENDER
  value: {{ $h.render | quote }}
- name: RENDER_PREPARE
  value: {{ $h.renderPrepare | quote }}
- name: UNRENDERED
  value: {{ $h.unrendered | default "fail" | quote }}
- name: MAX
  value: {{ $h.max | default 500 | int | quote }}
- name: BACKFILL
  value: {{ .Values.serve.backfill | quote }}
- name: HOME
  value: /work/home
- name: TMPDIR
  value: /work/tmp
{{- end -}}

{{- define "lapilli.historyFetchMounts" -}}
- name: rules-history-repo
  mountPath: /repo
{{- if .Values.serve.rulesHistory.tokenSecret.name }}
- name: rules-history-token
  mountPath: /var/run/lapilli/git
  readOnly: true
{{- end }}
{{- end -}}

{{- define "lapilli.historyWriteMounts" -}}
- name: rules-history
  mountPath: /var/lib/lapilli-history
- name: rules-history-repo
  mountPath: /repo
  readOnly: true
- name: rules-history-work
  mountPath: /work
- name: rules-history-script
  mountPath: /scripts
  readOnly: true
{{- end -}}

{{- define "lapilli.historyFetchEnv" -}}
{{- $h := .Values.serve.rulesHistory -}}
- name: REPOSITORY
  value: {{ $h.repository | quote }}
- name: BRANCH
  value: {{ $h.branch | quote }}
{{- if $h.tokenSecret.name }}
- name: TOKEN_FILE
  value: {{ printf "/var/run/lapilli/git/%s" $h.tokenSecret.key | quote }}
{{- end }}
{{- end -}}


{{/* The init containers. */}}
{{- define "lapilli.historyInit" -}}
{{- $h := .Values.serve.rulesHistory -}}
# Earlier versions of the rules, from git, for --rules-history: the
# clone with the token, then the versions written without it, so a
# chart's render commands never see it.
- name: rules-history-clone
  image: {{ $h.image | quote }}
  imagePullPolicy: IfNotPresent
  command: [sh, -c]
  args:
    - |
      {{- include "lapilli.historyFetch" . | nindent 6 }}
  env:
    {{- include "lapilli.historyFetchEnv" . | nindent 4 }}
    - name: HOME
      value: /repo/clone-home
    - name: EVERY
      value: "0"
  securityContext:
    {{- include "lapilli.containerSecurity" . | nindent 4 }}
  {{- with $h.resources }}
  resources:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  volumeMounts:
    {{- include "lapilli.historyFetchMounts" . | nindent 4 }}
- name: rules-history
  image: {{ include "lapilli.historyImage" . | quote }}
  imagePullPolicy: IfNotPresent
  command: [sh, -c]
  args:
    - |
      set -eu
      {{- include "lapilli.historySince" . | nindent 6 }}
      mkdir -p "$TMPDIR" "$HOME"
      out=/var/lib/lapilli-history/versions
      # What a start cut short left, too.
      rm -rf "$out" "$out.new"
      mkdir -p "$out"
      if [ ! -e /repo/cloned ]; then
        exit 0
      fi
      if ! REPO=/repo/clone SINCE=$since OUT="$out.new" sh /scripts/git.sh; then
        echo "couldn't write the history: serve starts without it" >&2
        rm -rf "$out.new"
        exit 0
      fi
      rmdir "$out" && mv "$out.new" "$out"
  env:
    {{- include "lapilli.historyEnv" . | nindent 4 }}
  securityContext:
    {{- include "lapilli.containerSecurity" . | nindent 4 }}
  {{- with $h.resources }}
  resources:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  volumeMounts:
    {{- include "lapilli.historyWriteMounts" . | nindent 4 }}
{{- end -}}

{{/* Following the branch: one container fetches it, with the token, and
another adds the versions of its new commits, without it. */}}
{{- define "lapilli.historySidecars" -}}
{{- $h := .Values.serve.rulesHistory -}}
- name: rules-history-fetch
  image: {{ $h.image | quote }}
  imagePullPolicy: IfNotPresent
  command: [sh, -c]
  args:
    - |
      {{- include "lapilli.historyFetch" . | nindent 6 }}
  env:
    {{- include "lapilli.historyFetchEnv" . | nindent 4 }}
    - name: HOME
      value: /repo/fetch-home
    - name: EVERY
      value: {{ $h.every | default 300 | int | quote }}
  securityContext:
    {{- include "lapilli.containerSecurity" . | nindent 4 }}
  {{- with $h.resources }}
  resources:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  volumeMounts:
    {{- include "lapilli.historyFetchMounts" . | nindent 4 }}
- name: rules-history-follow
  image: {{ include "lapilli.historyImage" . | quote }}
  imagePullPolicy: IfNotPresent
  command: [sh, -c]
  args:
    - |
      set -eu
      {{- include "lapilli.historySince" . | nindent 6 }}
      mkdir -p "$TMPDIR" "$HOME"
      exec env REPO=/repo/clone SINCE="$since" HISTORY=/var/lib/lapilli-history/versions GIT_SH=/scripts/git.sh sh /scripts/follow.sh
  env:
    {{- include "lapilli.historyEnv" . | nindent 4 }}
    - name: EVERY
      value: {{ $h.every | default 300 | int | quote }}
  securityContext:
    {{- include "lapilli.containerSecurity" . | nindent 4 }}
  {{- with $h.resources }}
  resources:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  volumeMounts:
    {{- include "lapilli.historyWriteMounts" . | nindent 4 }}
{{- end -}}

{{- define "lapilli.historyVolumes" -}}
- name: rules-history
  emptyDir: {}
- name: rules-history-repo
  emptyDir: {}
- name: rules-history-work
  emptyDir: {}
- name: rules-history-script
  configMap:
    name: {{ include "lapilli.fullname" . }}-rules-history
{{- if .Values.serve.rulesHistory.tokenSecret.name }}
- name: rules-history-token
  secret:
    secretName: {{ .Values.serve.rulesHistory.tokenSecret.name }}
{{- end }}
{{- end -}}

{{/* What the values for the rules history may be. */}}
{{- define "lapilli.historyChecks" -}}
{{- $h := .Values.serve.rulesHistory -}}
{{- if $h.repository }}
{{- if ne .Values.mode "serve" }}
{{- fail "serve.rulesHistory is for mode: serve" }}
{{- end }}
{{- if not (has (toString ($h.unrendered | default "fail")) (list "fail" "skip")) }}
{{- fail "serve.rulesHistory.unrendered: fail or skip" }}
{{- end }}
{{- if not (regexMatch "^[1-9][0-9]*$" (toString ($h.max | default 500 | int))) }}
{{- fail "serve.rulesHistory.max: the most versions written, a number" }}
{{- end }}
{{- if lt (int ($h.every | default 300)) 30 }}
{{- fail "serve.rulesHistory.every: seconds between fetches, 30 at least" }}
{{- end }}
{{- if and $h.renderPrepare (not $h.render) }}
{{- fail "serve.rulesHistory.renderPrepare prepares each commit's files for render: give render too" }}
{{- end }}
{{- if and $h.render (not (regexMatch "^[^@\\s]+@sha256:[0-9a-f]{64}$" (toString $h.renderImage))) }}
{{- fail "serve.rulesHistory.renderImage: NAME@sha256:..., an image with git, helm and kustomize pinned by digest: a tag can be moved to one that does anything with what the repository holds" }}
{{- end }}
{{- else if or $h.select $h.render $h.renderPrepare }}
{{- fail "serve.rulesHistory.select, render and renderPrepare go with serve.rulesHistory.repository" }}
{{- end }}
{{- end -}}
