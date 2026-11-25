{{/* lapilli.bool sets in.key, given as true or false, or quoted so, to the
bool it says: a template's if reads the text "false" as true, so
linkedOnly: "false" was --linked-only, and serve.everyoneAdmin: "false"
everyone an admin. Anything else fails, named as name; missing is false. */}}
{{- define "lapilli.bool" -}}
{{- $v := index .in .key -}}
{{- if kindIs "string" $v -}}
{{- if has (lower $v) (list "true" "false") -}}
{{- $_ := set .in .key (eq (lower $v) "true") -}}
{{- else -}}
{{- fail (printf "%s: true or false, not %q" .name $v) -}}
{{- end -}}
{{- else if not (or (kindIs "bool" $v) (kindIs "invalid" $v)) -}}
{{- fail (printf "%s: true or false" .name) -}}
{{- end -}}
{{- end -}}

{{/* The values added since 0.28.1, filled in where they are missing, as
they are after an upgrade with --reuse-values (which keeps the old release's
values, not the new chart's): every template reads them through this. */}}
{{- define "lapilli.defaults" -}}
{{- $s := .Values.serve -}}
{{- include "lapilli.bool" (dict "in" .Values "key" "linkedOnly" "name" "linkedOnly") -}}
{{- include "lapilli.bool" (dict "in" .Values "key" "matchStrict" "name" "matchStrict") -}}
{{- /* A Loki's, its ruler's rules, and Grafana's history: 0.70.0's. */ -}}
{{- include "lapilli.fill" (dict "in" .Values "key" "loki" "with" (dict "url" "" "tenant" "")) -}}
{{- include "lapilli.fill" (dict "in" .Values.loki "key" "tokenSecret" "with" (dict "name" "" "key" "token")) -}}
{{- include "lapilli.fill" (dict "in" .Values "key" "rules" "with" (dict)) -}}
{{- include "lapilli.fill" (dict "in" .Values.rules "key" "loki" "with" (dict "loaded" false "configMaps" (list))) -}}
{{- include "lapilli.bool" (dict "in" .Values.rules.loki "key" "loaded" "name" "rules.loki.loaded") -}}
{{- include "lapilli.fill" (dict "in" .Values.rules "key" "grafana" "with" (dict "url" "")) -}}
{{- if not (hasKey .Values.rules.grafana "history") }}{{ $_ := set .Values.rules.grafana "history" true }}{{ end -}}
{{- include "lapilli.bool" (dict "in" .Values.rules.grafana "key" "history" "name" "rules.grafana.history") -}}
{{- include "lapilli.fill" (dict "in" $s "key" "changes" "with" (dict)) -}}
{{- include "lapilli.fill" (dict "in" $s.changes "key" "lokiTokenSecret" "with" (dict "name" "" "key" "token")) -}}
{{- include "lapilli.bool" (dict "in" $s "key" "everyoneAdmin" "name" "serve.everyoneAdmin") -}}
{{- include "lapilli.bool" (dict "in" $s "key" "exposeWithoutSignIn" "name" "serve.exposeWithoutSignIn") -}}
{{- include "lapilli.fill" (dict "in" $s "key" "service" "with" (dict "type" "ClusterIP" "port" 80 "annotations" (dict) "loadBalancerClass" "" "loadBalancerSourceRanges" (list) "externalTrafficPolicy" "" "nodePort" nil "sourceRangesAreTheProxy" false)) -}}
{{- include "lapilli.fill" (dict "in" $s "key" "ingress" "with" (dict "enabled" false "className" "" "annotations" (dict) "hosts" (list) "path" "/" "pathType" "Prefix" "tls" (list) "https" true "serviceAnnotations" (dict))) -}}
{{- include "lapilli.fill" (dict "in" $s "key" "httpRoute" "with" (dict "enabled" false "annotations" (dict) "parentRefs" (list) "hostnames" (list) "https" true)) -}}
{{- include "lapilli.fill" (dict "in" $s "key" "signIn" "with" (dict "userClaim" "email" "groupsClaim" "groups" "sessionMaxAge" "")) -}}
{{- include "lapilli.fill" (dict "in" $s.signIn "key" "oidc" "with" (dict "issuer" "" "clientId" "" "scopes" (list) "hostedDomains" (list) "trustedClients" (list) "bearer" true)) -}}
{{- include "lapilli.fill" (dict "in" $s.signIn.oidc "key" "clientSecret" "with" (dict "name" "" "key" "client-secret")) -}}
{{- include "lapilli.fill" (dict "in" $s.signIn "key" "proxyJWT" "with" (dict "kind" "" "audience" "" "albArn" "" "issuer" "" "jwks" "" "header" "")) -}}
{{- include "lapilli.fill" (dict "in" $s.signIn "key" "sessionKeySecret" "with" (dict "name" "" "key" "session-key" "create" true)) -}}
{{- include "lapilli.fill" (dict "in" $s "key" "roles" "with" (dict "viewers" (list) "editors" (list) "admins" (list) "groupsHeader" "")) -}}
{{- include "lapilli.fill" (dict "in" $s "key" "ledger" "with" (dict "persistence" true "size" "2Gi" "storageClass" "" "existingClaim" "")) -}}
{{- include "lapilli.fill" (dict "in" $s.ledger "key" "postgres" "with" (dict "to" (list) "port" 5432)) -}}
{{- include "lapilli.fill" (dict "in" $s.ledger.postgres "key" "secret" "with" (dict "name" "" "key" "url")) -}}
{{- if and $s.ledger.postgres.secret.name $s.ledger.existingClaim }}
{{- fail "serve.ledger.postgres and serve.ledger.existingClaim are two places to keep the ledger: give one" }}
{{- end }}
{{- if not (hasKey $s "exposeWithoutSignIn") }}{{ $_ := set $s "exposeWithoutSignIn" false }}{{ end -}}
{{- if not (hasKey $s "replicas") }}{{ $_ := set $s "replicas" 1 }}{{ end -}}
{{- if not (hasKey $s "shutdownDelaySeconds") }}{{ $_ := set $s "shutdownDelaySeconds" 5 }}{{ end -}}
{{- include "lapilli.fill" (dict "in" $s "key" "podDisruptionBudget" "with" (dict "maxUnavailable" 1)) -}}
{{- if not (hasKey $s "readinessPath") }}{{ $_ := set $s "readinessPath" "/readyz" }}{{ end -}}
{{- include "lapilli.fill" (dict "in" $s "key" "backtest" "with" (dict "enabled" false "port" 8084 "tokens" (list) "share" "" "queue" "" "timeout" "" "from" (list))) -}}
{{- include "lapilli.fill" (dict "in" $s.backtest "key" "trust" "with" (dict)) -}}
{{- include "lapilli.fill" (dict "in" $s.backtest.trust "key" "configMap" "with" (dict "name" "" "key" "trust.yaml")) -}}
{{- include "lapilli.fill" (dict "in" $s.backtest "key" "service" "with" (dict "type" "ClusterIP" "port" 8084 "annotations" (dict) "loadBalancerClass" "" "loadBalancerSourceRanges" (list) "externalTrafficPolicy" "" "nodePort" nil "loadBalancerEndsTLS" false)) -}}
{{- include "lapilli.fill" (dict "in" $s.backtest "key" "ingress" "with" (dict "enabled" false "className" "" "annotations" (dict) "hosts" (list) "tls" (list) "loadBalancerEndsTLS" false)) -}}
{{- end -}}

