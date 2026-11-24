#!/bin/sh
# Copies the recipes the chart runs (recipes/incidents and recipes/changes,
# without their READMEs or testdata) into charts/lapilli/files/recipes, from
# which serve.incidentsAPI.copy and serve.changes.pullRequests mount them.
# A test fails when the copies differ from recipes/. Run it from anywhere.
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
for dir in incidents changes; do
  dest=$root/charts/lapilli/files/recipes/$dir
  rm -rf "$dest"
  mkdir -p "$dest"
  for f in "$root/recipes/$dir"/*; do
    [ -f "$f" ] || continue
    [ "$(basename "$f")" = README.md ] && continue
    cp "$f" "$dest/"
  done
done
