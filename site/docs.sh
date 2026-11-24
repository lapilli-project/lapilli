#!/bin/sh
# Puts docs/ on lapilli.dev: each page docs/README.md lists, and
# docs/README.md itself, copied into site/docs/ at /docs/NAME/, in the docs
# layout, with its links made the site's: a page's to the page, an image's
# in assets/ to the copy at the site's root, anything else in the
# repository's to GitHub. The sidebar, site/_data/docs.yml, is read from
# docs/README.md's headings and lists, so the pages are listed in one place.
# A page is a list item whose first link is to docs/NAME.md, and its
# description, for search results and links shared, is the rest of that
# item, or for the docs home its first sentence. Run by
# .github/workflows/pages.yml before Jekyll, with the site's base path (""
# for lapilli.dev); site/docs/ and site/_data/docs.yml aren't kept in git.
set -eu
base=${1-}
cd "$(dirname "$0")/.."
blob=https://github.com/lapilli-project/lapilli/blob/main
out=site/docs
rm -rf "$out" site/_data/docs.yml
mkdir -p "$out" site/_data
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Each page's description: NAME, a tab, the words after its link in
# docs/README.md's list, the item's lines joined, without backticks or
# links' targets; the title alone where the item has nothing more. The
# docs home's is the first sentence of its first paragraph.
awk '
  function flush() { if (name != "") print name "\t" text; name = "" }
  /^- \[[^]]*\]\([a-z0-9-]+\.md\)/ {
    flush()
    match($0, /^- \[[^]]*\]/); title = substr($0, 4, RLENGTH - 4)
    rest = substr($0, RLENGTH + 2); name = rest; sub(/\.md\).*/, "", name)
    sub(/^[a-z0-9-]+\.md\)/, "", rest); sub(/^: */, "", rest)
    text = (rest == "") ? title : toupper(substr(rest, 1, 1)) substr(rest, 2); next
  }
  name != "" && /^  [^ ]/ { l = $0; sub(/^ +/, "", l); text = text " " l; next }
  { flush() }
  END { flush() }
' docs/README.md >"$tmp/descriptions"
awk 'NR > 1 && /^[^#]/ { p = p (p == "" ? "" : " ") $0; next } p != "" { exit } END { print "index\t" p }' \
  docs/README.md | sed -E 's/\. .*//; s/\.$//; s/$/./' >>"$tmp/descriptions"
sed -i.bak -E 's/`//g; s/\[([^]]*)\]\([^)]*\)/\1/g' "$tmp/descriptions"

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
  desc=$(awk -F '\t' -v n="$2" '$1 == n { print $2; exit }' "$tmp/descriptions")
  [ -n "$desc" ] || { echo "$1 has no description in docs/README.md" >&2; exit 1; }
  {
    printf -- '---\nlayout: docs\ntitle: "%s"\ndescription: "%s"\npermalink: %s\nsource: %s\n---\n{%% raw %%}\n' \
      "$(printf '%s' "$title" | sed 's/["\\]/\\&/g')" "$(printf '%s' "$desc" | sed 's/["\\]/\\&/g')" "$3" "$1"
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

# The search's index, /docs/search.json: each page's title, URL and
# description, and its ## and ### headings with the ids kramdown's GFM
# parser gives them (lowercase; letters, digits, _ and - kept; spaces to -),
# so a result can go to the section. A heading whose id kramdown makes
# otherwise, one with a letter outside ASCII or a second of the same words
# on a page, is caught by test/a11y/site.mjs, which follows every result.
{
  printf '['
  sep=
  for n in index $pages; do
    if [ "$n" = index ]; then src=docs/README.md url=/docs/; else src=docs/$n.md url=/docs/$n/; fi
    printf '%s\n' "$sep"
    sep=,
    awk -F '\t' -v n="$n" -v url="$base$url" '
      function q(s) { gsub(/\\/, "\\\\", s); gsub(/"/, "\\\"", s); return "\"" s "\"" }
      function plain(s) { gsub(/`/, "", s); while (match(s, /\[[^]]*\]\([^)]*\)/)) { t = substr(s, RSTART + 1); sub(/\].*/, "", t); s = substr(s, 1, RSTART - 1) t substr(s, RSTART + RLENGTH) } return s }
      FNR == NR { if ($1 == n) desc = $2; next }
      /^(```|~~~)/ { fence = !fence; next }
      fence { next }
      /^# / && title == "" { title = plain(substr($0, 3)); next }
      /^###? / {
        text = $0; sub(/^#+ +/, "", text); sub(/ +\{#[^}]*\} *$/, "", text)
        id = tolower(text); gsub(/[^a-z0-9_ \t-]/, "", id); gsub(/[ \t]/, "-", id)
        heads = heads (heads == "" ? "" : ",") "[" q(id) "," q(plain(text)) "]"
      }
      END { printf "{\"title\":%s,\"url\":%s,\"description\":%s,\"headings\":[%s]}", q(title), q(url), q(desc), heads }
    ' "$tmp/descriptions" "$src"
  done
  printf '\n]\n'
} >"$out/search.json"

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