{{/* fill sets in[key] to the dict it holds, or a new one, with each key of
with it doesn't have. */}}
{{- define "lapilli.fill" -}}
{{- $m := index .in .key | default dict -}}
{{- range $k, $v := .with }}{{ if not (hasKey $m $k) }}{{ $_ := set $m $k $v }}{{ end }}{{ end -}}
{{- $_ := set .in .key $m -}}
{{- end -}}

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
{{- $s := .Values.serve -}}
{{- if not (has .Values.mode (list "cronjob" "serve")) }}
{{- fail "mode: cronjob or serve" }}
{{- end }}
{{- /* serve can run on firings copied in alone, with no rules or history. */ -}}
{{- $copied := and (eq .Values.mode "serve") $s.firings.tokenSecret.name }}
{{- $history := or $p.url .Values.gcpProject }}
{{- if and $p.url .Values.gcpProject }}
{{- fail "give one of prometheus.url and gcpProject, not both" }}
{{- end }}
{{- if and (not $history) (not $copied) }}
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
{{- if $r.ruler.url }}{{ $from = add1 $from }}{{ end }}
{{- /* Followed, the rules history is the rules. */}}
{{- if include "lapilli.historyFollows" . }}{{ $from = add1 $from }}{{ end }}
{{- /* A Loki ruler's rules (rules.loki, or, before 0.70.0, given through
     extraArgs: --loki URL --loki-loaded-rules) go beside the rest, or
     alone, as rules.alarms do. */}}
{{- $lk := .Values.loki }}
{{- $loki := or $r.loki.loaded $r.loki.configMaps }}
{{- $lokiSet := or $lk.url $lk.tenant $lk.tokenSecret.name $loki }}
{{- range .Values.extraArgs }}
{{- if or (hasPrefix "--loki-loaded-rules" (toString .)) (hasPrefix "--loki-rules" (toString .)) }}{{ $loki = true }}{{ end }}
{{- if and $lokiSet (hasPrefix "--loki" (toString .)) }}
{{- fail (printf "extraArgs: %s: give a Loki's flags as loki and rules.loki, not through extraArgs as well" (toString .)) }}
{{- end }}
{{- end }}
{{- if and $r.loki.loaded $r.loki.configMaps }}
{{- fail "rules.loki.loaded and rules.loki.configMaps are two ways to the same rules: give one" }}
{{- end }}
{{- if and (or $r.loki.loaded $r.loki.configMaps) (not $lk.url) }}
{{- fail "rules.loki is replayed against a Loki: give loki.url" }}
{{- end }}
{{- if and (or $lk.tenant $lk.tokenSecret.name) (not $lk.url) }}
{{- fail "loki.tenant and loki.tokenSecret go with loki.url" }}
{{- end }}
{{- /* A tenant's ID as lapilli takes one (--loki-tenant, --ruler-tenant):
     150 bytes at most, no |, /, \, line break, nor . or .. alone. The
     pattern is a raw string: in a quoted one, \ before the line break
     escapes it, and \ isn't matched. */}}
{{- $tenantBad := `[|/\\\r\n]` }}
{{- if and $lk.tenant (or (gt (len (toString $lk.tenant)) 150) (regexMatch $tenantBad (toString $lk.tenant)) (has (toString $lk.tenant) (list "." ".."))) }}
{{- fail (printf "loki.tenant %q: want a tenant's ID, as Loki takes one" (toString $lk.tenant)) }}
{{- end }}
{{- with $r.ruler.tenant }}
{{- if or (gt (len (toString .)) 150) (regexMatch $tenantBad (toString .)) (has (toString .) (list "." "..")) }}
{{- fail (printf "rules.ruler.tenant %q: want a tenant's ID, as Mimir and Cortex take one" (toString .)) }}
{{- end }}
{{- end }}
{{- with $p.tenant }}
{{- if or (gt (len (toString .)) 150) (regexMatch $tenantBad (toString .)) (has (toString .) (list "." "..")) }}
{{- fail (printf "prometheus.tenant %q: want a tenant's ID, as Mimir and Cortex take one" (toString .)) }}
{{- end }}
{{- if not $p.url }}
{{- fail "prometheus.tenant is the tenant of a Mimir or a Cortex at prometheus.url: it goes with prometheus.url" }}
{{- end }}
{{- end }}
{{- if and (not $r.grafana.history) (not $r.grafana.url) }}
{{- fail "rules.grafana.history judges the rules rules.grafana.url reads that can't be replayed: it goes with rules.grafana.url" }}
{{- end }}
{{- if or (gt (int $from) 1) (and (eq (int $from) 0) (not $copied) (not $r.alarms) (not $loki)) }}
{{- fail "rules: give one of rules.configMaps, rules.loaded, rules.grafana.url, rules.ruler.url and serve.rulesHistory (with no rules.configMaps, the rules are the repository's, followed), or rules.alarms, or a Loki's rules through extraArgs" }}
{{- end }}
{{- $sources := dict }}
{{- range $r.alarms }}
{{- if not (regexMatch "^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$" (toString .source)) }}
{{- fail (printf "rules.alarms: %q: a source is lowercase letters, digits and dashes, 40 at most, not starting or ending with a dash" (toString .source)) }}
{{- end }}
{{- if hasKey $sources (toString .source) }}
{{- fail (printf "rules.alarms: the source %s is named twice" .source) }}
{{- end }}
{{- if not (include "lapilli.alarmsKind" (toString .url)) }}
{{- fail (printf "rules.alarms: %s: want CloudWatch's, Azure Resource Manager's, Google Cloud Monitoring's or OCI Monitoring's API" (toString .url)) }}
{{- end }}
{{- $_ := set $sources (toString .source) (include "lapilli.alarmsKind" (toString .url)) }}
{{- end }}
{{- if and $r.alarmsEvery (or (ne .Values.mode "serve") (not $r.alarms)) }}
{{- fail "rules.alarmsEvery is how often serve reads rules.alarms" }}
{{- end }}
{{- $kind := include "lapilli.rulerKind" . }}
{{- if and (eq $kind "amazon") (ne $p.auth "aws") }}
{{- fail "rules.ruler.url on Amazon is read with the pod's identity: set prometheus.auth to aws" }}
{{- end }}
{{- if and (eq $kind "azure") (ne $p.auth "azure") }}
{{- fail "rules.ruler.url on Azure is read with the pod's identity: set prometheus.auth to azure" }}
{{- end }}
{{- $am := include "lapilli.alertmanager" . | fromJson }}
{{- if and (or $r.ruler.tokenSecret.name $r.ruler.tenant) (ne $kind "config") (or $r.ruler.url (not $am.url)) }}
{{- fail "rules.ruler.tokenSecret and rules.ruler.tenant are a Mimir's or a Cortex's, for rules.ruler.url or serve.checkContent.alertmanager.url" }}
{{- end }}
{{- /* Alertmanager's configuration, one way: a file from a Secret or a
     ConfigMap, the template files beside it, or Mimir's or Cortex's API,
     on the ruler's scheme and host when the ruler's token is sent. */}}
{{- $amFrom := 0 }}
{{- if ($am.secret).name }}{{ $amFrom = add1 $amFrom }}{{ end }}
{{- if ($am.configMap).name }}{{ $amFrom = add1 $amFrom }}{{ end }}
{{- if $am.url }}{{ $amFrom = add1 $amFrom }}{{ end }}
{{- if gt (int $amFrom) 1 }}
{{- fail "serve.checkContent.alertmanager: give one of secret, configMap and url" }}
{{- end }}
{{- if and $am.templateConfigMaps (not (or ($am.secret).name ($am.configMap).name)) }}
{{- fail "serve.checkContent.alertmanager.templateConfigMaps are mounted beside the configuration: give secret or configMap" }}
{{- end }}
{{- range $k := list ($am.secret).key ($am.configMap).key }}
{{- if and $k (not (regexMatch "^[-._a-zA-Z0-9]+$" (toString $k))) }}
{{- fail (printf "serve.checkContent.alertmanager: the key %q: a Secret's or a ConfigMap's key, letters, digits, -, _ and ." (toString $k)) }}
{{- end }}
{{- end }}
{{- with $am.url }}
{{- if not (regexMatch "^https?://[^/?#@]+(/[^?#]*)?$" (toString .)) }}
{{- fail (printf "serve.checkContent.alertmanager.url %q: want a Mimir's or a Cortex's configuration API, such as http://mimir:8080/api/v1/alerts, with no user, query or anchor" (toString .)) }}
{{- end }}
{{- if and $r.ruler.url $r.ruler.tokenSecret.name }}
{{- $a := urlParse (toString .) }}
{{- $b := urlParse (toString $r.ruler.url) }}
{{- if or (ne (lower $a.scheme) (lower $b.scheme)) (ne (lower $a.host) (lower $b.host)) }}
{{- fail "serve.checkContent.alertmanager.url isn't on rules.ruler.url's scheme and host, and rules.ruler.tokenSecret is the ruler's: give Mimir's or Cortex's /api/v1/alerts on the same host" }}
{{- end }}
{{- end }}
{{- end }}
{{- with ($s.checkContent).alertmanager }}
{{- if and (or (.secret).name (.configMap).name .url) (eq $.Values.mode "serve") (not ($s.checkContent).enabled) }}
{{- fail "serve.checkContent.alertmanager goes with the content check, which serve.checkContent.enabled turns off" }}
{{- end }}
{{- end }}
{{- if and (not $history) (or (gt (int $from) 0) $r.alarms .Values.impact) }}
{{- fail "rules and impact are replayed against history: give prometheus.url or gcpProject" }}
{{- end }}
{{- include "lapilli.historyChecks" . }}
{{- if and (eq .Values.mode "cronjob") (not .Values.incidents) (not .Values.impact) }}
{{- fail "give incidents, impact, or both" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $s.everyoneAdmin (or $s.roles.editors $s.roles.admins) }}
{{- fail "serve.everyoneAdmin is for no roles: with serve.roles, they say who may change what" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $s.everyoneAdmin $s.readOnly }}
{{- fail "serve.everyoneAdmin lets everyone change anything, and serve.readOnly no one" }}
{{- end }}
{{- $in := $s.signIn }}
{{- $signIns := 0 }}
{{- if $s.userHeader }}{{ $signIns = add1 $signIns }}{{ end }}
{{- if $in.oidc.issuer }}{{ $signIns = add1 $signIns }}{{ end }}
{{- if $in.proxyJWT.kind }}{{ $signIns = add1 $signIns }}{{ end }}
{{- if and (eq .Values.mode "serve") (gt (int $signIns) 1) }}
{{- fail "serve.userHeader, serve.signIn.oidc and serve.signIn.proxyJWT are three ways to say who people are: give one" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $in.oidc.issuer (not $in.oidc.clientId) }}
{{- fail "serve.signIn.oidc.clientId: the client lapilli is registered as with the provider" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $in.oidc.issuer (not (include "lapilli.externalURL" .)) }}
{{- fail "serve.signIn.oidc needs serve.externalURL, or an ingress host or httpRoute hostname to work it out from: the provider sends people back to its /auth/callback" }}
{{- end }}
{{- if and (eq .Values.mode "serve") (or $in.sessionMaxAge $in.sessionKeySecret.name) (not $in.oidc.issuer) }}
{{- fail "serve.signIn.sessionMaxAge and sessionKeySecret are serve.signIn.oidc's sessions" }}
{{- end }}
{{- if eq .Values.mode "serve" }}
{{- $n := int $s.replicas }}
{{- if lt $n 1 }}
{{- fail "serve.replicas: 1 or more" }}
{{- end }}
{{- if and (gt $n 1) (not $s.ledger.postgres.secret.name) }}
{{- fail (printf "serve.replicas: %d replicas share one ledger only in Postgres (serve.ledger.postgres.secret): a SQLite ledger is one pod's file, on a volume one node mounts. Keep serve.replicas at 1, or move the ledger to Postgres first (lapilli ledger copy)" $n) }}
{{- end }}
{{- if and (gt $n 1) $in.oidc.issuer (not $in.sessionKeySecret.name) }}
{{- fail "serve.replicas: replicas sign people in with one session key, from a Secret they all mount: give serve.signIn.sessionKeySecret.name, a Secret of your own. One the chart makes is made again by a render that can't look it up (helm template, Argo CD, Flux), and a pod started after would sign with another key" }}
{{- end }}
{{- $u := $s.podDisruptionBudget.maxUnavailable }}
{{- $ok := false }}
{{- if kindIs "string" $u }}
{{- /* Kubernetes rounds a percentage of pods up: one replica at least
must stay. */}}
{{- if regexMatch "^[1-9][0-9]?%$" $u }}{{ $p := int (trimSuffix "%" $u) }}{{ $ok = lt (div (add (mul $n $p) 99) 100) $n }}{{ end }}
{{- else }}{{ $ok = and (ge (int $u) 1) (lt (int $u) $n) }}{{ end }}
{{- if and (gt $n 1) (not $ok) }}
{{- fail (printf "serve.podDisruptionBudget.maxUnavailable: a number of pods, 1 to %d, or a percentage below 100%%, so that one replica at least stays" (sub $n 1)) }}
{{- end }}
{{- $d := toString $s.shutdownDelaySeconds }}
{{- if or (not (regexMatch "^[0-9]{1,3}$" $d)) (gt (int $d) 600) }}
{{- fail "serve.shutdownDelaySeconds: seconds, 0 to 600" }}
{{- end }}
{{- end }}
{{- if and (eq .Values.mode "serve") $in.proxyJWT.kind (not (has $in.proxyJWT.kind (list "google-iap" "aws-alb" "jwks"))) }}
{{- fail "serve.signIn.proxyJWT.kind: google-iap, aws-alb or jwks" }}
{{- end }}
{{- if and (eq .Values.mode "serve") (or $in.oidc.issuer $in.proxyJWT.kind) $s.everyoneAdmin }}
{{- fail "serve.everyoneAdmin is for no sign-in: to let everyone signed in change anything, give serve.roles.admins: ['*']" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $s.roles.groupsHeader (not $s.userHeader) }}
{{- fail "serve.roles.groupsHeader is serve.userHeader's: with serve.signIn, the groups are in serve.signIn.groupsClaim" }}
{{- end }}
{{- if and (eq .Values.mode "serve") (or $s.roles.editors $s.roles.admins $s.roles.viewers) (eq (int $signIns) 0) }}
{{- fail "serve.roles needs a sign-in, serve.signIn or serve.userHeader: who is who comes from it" }}
{{- end }}
{{- $own := or $in.oidc.issuer $in.proxyJWT.kind }}
{{- if and (eq .Values.mode "serve") $own (not (or $s.roles.viewers $s.roles.editors $s.roles.admins)) }}
{{- fail "serve.signIn lets in only those a role names: give serve.roles.viewers (people, group:NAME or *@DOMAIN), editors or admins. An identity provider signs in whoever it knows, which for a public one is anyone" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $own (or (not $s.roles.viewers) (has "*" $s.roles.viewers)) (or (has "*" $s.roles.editors) (has "*" $s.roles.admins) (has "*" ($s.changes.approvers | default list))) }}
{{- fail "serve.roles '*' gives a role to everyone the identity provider signs in: say who may come in with serve.roles.viewers (people, group:NAME or *@DOMAIN) first" }}
{{- end }}
{{- $ext := include "lapilli.externalURL" . }}
{{- if $ext }}
{{- $u := urlParse $ext }}
{{- if and (ne (toString $u.path) "") (ne (toString $u.path) "/") }}
{{- fail "serve.externalURL: lapilli is served at the root of its host, as its links are: give no path" }}
{{- end }}
{{- $h := regexReplaceAll ":[0-9]+$" (toString $u.host) "" }}
{{- if and (eq .Values.mode "serve") $in.oidc.issuer (eq $u.scheme "http") (not (regexMatch "^(localhost|.*\\.localhost|127(\\.[0-9]+){3}|\\[::1\\])$" $h)) }}
{{- fail (printf "serve.signIn.oidc: people reach lapilli at %s, over http: the session cookie would cross the network in the clear. Serve it over https (serve.ingress.https, serve.httpRoute.https, or serve.externalURL)" $ext) }}
{{- end }}
{{- end }}
{{- if and (eq .Values.mode "serve") $s.ingress.enabled (ne (toString $s.ingress.path) "/") }}
{{- fail "serve.ingress.path: lapilli is served at the root of its host, as its links are: /" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $s.userHeader (ne $s.service.type "ClusterIP") (not (and (eq $s.service.type "LoadBalancer") $s.service.loadBalancerSourceRanges $s.service.sourceRangesAreTheProxy)) }}
{{- fail "serve.userHeader trusts a plain header, which nothing can set or strip in front of a LoadBalancer or a NodePort of TCP: anyone who reaches it names themselves anyone. Put the proxy in front (an Ingress, a Gateway), use serve.signIn, or, for a LoadBalancer only the proxy reaches, name its addresses in serve.service.loadBalancerSourceRanges and set serve.service.sourceRangesAreTheProxy" }}
{{- end }}
{{- $svc := $s.service }}
{{- if and (eq .Values.mode "serve") (not (has ($svc.type | default "ClusterIP") (list "ClusterIP" "LoadBalancer" "NodePort"))) }}
{{- fail "serve.service.type: ClusterIP, LoadBalancer or NodePort" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $s.httpRoute.enabled (not $s.httpRoute.parentRefs) }}
{{- fail "serve.httpRoute.parentRefs: the Gateway the route attaches to" }}
{{- end }}
{{- if and (eq .Values.mode "serve") (include "lapilli.exposed" .) (eq (int $signIns) 0) (not $s.exposeWithoutSignIn) }}
{{- fail "serve.service, serve.ingress or serve.httpRoute serve the UI outside the cluster, where anyone who reaches it reads the ledger: sign people in (serve.signIn.oidc, serve.signIn.proxyJWT, or serve.userHeader behind a proxy that signs them in), or say it is meant with serve.exposeWithoutSignIn" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $s.userHeader (not $s.networkPolicy.enabled) (not $s.networkPolicy.othersKeptOut) }}
{{- fail "serve.userHeader trusts a header any pod in the cluster could set: enable serve.networkPolicy, with a network plugin that enforces it, so only the sign-in proxy reaches lapilli; or, if something else keeps the rest out (a service mesh's policy), say so with serve.networkPolicy.othersKeptOut" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $s.notify.slack (not .Values.report.slackWebhookSecret.name) }}
{{- fail "serve.notify.slack needs report.slackWebhookSecret" }}
{{- end }}
{{- $t := $s.trackers }}
{{- $port := include "lapilli.incidentsPort" . }}
{{- if and (eq .Values.mode "serve") $port $s.readOnly }}
{{- fail "serve.incidentsAPI, serve.firings and serve.trackers take writes, and serve.readOnly refuses them" }}
{{- end }}
{{- /* The chart's own trackers job is let in by the NetworkPolicy; a job of
your own needs from. */ -}}
{{- $others := or (and $s.incidentsAPI.tokenSecret.name (not $s.incidentsAPI.copy.enabled)) $s.firings.tokenSecret.name ($s.responses).tokenSecret.name (and $t.tokenSecret.name (not $t.tracker)) (and $s.changes.enabled $s.changes.tokenSecret.name (not $s.changes.pullRequests.enabled)) }}
{{- if and (eq .Values.mode "serve") $others $s.networkPolicy.enabled (not $s.incidentsAPI.from) }}
{{- fail "serve.incidentsAPI.from: name who may reach the port that takes incidents, firings, tasks and changes, the jobs that copy them and the change recipes; with serve.networkPolicy, no one else can" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $t.tokenSecret.name (or (and (eq $t.tokenSecret.name $s.incidentsAPI.tokenSecret.name) (eq $t.tokenSecret.key $s.incidentsAPI.tokenSecret.key)) (and (eq $t.tokenSecret.name $s.firings.tokenSecret.name) (eq $t.tokenSecret.key $s.firings.tokenSecret.key)) (and (eq $t.tokenSecret.name $s.notificationsAPI.tokenSecret.name) (eq $t.tokenSecret.key $s.notificationsAPI.tokenSecret.key)) (and (eq $t.tokenSecret.name $s.changes.tokenSecret.name) (eq $t.tokenSecret.key $s.changes.tokenSecret.key))) }}
{{- fail "serve.trackers.tokenSecret is another token's Secret: give the tasks token its own, so a job keeping tasks in step can write nothing else" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $t.tracker (not $t.tokenSecret.name) }}
{{- fail "serve.trackers.tracker needs serve.trackers.tokenSecret, the tasks token the job writes with" }}
{{- end }}
{{- $n := $s.notificationsAPI }}
{{- if and (eq .Values.mode "serve") (or $n.tokenSecret.name $n.grafanaSecret.name $n.snsTopics) $s.readOnly }}
{{- fail "serve.notificationsAPI takes writes, and serve.readOnly refuses them" }}
{{- end }}
{{- if and (eq .Values.mode "serve") (or $n.tokenSecret.name $n.grafanaSecret.name $n.snsTopics) $s.networkPolicy.enabled (not $n.from) }}
{{- fail "serve.notificationsAPI.from: name who may reach the port that takes notifications, the alert manager; with serve.networkPolicy, no one else can" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $s.incidentsAPI.tokenSecret.name (eq $s.incidentsAPI.tokenSecret.name $s.firings.tokenSecret.name) (eq $s.incidentsAPI.tokenSecret.key $s.firings.tokenSecret.key) }}
{{- fail "serve.firings.tokenSecret is serve.incidentsAPI.tokenSecret: give each its own token, so a job that may copy one can't write the other" }}
{{- end }}
{{- $rs := $s.responses | default dict }}
{{- $rt := $rs.tokenSecret | default dict }}
{{- if and (eq .Values.mode "serve") $rt.name (or (and (eq $rt.name $s.incidentsAPI.tokenSecret.name) (eq ($rt.key | default "token") $s.incidentsAPI.tokenSecret.key)) (and (eq $rt.name $s.firings.tokenSecret.name) (eq ($rt.key | default "token") $s.firings.tokenSecret.key)) (and (eq $rt.name $s.trackers.tokenSecret.name) (eq ($rt.key | default "token") $s.trackers.tokenSecret.key)) (and (eq $rt.name $s.notificationsAPI.tokenSecret.name) (eq ($rt.key | default "token") $s.notificationsAPI.tokenSecret.key)) (and (eq $rt.name $s.changes.tokenSecret.name) (eq ($rt.key | default "token") $s.changes.tokenSecret.key))) }}
{{- fail "serve.responses.tokenSecret is another port's token: give the jobs that send pages a token of their own" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $rt.name $s.readOnly }}
{{- fail "serve.responses.tokenSecret takes writes, and serve.readOnly refuses them" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $rs.configMap.name (not (or $s.firings.tokenSecret.name .Values.prometheus.url .Values.gcpProject)) }}
{{- fail "serve.responses.configMap needs prometheus.url or gcpProject, or serve.firings, to have firings to answer" }}
{{- end }}
{{- if and .Values.rollouts (not (or .Values.prometheus.url .Values.gcpProject)) }}
{{- fail "rollouts reads kube-state-metrics from the history: give prometheus.url or gcpProject" }}
{{- end }}
{{- if and (eq .Values.noisyBy "useful") (not (or .Values.rollouts $rs.configMap.name (and $s.deploys.configMap.name (eq .Values.mode "serve")))) }}
{{- fail "noisyBy: useful finds a rule noisy by what was done about its firings: give rollouts, serve.responses.configMap or serve.deploys" }}
{{- end }}
{{- $c := $s.changes }}
{{- if and (eq .Values.mode "serve") $c.enabled (eq (int $signIns) 0) }}
{{- fail "serve.changes needs a sign-in, serve.signIn or serve.userHeader: a change is approved by a person, and the sign-in says who" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $c.enabled (not (or $s.roles.editors $s.roles.admins)) }}
{{- fail "serve.changes needs roles, serve.roles.editors or serve.roles.admins: without them, whoever may change anything is an admin, who could resume changes paused and approve" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $c.enabled (not (or $s.roles.admins $c.approvers)) }}
{{- fail "serve.changes needs serve.changes.approvers or serve.roles.admins: who approves a change, the admins unless serve.changes.approvers says who" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $c.enabled $s.readOnly }}
{{- fail "serve.changes carries out changes, and serve.readOnly refuses them" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $c.enabled (not (has (int ($c.approvals | default 1)) (list 1 2))) }}
{{- fail "serve.changes.approvals: 1 or 2" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $c.enabled $c.tokenSecret.name (or (and (eq $c.tokenSecret.name $s.incidentsAPI.tokenSecret.name) (eq $c.tokenSecret.key $s.incidentsAPI.tokenSecret.key)) (and (eq $c.tokenSecret.name $s.firings.tokenSecret.name) (eq $c.tokenSecret.key $s.firings.tokenSecret.key)) (and (eq $c.tokenSecret.name $s.trackers.tokenSecret.name) (eq $c.tokenSecret.key $s.trackers.tokenSecret.key)) (and (eq $c.tokenSecret.name $s.notificationsAPI.tokenSecret.name) (eq $c.tokenSecret.key $s.notificationsAPI.tokenSecret.key))) }}
{{- fail "serve.changes.tokenSecret is another port's token: give the change recipes their own" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $c.enabled $c.grafanaTokenSecret.name (not .Values.rules.grafana.url) }}
{{- fail "serve.changes.grafanaTokenSecret is for the rules read from Grafana (rules.grafana)" }}
{{- end }}
{{- if and (eq .Values.mode "serve") (or $c.in $c.managed $c.rulerTokenSecret.name $c.lokiTokenSecret.name $c.awsRole $c.azureClientId $c.googleServiceAccount ($c.ociConfigSecret).name) (not $c.enabled) }}
{{- fail "serve.changes.in, managed, rulerTokenSecret, lokiTokenSecret, awsRole, azureClientId, googleServiceAccount and ociConfigSecret go with serve.changes.enabled" }}
{{- end }}
{{- range $c.in }}
{{- /* An alarms source named loki takes its own entries, as before 0.70.0. */}}
{{- if and (eq $.Values.mode "serve") (hasPrefix "loki:" (toString .)) (not $r.loki.loaded) (not $r.ruler.url) (not (hasKey $sources "loki")) }}
{{- fail (printf "serve.changes.in: %s names a namespace of Loki's ruler: set rules.loki.loaded, which reads it" (toString .)) }}
{{- end }}
{{- end }}
{{- if and (eq .Values.mode "serve") $c.in (not $r.ruler.url) (not $r.alarms) (not $r.loki.loaded) }}
{{- fail "serve.changes.in is for the rules read from a ruler (rules.ruler.url) or Loki's ruler (rules.loki.loaded), and the alarms in rules.alarms" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $c.managed (not $r.ruler.url) (not $r.alarms) }}
{{- fail "serve.changes.in and serve.changes.managed are for the rules read from a ruler (rules.ruler.url), and the alarms in rules.alarms" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $r.loki.loaded (hasKey $sources "loki") }}
{{- fail "rules.alarms: the source loki: with rules.loki.loaded, loki: in serve.changes.in names Loki's ruler's namespaces: name the alarms' source another" }}
{{- end }}
{{- if eq .Values.mode "serve" }}
{{- range $c.in }}
{{- $src := index (splitList ":" (toString .)) 0 }}
{{- if and (contains ":" (toString .)) (regexMatch "^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$" $src) (not (hasKey $sources $src)) (not $r.ruler.url) (not (and (eq $src "loki") $r.loki.loaded)) }}
{{- fail (printf "serve.changes.in: %s names the source %s, which rules.alarms doesn't" (toString .) $src) }}
{{- end }}
{{- end }}
{{- end }}
{{- if and (eq .Values.mode "serve") $c.lokiTokenSecret.name (not $r.loki.loaded) }}
{{- fail "serve.changes.lokiTokenSecret is for the rules of Loki's ruler (rules.loki.loaded)" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $c.lokiTokenSecret.name (eq $c.lokiTokenSecret.name $lk.tokenSecret.name) (eq $c.lokiTokenSecret.key $lk.tokenSecret.key) }}
{{- fail "serve.changes.lokiTokenSecret is loki.tokenSecret: give the writes a token of their own" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $c.lokiTokenSecret.name (eq $c.lokiTokenSecret.name $c.rulerTokenSecret.name) (eq $c.lokiTokenSecret.key $c.rulerTokenSecret.key) }}
{{- fail "serve.changes.lokiTokenSecret is serve.changes.rulerTokenSecret: give each ruler's writes a token of their own" }}
{{- end }}
{{- $clouds := values $sources }}
{{- if and (eq .Values.mode "serve") $c.googleServiceAccount (not (has "google" $clouds)) }}
{{- fail "serve.changes.googleServiceAccount is for Google Cloud's alerting policies (rules.alarms)" }}
{{- end }}
{{- if and (eq .Values.mode "serve") ($c.ociConfigSecret).name (not (has "oci" $clouds)) }}
{{- fail "serve.changes.ociConfigSecret is for OCI's alarms (rules.alarms)" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $c.managed (eq $kind "config") (not $r.alarms) }}
{{- fail "serve.changes.managed names tags, which a Mimir's or a Cortex's groups don't have: serve.changes.in alone says what Lapilli may change there" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $c.rulerTokenSecret.name (ne $kind "config") }}
{{- fail "serve.changes.rulerTokenSecret is for a Mimir's or a Cortex's rules (rules.ruler.url)" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $c.rulerTokenSecret.name (eq $c.rulerTokenSecret.name $r.ruler.tokenSecret.name) (eq $c.rulerTokenSecret.key $r.ruler.tokenSecret.key) }}
{{- fail "serve.changes.rulerTokenSecret is rules.ruler.tokenSecret: give the writes a token of their own" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $c.awsRole (ne $kind "amazon") (not (has "aws" $clouds)) }}
{{- fail "serve.changes.awsRole is for Amazon's rules (rules.ruler.url), or CloudWatch's alarms (rules.alarms)" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $c.azureClientId (ne $kind "azure") (not (has "azure" $clouds)) }}
{{- fail "serve.changes.azureClientId is for Azure's rule groups (rules.ruler.url), or its alert rules (rules.alarms)" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $s.mcpPropose (not (or $s.roles.editors $s.roles.admins)) }}
{{- fail "serve.mcpPropose needs serve.roles: without roles, whoever may change anything is an admin, and anyone who reaches /mcp could propose as anyone" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $s.teamDigests.teams (not $s.teamDigests.secretName) }}
{{- fail "serve.teamDigests.teams needs serve.teamDigests.secretName" }}
{{- end }}
{{- with $s.notify.webhookSecret }}
{{- if and (eq $.Values.mode "serve") .signingKey (not .name) }}
{{- fail "serve.notify.webhookSecret.signingKey is a key of serve.notify.webhookSecret.name's Secret: give the Secret's name" }}
{{- end }}
{{- if and (eq $.Values.mode "serve") .signingKey (eq (toString .signingKey) (toString .key)) }}
{{- fail "serve.notify.webhookSecret.signingKey is the key holding the URL: give the signing key a key of its own" }}
{{- end }}
{{- end }}
{{- $dests := dict }}
{{- range ($s.reports).destinations }}
{{- if not (regexMatch "^[a-z0-9][a-z0-9._-]{0,62}$" (toString .name)) }}
{{- fail (printf "serve.reports.destinations: %q: a name is lowercase letters, digits and . _ -" (toString .name)) }}
{{- end }}
{{- if has .name (list "slack" "webhook") }}
{{- fail (printf "serve.reports.destinations: %s is the name of report.slackWebhookSecret or notify.webhookSecret already" .name) }}
{{- end }}
{{- if hasKey $dests .name }}
{{- fail (printf "serve.reports.destinations: %s twice" .name) }}
{{- end }}
{{- if not .secretName }}
{{- fail (printf "serve.reports.destinations: %s needs a secretName" .name) }}
{{- end }}
{{- $_ := set $dests .name true }}
{{- end }}
{{- range ($s.notify).webhookDests }}
{{- if not (regexMatch "^[a-z0-9][a-z0-9._-]{0,62}$" (toString .name)) }}
{{- fail (printf "serve.notify.webhookDests: %q: a name is lowercase letters, digits and . _ -" (toString .name)) }}
{{- end }}
{{- if has .name (list "slack" "webhook") }}
{{- fail (printf "serve.notify.webhookDests: %s is the name of report.slackWebhookSecret or notify.webhookSecret already" .name) }}
{{- end }}
{{- if hasKey $dests .name }}
{{- fail (printf "serve.notify.webhookDests: %s twice, or a name serve.reports.destinations has: a route names a destination by its name alone" .name) }}
{{- end }}
{{- if not .secretName }}
{{- fail (printf "serve.notify.webhookDests: %s needs a secretName" .name) }}
{{- end }}
{{- if and .signingKey (eq (toString .signingKey) (toString (.key | default "url"))) }}
{{- fail (printf "serve.notify.webhookDests: %s's signingKey is the key holding the URL: give the signing key a key of its own" .name) }}
{{- end }}
{{- if and (eq $.Values.mode "serve") (not $s.config) }}
{{- fail "serve.notify.webhookDests take what the routes in serve.config's notify: send them: give serve.config" }}
{{- end }}
{{- $_ := set $dests .name true }}
{{- end }}
{{- /* The languages --lang takes: internal/serve's Langs (TestChartLangs). */}}
{{- if not (has ($s.notify.lang | default "en") (list "en" "es" "ja" "ko")) }}
{{- fail "serve.notify.lang: en, es, ja or ko" }}
{{- end }}
{{- /* Pull request backtests from CI jobs, on a port of their own
(docs/design/backtests.md): refused where lapilli serve refuses
--backtest-listen, and outside the cluster without TLS. */}}
{{- $b := $s.backtest }}
{{- if and $b.enabled (ne .Values.mode "serve") }}
{{- fail "serve.backtest is lapilli serve's: set mode: serve" }}
{{- end }}
{{- if and (eq .Values.mode "serve") (not $b.enabled) (or $b.trust.configMap.name $b.tokens $b.share $b.queue $b.timeout $b.ingress.enabled (ne ($b.service.type | default "ClusterIP") "ClusterIP")) }}
{{- fail "serve.backtest.trust, tokens, share, queue, timeout, service and ingress go with serve.backtest.enabled" }}
{{- end }}
{{- if and (eq .Values.mode "serve") $b.enabled }}
{{- if not $b.trust.configMap.name }}
{{- fail "serve.backtest.trust.configMap.name: the ConfigMap holding --backtest-trust's file, who may ask; the port takes no one without it" }}
{{- end }}
{{- if $s.readOnly }}
{{- fail "serve.backtest queues backtests in the ledger, and serve.readOnly refuses writes" }}
{{- end }}
{{- if not $history }}
{{- fail "serve.backtest replays a pull request's rules against the history: give prometheus.url or gcpProject" }}
{{- end }}
{{- if and (eq (int $from) 0) (not $r.alarms) (not $loki) }}
{{- fail "serve.backtest needs rules lapilli serve replays (rules.configMaps, rules.loaded, rules.grafana.url, rules.ruler.url, serve.rulesHistory or rules.alarms): with firings copied in alone, it refuses the port" }}
{{- end }}
{{- range .Values.extraArgs }}
{{- if has (toString .) (list "--alerts" "--alerts=true") }}
{{- fail "serve.backtest replays a pull request's rules, and --alerts (extraArgs) judges the ALERTS series, not rules" }}
{{- end }}
{{- end }}
{{- $used := list 8080 (int $s.metricsPort) }}
{{- if include "lapilli.incidentsPort" . }}{{ $used = append $used (int $s.incidentsAPI.port) }}{{ end }}
{{- if or $s.notificationsAPI.tokenSecret.name $s.notificationsAPI.grafanaSecret.name $s.notificationsAPI.snsTopics }}{{ $used = append $used (int $s.notificationsAPI.port) }}{{ end }}
{{- $bp := int $b.port }}
{{- if or (lt $bp 1) (gt $bp 65535) (has $bp $used) }}
{{- fail (printf "serve.backtest.port: %v: a port of its own, 1 to 65535, none of the pod's others (%s)" $b.port (join ", " $used)) }}
{{- end }}
{{- with $b.share }}
{{- if or (not (regexMatch "^[0-9]*\\.?[0-9]+$" (toString .))) (lt (float64 (toString .)) 0.05) (gt (float64 (toString .)) 0.75) }}
{{- fail "serve.backtest.share: of --max-rate, the backtests' share, 0.05 to 0.75" }}
{{- end }}
{{- end }}
{{- with $b.queue }}
{{- if or (not (regexMatch "^[0-9]+$" (toString .))) (lt (int .) 1) (gt (int .) 1000) }}
{{- fail "serve.backtest.queue: backtests waiting at most, 1 to 1000" }}
{{- end }}
{{- end }}
{{- /* A plain token is the entry's alone: another port's would let a job
that may write there backtest too, and the reverse. */}}
{{- $taken := list }}
{{- range (list $s.incidentsAPI.tokenSecret $s.firings.tokenSecret ($s.responses | default dict).tokenSecret $s.trackers.tokenSecret $s.notificationsAPI.tokenSecret $s.changes.tokenSecret) }}
{{- if .name }}{{ $taken = append $taken (printf "%s/%s" .name .key) }}{{ end }}
{{- end }}
{{- $seen := dict }}
{{- range $b.tokens }}
{{- if not .secretName }}
{{- fail "serve.backtest.tokens: each names a secretName, the Secret holding a plain token, and its key" }}
{{- end }}
{{- $id := printf "%s/%s" .secretName (.key | default "token") }}
{{- if hasKey $seen $id }}
{{- fail (printf "serve.backtest.tokens: %s twice" $id) }}
{{- end }}
{{- if has $id $taken }}
{{- fail (printf "serve.backtest.tokens: %s is another port's token: give the backtests a token of their own" $id) }}
{{- end }}
{{- $_ := set $seen $id true }}
{{- end }}
{{- $type := $b.service.type | default "ClusterIP" }}
{{- if not (has $type (list "ClusterIP" "LoadBalancer" "NodePort")) }}
{{- fail "serve.backtest.service.type: ClusterIP, LoadBalancer or NodePort" }}
{{- end }}
{{- if and (ne $type "ClusterIP") (not $b.service.loadBalancerEndsTLS) }}
{{- fail (printf "serve.backtest.service.type %s puts the backtest port outside the cluster, where it is taken only over TLS, and lapilli serves it in plain http: end TLS at the load balancer (an AWS NLB's TLS listener, an OCI load balancer's) and say so with serve.backtest.service.loadBalancerEndsTLS, or keep the Service a ClusterIP and use serve.backtest.ingress, with tls" $type) }}
{{- end }}
{{- if and $b.ingress.enabled (not $b.ingress.tls) (not $b.ingress.loadBalancerEndsTLS) }}
{{- fail "serve.backtest.ingress: the backtest port is exposed outside the cluster only with TLS: give serve.backtest.ingress.tls, or, for TLS ended at the load balancer in front with a certificate of its own (an ALB's certificate-arn, a GKE managed certificate), say so with serve.backtest.ingress.loadBalancerEndsTLS" }}
{{- end }}
{{- if and $s.networkPolicy.enabled (not $b.from) }}
{{- fail "serve.backtest.from: name who may reach the port that takes pull request backtests: the runners in the cluster, the ingress controller's pods, or the load balancer's addresses (ipBlock); with serve.networkPolicy, no one else can" }}
{{- end }}
{{- end }}
{{- end -}}

