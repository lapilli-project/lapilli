#!/bin/sh
# Writes the GitHub wiki from docs/: Home from docs/README.md, and a page
# for each page docs/README.md lists, named by its # title, which the wiki
# shows above it in place of the title. Links are made the wiki's: a
# page's to its page, an image's in assets/ to the file on GitHub, anything
# else in the repository's to GitHub. _Sidebar.md is docs/README.md's
# headings and lists; each page ends with the file it is copied from. The
# wiki is only a copy: docs/ is where the pages are changed, reviewed and
# tested, and the wiki is written again from it on every change to main
# (.github/workflows/wiki.yml), its other pages removed.
#
# wiki.sh DIR writes the pages into DIR, an empty directory or a checkout
# of the wiki, whose files other than .git are removed first: DIR outside
# the repository, read where wiki.sh is run from.
set -eu
[ -d "${1:?wiki.sh DIR}" ] || { echo "$1 isn't a directory" >&2; exit 1; }
out=$(cd "$1" && pwd -P)
cd "$(dirname "$0")/.."
here=$(pwd -P)
case "$out/" in
"$here"/*) echo "$out is in the repository: give a directory outside it" >&2; exit 1 ;;
esac
if [ ! -d "$out/.git" ] && [ -n "$(ls -A "$out")" ]; then
  echo "$out is neither empty nor a checkout of the wiki" >&2
  exit 1
fi
repo=https://github.com/lapilli-project/lapilli
blob=$repo/blob/main
raw=https://raw.githubusercontent.com/lapilli-project/lapilli/main
wiki=$repo/wiki

# The pages, in docs/README.md's order, as site/docs.sh reads them.
pages=$(sed -nE 's/^- \[[^]]*\]\(([a-z0-9-]+)\.md\).*/\1/p' docs/README.md)
for f in docs/*.md; do
  n=$(basename "$f" .md)
  [ "$n" = README ] && continue
  printf '%s\n' "$pages" | grep -qx "$n" || {
    echo "docs/$n.md isn't listed in docs/README.md" >&2
    exit 1
  }
done

# A page's wiki name: its title, without what GitHub says a wiki title
# can't hold (\ / : * ? " < > |) and without backticks, spaces as hyphens.
# What is left is letters, digits and , . ' ( ) - alone, as the name goes
# into sed's rules below and into file names.
name() {
  [ -f "docs/$1.md" ] || { echo "docs/README.md lists docs/$1.md, which isn't there" >&2; exit 1; }
  t=$(sed -n 's/^# //p' "docs/$1.md" | head -n 1)
  [ -n "$t" ] || { echo "docs/$1.md has no # title" >&2; exit 1; }
  w=$(printf '%s' "$t" | tr -d '\\/:*?"<>|`' | tr -s ' ' '-')
  printf '%s' "$w" | grep -Eqx "[A-Za-z0-9][A-Za-z0-9,.'()-]*" || {
    echo "docs/$1.md: the title \"$t\" makes a wiki name of more than letters, digits and , . ' ( ) -" >&2
    exit 1
  }
  printf '%s' "$w"
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
# The wiki's own pages, which no page may be named as.
printf '%s\n' Home _Sidebar _Footer >"$tmp/names"
# One rule a page, so a link to it, with its #anchor, goes to its wiki
# page; then the docs home, images, and the rest of the repository.
: >"$tmp/links.sed"
for n in $pages; do
  w=$(name "$n")
  grep -qix -- "$w" "$tmp/names" && {
    echo "docs/$n.md would be the wiki's page $w, which is another's" >&2
    exit 1
  }
  printf '%s\n' "$w" >>"$tmp/names"
  printf '%s %s\n' "$n" "$w" >>"$tmp/pages"
  printf 's#\\]\\(%s\\.md(\\#[^)]*)?\\)#](%s/%s\\1)#g\n' "$n" "$wiki" "$w" >>"$tmp/links.sed"
done
cat >>"$tmp/links.sed" <<EOF
s#\\]\\(README\\.md(\\#[^)]*)?\\)#]($wiki\\1)#g
s#\\]\\(\\.\\./assets/([^)]*)\\)#]($raw/assets/\\1)#g
s#(src|srcset)="\\.\\./assets/#\\1="$raw/assets/#g
s#\\]\\(\\.\\./([^)]*)\\)#]($blob/\\1)#g
s#\\]\\(([a-z][^):]*)\\)#]($blob/docs/\\1)#g
EOF

page() { # page SOURCE WIKINAME
  # The title is the wiki's own: the page starts below it.
  awk 'done || !/^# / { print; next } { done = 1; getline; if ($0 != "") print }' "$1" >"$tmp/body"
  {
    sed -E -f "$tmp/links.sed" "$tmp/body"
    printf '\n---\n\nThis page is copied from [%s](%s/%s), where it is changed: a pull request there changes it here.\n' \
      "$1" "$blob" "$1"
  } >"$out/$2.md"
}

find "$out" -mindepth 1 -maxdepth 1 ! -name .git -exec rm -rf {} +
page docs/README.md Home
while read -r n w; do
  page "docs/$n.md" "$w"
done <"$tmp/pages"

# The sidebar: each ## heading of docs/README.md, and its pages.
{
  printf '**[Lapilli docs](%s)**\n' "$wiki"
  awk '
    /^## / { sub(/^## /, ""); printf "\n**%s**\n\n", $0; next }
    /^- \[[^]]*\]\([a-z0-9-]+\.md\)/ {
      match($0, /^- \[[^]]*\]\([a-z0-9-]+\.md\)/); print substr($0, 1, RLENGTH)
    }
  ' docs/README.md | sed -E -f "$tmp/links.sed"
  printf '\nAlso at [lapilli.dev/docs](https://lapilli.dev/docs/).\n'
} >"$out/_Sidebar.md"

# Every link to the wiki goes to a page written here.
grep -ohE "\\]\\($wiki/[^)#]*" "$out"/*.md | sed "s#^](##" | sort -u | while read -r u; do
  [ "$u" = "$wiki" ] && continue
  [ -f "$out/${u#"$wiki"/}.md" ] || { echo "a link goes to $u, which isn't a page" >&2; exit 1; }
done
