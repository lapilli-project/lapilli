#!/bin/sh
# Puts docs/ on lapilli.dev: each page docs/README.md lists, and
# docs/README.md itself, copied into site/docs/ at /docs/NAME/, in the docs
# layout, with its links made the site's: a page's to the page, an image's
# in assets/ to the copy at the site's root, anything else in the
# repository's to GitHub. The sidebar, site/_data/docs.yml, is read from
# docs/README.md's headings and lists, so the pages are listed in one place.
# A page is a list item whose first link is to docs/NAME.md. Run by
# .github/workflows/pages.yml before Jekyll, with the site's base path (""
# for lapilli.dev); site/docs/ and site/_data/docs.yml aren't kept in git.
set -eu
base=${1-}
cd "$(dirname "$0")/.."
blob=https://github.com/lapilli-project/lapilli/blob/main
out=site/docs
rm -rf "$out" site/_data/docs.yml
mkdir -p "$out" site/_data

# The pages, in docs/README.md's order.
pages=$(sed -nE 's/^- \[[^]]*\]\(([a-z0-9-]+)\.md\).*/\1/p' docs/README.md)
for f in docs/*.md; do
  n=$(basename "$f" .md)
  [ "$n" = README ] && continue
  printf '%s\n' "$pages" | grep -qx "$n" || {
    echo "docs/$n.md isn't listed in docs/README.md" >&2
    exit 1
  }
done

page() { # page SOURCE NAME URL
  if grep -q 'endraw' "$1"; then
    echo "$1 has endraw, which would end the page's raw block early" >&2
    exit 1
  fi
  # pages.yml copies assets/*.png to the site's root, and nothing else.
  if grep -Eo '\.\./assets/[^)" ]+' "$1" | grep -Ev '^\.\./assets/[^/]+\.png$'; then
    echo "$1: only a .png at the top of assets/ is on the site" >&2
    exit 1
  fi
  title=$(sed -n 's/^# //p' "$1" | head -n 1 | tr -d '`')
  [ -n "$title" ] || { echo "$1 has no # title" >&2; exit 1; }
  {
    printf -- '---\nlayout: docs\ntitle: "%s"\npermalink: %s\nsource: %s\n---\n{%% raw %%}\n' \
      "$(printf '%s' "$title" | sed 's/["\\]/\\&/g')" "$3" "$1"
    # Links: images in assets/ first, then pages, then the rest of the
    # repository. A link to a page or to the docs home keeps its #anchor.
    sed -E \
      -e "s#\]\(\.\./assets/([^)]*)\)#]($base/\1)#g" \
      -e "s#(src|srcset)=\"\.\./assets/#\1=\"$base/#g" \
      -e "s#\]\(README\.md(\#[^)]*)?\)#]($base/docs/\1)#g" \
      -e "s#\]\(([a-z0-9-]+)\.md(\#[^)]*)?\)#]($base/docs/\1/\2)#g" \
      -e "s#\]\(\.\./([^)]*)\)#]($blob/\1)#g" \
      -e "s#\]\(([a-z][^):]*)\)#]($blob/docs/\1)#g" \
      "$1"
    printf '{%% endraw %%}\n'
  } >"$out/$2.md"
}

page docs/README.md index /docs/
for n in $pages; do
  page "docs/$n.md" "$n" "/docs/$n/"
done

# The sidebar: a group for each ## heading of docs/README.md, and its pages.
awk -v base="$base" '
  function q(s) { gsub(/\\/, "\\\\", s); gsub(/"/, "\\\"", s); return "\"" s "\"" }
  /^## / { sub(/^## /, ""); print "- group: " q($0); print "  pages:"; next }
  /^- \[[^]]*\]\([a-z0-9-]+\.md\)/ {
    match($0, /^- \[[^]]*\]/); t = substr($0, 4, RLENGTH - 4)
    u = substr($0, RLENGTH + 2); sub(/\.md\).*/, "", u)
    print "    - title: " q(t); print "      url: " q("/docs/" u "/")
  }
' docs/README.md >site/_data/docs.yml