{{/* serve.backtest.tokens by Secret, as JSON: each Secret's keys, mounted
together at /var/run/lapilli/backtest/tokens/SECRET/. */}}
{{- define "lapilli.backtestTokens" -}}
{{- $by := dict -}}
{{- range .Values.serve.backtest.tokens -}}
{{- $k := .key | default "token" -}}
{{- $have := index $by .secretName | default list -}}
{{- if not (has $k $have) }}{{ $_ := set $by .secretName (append $have $k) }}{{ end -}}
{{- end -}}
{{- toJson $by -}}
{{- end -}}

{{/* serve.checkContent.alertmanager as serve reads it: nothing in cronjob
mode, or with the content check off. A release upgraded with
--reuse-values from before it has none. */}}
{{- define "lapilli.alertmanager" -}}
{{- $s := .Values.serve -}}
{{- if and (eq .Values.mode "serve") (($s.checkContent).enabled) -}}
{{- toJson (($s.checkContent).alertmanager | default dict) -}}
{{- else -}}
{{- toJson dict -}}
{{- end -}}
{{- end -}}

{{/* Whether rules.ruler's tenant and token are given: with rules.ruler.url,
or, without it, for serve.checkContent.alertmanager.url, Mimir's or
Cortex's Alertmanager, which is read with them (--alertmanager-config). */}}
{{- define "lapilli.rulerCreds" -}}
{{- $am := include "lapilli.alertmanager" . | fromJson -}}
{{- if or .Values.rules.ruler.url $am.url -}}
true
{{- end -}}
{{- end -}}

