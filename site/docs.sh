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
# for lapilli.dev); site/docs/, site/ko/docs/ and site/_data/docs*.yml aren't
# kept in git.
#
# Translations: a page docs/ko/NAME.md is put at /ko/docs/NAME/ the same way,
# with docs/ko/README.md as its home and its sidebar. Line 1 of a
# translation is <!-- translated from docs/NAME.md at blob HASH -->, HASH
# what `git hash-object docs/NAME.md` said when it was translated; the page
# says so when the English page has changed since. A translation's links
# are NAME.md (a translated page), ../NAME.md (the English page),
# ../../assets/NAME.png (an image), ../../PATH (a file of the repository)
# and ../PATH (a file of docs/). docs/ko/README.md lists every page
# docs/README.md lists, as NAME.md when translated and ../NAME.md when not.
# Without docs/ko/ nothing of this happens.
set -eu
# Bytes, not characters, in every awk and sed below, as CI's mawk reads
# them: a UTF-8 awk (macOS's) cuts Korean at the wrong place otherwise.
export LC_ALL=C
base=${1-}
cd "$(dirname "$0")/.."
blob=https://github.com/lapilli-project/lapilli/blob/main
out=site/docs
kout=site/ko/docs
rm -rf "$out" "$kout" site/_data/docs.yml site/_data/docs_ko.yml
mkdir -p "$out" site/_data
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fail() { echo "$*" >&2; exit 1; }

# Each page's description: NAME, a tab, the words after its link in a
# README's list, the item's lines joined, without backticks or links'
# targets; the title alone where the item has nothing more; and a tab and
# the title. The home's is the first sentence of its first paragraph after
# the title.
describe() { # describe README
  awk '
    function flush() { if (name != "") print name "\t" text "\t" title; name = "" }
    /^- \[[^]]*\]\((\.\.\/)?[a-z0-9-]+\.md\)/ {
      flush()
      match($0, /^- \[[^]]*\]/); title = substr($0, 4, RLENGTH - 4)
      rest = substr($0, RLENGTH + 2); sub(/^\.\.\//, "", rest); name = rest; sub(/\.md\).*/, "", name)
      if (name == "README") { name = ""; next }
      sub(/^[a-z0-9-]+\.md\)/, "", rest); sub(/^: */, "", rest)
      text = (rest == "") ? title : toupper(substr(rest, 1, 1)) substr(rest, 2); next
    }
    name != "" && /^  [^ ]/ { l = $0; sub(/^ +/, "", l); text = text " " l; next }
    { flush() }
    END { flush() }
  ' "$1"
  awk '/^# / { seen = 1; next } !seen { next } /^[^#]/ { p = p (p == "" ? "" : " ") $0; next } p != "" { exit } END { print "index\t" p }' "$1" |
    sed -E 's/\. .*//; s/\.$//; s/$/./'
}
describe docs/README.md >"$tmp/descriptions"
sed -i.bak -E 's/`//g; s/\[([^]]*)\]\([^)]*\)/\1/g' "$tmp/descriptions"

