# More languages, an accessible UI, provenance beside a release, tests without races

Status: built, for v0.28.0. Four smaller pieces of the same release as
[loki.md](loki.md), each short. None adds a command or changes how a
firing is judged; none changes the ledger. They are written down so the
review can see what was decided, and why.

## 1. Spanish and Japanese

**What.** `internal/serve/i18n/es.json` and `ja.json`, every text of the UI
and every line `serve` posts, beside English and Korean. The language
picker at the top of each page offers them, each named in its own language;
the browser's `Accept-Language` picks one when nothing was picked; `--lang`
takes `es` and `ja` for what is posted to Slack and in reports. The tests
that hold Korean hold them too: every page rendered in every language finds
a translation for every text (`TestPagesInEachLanguage`), every refusal and
line told has one (`TestRefusalsTranslated`), and each takes the same
arguments as its English (`TestCatalogs`).

**Words.** As the Korean does, the words SREs use, not a literal
translation of Lapilli's own: backtest (バックテスト, backtest), alert
(アラート, alerta), fired (発火, se disparó), caught (検知, detectó),
missed (見逃し, sin detectar), noisy rule (ノイズルール, regla ruidosa),
quiet window (メンテナンスウィンドウ, ventana de mantenimiento), precision
(適合率, precisión), on-call (オンコール, guardia). The stones keep their
names (白石・黒石, piedra blanca y negra) beside ○ and ●, as the Korean
does.

**Builder's decision.** Written with a machine's help, checked by the tests
for completeness and arguments, and read through by the builder; not yet
by a native speaker. README.md and ROADMAP.md say so, and ask for
corrections. Neutral Spanish, not one country's.

## 2. Accessibility

**Why.** The UI's colours were checked for colour blindness; nothing else
was. A person on a screen reader, on the keyboard alone, or at 200% zoom
on a phone should be able to judge a rule as anyone else does.

**How it was checked.** Two ways, before and after:

- axe-core 4.14.0 in a headless Chromium (Playwright 1.64.1's image, by
  digest), against the pages the real handler serves with a ledger full of
  data: the 35 pages the language test renders, the proposals' and a 404,
  in each of the four languages, in light and dark, with the WCAG 2.0, 2.1
  and 2.2 A and AA rules; and whether a page scrolls sideways at 320 CSS
  px, which 200% zoom on a 640px window comes to (`test/a11y/axe.sh`).
- A Go test parsing every page in every language (`TestPagesAccessible`,
  with `golang.org/x/net/html`, already in the module graph and now a
  direct dependency): one h1 and no heading levels skipped; header, nav,
  main and footer; a skip link first, to main; a name for every input,
  select, textarea, link and button; alt on every image; every SVG either
  hidden or an image with a name; header cells with scope; no positive
  tabindex. `TestContrast` reads the colour tokens from the CSS and holds
  text at 4.5:1 and borders and the focus ring at 3:1, light and dark.
  `TestChartsInTables` holds that every week a chart draws is in the table
  below it, a column for each measure, and that each number a chart draws
  is the one in its week's row and its measure's column.
  `TestCSSAccessible` holds the stylesheet's part: a focus ring of 2px in
  the accent colour, the skip link hidden until it has the focus, tabs not
  chosen underlined, and no motion. `TestPagesAccessibleInMoreStates`
  checks pages in states the others don't render: a task with its
  tracker link, read-only, nothing judged yet, a proposal as an approver
  and as an admin sees it (added in review: a tracker link losing its
  words went unnoticed before).

**What was found, and fixed:**

| | Before | After |
|---|---|---|
| axe `label` (critical) | 80 nodes, the settings | 0 |
| axe `link-in-text-block` (serious) | 56 nodes, rules and incidents, both schemes | 0 |
| sideways scroll at 320px | runs 200px, outcomes 14 to 26px | 0 |
| the structure test | 1,214 problems: no skip link, one title for every page, 772 header cells without scope, 36 empty ones, controls without names | 0 |

By criterion: a skip link (2.4.1); each page's title its h1 (2.4.2);
labelled navigation for the pages and the languages, each language's name
in its own `lang` (1.3.1, 3.1.2); names for every field, comment box and
the tracker link (4.1.2); header cells with scope, and the heatmap with
row headers and its counts in text (1.3.1); the sparklines read out their
weeks, and the precision chart, said to be in the table below, now is
(1.1.1); unselected tabs underlined, link underlines and field borders at
3.2:1 where they were 1.4:1, and dark mode's "better" at 4.89:1 where it
was 4.34:1 (1.4.1, 1.4.3, 1.4.11); long words wrap and wide tables scroll
in their own box (1.4.10); one focus ring for everything, `summary`
included (2.4.7); in-page jumps land below the sticky header (2.4.11);
the tracker link at least 24px square (2.5.8). There is no animation, so
nothing for reduced motion. The look is otherwise unchanged; screenshots
were compared light, dark and at 320px.