{{/* The kind of ruler rules.ruler.url is, by its host, as lapilli tells it:
amazon, azure, config (Mimir's or Cortex's API), or none. */}}
{{- define "lapilli.rulerKind" -}}
{{- $u := .Values.rules.ruler.url -}}
{{- if not $u -}}
{{- else if regexMatch "^https?://([^/]*\\.)?aps(-fips)?\\.[a-z0-9-]+\\.(vpce\\.)?(amazonaws\\.com(\\.cn)?|api\\.aws)(:[0-9]+)?(/|$)" $u -}}
amazon
{{- else if regexMatch "^https?://management\\.(azure\\.com|usgovcloudapi\\.net|chinacloudapi\\.cn)(:[0-9]+)?(/|$)" (lower $u) -}}
azure
{{- else -}}
config
{{- end -}}
{{- end -}}

{{/* The cloud a rules.alarms URL is, by its host, as lapilli tells it: aws,
azure, google, oci, or none. */}}
{{- define "lapilli.alarmsKind" -}}
{{- $u := lower . -}}
{{- if regexMatch "^https://([^/]*\\.)?monitoring(-fips)?\\.[a-z0-9-]+\\.(vpce\\.)?(amazonaws\\.com(\\.cn)?|api\\.aws)/?$" $u -}}
aws
{{- else if regexMatch "^https://management\\.(azure\\.com|usgovcloudapi\\.net|chinacloudapi\\.cn)/subscriptions/[^/?]+(/resourcegroups/[^/?]+)?/?$" $u -}}
azure
{{- else if regexMatch "^https://monitoring\\.googleapis\\.com/v3/projects/[^/?]+/alertpolicies/?$" $u -}}
google
{{- else if regexMatch "^https://telemetry\\.[a-z0-9-]+\\.(oraclecloud[0-9]*\\.com|oraclegovcloud\\.(com|uk)|oraclecloud\\.eu)/20180401/alarms/?\\?([^#]*&)?compartmentid=ocid1\\." $u -}}
oci
{{- end -}}
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
{{- with $p.tenant }}
- {{ printf "--prometheus-tenant=%s" . | quote }}
{{- end }}
{{- else if .Values.gcpProject }}
- {{ printf "--gcp-project=%s" .Values.gcpProject | quote }}
{{- end }}
{{- if and (eq .Values.mode "serve") $r.configMaps }}
{{- /* serve reads one directory: every ConfigMap's keys in it, as one projected volume */}}
- "--rules=/etc/lapilli/rules/0"
{{- else }}
{{- range $i, $_ := $r.configMaps }}
- {{ printf "--rules=/etc/lapilli/rules/%d" $i | quote }}
{{- end }}
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
{{- with $r.ruler.url }}
- {{ printf "--ruler=%s" . | quote }}
{{- end }}
{{- if and (include "lapilli.rulerCreds" .) $r.ruler.tenant }}
- {{ printf "--ruler-tenant=%s" $r.ruler.tenant | quote }}
{{- end }}
{{- if and (include "lapilli.rulerCreds" .) $r.ruler.tokenSecret.name }}
- {{ printf "--ruler-token-file=/var/run/lapilli/ruler/%s" ($r.ruler.tokenSecret.key | default "token") | quote }}
{{- end }}
{{- range $r.alarms }}
- {{ printf "--alarms=%s=%s" .source .url | quote }}
{{- end }}
{{- if and $r.grafana.url (not $r.grafana.history) }}
- --grafana-history=false
{{- end }}
{{- with .Values.loki }}
{{- if .url }}
- {{ printf "--loki=%s" .url | quote }}
{{- with .tenant }}
- {{ printf "--loki-tenant=%s" . | quote }}
{{- end }}
{{- if .tokenSecret.name }}
- {{ printf "--loki-token-file=/var/run/lapilli/loki/%s" (.tokenSecret.key | default "token") | quote }}
{{- end }}
{{- end }}
{{- end }}
{{- if $r.loki.loaded }}
- --loki-loaded-rules
{{- end }}
{{- if and (eq .Values.mode "serve") $r.loki.configMaps }}
{{- /* serve reads one directory: every ConfigMap's keys in it, as one projected volume */}}
- "--loki-rules=/etc/lapilli/loki-rules/0"
{{- else }}
{{- range $i, $_ := $r.loki.configMaps }}
- {{ printf "--loki-rules=/etc/lapilli/loki-rules/%d" $i | quote }}
{{- end }}
{{- end }}
{{- if .Values.incidents }}
- --incidents=/etc/lapilli/incidents/incidents.yaml
{{- end }}
{{- if kindIs "map" .Values.impact }}
- --impact-file=/etc/lapilli/impact/impact.yaml
{{- else if .Values.impact }}
- {{ printf "--impact=%s" .Values.impact | quote }}
{{- end }}
{{- range .Values.match }}
- {{ printf "--match=%s" . | quote }}
{{- end }}
{{- with .Values.noisyFirings }}
- {{ printf "--noisy-firings=%d" (int .) | quote }}
{{- end }}
{{- with .Values.noisyPrecision }}
- {{ printf "--noisy-precision=%v" . | quote }}
{{- end }}
{{- if .Values.rollouts }}
- --rollouts
{{- end }}
{{- range .Values.rolloutMatch }}
- {{ printf "--rollout-match=%s" . | quote }}
{{- end }}
{{- with .Values.routine }}
- {{ printf "--routine=%v" . | quote }}
{{- end }}
{{- with .Values.noisyBy }}
- {{ printf "--noisy-by=%s" . | quote }}
{{- end }}
{{- if .Values.matchStrict }}
- --match-strict
{{- end }}
{{- if .Values.linkedOnly }}
- --linked-only
{{- end }}
{{- with .Values.hours.work }}
- {{ printf "--work-hours=%s" . | quote }}
{{- end }}
{{- with .Values.hours.sleep }}
- {{ printf "--sleep-hours=%s" . | quote }}
{{- end }}
{{- with .Values.hours.shifts }}
- {{ printf "--shifts=%s" . | quote }}
{{- end }}
{{- with .Values.maxSamples }}
- {{ printf "--max-samples=%d" (int64 .) | quote }}
{{- end }}
{{- if .Values.report.slackWebhookSecret.name }}
- {{ printf "--slack-webhook-file=/var/run/lapilli/slack/%s" .Values.report.slackWebhookSecret.key | quote }}
{{- end }}
{{- end -}}