# The pages, in docs/README.md's order.
pages=$(sed -nE 's/^- \[[^]]*\]\(([a-z0-9-]+)\.md\).*/\1/p' docs/README.md)
for f in docs/*.md; do
  n=$(basename "$f" .md)
  [ "$n" = README ] && continue
  printf '%s\n' "$pages" | grep -qx "$n" || fail "docs/$n.md isn't listed in docs/README.md"
done

# The translations, if there are any: docs/ko/. Each starts with its marker,
# and docs/ko/README.md lists every page.
ko=0
for f in docs/ko/*.md; do
  if [ -f "$f" ]; then ko=1; fi
done
if [ "$ko" = 1 ]; then
  [ -f docs/ko/README.md ] || fail "docs/ko/ has pages but no README.md, their home and the list of the pages"
  for f in docs/ko/*.md; do
    n=$(basename "$f" .md)
    marker=$(head -n 1 "$f")
    printf '%s\n' "$marker" | grep -Eq '^<!-- translated from docs/[A-Za-z0-9-]+\.md at blob [0-9a-f]{40} -->$' ||
      fail "$f: line 1 isn't <!-- translated from docs/$n.md at blob HASH -->, HASH 40 hex digits"
    from=$(printf '%s\n' "$marker" | sed -E 's#^<!-- translated from docs/([A-Za-z0-9-]+)\.md at blob .*#\1#')
    [ -f "docs/$from.md" ] || fail "$f: its marker names docs/$from.md, which isn't there"
    [ "$from" = "$n" ] || fail "$f: its marker is of docs/$from.md, not docs/$n.md"
  done
  # The Korean README's items: those linked as NAME.md (translated), those as
  # ../NAME.md (not), and all, in its order.
  kloc=$(sed -nE 's/^- \[[^]]*\]\(([a-z0-9-]+)\.md\).*/\1/p' docs/ko/README.md | grep -vx README || true)
  kext=$(sed -nE 's/^- \[[^]]*\]\(\.\.\/([a-z0-9-]+)\.md\).*/\1/p' docs/ko/README.md | grep -vx README || true)
  kpages=$(sed -nE 's/^- \[[^]]*\]\((\.\.\/)?([a-z0-9-]+)\.md\).*/\2/p' docs/ko/README.md | grep -vx README || true)
  for f in docs/ko/*.md; do
    n=$(basename "$f" .md)
    [ "$n" = README ] && continue
    printf '%s\n' "$kloc" | grep -qx "$n" || fail "docs/ko/$n.md isn't listed in docs/ko/README.md as $n.md"
  done
  for n in $kloc; do
    [ -f "docs/ko/$n.md" ] || fail "docs/ko/README.md lists $n.md, which isn't in docs/ko/"
  done
  for n in $kext; do
    [ -f "docs/ko/$n.md" ] && fail "docs/ko/README.md lists ../$n.md, but docs/ko/$n.md is there: list it as $n.md"
    [ -f "docs/$n.md" ] || fail "docs/ko/README.md lists ../$n.md, which isn't there"
  done
  for n in $pages; do
    printf '%s\n' "$kpages" | grep -qx "$n" || fail "docs/ko/README.md leaves out $n, which docs/README.md lists"
  done
  describe docs/ko/README.md >"$tmp/descriptions_ko"
  sed -i.bak -E 's/`//g; s/\[([^]]*)\]\([^)]*\)/\1/g' "$tmp/descriptions_ko"
fi

# A string for a double-quoted YAML scalar.
yamlq() { printf '%s' "$1" | sed 's/["\\]/\\&/g'; }

page() { # page SOURCE NAME URL LANG DESCRIPTIONS OUTDIR
  src=$1 name=$2 url=$3 lang=$4 descs=$5 dir=$6
  if grep -q 'endraw' "$src"; then
    fail "$src has endraw, which would end the page's raw block early"
  fi
  # pages.yml copies assets/*.png to the site's root, and nothing else.
  up='\.\.'
  [ "$lang" = ko ] && up='\.\./\.\.'
  if grep -Eo "$up/assets/[^)\" ]+" "$src" | grep -Ev "^$up/assets/[^/]+\.png\$"; then
    fail "$src: only a .png at the top of assets/ is on the site"
  fi
  title=$(sed -n 's/^# //p' "$src" | head -n 1 | tr -d '`')
  [ -n "$title" ] || fail "$src has no # title"
  desc=$(awk -F '\t' -v n="$name" '$1 == n { print $2; exit }' "$descs")
  [ -n "$desc" ] || fail "$src has no description in $(dirname "$src")/README.md"
  # The page's name as a file of docs/, and the same page in the other language.
  file=$name
  [ "$name" = index ] && file=README
  extra=
  if [ "$lang" = ko ]; then
    for l in $(grep -Eo '\]\([a-z0-9-]+\.md' "$src" | sed -E 's/^\]\(//; s/\.md$//' | sort -u); do
      [ "$l" = README ] || [ -f "docs/ko/$l.md" ] || fail "$src links $l.md, which isn't translated: link ../$l.md for the English page"
    done
    for l in $(grep -Eo '\]\(\.\./[a-z0-9-]+\.md' "$src" | sed -E 's/^\]\(\.\.\///; s/\.md$//' | sort -u); do
      [ -f "docs/$l.md" ] || fail "$src links ../$l.md, which isn't there"
    done
    hash=$(head -n 1 "$src" | sed -E 's/.* at blob ([0-9a-f]+) -->$/\1/')
    en=/docs/$name/
    [ "$name" = index ] && en=/docs/
    extra="original: $en"
    [ "$(git hash-object "docs/$file.md")" = "$hash" ] || extra="$extra
stale: true"
    extra="$extra
translations:
  en: $en"
    pre="lang: ko
"
  elif [ "$ko" = 1 ] && [ -f "docs/ko/$file.md" ]; then
    extra="translations:
  ko: /ko/docs/$name/"
    [ "$name" = index ] && extra="translations:
  ko: /ko/docs/"
    pre=
  else
    pre=
  fi
  {
    printf -- '---\nlayout: docs\n%stitle: "%s"\ndescription: "%s"\npermalink: %s\nsource: %s\n' \
      "$pre" "$(yamlq "$title")" "$(yamlq "$desc")" "$url" "$src"
    [ -z "$extra" ] || printf '%s\n' "$extra"
    printf -- '---\n{%% raw %%}\n'
    # Links: images in assets/ first, then pages, then the rest of the
    # repository. A link to a page or to the docs home keeps its #anchor.
    if [ "$lang" = ko ]; then
      sed -E \
        -e 1d \
        -e "s#\]\(\.\./\.\./assets/([^)]*)\)#]($base/\1)#g" \
        -e "s#(src|srcset)=\"\.\./\.\./assets/#\1=\"$base/#g" \
        -e "s#\]\(\.\./\.\./([^)]*)\)#]($blob/\1)#g" \
        -e "s#\]\(\.\./README\.md(\#[^)]*)?\)#]($base/docs/\1)#g" \
        -e "s#\]\(\.\./([a-z0-9-]+)\.md(\#[^)]*)?\)#]($base/docs/\1/\2)#g" \
        -e "s#\]\(\.\./([^)]*)\)#]($blob/docs/\1)#g" \
        -e "s#\]\(README\.md(\#[^)]*)?\)#]($base/ko/docs/\1)#g" \
        -e "s#\]\(([a-z0-9-]+)\.md(\#[^)]*)?\)#]($base/ko/docs/\1/\2)#g" \
        -e "s#\]\(([a-z][^):]*)\)#]($blob/docs/ko/\1)#g" \
        "$src"
    else
      sed -E \
        -e "s#\]\(\.\./assets/([^)]*)\)#]($base/\1)#g" \
        -e "s#(src|srcset)=\"\.\./assets/#\1=\"$base/#g" \
        -e "s#\]\(README\.md(\#[^)]*)?\)#]($base/docs/\1)#g" \
        -e "s#\]\(([a-z0-9-]+)\.md(\#[^)]*)?\)#]($base/docs/\1/\2)#g" \
        -e "s#\]\(\.\./([^)]*)\)#]($blob/\1)#g" \
        -e "s#\]\(([a-z][^):]*)\)#]($blob/docs/\1)#g" \
        "$src"
    fi
    printf '{%% endraw %%}\n'
  } >"$dir/$name.md"
}

page docs/README.md index /docs/ en "$tmp/descriptions" "$out"
for n in $pages; do
  page "docs/$n.md" "$n" "/docs/$n/" en "$tmp/descriptions" "$out"
done
if [ "$ko" = 1 ]; then
  mkdir -p "$kout"
  page docs/ko/README.md index /ko/docs/ ko "$tmp/descriptions_ko" "$kout"
  for n in $kloc; do
    page "docs/ko/$n.md" "$n" "/ko/docs/$n/" ko "$tmp/descriptions_ko" "$kout"
  done
fi

# A search's index, /docs/search.json or /ko/docs/search.json: each page's
# title, URL and description, and its ## and ### headings with the ids
# kramdown's GFM parser gives them, so a result can go to the section. That
# is: lower case; every character but a letter, digit, _ and - (and a space)
# left out, which in this awk is every ASCII punctuation character and a few
# of Unicode's (the dashes, the quotes, ·, … and the arrows), Hangul and
# other letters kept; spaces to -; the n-th repeat of an id on the page, of
# any heading, with -n. test/a11y/site.mjs follows every result and compares
# the ids with the page's.
search() { # search LANG DESCRIPTIONS
  printf '['
  sep=
  if [ "$1" = ko ]; then names="index $kpages"; else names="index $pages"; fi
  for n in $names; do
    own=1
    if [ "$n" = index ]; then
      if [ "$1" = ko ]; then src=docs/ko/README.md url=/ko/docs/; else src=docs/README.md url=/docs/; fi
    elif [ "$1" = ko ] && [ -f "docs/ko/$n.md" ]; then
      src=docs/ko/$n.md url=/ko/docs/$n/
    else
      src=docs/$n.md url=/docs/$n/
      [ "$1" = ko ] && own=0
    fi
    printf '%s\n' "$sep"
    sep=,
    awk -F '\t' -v n="$n" -v url="$base$url" -v own="$own" '
      function q(s) { gsub(/\\/, "\\\\", s); gsub(/"/, "\\\"", s); gsub(/\t/, "\\t", s); gsub(/[\001-\010\013-\037\177]/, "", s); return "\"" s "\"" }
      function plain(s) { gsub(/`/, "", s); while (match(s, /\[[^]]*\]\([^)]*\)/)) { t = substr(s, RSTART + 1); sub(/\].*/, "", t); s = substr(s, 1, RSTART - 1) t substr(s, RSTART + RLENGTH) } return s }
      function slug(s) {
        s = tolower(s)
        gsub(/·|—|–|’|‘|“|”|…|→|←/, "", s)
        gsub(/[\001-\010\013-\037\041-\054\056\057\072-\100\133-\136\140\173-\177]/, "", s)
        gsub(/[ \t]/, "-", s)
        return s
      }
      FNR == NR { if ($1 == n) { desc = $2; own_title = $3 } next }
      /^(```|~~~)/ { fence = !fence; next }
      fence { next }
      /^# / && title == "" { title = plain(substr($0, 3)) }
      /^#+ / && match($0, /^#+/) && RLENGTH <= 6 {
        # Every heading takes the id kramdown gives it, so that a repeat of
        # one on the page gets -1, -2 as kramdown counts them, whatever its
        # level; only the ## and ### ones are listed.
        text = $0; lvl = RLENGTH; sub(/^#+ +/, "", text); sub(/ +\{#[^}]*\} *$/, "", text)
        id = slug(text)
        if (id == "") id = "section"
        if (id in seen) id = id "-" ++seen[id]; else seen[id] = 0
        if (lvl == 2 || lvl == 3) heads = heads (heads == "" ? "" : ",") "[" q(id) "," q(plain(text)) "]"
      }
      END {
        if (!own) title = own_title
        printf "{\"title\":%s,\"url\":%s,\"description\":%s,\"headings\":[%s]}", q(title), q(url), q(desc), heads
      }
    ' "$2" "$src"
  done
  printf '\n]\n'
}
search en "$tmp/descriptions" >"$out/search.json"
[ "$ko" = 1 ] && search ko "$tmp/descriptions_ko" >"$kout/search.json"

# The sidebar: a group for each ## heading of a README, and its pages; of
# docs/ko/README.md, each page also with the language it is in.
sidebar() { # sidebar README LANG
  awk -v lang="$2" '
    function q(s) { gsub(/\\/, "\\\\", s); gsub(/"/, "\\\"", s); return "\"" s "\"" }
    /^## / { sub(/^## /, ""); print "- group: " q($0); print "  pages:"; next }
    /^- \[[^]]*\]\((\.\.\/)?[a-z0-9-]+\.md\)/ {
      match($0, /^- \[[^]]*\]/); t = substr($0, 4, RLENGTH - 4)
      u = substr($0, RLENGTH + 2); here = (u !~ /^\.\.\//); sub(/^\.\.\//, "", u); sub(/\.md\).*/, "", u)
      if (u == "README") next
      print "    - title: " q(t)
      if (lang == "ko") { print "      url: " q((here ? "/ko/docs/" : "/docs/") u "/"); print "      lang: " (here ? "ko" : "en") }
      else print "      url: " q("/docs/" u "/")
    }
  ' "$1"
}
sidebar docs/README.md en >site/_data/docs.yml
[ "$ko" = 1 ] && sidebar docs/ko/README.md ko >site/_data/docs_ko.yml
exit 0
