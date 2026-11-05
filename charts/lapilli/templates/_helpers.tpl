{{- define "lapilli.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 52 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 52 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "lapilli.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "lapilli.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- .Values.serviceAccount.name | default (include "lapilli.fullname" .) -}}
{{- else -}}
{{- .Values.serviceAccount.name | default "default" -}}
{{- end -}}
{{- end -}}

{{/* The checks both modes make of the values. */}}
{{- define "lapilli.checks" -}}
{{- $p := .Values.prometheus -}}
{{- if not (has .Values.mode (list "cronjob" "serve")) }}
{{- fail "mode: cronjob or serve" }}
{{- end }}
{{- if eq (empty $p.url) (empty .Values.gcpProject) }}
{{- fail "give one of prometheus.url and gcpProject" }}
{{- end }}
{{- if and $p.auth (not (has $p.auth (list "aws" "azure" "google"))) }}
{{- fail "prometheus.auth: aws, azure or google" }}
{{- end }}
{{- if and .Values.gcpProject $p.auth (ne $p.auth "google") }}
{{- fail "gcpProject is Google Cloud's: leave prometheus.auth empty or google" }}
{{- end }}
{{- if and $p.auth $p.tokenSecret.name }}
{{- fail "prometheus.auth and prometheus.tokenSecret don't go together" }}
{{- end }}
{{- $r := .Values.rules -}}
{{- $from := 0 }}
{{- if $r.configMaps }}{{ $from = add1 $from }}{{ end }}
{{- if $r.loaded }}{{ $from = add1 $from }}{{ end }}
{{- if $r.grafana.url }}{{ $from = add1 $from }}{{ end }}
{{- if ne (int $from) 1 }}
{{- fail "rules: give one of rules.configMaps, rules.loaded and rules.grafana.url" }}
{{- end }}
{{- if and (eq .Values.mode "serve") (gt (len $r.configMaps) 1) }}
{{- fail "rules.configMaps: serve reads one; put the rules in one ConfigMap" }}
{{- end }}
{{- if and (eq .Values.mode "cronjob") (not .Values.incidents) (not .Values.impact) }}
{{- fail "give incidents, impact, or both" }}
{{- end }}
{{- $s := .Values.serve -}}
{{- if and (eq .Values.mode "serve") (or $s.roles.editors $s.roles.admins $s.roles.groupsHeader) (not $s.userHeader) }}
{{- fail "serve.roles needs serve.userHeader: who is who comes from the proxy that signs people in" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $s.userHeader (not $s.networkPolicy.enabled) (not $s.networkPolicy.othersKeptOut) }}
{{- fail "serve.userHeader trusts a header any pod in the cluster could set: enable serve.networkPolicy, with a network plugin that enforces it, so only the sign-in proxy reaches lapilli; or, if something else keeps the rest out (a service mesh's policy), say so with serve.networkPolicy.othersKeptOut" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $s.notify.slack (not .Values.report.slackWebhookSecret.name) }}
{{- fail "serve.notify.slack needs report.slackWebhookSecret" }}
{{- end }}
{{- if not (has ($s.notify.lang | default "en") (list "en" "ko")) }}
{{- fail "serve.notify.lang: en or ko" }}
{{- end }}
{{- end -}}

{{/* The flags lapilli verdict and lapilli serve share: where the history,
the rules and the incidents are, and how to judge. */}}
{{- define "lapilli.args" -}}
{{- $p := .Values.prometheus -}}
{{- $r := .Values.rules -}}
{{- with .Values.report.timeZone | default .Values.timeZone }}
- {{ printf "--tz=%s" . | quote }}
{{- end }}
{{- if $p.url }}
- {{ printf "--prometheus=%s" $p.url | quote }}
{{- with $p.auth }}
- {{ printf "--auth=%s" . | quote }}
{{- end }}
{{- with $p.awsRegion }}
- {{ printf "--aws-region=%s" . | quote }}
{{- end }}
{{- if $p.tokenSecret.name }}
- {{ printf "--bearer-token-file=/var/run/lapilli/token/%s" $p.tokenSecret.key | quote }}
{{- end }}
{{- else }}
- {{ printf "--gcp-project=%s" .Values.gcpProject | quote }}
{{- end }}
{{- range $i, $_ := $r.configMaps }}
- {{ printf "--rules=/etc/lapilli/rules/%d" $i | quote }}
{{- end }}
{{- if $r.loaded }}
- --loaded-rules
{{- end }}
{{- with $r.grafana.url }}
- {{ printf "--grafana=%s" . | quote }}
{{- end }}
{{- if and $r.grafana.url $r.grafana.tokenSecret.name }}
- {{ printf "--grafana-token-file=/var/run/lapilli/grafana/%s" ($r.grafana.tokenSecret.key | default "token") | quote }}
{{- end }}
{{- if .Values.incidents }}
- --incidents=/etc/lapilli/incidents/incidents.yaml
{{- end }}
{{- with .Values.impact }}
- {{ printf "--impact=%s" . | quote }}
{{- end }}
{{- range .Values.match }}
- {{ printf "--match=%s" . | quote }}
{{- end }}
{{- if .Values.report.slackWebhookSecret.name }}
- {{ printf "--slack-webhook-file=/var/run/lapilli/slack/%s" .Values.report.slackWebhookSecret.key | quote }}
{{- end }}
{{- end -}}

{{- define "lapilli.mounts" -}}
{{- $p := .Values.prometheus -}}
{{- $r := .Values.rules -}}
{{- range $i, $_ := $r.configMaps }}
- name: rules-{{ $i }}
  mountPath: /etc/lapilli/rules/{{ $i }}
  readOnly: true
{{- end }}
{{- if .Values.incidents }}
- name: incidents
  mountPath: /etc/lapilli/incidents
  readOnly: true
{{- end }}
{{- if $p.tokenSecret.name }}
- name: token
  mountPath: /var/run/lapilli/token
  readOnly: true
{{- end }}
{{- if and $r.grafana.url $r.grafana.tokenSecret.name }}
- name: grafana-token
  mountPath: /var/run/lapilli/grafana
  readOnly: true
{{- end }}
{{- if .Values.report.slackWebhookSecret.name }}
- name: slack
  mountPath: /var/run/lapilli/slack
  readOnly: true
{{- end }}
{{- end -}}

{{- define "lapilli.volumes" -}}
{{- $p := .Values.prometheus -}}
{{- $r := .Values.rules -}}
{{- range $i, $name := $r.configMaps }}
- name: rules-{{ $i }}
  configMap:
    name: {{ $name }}
{{- end }}
{{- if .Values.incidents }}
- name: incidents
  configMap:
    name: {{ include "lapilli.fullname" . }}-incidents
{{- end }}
{{- if $p.tokenSecret.name }}
- name: token
  secret:
    secretName: {{ $p.tokenSecret.name }}
{{- end }}
{{- if and $r.grafana.url $r.grafana.tokenSecret.name }}
- name: grafana-token
  secret:
    secretName: {{ $r.grafana.tokenSecret.name }}
{{- end }}
{{- if .Values.report.slackWebhookSecret.name }}
- name: slack
  secret:
    secretName: {{ .Values.report.slackWebhookSecret.name }}
{{- end }}
{{- end -}}

{{/* The pod's securityContext, the container's, and where it runs. */}}
{{- define "lapilli.podSecurity" -}}
runAsNonRoot: true
runAsUser: 65532
runAsGroup: 65532
fsGroup: 65532
seccompProfile:
  type: RuntimeDefault
{{- end -}}

{{- define "lapilli.containerSecurity" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities:
  drop: [ALL]
{{- end -}}

{{- define "lapilli.placement" -}}
{{- with .Values.nodeSelector }}
nodeSelector:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.tolerations }}
tolerations:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.affinity }}
affinity:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- end -}}

{{- define "lapilli.podMeta" -}}
labels:
  {{- include "lapilli.selector" . | nindent 2 }}
  {{- include "lapilli.labels" . | nindent 2 }}
  {{- range $k, $v := .Values.podLabels }}
  {{ $k }}: {{ $v | toString | quote }}
  {{- end }}
{{- with .Values.podAnnotations }}
annotations:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- end -}}

{{- define "lapilli.selector" -}}
app.kubernetes.io/component: {{ .Values.mode }}
{{- end -}}