{{- define "lapilli.mounts" -}}
{{- $p := .Values.prometheus -}}
{{- $r := .Values.rules -}}
{{- if and (eq .Values.mode "serve") $r.configMaps }}
- name: rules-0
  mountPath: /etc/lapilli/rules/0
  readOnly: true
{{- else }}
{{- range $i, $_ := $r.configMaps }}
- name: rules-{{ $i }}
  mountPath: /etc/lapilli/rules/{{ $i }}
  readOnly: true
{{- end }}
{{- end }}
{{- if .Values.incidents }}
- name: incidents
  mountPath: /etc/lapilli/incidents
  readOnly: true
{{- end }}
{{- if kindIs "map" .Values.impact }}
- name: impact
  mountPath: /etc/lapilli/impact
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
{{- if and (include "lapilli.rulerCreds" .) $r.ruler.tokenSecret.name }}
- name: ruler-token
  mountPath: /var/run/lapilli/ruler
  readOnly: true
{{- end }}
{{- if and .Values.loki.url .Values.loki.tokenSecret.name }}
- name: loki-token
  mountPath: /var/run/lapilli/loki
  readOnly: true
{{- end }}
{{- if and (eq .Values.mode "serve") $r.loki.configMaps }}
- name: loki-rules-0
  mountPath: /etc/lapilli/loki-rules/0
  readOnly: true
{{- else }}
{{- range $i, $_ := $r.loki.configMaps }}
- name: loki-rules-{{ $i }}
  mountPath: /etc/lapilli/loki-rules/{{ $i }}
  readOnly: true
{{- end }}
{{- end }}
{{- if include "lapilli.saToken" . }}
- name: service-account-token
  mountPath: /var/run/secrets/kubernetes.io/serviceaccount
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
{{- if and (eq .Values.mode "serve") $r.configMaps }}
{{- /* One directory for serve, which reads one: every ConfigMap's keys,
which must not share a name, side by side. */}}
- name: rules-0
  projected:
    sources:
      {{- range $r.configMaps }}
      - configMap:
          name: {{ . }}
      {{- end }}
{{- else }}
{{- range $i, $name := $r.configMaps }}
- name: rules-{{ $i }}
  configMap:
    name: {{ $name }}
{{- end }}
{{- end }}
{{- if .Values.incidents }}
- name: incidents
  configMap:
    name: {{ include "lapilli.fullname" . }}-incidents
{{- end }}
{{- if kindIs "map" .Values.impact }}
- name: impact
  configMap:
    name: {{ include "lapilli.fullname" . }}-impact
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
{{- if and (include "lapilli.rulerCreds" .) $r.ruler.tokenSecret.name }}
- name: ruler-token
  secret:
    secretName: {{ $r.ruler.tokenSecret.name }}
{{- end }}
{{- if and .Values.loki.url .Values.loki.tokenSecret.name }}
- name: loki-token
  secret:
    secretName: {{ .Values.loki.tokenSecret.name }}
{{- end }}
{{- if and (eq .Values.mode "serve") $r.loki.configMaps }}
{{- /* One directory for serve, as for rules.configMaps. */}}
- name: loki-rules-0
  projected:
    sources:
      {{- range $r.loki.configMaps }}
      - configMap:
          name: {{ . }}
      {{- end }}
{{- else }}
{{- range $i, $name := $r.loki.configMaps }}
- name: loki-rules-{{ $i }}
  configMap:
    name: {{ $name }}
{{- end }}
{{- end }}
{{- if .Values.report.slackWebhookSecret.name }}
- name: slack
  secret:
    secretName: {{ .Values.report.slackWebhookSecret.name }}
{{- end }}
{{- if include "lapilli.saToken" . }}
# What Kubernetes would mount in every container, mounted in lapilli's alone.
- name: service-account-token
  projected:
    sources:
      - serviceAccountToken:
          path: token
          expirationSeconds: 3607
      - configMap:
          name: kube-root-ca.crt
          items:
            - key: ca.crt
              path: ca.crt
      - downwardAPI:
          items:
            - path: namespace
              fieldRef:
                # Said, as the API server would default it: a GitOps diff of
                # the applied fields sees a default inside an atomic list.
                apiVersion: v1
                fieldPath: metadata.namespace
{{- end }}
{{- end -}}

