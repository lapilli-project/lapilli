{{- define "kairn.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/* Selector labels. `app.kubernetes.io/name: kairn` is what `kairn demo` looks for. */}}
{{- define "kairn.selectorLabels" -}}
app.kubernetes.io/name: kairn
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "kairn.labels" -}}
{{ include "kairn.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "kairn.bundlePath" -}}/var/lib/kairn/bundles{{- end -}}

{{/* The Secret holding the webhook token. */}}
{{- define "kairn.webhookTokenSecret" -}}
{{- .Values.webhook.auth.existingSecret | default (printf "%s-webhook-token" (include "kairn.fullname" .)) -}}
{{- end -}}
