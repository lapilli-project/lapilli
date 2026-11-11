# Deploy histories, as each tool documents them

Each file has the shape its tool's types give, with the revisions written
`REV_ONE`, `REV_THREE` and `REV_MERGE` for the test's own commits, which
`internal/rules/deploys_test.go` puts in (`REV_MERGE12` is the first twelve
digits, as a chart version carries them).

| File | Tool | Documentation |
|---|---|---|
| `argocd-application.json` | an Argo CD Application, `status.history` (`RevisionHistory`) and `operationState`, as `GET /api/v1/applications/NAME` and `kubectl get application -o json` give it | https://github.com/argoproj/argo-cd/blob/master/pkg/apis/application/v1alpha1/types.go |
| `flux-kustomization.json` | a Flux Kustomization with `status.history` (kustomize-controller 1.7, Flux 2.7) | https://fluxcd.io/flux/components/kustomize/kustomizations/, https://github.com/fluxcd/pkg/blob/main/apis/meta/history.go |
| `flux-kustomization-2.6.json` | one before Flux 2.7: `lastAppliedRevision` and its `Ready` condition only | as above |
| `flux-helmrelease.json` | a Flux HelmRelease's `status.history` snapshots, a chart version built from git ending `+SHA` | https://fluxcd.io/flux/components/helm/helmreleases/, https://github.com/fluxcd/helm-controller/blob/main/api/v2/snapshot_types.go |
