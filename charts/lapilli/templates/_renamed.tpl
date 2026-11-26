{{/* The values renamed since 1.0's promise began: from the next major release
only, the old key is not taken. Each line is the old key's path and the new's,
as dotted paths: "serve.oldName: serve.newName". The old name goes out of
values.yaml, which has the new; docs/upgrading.md lists them. */}}
{{- define "lapilli.renamedValues" -}}
serve.incidentsAPI.port: serve.jobsAPI.port
serve.incidentsAPI.from: serve.jobsAPI.from
serve.incidentsAPI.copy: serve.jobsAPI.copy
gcpProject: prometheus.googleProject
serve.changes.tokenSecret: serve.changes.recipeTokenSecret
serve.checkContent.alertmanager: alertmanager
serve.checkContent.scrapeInterval: scrapeInterval
serve.roles.groupsHeader: serve.groupsHeader
{{- end -}}
