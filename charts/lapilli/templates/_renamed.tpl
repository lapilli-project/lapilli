{{/* The values renamed since 1.0's promise began: from the next major release
only, the old key is not taken. Each entry is the old key's dotted path, then
to (the new key's path), serveOnly (0.71.0 read it in serve mode alone: in
cronjob mode it stays ignored) and was (the old key's default in 0.71.0, the last
release before the rename; left out when 0.71.0 had no such key). The new
key's default is values.yaml's. The old name goes out of values.yaml, which
has the new; docs/upgrading.md lists them. */}}
{{- define "lapilli.renamedValues" -}}
serve.incidentsAPI.port:
  to: serve.jobsAPI.port
  was: 8082
serve.incidentsAPI.from:
  to: serve.jobsAPI.from
  was: []
serve.incidentsAPI.copy:
  to: serve.jobsAPI.copy
gcpProject:
  to: prometheus.googleProject
  was: ""
serve.changes.tokenSecret:
  to: serve.changes.recipeTokenSecret
  was: {name: "", key: token}
serve.checkContent.alertmanager:
  to: alertmanager
  serveOnly: true
  was:
    secret: {name: "", key: alertmanager.yaml}
    configMap: {name: "", key: alertmanager.yaml}
    templateConfigMaps: []
    url: ""
serve.checkContent.scrapeInterval:
  to: scrapeInterval
  serveOnly: true
  was: ""
serve.roles.groupsHeader:
  to: serve.groupsHeader
  was: ""
{{- end -}}