**Not fixed.** The heatmap cuts a long rule's name short; the whole name is
in its link and on the rule's page. The rules page is about 9% larger for
the weeks its sparklines now read out; its render time and the scale
budget are unchanged.

**Builder's decision.** No script, as the principle has it: every fix is
HTML and CSS. The axe check isn't in CI: it pulls a browser image; the Go
tests that hold its findings are.

## 3. Provenance as a release asset

**Why.** Each release's archives, `SHA256SUMS`, chart and image are
attested (SLSA provenance, signed with Sigstore through GitHub's OIDC
token), and the attestations are kept in GitHub's attestation store.
OpenSSF Scorecard's Signed-Releases check looks at a release's assets, not
that store, and someone checking a download without GitHub's API has
nothing to check it against.

**What.** `actions/attest-build-provenance` (v4.2.2, through
`actions/attest` v4.2.1) writes each attestation's Sigstore bundle to a
file of its own, `RUNNER_TEMP/XXXXXX/attestation.json`, one JSON object on
one line, and says where as `bundle-path` (read from its source at the
pinned commits). The release workflow gives both attestation steps an id,
puts their two bundles together, a line each, as
`dist/lapilli-vX.Y.Z.intoto.jsonl`, checks that the file is two lines,
each a Sigstore bundle (`mediaType` `application/vnd.dev.sigstore.bundle…`)
of an in-toto statement, and publishes it with the release, its seventh
asset. A bundle that isn't there, or isn't one, stops the workflow before
the GitHub release is made; the image and the chart are pushed to ghcr.io
before that, as the image's attestation needs the image's digest, and
stay there, attested in GitHub's store. SECURITY.md says how to check a
download with it, offline: `gh attestation verify FILE --bundle
lapilli-vX.Y.Z.intoto.jsonl --custom-trusted-root trusted_root.jsonl --repo
lapilli-project/lapilli --signer-workflow
lapilli-project/lapilli/.github/workflows/release.yml --source-ref
refs/tags/vX.Y.Z`, or cosign; both were checked against v0.27.0's
attestation (`gh attestation download`), each passing with its tag and
failing with another's.

**Checked how.** The workflow can't be run here. Each step was read
against the actions' source at the pinned commits; `actionlint` (with
shellcheck) passes; the `jq` check was run against bundles of both media
type forms, and against a file of three lines, which it refuses.

**Builder's decisions.**

- `.intoto.jsonl`, the name Scorecard counts as provenance, rather than
  `.sigstore.json`, which it counts as a signature: the bundles hold SLSA
  provenance. One file for the release, as `gh attestation verify
  --bundle` reads a file of several and picks the bundle whose subject is
  the file checked.
- The image's bundle is in the same file. Checking the image with it still
  asks the registry for the image's digest.

## 4. Tests without races

**What.** `go test -race ./internal/serve` failed: 70 race reports over the
package, 80 in the `TestTrackerRecipe*` tests run alone, every one in
`trackers_recipes_test.go`. The fake trackers (GitHub, GitLab, Jira,
Linear and the rest, standing in for what the recipes call) held a lock in
their handlers, while the tests read and wrote the same issues, counts and
switches without it.

**Fixed** in the test alone: the fakes have helpers that take the lock
(`with`, `created`, `plant`, `copyOf`, `last`), and the tests go through
them; what each test checks is unchanged. No production code raced. After:
none, the package passing under `-race`. Taking one lock out again brings
12 reports back.

**Builder's decision.** Under `-race` the package takes nine to ten
minutes on a busy machine, close to `go test`'s default timeout: run it
with `-timeout 30m`. CI doesn't run `-race` today; whether it should is a
question below.

## Questions for review

- Should the Spanish be one country's? Neutral Spanish reads a little
  stiff to everyone.
- Should the release's bundles also be signed as a whole, so the file
  itself can be checked before it is read? Each bundle inside is checked
  on its own already.
- Should CI run `go test -race`? It would have caught the trackers' fakes
  when they were written, at ten minutes a run.
- Should axe-core run in CI too, or stay a script to run by hand beside
  the Go tests that hold what it found?
