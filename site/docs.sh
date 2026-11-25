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
# for lapilli.dev); site/docs/, site/LANG/docs/ and site/_data/docs*.yml
# aren't kept in git.
#
# Translations: a page docs/LANG/NAME.md, LANG a language of
# site/_data/i18n.yml's order (Korean and Japanese so far), is put at
# /LANG/docs/NAME/ the same way, with docs/LANG/README.md as its home and its
# sidebar. Line 1 of a translation is
# <!-- translated from docs/NAME.md at blob HASH -->, HASH what
# `git hash-object docs/NAME.md` said when it was translated; the page says
# so when the English page has changed since. A translation's links are
# NAME.md (a translated page), ../NAME.md (the English page),
# ../../assets/NAME.png (an image), ../../PATH (a file of the repository)
# and ../PATH (a file of docs/). docs/LANG/README.md lists every page
# docs/README.md lists, as NAME.md when translated and ../NAME.md when not.
# Without docs/LANG/ nothing of this happens for LANG.
set -eu
# Bytes, not characters, in every awk and sed below, as CI's mawk reads
# them: a UTF-8 awk (macOS's) cuts Korean at the wrong place otherwise.
export LC_ALL=C
base=${1-}
cd "$(dirname "$0")/.."
blob=https://github.com/lapilli-project/lapilli/blob/main
fail() { echo "$*" >&2; exit 1; }
out=site/docs
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
# The languages of the site but English, in site/_data/i18n.yml's order.
langs=$(sed -nE 's/^order: \[(.*)\].*/\1/p' site/_data/i18n.yml | tr ',' ' ' | tr -s ' ' '\n' | grep -vx -e en -e '' || true)
[ -n "$langs" ] || fail "site/_data/i18n.yml has no order: [en, ...] line"
rm -rf "$out" site/_data/docs.yml
for l in $langs; do
  rm -rf "site/$l/docs" "site/_data/docs_$l.yml"
done
mkdir -p "$out" site/_data

# Each page's description: NAME, a tab, the words after its link in a
# README's list, the item's lines joined, without backticks or links'
# targets; the title alone where the item has nothing more; and a tab and
# the title. The home's is the first sentence of its first paragraph after
# the title. A Japanese item's words may follow its link after a full-width
# colon, and a Japanese sentence ends at its 。.
describe() { # describe README
  awk '
    function flush() { if (name != "") print name "\t" text "\t" title; name = "" }
    /^- \[[^]]*\]\((\.\.\/)?[a-z0-9-]+\.md\)/ {
      flush()
      match($0, /^- \[[^]]*\]/); title = substr($0, 4, RLENGTH - 4)
      rest = substr($0, RLENGTH + 2); sub(/^\.\.\//, "", rest); name = rest; sub(/\.md\).*/, "", name)
      if (name == "README") { name = ""; next }
      sub(/^[a-z0-9-]+\.md\)/, "", rest); sub(/^(: *|：)/, "", rest)
      text = (rest == "") ? title : toupper(substr(rest, 1, 1)) substr(rest, 2); next
    }
    name != "" && /^  [^ ]/ { l = $0; sub(/^ +/, "", l); text = text " " l; next }
    { flush() }
    END { flush() }
  ' "$1"
  awk '/^# / { seen = 1; next } !seen { next } /^[^#]/ { p = p (p == "" ? "" : " ") $0; next } p != "" { exit } END { print "index\t" p }' "$1" |
    sed -E -e 's/。.*/。/' -e t -e 's/\. .*//' -e 's/\.$//' -e 's/$/./'
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