{{/* The pod's securityContext, the container's, and where it runs. */}}
{{/* Where people reach the UI: serve.externalURL, or worked out from the
ingress's first host or the HTTPRoute's first hostname, https unless their
https says false. Hosts with a wildcard name no one place. */}}
{{- define "lapilli.externalURL" -}}
{{- $s := .Values.serve -}}
{{- $ihosts := list -}}
{{- if $s.ingress.enabled }}{{ range $s.ingress.hosts }}{{ if not (contains "*" (toString .)) }}{{ $ihosts = append $ihosts (toString .) }}{{ end }}{{ end }}{{ end -}}
{{- $rhosts := list -}}
{{- if $s.httpRoute.enabled }}{{ range $s.httpRoute.hostnames }}{{ if not (contains "*" (toString .)) }}{{ $rhosts = append $rhosts (toString .) }}{{ end }}{{ end }}{{ end -}}
{{- if $s.externalURL -}}
{{- $s.externalURL -}}
{{- else if $ihosts -}}
{{- printf "%s://%s" (ternary "https" "http" (ne (toString $s.ingress.https) "false")) (first $ihosts) -}}
{{- else if $rhosts -}}
{{- printf "%s://%s" (ternary "https" "http" (ne (toString $s.httpRoute.https) "false")) (first $rhosts) -}}
{{- end -}}
{{- end -}}

