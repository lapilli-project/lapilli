# Changing rules that are rendered

Status: proposed. The third kind of place a change is written by
[changes.md](changes.md)'s git path: where the rule isn't written as it is
read, but rendered, by Helm (and later Kustomize), from a template and
values. Under [GOVERNANCE.md](../../GOVERNANCE.md) a change to what a
recipe may write is designed first; with one maintainer it may be merged
before its week is out, and this note will say so. Its open questions are
at the end.

## Why

Many teams don't keep alert rules as rule files. Each team's rules are a
`PrometheusRule` in its own namespace, rendered from a chart, with the
thresholds and `for` in the team's values: a shared chart (a "monitoring
mixin" or a company's own) and a values file per team or per environment.
Lapilli reads those rules where they run (the cluster's PrometheusRules,
the Prometheus API, or the rendered output in a pull request's backtest),
and a person can approve a longer `for` in its UI. But the recipe that
opens the pull request finds the file by the hash of the document Lapilli
read, and no file in the repository is that document: it tells Lapilli the
change failed, and a person has to make it by hand. For these teams, the
step changes.md was written for, one click by a person who may make it,
doesn't exist.

## What it does

The changes recipe (`recipes/changes/lib.sh`) gets a second way to find
where a rule is written, used only when the hash finds no file, and only
when it is set up for it:

- `RENDER`, `RENDER_PREPARE`: the command that renders the repository's
  rules, as the backtest recipe has them (`helm template team-a charts/alerts
  -f values/team-a.yaml`), run in the clone at `BASE`.
- `RENDER_VALUES`: the values files, space-separated and in the order the
  command gives them, that a change may be written to. Nothing else is
  written: not a template, not a file `RENDER_VALUES` doesn't name, and not
  a file outside `RULES_PATHS`.

For a proposal whose document no file is, the recipe:

1. **Renders** the clone at `BASE`, and finds the rule in the output by its
   identity as Lapilli keeps it (group, name, labels; the rule's own hash,
   not the document's). Not found, or found twice: it fails as today.
2. **Traces the field** the proposal changes (`for`, or a threshold) to the
   template: Helm writes `# Source: CHART/templates/FILE` above each
   document, and the recipe reads that template's text for the rule's block
   (`- alert: NAME` and the lines indented under it). The field's value
   there is one of:
   - a reference to a value, `{{ .Values.a.b.c }}` (or with `| default X`,
     or `quote`): the path `a.b.c`. Anything else between `{{ }}` (a
     function, a range variable, `include`, `tpl`) is not traced;
   - text written in the template: not traced either (below, *Templates*).
3. **Writes the value** in the last file of `RENDER_VALUES` that sets
   `a.b.c`, by editing the YAML's tree in place, as rule files are edited
   (comments, order and quoting kept; changes.md, *The edit*). A path no
   file sets isn't added: it fails, saying where the value would go.
4. **Renders again**, with the edit, and checks the output against the
   rendered output before it, document by document: the one rule changed,
   in that one field, to what Lapilli wrote, and nothing else in any
   document, any namespace. Anything else (another rule moved too, because
   two rules read the same value; a document added or gone) and the edit is
   thrown away: it fails, naming what else would change.
5. **Opens the pull request** as today, its body saying the values file and
   key it changed and the rendered diff, and tells Lapilli the file and key,
   which the proposal's page shows.

**The value is checked before it is written.** What Lapilli sends is not
trusted (changes.md): the recipe writes only a value of the field's own
kind: a duration (`^[0-9]+(ms|s|m|h|d|w|y)$`, Prometheus's form) for `for`
and `keep_firing_for`, a number for a threshold. A string that a template
could read as a template (`tpl`), a map, a list or a value with `{{` in it
is never written. So a Lapilli someone took over can move a `for` it was
asked to move, in a values file a person named, and nothing that the render
then runs differently.

**Several environments.** When the same values key renders rules in
several places (one chart, `values/prod.yaml` and `values/staging.yaml`
both set nothing and the chart's own `values.yaml` does), the edit to the
file that sets it changes every place that reads it. Step 4 sees it: with
`RENDER` rendering only one environment it can't. So `RENDER` may be given
once per environment, `RENDER_1`, `RENDER_2` and so on with their
`RENDER_VALUES_n`, and step 4 renders every one: an edit that changes a
rule anywhere but where the proposal was made fails, unless
`RENDER_SHARED=true` says a change shared by every environment is meant,
and then the pull request lists each.

## What it doesn't do

- **Templates.** A threshold written as text in a template (`expr: rate(x[5m]) > 0.05`)
  belongs to every team that renders the chart. Changing it is the chart's
  owners' decision, in the chart's own repository and release: the recipe
  fails, and the proposal's page says which template and line, for a
  person to move it into values or change it. Not built: an edit of a
  template's text. (A question below.)
- **Expressions.** A threshold inside `expr` that comes from a value
  (`> {{ .Values.errors.max }}`) is traced like any value; one built by a
  function (`{{ mul .Values.x 100 }}`) is not.
- **Kustomize, Jsonnet, Terraform.** Kustomize's patches are YAML and could
  be traced the same way (a patch file is a values file of its own kind);
  not in the first step. Jsonnet and Terraform's HCL aren't YAML: their
  rules stay "make the change where it is written". Terraform's
  `grafana_rule_group` read from Terraform's JSON is in this group.
- **Rules read back from a cluster with no repository.** A PrometheusRule
  made by hand in a cluster has no render to trace: a change to it is the
  person's, as now.
- **Anything but `for`, `keep_firing_for` and a threshold.** Turning a rule
  off or deleting it is a change of the template's shape (a condition
  around the block, or a list in values the chart may not have): the
  recipe fails for those, with the rendered diff, as now.

## Built, it would be

- `recipes/changes/lib.sh`: steps 1 to 5, in POSIX sh with jq as the
  recipe is; the YAML edit of a values file by `lapilli` itself
  (`lapilli edit-values FILE KEY VALUE`, a small command that edits a
  YAML file's tree in place, by the same code serve edits rule files
  with), so the shell does no YAML. The recipe then needs `lapilli` in its
  image as well as `helm`.
- The changes API (`--changes-token-file`): each approved proposal carries
  what the recipe needs to find the rule in the render (its group, name and
  labels as kept, and the field and value it changes), most of which it
  carries already; and takes back `{"written": {"file": ..., "key": ...}}`.
- The proposal's page: "written to values/team-a.yaml, at alerts.checkout.for".
- Tests: a chart in testdata with a team's values, the recipe's run end to
  end against a local bare repository (as the changes recipe's tests do),
  the refusals (a shared key, a traced template text, a value of the wrong
  kind, a file not in `RENDER_VALUES`), and fuzzing the template block
  reader.

## Questions for the maintainer

1. **Template text.** Should the recipe ever edit a template's text, when
   only one environment renders it (`RENDER_1` alone, the chart in the
   same repository)? Proposed: no, in the first step; the page says where.
2. **Adding a key.** When no values file sets the key the template reads
   (the chart's `default` is used), should the recipe add it to the last
   file of `RENDER_VALUES`? Proposed: yes, behind `RENDER_VALUES_ADD=true`,
   off by default, since adding a key is a new override a reviewer may not
   expect.
3. **Kustomize** in the first step, or after Helm is used?
