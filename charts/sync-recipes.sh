#!/bin/sh
# Copies the recipes the chart runs (recipes/incidents, recipes/changes and
# recipes/responses,
# without their READMEs or testdata) into charts/lapilli/files/recipes, from
# which serve.jobsAPI.copy (its responses/ recipes, which need ../incidents beside
# them) and serve.changes.pullRequests mount them.
# A test fails when the copies differ from recipes/. Run it from anywhere.
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
for dir in incidents changes responses; do
  dest=$root/charts/lapilli/files/recipes/$dir
  rm -rf "$dest"
  mkdir -p "$dest"
  for f in "$root/recipes/$dir"/*; do
    [ -f "$f" ] || continue
    [ "$(basename "$f")" = README.md ] && continue
    cp "$f" "$dest/"
  done
done