{{/* The host names lapilli answers to besides those it always does:
serve.allowedHosts, the ingress's hosts and the HTTPRoute's hostnames,
comma-separated. */}}
{{- define "lapilli.allowedHosts" -}}
{{- $s := .Values.serve -}}
{{- $h := list -}}
{{- range $s.allowedHosts }}{{ $h = append $h (toString .) }}{{ end -}}
{{- if $s.ingress.enabled }}{{ range $s.ingress.hosts }}{{ if not (contains "*" (toString .)) }}{{ $h = append $h (toString .) }}{{ end }}{{ end }}{{ end -}}
{{- if $s.httpRoute.enabled }}{{ range $s.httpRoute.hostnames }}{{ if not (contains "*" (toString .)) }}{{ $h = append $h (toString .) }}{{ end }}{{ end }}{{ end -}}
{{- join "," (uniq $h) -}}
{{- end -}}

{{/* Whether the UI is served outside the cluster: a LoadBalancer or
NodePort, an Ingress, or an HTTPRoute. */}}
{{- define "lapilli.exposed" -}}
{{- $s := .Values.serve -}}
{{- if or (ne ($s.service.type | default "ClusterIP") "ClusterIP") $s.ingress.enabled $s.httpRoute.enabled -}}
true
{{- end -}}
{{- end -}}