# The translations, if there are any: docs/LANG/ for a language of the site.
# Each page starts with its marker, and docs/LANG/README.md lists every page.
# The translated languages, in the site's order, and for each of them, in
# files under $tmp: loc_LANG, the README's items linked as NAME.md
# (translated), ext_LANG, those as ../NAME.md (not), and pages_LANG, all, in
# its order.
tlangs=
for l in $langs; do
  for f in "docs/$l"/*.md; do
    if [ -f "$f" ]; then tlangs="$tlangs $l"; break; fi
  done
done
for l in $tlangs; do
  [ -f "docs/$l/README.md" ] || fail "docs/$l/ has pages but no README.md, their home and the list of the pages"
  for f in "docs/$l"/*.md; do
    n=$(basename "$f" .md)
    marker=$(head -n 1 "$f")
    printf '%s\n' "$marker" | grep -Eq '^<!-- translated from docs/[A-Za-z0-9-]+\.md at blob [0-9a-f]{40} -->$' ||
      fail "$f: line 1 isn't <!-- translated from docs/$n.md at blob HASH -->, HASH 40 hex digits"
    from=$(printf '%s\n' "$marker" | sed -E 's#^<!-- translated from docs/([A-Za-z0-9-]+)\.md at blob .*#\1#')
    [ -f "docs/$from.md" ] || fail "$f: its marker names docs/$from.md, which isn't there"
    [ "$from" = "$n" ] || fail "$f: its marker is of docs/$from.md, not docs/$n.md"
  done
  sed -nE 's/^- \[[^]]*\]\(([a-z0-9-]+)\.md\).*/\1/p' "docs/$l/README.md" | grep -vx README >"$tmp/loc_$l" || true
  sed -nE 's/^- \[[^]]*\]\(\.\.\/([a-z0-9-]+)\.md\).*/\1/p' "docs/$l/README.md" | grep -vx README >"$tmp/ext_$l" || true
  sed -nE 's/^- \[[^]]*\]\((\.\.\/)?([a-z0-9-]+)\.md\).*/\2/p' "docs/$l/README.md" | grep -vx README >"$tmp/pages_$l" || true
  for f in "docs/$l"/*.md; do
    n=$(basename "$f" .md)
    [ "$n" = README ] && continue
    grep -qx "$n" "$tmp/loc_$l" || fail "docs/$l/$n.md isn't listed in docs/$l/README.md as $n.md"
  done
  for n in $(cat "$tmp/loc_$l"); do
    [ -f "docs/$l/$n.md" ] || fail "docs/$l/README.md lists $n.md, which isn't in docs/$l/"
  done
  for n in $(cat "$tmp/ext_$l"); do
    [ -f "docs/$l/$n.md" ] && fail "docs/$l/README.md lists ../$n.md, but docs/$l/$n.md is there: list it as $n.md"
    [ -f "docs/$n.md" ] || fail "docs/$l/README.md lists ../$n.md, which isn't there"
  done
  for n in $pages; do
    grep -qx "$n" "$tmp/pages_$l" || fail "docs/$l/README.md leaves out $n, which docs/README.md lists"
  done
  describe "docs/$l/README.md" >"$tmp/descriptions_$l"
  sed -i.bak -E 's/`//g; s/\[([^]]*)\]\([^)]*\)/\1/g' "$tmp/descriptions_$l"
done

# A string for a double-quoted YAML scalar.
yamlq() { printf '%s' "$1" | sed 's/["\\]/\\&/g'; }

page() { # page SOURCE NAME URL LANG DESCRIPTIONS OUTDIR
  src=$1 name=$2 url=$3 lang=$4 descs=$5 dir=$6
  if grep -q 'endraw' "$src"; then
    fail "$src has endraw, which would end the page's raw block early"
  fi
  # pages.yml copies assets/*.png to the site's root, and nothing else.
  up='\.\.'
  [ "$lang" != en ] && up='\.\./\.\.'
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
  pre=
  if [ "$lang" != en ]; then
    for pl in $(grep -Eo '\]\([a-z0-9-]+\.md' "$src" | sed -E 's/^\]\(//; s/\.md$//' | sort -u); do
      [ "$pl" = README ] || [ -f "docs/$lang/$pl.md" ] || fail "$src links $pl.md, which isn't translated: link ../$pl.md for the English page"
    done
    for pl in $(grep -Eo '\]\(\.\./[a-z0-9-]+\.md' "$src" | sed -E 's/^\]\(\.\.\///; s/\.md$//' | sort -u); do
      [ -f "docs/$pl.md" ] || fail "$src links ../$pl.md, which isn't there"
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
    for pl in $tlangs; do
      [ "$pl" = "$lang" ] && continue
      [ -f "docs/$pl/$file.md" ] || continue
      extra="$extra
  $pl: /$pl/docs/$name/"
      [ "$name" = index ] && extra="${extra%$name/}"
    done
    pre="lang: $lang
"
  else
    tr=
    for pl in $tlangs; do
      [ -f "docs/$pl/$file.md" ] || continue
      tr="$tr
  $pl: /$pl/docs/$name/"
      [ "$name" = index ] && tr="${tr%$name/}"
    done
    [ -z "$tr" ] || extra="translations:$tr"
  fi
  {
    printf -- '---\nlayout: docs\n%stitle: "%s"\ndescription: "%s"\npermalink: %s\nsource: %s\n' \
      "$pre" "$(yamlq "$title")" "$(yamlq "$desc")" "$url" "$src"
    [ -z "$extra" ] || printf '%s\n' "$extra"
    printf -- '---\n{%% raw %%}\n'
    # Links: images in assets/ first, then pages, then the rest of the
    # repository. A link to a page or to the docs home keeps its #anchor.
    if [ "$lang" != en ]; then
      sed -E \
        -e 1d \
        -e "s#\]\(\.\./\.\./assets/([^)]*)\)#]($base/\1)#g" \
        -e "s#(src|srcset)=\"\.\./\.\./assets/#\1=\"$base/#g" \
        -e "s#\]\(\.\./\.\./([^)]*)\)#]($blob/\1)#g" \
        -e "s#\]\(\.\./README\.md(\#[^)]*)?\)#]($base/docs/\1)#g" \
        -e "s#\]\(\.\./([a-z0-9-]+)\.md(\#[^)]*)?\)#]($base/docs/\1/\2)#g" \
        -e "s#\]\(\.\./([^)]*)\)#]($blob/docs/\1)#g" \
        -e "s#\]\(README\.md(\#[^)]*)?\)#]($base/$lang/docs/\1)#g" \
        -e "s#\]\(([a-z0-9-]+)\.md(\#[^)]*)?\)#]($base/$lang/docs/\1/\2)#g" \
        -e "s#\]\(([a-z][^):]*)\)#]($blob/docs/$lang/\1)#g" \
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
for l in $tlangs; do
  mkdir -p "site/$l/docs"
  page "docs/$l/README.md" index "/$l/docs/" "$l" "$tmp/descriptions_$l" "site/$l/docs"
  for n in $(cat "$tmp/loc_$l"); do
    page "docs/$l/$n.md" "$n" "/$l/docs/$n/" "$l" "$tmp/descriptions_$l" "site/$l/docs"
  done
done

# A search's index, /docs/search.json or /LANG/docs/search.json: each page's
# title, URL and description, and its ## and ### headings with the ids
# kramdown's GFM parser gives them, so a result can go to the section. That
# is: lower case; every character but a letter, digit, _ and - (and a space)
# left out, which in this awk is every ASCII punctuation character and a few
# of Unicode's (the dashes, the quotes, ·, … and the arrows, and the Japanese
# and full-width marks: 、。「」『』（）［］【】？！：；・～〜，．／ and the
# ideographic space), Hangul, kana, kanji and other letters kept; spaces to
# -; the n-th repeat of an id on the page, of any heading, with -n. test/a11y/site.mjs follows every result and compares
# the ids with the page's.
search() { # search LANG DESCRIPTIONS
  printf '['
  sep=
  if [ "$1" != en ]; then names="index $(cat "$tmp/pages_$1")"; else names="index $pages"; fi
  for n in $names; do
    own=1
    if [ "$n" = index ]; then
      if [ "$1" != en ]; then src=docs/$1/README.md url=/$1/docs/; else src=docs/README.md url=/docs/; fi
    elif [ "$1" != en ] && [ -f "docs/$1/$n.md" ]; then
      src=docs/$1/$n.md url=/$1/docs/$n/
    else
      src=docs/$n.md url=/docs/$n/
      [ "$1" != en ] && own=0
    fi
    printf '%s\n' "$sep"
    sep=,
    awk -F '\t' -v n="$n" -v url="$base$url" -v own="$own" '
      function q(s) { gsub(/\\/, "\\\\", s); gsub(/"/, "\\\"", s); gsub(/\t/, "\\t", s); gsub(/[\001-\010\013-\037\177]/, "", s); return "\"" s "\"" }
      function plain(s) { gsub(/`/, "", s); while (match(s, /\[[^]]*\]\([^)]*\)/)) { t = substr(s, RSTART + 1); sub(/\].*/, "", t); s = substr(s, 1, RSTART - 1) t substr(s, RSTART + RLENGTH) } return s }
      function slug(s) {
        s = tolower(s)
        gsub(/·|—|–|’|‘|“|”|…|→|←|、|。|「|」|『|』|（|）|［|］|【|】|？|！|：|；|・|～|〜|，|．|／|　/, "", s)
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
for l in $tlangs; do
  search "$l" "$tmp/descriptions_$l" >"site/$l/docs/search.json"
done

# The sidebar: a group for each ## heading of a README, and its pages; of
# docs/LANG/README.md, each page also with the language it is in.
sidebar() { # sidebar README LANG
  awk -v lang="$2" '
    function q(s) { gsub(/\\/, "\\\\", s); gsub(/"/, "\\\"", s); return "\"" s "\"" }
    /^## / { sub(/^## /, ""); print "- group: " q($0); print "  pages:"; next }
    /^- \[[^]]*\]\((\.\.\/)?[a-z0-9-]+\.md\)/ {
      match($0, /^- \[[^]]*\]/); t = substr($0, 4, RLENGTH - 4)
      u = substr($0, RLENGTH + 2); here = (u !~ /^\.\.\//); sub(/^\.\.\//, "", u); sub(/\.md\).*/, "", u)
      if (u == "README") next
      print "    - title: " q(t)
      if (lang != "en") { print "      url: " q((here ? "/" lang "/docs/" : "/docs/") u "/"); print "      lang: " (here ? lang : "en") }
      else print "      url: " q("/docs/" u "/")
    }
  ' "$1"
}
sidebar docs/README.md en >site/_data/docs.yml
for l in $tlangs; do
  sidebar "docs/$l/README.md" "$l" >"site/_data/docs_$l.yml"
done
exit 0
