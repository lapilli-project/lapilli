{{- define "lapilli.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/* Selector labels. `app.kubernetes.io/name: lapilli` is what `lapilli demo` looks for. */}}
{{- define "lapilli.selectorLabels" -}}
app.kubernetes.io/name: lapilli
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "lapilli.labels" -}}
{{ include "lapilli.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "lapilli.bundlePath" -}}/var/lib/lapilli/bundles{{- end -}}

{{/*
The image reference, for both containers. `image.digest` pins the exact bytes and wins: a tag is
mutable, so an install pinned only by tag records evidence produced by whatever
`ghcr.io/…:v0.1.0` resolves to today. With a digest the tag is not rendered at all — an image
reference carries one or the other, and printing both would suggest the tag was checked.
A private mirror has a different digest for the same image, so `digest: ""` stays valid and
`imagePullSecrets` is unaffected either way.
*/}}
{{- define "lapilli.image" -}}
{{- if .Values.image.digest -}}
{{ .Values.image.repository }}@{{ .Values.image.digest }}
{{- else -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}
{{- end -}}

{{/*
The health-port (8081) ingress rule for any NetworkPolicy that selects the controller pod:
`/healthz` for the kubelet's probes and `/metrics` for whatever scrapes it. An Ingress policy
denies every port it does not name, so this rule has to be present in each of them.
`webhook.networkPolicy.healthFrom` empty renders a rule with no `from`, i.e. from anywhere.
*/}}
{{- define "lapilli.npHealthRule" -}}
- ports: [{ port: health, protocol: TCP }]
{{- with .Values.webhook.networkPolicy.healthFrom }}
  from: {{- toYaml . | nindent 4 }}
{{- end }}
{{- end -}}

{{/*
The mcp-port ingress rule. The peer list is required, because the mcp container serves bundle
contents — incident evidence — to anything that can reach the port, and an empty list here would
silently mean "from anywhere".
*/}}
{{- define "lapilli.npMcpRule" -}}
{{- if not .Values.mcp.networkPolicy.from }}
{{- fail "mcp.networkPolicy.from must list the peers allowed to reach the mcp port: an ingress NetworkPolicy on this pod denies every port it does not name, so with mcp.enabled the mcp port needs a rule, and an empty list would render one that allows every pod in the cluster" }}
{{- end -}}
- ports: [{ port: mcp, protocol: TCP }]
  from: {{- toYaml .Values.mcp.networkPolicy.from | nindent 4 }}
{{- end -}}

{{/* The Secret holding the webhook token. */}}
{{- define "lapilli.webhookTokenSecret" -}}
{{- .Values.webhook.auth.existingSecret | default (printf "%s-webhook-token" (include "lapilli.fullname" .)) -}}
{{- end -}}

{{- define "lapilli.mcpTokenSecret" -}}
{{- .Values.mcp.existingSecret | default (printf "%s-mcp-token" (include "lapilli.fullname" .)) -}}
{{- end -}}

{{/*
The notification routes as the controller reads them: `pathSecret` is dropped (it names the
Secret this chart mounts, not something the controller looks up), and every other field is
passed through so an unknown key fails the schema rather than being silently ignored.
*/}}
{{- define "lapilli.notifyRoutes" -}}
{{- $out := list -}}
{{- range .Values.notify.routes -}}
{{- $route := omit . "pathSecret" -}}
{{- $out = append $out $route -}}
{{- end -}}
{{- toJson $out -}}
{{- end -}}

{{/*
persistence.size in bytes. Helm has no unit parser, so the suffixes Kubernetes actually accepts for a
PVC are handled explicitly and anything else fails the render rather than silently becoming a wrong
retention ceiling.
*/}}
{{- define "lapilli.persistenceBytes" -}}
{{- $s := .Values.persistence.size | toString -}}
{{- if hasSuffix "Gi" $s -}}{{ mul (trimSuffix "Gi" $s | int) 1073741824 }}
{{- else if hasSuffix "Mi" $s -}}{{ mul (trimSuffix "Mi" $s | int) 1048576 }}
{{- else if hasSuffix "Ti" $s -}}{{ mul (trimSuffix "Ti" $s | int) 1099511627776 }}
{{- else if hasSuffix "G" $s -}}{{ mul (trimSuffix "G" $s | int) 1000000000 }}
{{- else if hasSuffix "M" $s -}}{{ mul (trimSuffix "M" $s | int) 1000000 }}
{{- else if regexMatch "^[0-9]+$" $s -}}{{ $s }}
{{- else -}}{{ fail (printf "persistence.size %q: Lapilli derives retention.maxBytes from it and understands only Ti/Gi/Mi/G/M or plain bytes; set retention.maxBytes explicitly" $s) }}
{{- end -}}
{{- end -}}