{{/* The ports of serve's Service beside the UI's. */}}
{{- define "lapilli.innerPorts" -}}
# /metrics and the health checks, alone: what Prometheus needs.
- name: metrics
  port: {{ .Values.serve.metricsPort }}
  targetPort: metrics
{{- if include "lapilli.incidentsPort" . }}
# Incidents, firings, tasks and changes, each with a token, alone: what
# jobs copying them, keeping tasks in step with a tracker, and the change
# recipes need.
- name: incidents
  port: {{ .Values.serve.incidentsAPI.port }}
  targetPort: incidents
{{- end }}
{{- $n := .Values.serve.notificationsAPI }}
{{- if or $n.tokenSecret.name $n.grafanaSecret.name $n.snsTopics }}
# Notifications as alert managers sent them, alone.
- name: notifications
  port: {{ $n.port }}
  targetPort: notifications
{{- end }}
{{- if .Values.serve.backtest.enabled }}
# Pull request backtests from CI jobs, alone; FULLNAME-backtest has it too.
- name: backtest
  port: {{ int .Values.serve.backtest.port }}
  targetPort: backtest
{{- end }}
{{- end -}}

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
{{- $a := deepCopy (.Values.podAnnotations | default dict) }}
{{- /* --impact-file is read as serve starts: a change to the impact
mapping changes the pod, which starts it again with the file. */ -}}
{{- if kindIs "map" .Values.impact }}
{{- $_ := set $a "checksum/impact" (toYaml .Values.impact | sha256sum) }}
{{- end }}
{{- if and (eq .Values.mode "serve") .Values.serve.rulesHistory.repository }}
{{- /* The rules history's containers run git, and RENDER, on what a
repository holds: EKS's webhook (IRSA, Pod Identity) and Azure's give them
no cloud identity. GKE's metadata server answers every container alike. */ -}}
{{- range $k, $sep := dict "eks.amazonaws.com/skip-containers" "," "azure.workload.identity/skip-containers" ";" }}
{{- $skip := include "lapilli.historyContainers" $ | splitList " " | join $sep }}
{{- with index $a $k }}{{ $skip = printf "%s%s%s" (toString .) $sep $skip }}{{ end }}
{{- $_ := set $a $k $skip }}
{{- end }}
{{- end }}
{{- with $a }}
annotations:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- end -}}

{{/* Whether lapilli's own container is given the service account's token,
where Kubernetes would mount it: OKE's workload identity reads it there;
GKE's, EKS's and Azure's give theirs another way, and have it as before.
Only that container: the pod's others, the rules history's, run git and
RENDER on what a repository holds. "true", or nothing. */}}
{{- define "lapilli.saToken" -}}
{{- $oci := false }}
{{- range .Values.rules.alarms }}
{{- if eq (include "lapilli.alarmsKind" (toString .url)) "oci" }}{{ $oci = true }}{{ end }}
{{- end }}
{{- if or .Values.gcpProject .Values.prometheus.auth $oci }}true{{ end -}}
{{- end -}}

{{/* The image, by its digest when there is one: a release's chart has its
own image's (the lapilli.dev/image-digest annotation, and image.digest), so
the pod runs the image the release built and attested, whatever the tag
points to later. Another tag kept with the release's digest would run the
release's image under another version's name: refused. */}}
{{- define "lapilli.image" -}}
{{- $i := .Values.image -}}
{{- $own := index (.Chart.Annotations | default dict) "lapilli.dev/image-digest" | default "" -}}
{{- if and $i.digest (not (regexMatch "^sha256:[0-9a-f]{64}$" $i.digest)) -}}
{{- fail "image.digest: want sha256: and 64 hex digits, or empty" -}}
{{- end -}}
{{- if and $i.tag (ne $i.tag .Chart.AppVersion) $own (eq $i.digest $own) -}}
{{- fail (printf "image.tag %s: image.digest is %s's, this chart's own: give the digest of %s's image too, or image.digest empty" $i.tag .Chart.AppVersion $i.tag) -}}
{{- end -}}
{{- printf "%s:%s" $i.repository ($i.tag | default .Chart.AppVersion) -}}
{{- with $i.digest }}@{{ . }}{{ end -}}
{{- end -}}

{{- define "lapilli.selector" -}}
app.kubernetes.io/component: {{ .Values.mode }}
{{- end -}}

{{/* Whether the port that takes incidents, firings, tasks and changes is open:
"true", or nothing. */}}
{{- define "lapilli.incidentsPort" -}}
{{- $s := .Values.serve -}}
{{- if or $s.incidentsAPI.tokenSecret.name $s.firings.tokenSecret.name ($s.responses).tokenSecret.name $s.trackers.tokenSecret.name (and $s.changes.enabled $s.changes.tokenSecret.name) }}true{{ end -}}
{{- end -}}
