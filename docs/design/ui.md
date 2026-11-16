# The web UI of `lapilli serve`

Status: phases 1 and 2 in v0.35.0; phase 3, a rule's page, in v0.37.0;
phase 4, the way to change things, in v0.38.0; phase 5, many at once and
ways in, in v0.39.0; the two items left after it (a proposal for the rules
picked, a rule's labels as chips on the list), with the rules table made to
fit a laptop's screen, in v0.40.0; first use (bundle 20), built,
unreleased; status colours (bundle 28), built, unreleased. This is how the
pages are put together: the status colours every page uses, the frame
every page has, the team and period carried from page to page, the
search, the rules list and its filters, a rule's page, the way to change
things (a proposal's page, the lists' facets, a quiet window's preview,
the Manage page), what can be done to many at once and the ways from a
number to what makes it, what a first-time reader would get stuck on, and
what is left.
[serve.md](serve.md) says what each page shows;
[access.md](access.md) how the pages meet WCAG 2.2 AA.

## What holds for every page

- **No script.** The Content-Security-Policy allows none, and no style
  either but the one stylesheet: no `<script>`, no `<style>`, no `style=`
  attribute. Everything is drawn on the server; what a script would do in
  another UI is a link, a GET or POST form, or a `<details>` here (see
  "Without a script" below).
- **Four languages.** Every text goes through `t` and is in
  `internal/serve/i18n/{es,ja,ko}.json`; `TestPagesInEachLanguage` fails on
  a text a page shows without one, and `TestFrameTextsTranslated` on a text
  a template or the frame names that the pages it renders don't show. The
  catalogs have no plurals of their own: a count of one has a text of its
  own where English says it apart ("1 line the same", "1 of 1 proposal").
  In Korean and Japanese a person is named with an honorific (님, さん),
  Lapilli itself and a recipe without ("Lapilli 가 만듦").
- **WCAG 2.2 AA.** A sign (●, ≠, ⚠) is never alone: each badge has its name
  in words, the sign `aria-hidden`. Text is 4.5:1 and the edges of controls
  3:1 on every surface, in light and dark (`TestContrast` holds the
  stylesheet's tokens to it). At 320 CSS px and at 200% zoom no page scrolls
  sideways; a wide table scrolls in its own box. A field's label holds it
  within the page's width, a select as wide as its longest option among
  them: a team's long name doesn't push the bar past 320 px.
- **Bounds.** A page asked with a query of more than 8192 bytes is
  refused, 414, before anything is read for it. A search (`q=`, the bar's
  or the rules') of more than 500 characters is left out whole, and the
  page says so; of its words, the first 20 are used, and the rest named,
  "Left out: more than 20 words". A facet takes 20 values, a value given
  twice once. Several `q=` are one search, joined. What a page does with
  what it is asked doesn't grow past these.
- **A few at a time.** A page not kept already is worked out with seven
  others at most (`pageRenders`); another waits its turn up to ten
  seconds, then is answered 503, with `Retry-After`. The turn is held
  while the page is worked out and rendered, and given back before any of
  it goes to the network, so that readers who don't read hold none
  (`TestSlowReaders`); and while the page waits for the heavy turn (the
  settings tried over a week, a report's preview), given back and taken
  again once it has that one (`TestHeavyHoldsNoTurn`). A page is rendered
  whole into one buffer; one past what the page cache keeps (2 MB: the
  rules list at two thousand rules is 5.5) goes from that buffer to the
  response, not copied into the cache's besides, and has 30 seconds to
  go to the network, then the server gives it up; an API's answer as big
  keeps the server's two minutes. Only pages wait, a quiet window's
  preview among them (`TestPreviewTakesTurn`): the API, MCP, the health
  checks, the metrics and the ports that take notifications and copies
  don't. Forty-eight `/rules` at once, at two
  thousand rules, went from a peak of 1.4 GB of heap to 550 MB.

## Status colours

What a state is reads at a glance, as it does in Argo CD: fine, needs a
look, broken, under way, or not known. Each state the UI draws is one of
five kinds, and the same kind on every page.

### The tokens

Each kind is three tokens of the stylesheet, in light and in dark: its
text colour (`--ok`), a wash (`--ok-wash`) and a line (`--ok-line`).

| Kind | What it means | Light: text, wash, line | Dark: text, wash, line | Sign |
|---|---|---|---|---|
| `ok` | fine, done, as predicted | `#1b6a4c`, `#e3f1e9`, `#3f9068` | `#86d4ac`, `#1d332a`, `#468a6a` | ✓ |
| `warn` | needs a person's look: noisy, stale, waiting for approval, going nowhere | `#8a5a00`, `#f8eed5`, `#ad7f22` | `#e6b450`, `#3a311d`, `#a07a2c` | ⚠︎ |
| `bad` | failed, missed, overdue, given up | `#a3271b`, `#fbebe8`, `#c65b4f` | `#ef8a7e`, `#3d2422`, `#ba645a` | ✕ (✗ for missed, as before) |
| `info` | under way, or in effect now | `--on-blue`, `--tint-blue`, `#6389be` | `--on-blue`, `--tint-blue`, `#5b7ea8` | ↻ under way, ● in effect |
| `unknown` | not known: no record, not judged | `--dim`, `--panel`, `--line-ui` | the same | ? or – |

The ok green is a cool one, a teal-green as Argo CD's, told apart from
the olive of the accent. Each text colour is 4.5:1 or more on its wash,
the card, the background and the panel; ink, dim and the accent are 4.5:1
or more on each wash, as a notice's body is ink, its quiet words dim and
its links olive; each line, and a link's underline (`--line-ui`), is 3:1
or more on the card, the background and the wash. `TestContrast` holds
every one of these pairs, in light and dark, and resolves a token that
names another (`var(--x)`), so a change to the palette that breaks one
fails.

`--better` and `--worse` are `--ok` and `--bad`, by those names still (a
diff's `+` and `−` lines are them). The incidents chart's fills
(`--viz-good`, `--viz-warn`, `--viz-bad`: caught, only noisy rules,
missed) are the kinds' hues as chart fills, dark mode its own: light
`#198f68`, `#ae8809`, `#bc1523`, 3.95, 3.23 and 6.24:1 on the card; dark
`#2bab81`, `#c38822`, `#d14747`, 5.02, 4.76 and 3.26:1. Every pair of them
is 8 or more apart (ΔE in OKLab, ×100) under protanopia and deuteranopia
as Machado, Oliveira and Fernandes (2009) simulate them at full severity,
the closest 8.5 in each mode; caught and missed are apart in lightness
too (OKLab L 0.58 and 0.51 in light, 0.66 and 0.59 in dark), so that they
differ for those who see neither red nor green. `TestChartFillsApart`
works these out from the stylesheet, as the dataviz validator does; the
legend and the table below the chart say them too.

### Where a kind is drawn

On small surfaces alone; never a row, a card or a page filled. A kind is a
class, `status-ok`, `status-warn`, `status-bad`, `status-info` or
`status-unknown`, which sets the kind's three colours on the element; what
it is on draws them:

- **A word, a cell, a sign**: the text colour (an overdue date, a run's
  error, the missed count in the table of the weeks, an incident's "Caught"
  cell, a tile's number, the overview's "Nothing to look at now").
- **A badge or a pill** (`.badge`, `.pill`): the text, the wash, and the
  line as its edge, its sign in its text. The shape is the badges' as
  before.
- **A notice** (`.notice`): a box of ink on the wash, a line of the kind
  4px wide down its left, its sign (`.sign`) first, in the text colour.
  What was a red box (`p.warn`, `div.warn`) is a notice of warn or bad, by
  what it says; the banners above every page (changes paused, the
  server's work, its error) are notices too.

The olive accent keeps what can be done (links, buttons, the focus ring,
the page a reader is on, a hint that suggests an action, "rules changed"
marks); the blue and the warm grey of the charts keep what caught
something and what didn't (stones, strips, timelines); the lavender ramp
keeps where the noise is, how much, not a status, with the quiet windows'
marks. The Noisy badge, which was lavender, is warn.

### The mapping

`statusKinds` (`internal/serve/statuskind.go`) is the kind of each state a
page draws from the ledger, by what it is a state of and its value; the
templates ask it (`status`), and a state it doesn't list is unknown. The
rest are written in the template that draws them.

| Kind | States |
|---|---|
| ok | a rule's `○ OK`, of a rule some of whose firings were judged; a proposal `✓ approved`, `● applied`; a task done; a rule to keep (`✓ keep`); a rule change "as predicted"; Outcomes' Runs, "all ran", the runs up to date; notifications, "Last sent from the outbox"; an incident caught (its "Caught" cell); the overview's "Nothing to look at now", the one empty state that says all is well (`allClear`); a change "better"; the incidents chart's caught |
| warn | `● Noisy`; `≠ Differs`, `◌ Not replayed`; `⚠ n to check`; rules not replayed (Outcomes' tile, the Runs page's "Not replayed" cell, "no longer replayed"); a proposal `◷ awaiting approval`, stale (`⟳`, on the list and its page), one event alone can't fire it, made by an agent (its notice, its badge, its line on the list), a backtest that didn't finish, an earlier backtest's misses, closed as expired; changes paused (the banner, the Proposals page, Settings); the record gone quiet (Outcomes' tile and note, the overview's line); what `--config` declares that can't be kept, what was given back; teams whose names differ only in case; no one can approve; the digest or a report going nowhere, a report's place gone; notices waiting, or failed and to be tried again; the check's series empty; reports stopped; a report's or a quiet window's team removed; an incident caught only by noisy rules (`◐`); a rule change "not as predicted" |
| bad | a run's error, and the server's error banner; `--config` that can't be used at all, or that the ledger couldn't be kept to; notices given up on; the digest failed; an on-call schedule's read failed; Outcomes' Runs, "n failed"; a task overdue; a proposal closed as failed, and its reason; a backtest that misses incidents; would have caught while it was off; an incident missed (`✗ nothing fired`, "Nothing fired."), the missed count of the table of the weeks; a search not understood; a change "worse" |
| info | a proposal `↻ being applied`; `✎ Proposed`; a quiet window `● active now`; the server's work (`↻`); catching up on the overview; behind on Outcomes, and its Runs tile ("all ran") while it is; a rule change "measuring", "rule written"; `⇣ Recorded only`; a task open or doing, and the overview's link to one ("task #n"); a rule to tune (`± tune`); the reason of a proposal approved or applied (it waited while its rule fired) |
| unknown | `? No record`, the weeks' "no record" tag, the notes of weeks with no record; "–"; a backtest with no record (`no_record`); a rule none of whose firings was judged (`? No record`; `ruleRow.PerfClass`); `– Silent`, `⧉ Spare`; a proposal undone, closed as refused, dropped or changed in review, and the reason of one refused; declared; a rule to retire (`⊘ retire`, struck through); a task dropped; a rule change superseded, without a week before it, taken away or replayed again; Outcomes' Runs before any run |
| no colour | what is left to do or not found, but isn't a verdict: "No proposals yet.", "No proposal waiting for approval is stale.", every other empty state; the Orphans page, whose names are neither well nor wrong |

Three were left open, and are so: catching up is info, as Lapilli does it
by itself and nothing is for a person to do (behind, on Outcomes, the
same); a task doing is info with open, both under way, as lavender was the
noise's; and the sidebar's counts have no colour, as each is a number whose
word ("to approve", "open") is read out but not shown, so a colour there
would say a state alone.

### Two rules

- **What Lapilli can't judge is never ok or bad.** No record, no verdict:
  a week, a rule or a backtest with no incident written down to judge its
  firings by is unknown, and a number of it is "–" in no colour. A rule
  none of whose firings was judged is listed as OK when it isn't noisy
  (the Performance facet counts it so), but its badge is unknown, and its
  precision "–" beside it says why in words. A headline number's change is
  better or worse only when both weeks were judged (`tile.Better`,
  `tile.Worse` stay false otherwise), and a no-record backtest's difference
  is a plain number or "–".
- **Colour never says a state alone.** Each kind has a word or a sign
  beside it: a badge its sign and name, a notice its sign and sentence, a
  cell its words or the column's. A quiet window in effect now says
  "active now"; an overdue task "overdue" (기한 지남); a headline number's
  change "better" or "worse", as a rule's panel says it.

`TestStatusKinds` renders the pages the accessibility checks do, with a
proposal in every state and the other states they lack added
(`statusStates`), and seven deployments beside them (nothing judged yet,
no record, nothing to look at, an incident only a noisy rule caught, rule
changes measured, notices sent and failed, changes paused:
`statusSites`). Each state of its table (`statusWant`) is named with the
pages it is on. On those pages it is found by its words, and every
element that says it is drawn in its kind, by its own class or the one
around it, so that a class left out fails as a kind changed does, and a
state not found fails; on every page, an element coloured that says it is
of its kind (`○ OK` of a rule with nothing judged, and "all ran" behind,
excepted, `statusAlso`). Every notice starts with its sign; "–" or no
record drawn ok or bad fails, and so does a rule none of whose firings was
judged drawn ok or bad, on the list or its page. `TestOutcomesRunsKind`
holds the Runs tile to ok, info and bad; `TestChartFillsApart` the chart's
fills. `TestA11yServe` serves `statusStates` too, so that axe checks each
kind's contrast in a browser, in light and dark.

## The frame

### The sidebar

The pages are in four groups, each named, not a link:

| Group | Pages |
|---|---|
| See | Overview `/`, Rules `/rules`, Incidents `/incidents`, Firings `/firings` |
| Fix | Tasks `/tasks`, Proposals `/proposals`, Quiet windows `/windows` |
| Report | Reports `/reports`, Outcomes `/outcomes` |
| Manage | Teams `/teams`, Roles `/roles`, Settings `/settings`, Runs `/runs`, Orphans `/orphans` |

Roles is listed for admins, who alone may open it. A group's name isn't a
link, but Manage's, which goes to `/manage`, the page of its own (below),
`aria-current` there. Tasks has the open
tasks beside it, and Proposals those waiting for an approval, each the
team's in effect (picked, or the reader's own), as their lists have them,
or all teams': two counts the ledger gives once each time it changes
(`Server.waiting`: the open tasks by team, the proposals waiting by
rule, counted by the database), not once a page.

On a screen 960 CSS px wide or more, the sidebar stays on the left as the
page scrolls, with the languages at its foot, each name kept whole (한국어
was broken between its syllables there). Narrower, or zoomed in, it is
a `<details>` under the name, closed until opened, whose summary names the
page the reader is on ("Menu · Rules"), and nothing stays put over the
page. A `<details>` can't be open on one screen and closed on another
without a script, so the sidebar is drawn twice, as the menu (`nav
aria-label="Menu"`) and as the sidebar (`nav aria-label="Pages"`), and the
stylesheet shows one; the other is `display: none`, out of the
accessibility tree. The page the reader is on has `aria-current="page"`;
the page an item is of (Rules, on a rule's page), `aria-current="true"`.
The skip link is first, as before.

### Error pages

An error page (403, 404, 414, 500, 503), and the page after signing out, are
shown to whoever asked, a stranger refused too. They are the frame alone:
no count beside Tasks or Proposals, no team or period on any link, no
reader's own team, and no status line or version (a failed run's error
can name a host inside), so that a stranger reads nothing of the ledger
or of the server from them, nor whether the team the URL names is there
(`bare`).

Where a reader goes back to after signing in is kept to 2048 bytes
(`maxNext`), as the sign-in's cookie holds it and a browser drops a cookie
past 4096: past that, the page without its query.

### Breadcrumbs

Every page starts with `<nav aria-label="Breadcrumb"><ol>`: the group, the
page, and the item, the last with `aria-current="page"`: See › Rules ›
LatencyP99; Fix › Proposals › #12. The group isn't a link, but Manage,
whose page lists its pages (Manage › Teams); the page is, with the page
context. A proposal's item is its `#id` alone, the page's title its whole
heading (`crumbed`).

### The bar

Under the breadcrumbs, every page has a search (below), and the pages that
show one team's or a period, a GET form to pick them: the team (all teams,
or one) on the overview, the rules, the incidents, the tasks and the proposals; the period
(1, 4 or 12 weeks) on the rules, the incidents and a rule's page. The
overview has no period: its tiles are last week beside the week before, by
design. The form sends the page's other parameters again, as they were, and
is named for what it chooses: "Team", "Period", or "Team and period". A
rule's page is twelve weeks unless asked, as it has always been, and its
form is sent to the rule's own page with the tab it shows, and nothing
else it was asked. On a page that shows a period of its own other than a
rule's page's twelve weeks (the rules, the incidents: four unless asked),
the bar's search has its weeks, so that a rule it opens is over them.

### A page's main action

A page whose purpose includes adding something has it in its header, beside
the title, as a link to the form on the page, `#add` alone, which loads
nothing again: Incidents "Write one down", Tasks "Add a task", Quiet
windows "Add a quiet window", Teams "Add a team", Reports "Add a report". A
reader who may not use it sees the button disabled, never hidden, with why
beside it, tied with `aria-describedby`:

| Why | Said |
|---|---|
| `--read-only` | Read-only: lapilli serve was started with --read-only |
| no roles, listening beyond loopback | Read-only: no one is given a role here |
| roles, and no one signed in | Read-only: no one is signed in |
| signed in, the role isn't enough | Editors only; Admins only (teams) |
| a team's editor, on another team's rule | Only search's editors (the owning team in `<bdi>`) |
| no destination for a report | Nowhere to send a report yet |
| an editor, and no team for a report | No team yet: an editor makes a team's report |

A team's editor may add their team's tasks, quiet windows and reports, and
no incident: the incidents' action is disabled for them, "Editors only".

The forms elsewhere on a page do the same (phase 4): where one is for those
who may change something, everyone else sees one button, disabled, naming
what it does, with why beside it, in place of the whole form (`off`, with
`offOf` for a role and `offTeam` for what is a team's, "Only search's
editors"; where several share a reason, the first says it and the rest are
the button alone, tied to it, `again`; a row's drawn quietly, `quiet`): a
quiet window's and a task's change, a team's (`?edit=`), the
settings' save, the switch that pauses changes (Admins only), a report's
change, an incident's (its change, whether it was real, a task about it),
a rule's (what to do first, its note, a fix to what it says, proposing a
change, a task about it) and a proposal's (below). Comments are as before.
The server checks every write as it did: a disabled button is never the
only check. Phase 5's are drawn so too, the overview's "Make it a task",
the firings' "These were real" and "This was real" and the rules list's
bar, and a rule's header, whose buttons disabled for one reason share it,
said after the last of them; `TestDisabledOneWay` holds every disabled
button on every page to it. A quiet button disabled looks as any disabled
one (`TestDisabledLooksDisabled`): until 0.40.1 its own look came after
the disabled one and drew it as one that could be pressed.

### Empty states

One partial, `empty`, in three kinds, as Grafana has them:

- `first`: nothing judged yet; the first run is replaying the weeks before
  it. The overview and the rules.
- `notfound`: what was asked for matches nothing. It says so, and gives the
  ways out: clear the filters; with a team picked, the same filters in all
  teams. The rules, and the search.
- `done`: nothing left to do. The overview's "What to look at": "Nothing to
  look at now.", the one that says all is well too, ok with its sign
  (`allClear`); the others ("No proposals yet.") in no colour.

## The page context

The team and the period are read from the URL once a request (`pageCtx`),
and every link from one page to another carries them, through pages that
don't use them, so they hold however the reader goes.

- `team=NAME` is that team's; `team=` with nothing after it is all teams,
  picked. A team no one has is all teams, as before.
- `weeks=1|4|12`; anything else is the page's own default (4).
- Links are made with the page's `Link`: `{{$.Link "/incidents"}}` (and
  `$.Rule NAME`, `$.Incident ID`) is the path with `team=` and `weeks=` as
  the URL had them, unless the path sets them; pairs after it set others,
  an empty value taking one out, but `team`, where it is all teams. Only
  pages are linked so: not `/static/`, `/auth/`, `/api/`, `/settings.yaml`
  or a form's POST target. A link to a rule's page from a page that shows
  a period of its own (`$.Rule`, `$.RuleAt NAME "#frag"`) has that page's
  weeks even when the URL hasn't them (`pageCtx.ofPage`): the rules list
  is four weeks unless asked and a rule's page twelve, so a row's numbers
  and the page it opens are over the same weeks. So do the overview's
  items that speak of four weeks. A form going to another page (the search), or
  a GET form of a page's own (the firings', settings' "Try them"), has
  them as hidden fields. The form that adds or changes a task is sent to
  `/tasks` with them in its URL, and the task's page it goes to after
  keeps them.
- A link that adds a task about a rule names the rule's own team
  (`team=` when no team owns it), whatever the page's: the tasks page
  picks the team in its URL for the form, and the task is its owner's.
  One about an incident, which no team owns, `team=`.

### The reader's own team

With no `team=` in the URL, a reader who edits one team, and only one,
sees that team's (`Server.myTeam`): granted a team's editor's role, and no
editor of every team, nor an admin. Everyone else sees all teams: an editor
or admin, an editor of two teams or more, a viewer, whoever isn't signed in.
The bar says the team is theirs ("payments (your team)"). Links don't carry
a team taken so, and the next page takes it again; picking all teams sends
`team=`, so a reader whose own team is taken can still see every team's.
Only the pages take it: `/api/v1/rules`, the other API and MCP take a team
only when asked.

## Search

The bar's search is `GET /search?q=`. It finds rules by name, incidents by
id or title, and teams by name, each part of `q` (words, or a phrase in
quotes) in the name, in any case: fifty of each kind at most, the closest
first (the same, then starting so, then anywhere), with how many were
found. The incidents are those of the twelve weeks kept, and any whose id
is `q`; the rules, those that fired in them and those in effect.

- One found, of every kind together, is opened (303) with the page context:
  a rule's page, an incident's, or a team's overview; but a rule or an
  incident named `.` or `..`, whose path a redirect would clean into
  another page's, is listed.
- Each word is looked for once (twenty at most, as everywhere), in names
  each lowered once. A search with no word, such as a lone quote, is none:
  the help, and nothing found.
- More rules than fifty found give a link to all of them on the rules
  page: the words as words there, quoted where one has a colon, and
  `team=`, so that the reader's own team isn't taken, and the page shows
  as many as were counted.
- A filter for the rules in `q` (`team:`, `severity:`, `is:`, `status:`,
  `has:`, `label:`, `group:`) makes it a search of the rules: 303 to the
  rules page, in its canonical form (below).
- None found says so, with links to all rules and all incidents.

Every redirect is to a fixed path here (`/rules`, `/rules/NAME`,
`/incidents/ID`, `/`), its values escaped with `url.PathEscape` and
`url.Values`; nothing typed reaches a Location's host or path unescaped.

## The rules list

### Badges

Each rule has a badge on each of three axes, and what is under way:

| Axis | Badges |
|---|---|
| How it did, over the weeks shown (one of) | `● Noisy` ("Noisy, 3 wk" when it has been noisy weeks running), `? No record` (none of its firings judged, or as many unjudged as a noisy rule fires: no incident written down, or found by `--impact`, in the week or the two before it, each week judged by its own; the overview's panel lists only those firing as often as a noisy rule), `○ OK` (judged, and not noisy), `– Silent` (no firing in the weeks shown), `⧉ Spare` (another rule caught every incident it caught, as soon) |
| Its backtest, only when it can't be taken for granted | `≠ Differs` (its replay differed from what really fired over the last week checked), `◌ Not replayed`, `⇣ Recorded only` (copied in from where it fired; its condition unknown) |
| What people mean to do (its note's status) | `✓ keep`, `± tune`, `⊘ retire` |
| Under way | `✎ Proposed` (a change waits for approval), `⚠ n to check` (what its page lists to check, below) |

Noisy wins over Spare: a rule that is both is noisy first. A week with no
record is counted, not judged (docs/design/serve.md): over several weeks,
the list judges each by its own, and says once how many of those shown had
none; with none of them judged, that no precision is known and no rule is
called noisy, links to the incidents to write them down, and shows each
rule's precision as `–`. With a team picked, by the incidents the team
owns. The overview's precision and noisy rules are `–` for such a week,
where the noise is leaves its black stones out and says so, and "What to
look at" says it first, after the rules the last digest found firing with
no record (below). Differs is
looked up for a rule that didn't fire in the weeks shown too: one replayed
to nothing while it really fired is the case it is most for. A badge with a
reason (Not replayed, Recorded only) has it in its `title`; the rule's page
says it in words, a rule that never fired too. The badge partial, `badges`,
is shared with the rule's page. Proposed and "to check" link to the rule's
proposals tab and to its list of what to check (`#checks`), in the period
the list shows. "n to check" counts the conditions its page lists, one
function working them out for both (`condsOf`): the list reads the
alarms' ties and the stale proposals once for every row. The Progress
facet's "Problems to check" (`has=facts`) still picks the rules with
problems of fact in what they say, and the legend says the badge counts
those with the rest. The suggestion (a
longer `for` that would quiet it) stays as one quiet line under the badges:
it is what to do.

Near the filters, "What the badges mean", a `<details>`, lists each badge,
its sign and its name as the facet names it, with a line on what it means,
facet by facet (`legendOf`, from the facets and `badgeMeans`).

### Filters

The facets are one GET form to `/rules`: the search box, a checkbox for
each value, the context as hidden fields (team, weeks, order), and Apply;
beside it, "Clear filters". On a screen 1600 px wide or more it is a panel
beside the table, the page let wider for it (1400 px); narrower, a
`<details>` above it, open when a filter is in effect, and the table has
the page's width (below, "The table fits a laptop's screen"). Like the
sidebar, it is drawn twice and shown once.

| Facet | Parameter | Values |
|---|---|---|
| Performance | `perf` | `noisy`, `unrecorded`, `ok`, `silent`, `spare` |
| Severity | `severity` | each the rules have; `none` for a rule without one. A value the same as one of theirs but for its case is theirs |
| Trust | `trust` | `differs`, `unreplayed`, `recorded` |
| Disposition | `status` | `keep`, `tune`, `retire`, `none` |
| Progress | `has` | `proposal`, `facts` |

Values of a facet are ORed, facets ANDed, and the search box and the team
always apply. Each value has a count: how many rules the list would show
with that value picked alone in its facet, and what the other facets pick
(`TestFacetCounts` checks it, on filters picked at random). A value with
none is shown, dimmed, and can still be picked.

A rule that didn't fire in the weeks shown is listed below the table, as
before, unless Performance picks Silent: then it is a row, its numbers
nought, so what Silent counts is what it shows. Those listed below are the
rules in effect that never fired in the twelve weeks kept, and those that
fired in them but not in the weeks shown, which the list before left out.
The header says how many rules are shown, of how many there are for the
team and the weeks: "14 of 63 rules".

Every filter in effect has a chip, a link to the same page without it,
named "Remove filter: Severity: critical": each value, the words, each
`label:` and `group:`, and the team, whose chip gives `team=`, all teams.
"Clear filters" keeps the team, the weeks and the order. The order is as
before: the noisiest first, the most re-fired, the most during deploys;
each order's link is the same page in it, every filter kept. A chip's
link, and an order's, are made from what the page read of its URL
(`rulesAsk`), with one thing changed, never from the URL itself: what the
page doesn't take, or left out, isn't in them. With no rule shown and
some listed below that didn't fire, the page says none of them fired, not
that none matches.

### A rule's labels

Each row has the labels `label:` picks the rule by as chips, under its name
and badges: the labels it fires with, its group's included, as the rules in
effect have them, or, of a rule recorded elsewhere, those all its alerts
have (`ruleMeta`), as the filter matches them; but its severity, which has
its column and its facet. Five a row, by name, the rest behind "+N", a
`<details>`. Each is `name=value` in `<bdi>`, a link to the list as it was
asked with `label:NAME=VALUE` in the search too, made from what the page
read of its URL (`rulesAsk`), as a filter chip's is: canonical, so no
redirect, and carrying no result of a bulk write. One in the search already
is drawn solid, and isn't a link; nor is one the search couldn't take,
the box reading what the link writes back as another filter, or none (a
value with a double quote, or with spaces after it, which the box takes
off; a name with "=", which it cuts the value at), or a search with its
twenty words, past 500 characters, or a page past 8192 bytes.
`TestRuleLabelChips` follows each link and holds what it shows to the rows
that had the chip, the value in any case, as the filter matches. At two
thousand rules of six labels each (`TestManyAtOnceScale`), the list worked
out afresh is 6.4 MB and 260 ms, 1.2 MB and 30 ms more than without them.

### The table fits a laptop's screen

On a real deployment (62 rules, Korean, long rule names and label values,
Chromium at 1440×900) the table was wider than its box, its last three
columns reachable only by scrolling it sideways. Measured by column, what
grew was a number's notes kept on one line (re-fired 257 px, during deploys
199, nothing 336 on the demo ledger), a name or a label value in one word
(599), a team's name (111) and the headers, and the facets beside the table
from 1200 px left it 758 px of a 1280 px screen and 850 of 1440. Now names
and label values wrap anywhere, a team's name between its words and after
its hyphens, a number's notes under it, a little smaller, a header between
its words, a little smaller (Korean's at its spaces, `keep-all`, not
between its syllables); the rule's column's floor is 10rem, not 16; the
cells are a little tighter; and the facets are beside the table from 1600
px, and, above it, side by side as they fit, so that a filter in effect,
which opens them, doesn't push the table a screen down. The table has 1008
px at 1280 and 1100 at 1440, and fits in each language with rules named as
long as a real set's and with the demo ledger's; at 320 px and at 200% zoom
it scrolls in its box. `TestA11yServe` has such rules (`longNamed`), and `test/a11y/axe.mjs`
checks the table is no wider than its box at 1280, 1440, 1600 and 1920 px,
in each language: against the stylesheet before, it found 96 (Spanish at
1280, 1096 px too wide).

### The search box

Words are parts of a rule's name, in any case, each one in it; a phrase in
quotes is one, and is how to search for a name with a colon (`"job:rate5m"`).
Filters are written `key:value`, and `key:"a value"` when the value has
spaces (`label:"a=b c"`, `team:"a b"`):

| Filter | Is |
|---|---|
| `team:NAME` | the team |
| `severity:VALUE` | the severity facet |
| `is:noisy`, `is:unrecorded`, `is:ok`, `is:silent`, `is:spare` | Performance |
| `is:differs`, `is:unreplayed`, `is:recorded` | Trust |
| `is:proposed`, `is:facts` | Progress |
| `status:keep\|tune\|retire\|none` | Disposition |
| `has:proposal\|facts` | Progress |
| `label:KEY=VALUE` | the label, as the rule fires with it (its group's included) |
| `group:NAME` | the rule's group |

**Canonical URLs.** A search with a filter a facet has, or `team:`, is sent
(303) to the URL with it as a parameter, and `q` keeps the words, `label:`
and `group:` (`/rules?q=is:noisy+err` → `/rules?perf=noisy&q=err`), so the
checkboxes always show what is in effect and unchecking one works. The
filters are moved from the whole search, and the twenty words are counted
in what stays in the box. A key the box doesn't take, a value its key
doesn't, or a team no one has stays in `q`, is left out of the filtering,
and is named in a message that lists the keys and values the box takes;
never an error page.

## A rule's page

`/rules/NAME`, in five tabs. What every tab has at its top:

### The header

- The breadcrumb, See › Rules › NAME; the name, in `<bdi>`, as the `h1`,
  and the badges as the list has them, but "to check", which the header
  says in full.
- A line of what is known of it, each only when known: its team, its
  severity (the ledger's, or its labels'), its `for` as the rules in
  effect have it, its group, and its runbook: the one a person gave its
  note in Lapilli, or, without one, the one the rule names, said so,
  "Runbook, from the rule", with the host it goes to beside it.
- Three actions: "Propose a change" (the proposals tab's forms), "Add a
  task" (the tasks page's form, about it, for the team that owns it, `team=`
  for none), "Edit the note" (the summary's form, opened by `edit=note`;
  any other `edit=` is nothing). To a reader who may not, each is shown
  disabled, `aria-describedby` the reason; those disabled for one reason
  share the sentence that says it. Proposing is for editors; a team's
  editor adds the tasks and edits the notes of their team's rules, and is
  told whose editors may for another team's ("Only search's editors").
- "⚠ n to check", as Argo CD has an application's conditions: a
  `<details id="checks">` listing, each with a link to where it is
  explained, the problems of fact in what it says (to What it says), its
  replay differing from what really fired over the last week (to the
  firings tab's replay check), why it isn't replayed, or is in part (to
  its versions), that its record is copied in, a cloud's alarm's history
  or from elsewhere (to its latest firings), that an alarm has none (to
  Runs), and proposals made against a version of it no longer in effect
  (to its proposals). With none, there is no `<details>`. The rules
  list's badge counts the same (`condsOf`).

### The panel

Five numbers over the page's period, each a card: precision (and how many
of how many caught something), firings (and of them those that caught
nothing, and how many more a quiet window excused), incidents caught (and
those it nearly caught: the weeks' near misses, an incident once, as the
latest week that names it says), human cost (the notifications sent, and
of them those while people slept and off hours; in Korean 알림 부담), and
the backtest (the share of its firings alike in its replay and in what
really fired, of how many). Each label is a `<details>` "?" with a
sentence that says what it is. A number with nothing to say is "–":
precision without a firing, notifications not kept, a backtest not
checked; never 0%.

The period is the rules list's: the weeks from the Monday 1, 4 or 12 weeks
back, to now, and the page says it: "The last 4 weeks, from 2026-10-12."
Beside each number is its change from the span before it, as long as the
period, to the second, and ending where it starts: `[from − (now − from),
from)`. A replay's check is a whole day's, so the backtest is set beside
as many whole days before as the period has checked. A number is set
beside that span only when what it comes from holds all of it, and none
of it was pruned (`--keep`): for a rule it replays, the replay's runs from
its start on; for one recorded elsewhere, its source's copies without a
gap (`Coverage`) from the span's start, and still coming, not quiet past
`--source-quiet`: a copy that stopped would read as a rule grown quieter;
for a cloud's alarm, its history from when its tie says, and its source's
copies likewise; the notifications from the first kept (they come as they
are sent, so a receiver that stops sending isn't told from a quiet rule), and only in the pages (the twelve
weeks shown), whatever the replay holds. Precision is set beside only
when the rule fired in both. The shares' changes are in points, each
signed (+3, −2, ±0), and a change better or worse says so in words as
well as colour ("+27 worse"). The period's line says how they changed
only when one is set beside.

### The strip

The last 84 days, a cell a day, drawn on the server as an SVG, each day
with a `<title>` of its numbers: caught something (a cell with a hole, as
○), caught nothing only (solid, as ●), excused only (dashed), no firing
(low). Above the cells, marks: a diamond where the rule changed (a new
version of it, or it taken away or put back); a triangle on a day it fired
during a deploy of what it is about, as the ledger judged each of its
firings (the days' tally, `ledger.RuleDeploys`: no deploy is read or
matched against labels for the page); and a bar over each day a quiet
window was over it: a day a window excused one of its firings, the
ledger's own say, and a day a window covers that applies to the rule by
its own name, team and labels, as the rules in effect have them, or, for
one recorded elsewhere, the labels all its alerts have
(`ledger.Window.Applies`, the window's match without its time).

A window that doesn't apply is passed over before its repeats are worked
out. One by a schedule (every month, or cron) is walked back from the
strip's last day, so it costs its repeats and one lookup back; past four
repeats a day, it is a lookup a day. One every day or week covers what
`Covers` says of each repeat: from its start, and from the start of the
first day of its period, by the ledger's own day starts (`Midnight`),
which aren't the hour before where the clocks skip midnight
(America/Santiago, 2026-09-06). `TestWindowDays` sets the days marked
beside `Covers` in UTC and four zones whose clocks change over the strip.
A date the pages read (`/firings?day=`, a window's until, a task's due,
the summary's from and to) is the ledger's day likewise (`dateDay`).

The legend names each kind of cell and the marks the strip has, a sign
and words; the same numbers are a table, a day a row, in a `<details>`
under it. The days a rule fired during a deploy, and how often, are on
its page for every reader who may read the rule, as the firings tab's
"During deploys" already was: when a team deploys is nothing kept from
them.

The days, and the span before the period, are one read of the rule's
daily tallies (`RuleSpans`, by the days' index, the span's start a cut of
its own); its days during deploys and its quiet windows, one read each.
At two thousand rules and twelve weeks of firings, a rule's page takes
about 60 ms, each tab and period alike (`TestRulesListScale`). Against 480
yearly windows by cron that match nothing, 440 daily windows for it with
labels it hasn't, or 50,000 deploys, it took 161, 77 and 169 ms; it takes
3, 3 and 1 ms, and 6 ms against 480 yearly windows that apply to it.

The header, the panel and the strip are worked out for the HTML page
alone, and the versions' diffs on the versions tab alone: MCP's rule tool
reads the rest, and says what it said. The page is cached by what it
reads of its query, `tab`, `weeks`, `team` and `edit`, in one order
(`ruleQuery`): another parameter, or another order, is the same page, not
worked out again, and isn't said back on it.

### The tabs

`?tab=`, a query parameter, as a rule's name may hold a `/`: links in a
`nav` named "This rule", each carrying the page context, the one shown
`aria-current`, with a count where it means something. The tabs' names
are texts of their own, in the context "rule tab" (`trIn`, kept as `rule
tab|Firings`), so that they are translated apart from the sidebar's pages
of the same words.

| Tab | Count | Sections |
|---|---|---|
| Summary (no `tab=`) | | What to do first (the suggestion, "Propose it", "Make it a task"), About this rule (its team, disposition, runbook, note, and the form to change them), Incidents it caught (what it adds to the others, spare or not), What it says (the content check, and the forms that propose a fix) |
| `firings` | its firings over the period | Latest firings (and all of them by day), Refired (by week, and its alerts that re-fired most, over the period), Notifications (by receiver, repeats), While people slept and off hours, During deploys, The replay beside what really fired (over the period, and the last firings found only one way) |
| `versions` | its versions | the table, and each of the latest twenty versions beside the one before it, line by line by their longest common run: its expression, `for`, `keep_firing_for`, and a label a line, its value quoted; a side past 400 lines is shown whole, after the one before. Copied, a diff is its lines alone |
| `proposals` | those not finished with | its proposals, the forms to propose a change (open, not in a `<details>`), the changes reviewed |
| `activity` | its tasks and comments | its tasks (and a link to add one), comments (`#comments`) |

A `tab=` that isn't one of these is the summary, and the page never says
what it was. Links to where a section was before it was a tab still land:
`#says` is on the summary; `#proposals` and `#refired` are, on the
summary, a line shown only when a link goes to it (`:target`), linking to
the tab. The links the pages make go to the tab (`?tab=proposals#proposals`);
a notice of a comment on a rule links to `?tab=activity#comments`, and one
that a rule's replay differs to `/rules?trust=differs`.
`TestRuleSectionsKept` holds every text the page had before its tabs to
some tab, and each section to its own.

### The demo ledger

`TestDemoServe` (`internal/serve/demo_test.go`) serves, at
`LAPILLI_DEMO_SERVE`, a ledger of twelve weeks of three teams' alerts
(checkout, platform, search), made from a fixed clock and seed: noisy rules
and precise ones, a spare one, one whose replay differs, some that never
fire, incidents caught, missed and caught only by noisy rules, deploys, a
weekly quiet window, a proposal to approve, tasks, comments, and versions
of the rules over time. What people kept in it is dated in its own weeks,
so its pages are the same however late it is made. Every team, rule,
label value and person in it is one it declares (`demoTeams`,
`demoRules`, `demoPods`, `demoIncidents`, `demoPeople`), and
`TestDemoLedger` holds it to that, and to what its screenshots show.
Like `TestA11yServe`, it listens on loopback alone (everyone is an admin
on it), refusing any other address before it makes anything, and stops
on SIGINT or SIGTERM, its ledger taken away. It answers `/demo-ready`,
which `test/screenshots/shots.sh` waits for before it takes the README's
screenshots, in the Playwright image `axe.sh` uses, or with `--local`
with Node and the browser of the same version; on any exit the script
stops the server and removes what it made.

## The way to change things

Phase 4: a proposal's page, the lists' facets beside the rules', a quiet
window's preview, and the Manage page.

### A diff in its options

A proposal's change and a rule's versions are shown by one component
(`diffweb.go`), its options as links: `?diff=unified|split` and
`?context=3|all`, unified and three lines by default, anything else the
default and never said back. Unified is a `pre`, a line a span with its
sign (`-`, `+`, a space), so that copied it is its lines alone; the lines
the same more than three from a change are one row, "⋯ 12 lines the same".
Side by side is a table headed with its two sides ("Now", "As proposed";
on the versions tab "The version before", "This version"), a run taken out
set beside the run put in after it, line by line, each line's sign in its
cell, the table scrolling in its own box. The line diff is the versions
tab's, by the longest common run, a side past 400 lines shown whole. A
rule's page is cached by the options on its versions tab alone
(`ruleQuery`).

### A proposal's page

`/proposals/{id}`, in Argo CD's order for a change (its diff, its sync's
dry run, its history), with the backtest in the dry run's place:

- **The header**: `#id` and what it changes in words, "Errors's for,
  2m → 15m" (a threshold from what to what, turning a rule off, deleting
  it, what it says, undoing #n), as the `h1`, its values in `<bdi>`, each
  one line, and each from what the proposal keeps apart, never read out of
  the rule's text, whose lines a group's name holding a line break (which
  0.34.1's rule keeps, as it keeps a description's) could make look like a
  `for`: the new `for` from its edit, the old one from the rule it was
  proposed against, as the rules in effect when it was made (or the
  latest) have it by its hash, and, with neither, the new one alone
  ("Errors's for, to 15m"); a threshold's from its edit; its
  state as a badge, a sign and a word: ◷ awaiting approval, ✓ approved,
  ↻ being applied (checked again and waiting for a recipe, sent and not
  yet read back, its pull request open, merged and not yet read back),
  ● applied, ↶ undone, ⊘ closed (refused, failed, dropped, changed in
  review); its rule; who made it, an agent marked so; its way (a pull
  request; Grafana's, a ruler's or a cloud's API; or for a person to
  make); when. Breadcrumb Fix › Proposals › #id.
- **The change**: the rule as it is and as proposed, in the diff's
  options, a side past 400 lines shown whole, after the other, and said
  so, as the versions tab does; for the git path, the file as the pull request changes it, in a
  `<details>`; for one with no path, the pull request's description to
  copy.
- **The backtest**: a table, now beside as proposed, from the evidence the
  proposal kept when it was made: fired, caught something, the incidents
  one misses that the other catches, the incidents caught later, each with
  its difference signed and, where better or worse, said so in words. The
  evidence holds which incidents are caught later, not by how much, so
  that is the last row: no time is made up. One that misses an incident
  the rule catches now has a warning above the table naming it. Retiring a
  rule shows its record over the weeks it carries as a table instead; a
  fix to what a rule says has nothing to backtest, and says so. Since the
  release after 0.41.0, a backtest whose period had no incident written
  down to judge its firings by (`no_record`, kept with it, or worked out
  from the ledger for one made before) says so once above the table, that
  its firings are counted, not judged, and fewer aren't better, nor more
  worse; its firings' difference is a number with no word or colour
  (`moved.Plain`), and what was caught or missed is "–", with no incident
  named. A `for` one event can't outlast, as proposed, is worked out when
  the page is read, whatever lapilli made it (changes.md, "Read when
  shown").
- **Stale**: one waiting for approval whose backtest was replayed or
  judged otherwise than this lapilli does (`verdict.Semantics`), or
  whose period's record changed since, says so at the top, "It is stale,
  so it isn't approved until it is backtested again:" and why, with a
  link to its backtest, where an editor has "Backtest again" (a form of
  its own, with what it does: replayed with this lapilli over the twelve
  weeks to now, the backtest replaced kept in the history, the approvals
  given to it withdrawn), and anyone else the button disabled with why.
  Approving it is shown disabled to an approver, "Held until it is
  backtested again: it is stale". One whose rule changed since says so as
  before. Every incident a backtest of it named as missed and this one
  doesn't is said under its table, "An earlier backtest of it, before it
  was backtested again, said that as proposed it misses:".
- **An undo's page** says it puts back the rule as it was before the
  change it undoes, linked, and, when an agent proposed that change, for
  whom. Its backtest is that change's, read the other way, "With #n"
  beside "After undoing it": undone, it misses what the change gained and
  catches what the change lost, with the warning when it misses an
  incident the change catches; the incidents the change caught later are
  caught sooner. An undo keeps of the change only its edit (where it was
  written, what of the rule it touched), never its words, where it came
  from or its backtest, which aren't the asker's and say what the change
  did; an undo made before, which kept them whole, is shown so too, and
  its pull request's description likewise.
- **The approval**: approvals n of m (an undo, one of one: approved by the
  approver who asks); who approved, when; the form, with a box "I have read
  the backtest" ("the change and its evidence" without one), `required` in
  the form and checked by the server, an approval without it refused, 400.
  To a reader who may not approve, the button disabled with why: Approvers
  only, You proposed this, Your agent proposed this, You approved it,
  Read-only, Changes are off, a change for a person to make, or, to an
  approver signed in by a bearer token as an API client is (an
  oauth2-proxy's on the `Authorization` header among them), approve from
  a browser signed in with a session: a form is taken from a session
  alone, so the page is kept apart for a bearer token. Who may
  approve is as it was. Each reason is what the handler allows: dropping
  one waiting for approval or approved is an editor's or an approver's
  ("Editors and approvers only"), one ready an approver's or an admin's
  ("Approvers and admins only"), closing an admin's, making it a task an
  editor's.
- **History**: what happened to it, in order, each with who and when:
  made (by an agent, for whom), each approval (k of m), approved or waiting
  again as `--approvals` changed, each state it moved to (with the pull
  request's link, or why it was refused or failed), an approved change
  waiting while its rule fires ("waits: it fires now", with why), an undo
  asked for, linked to it, a backtest run again ("Backtested again by bo,
  with lapilli v0.42.0", "Backtested again by an agent, for bo", or by
  Lapilli; the backtest it replaced from when to when, with its firings
  and the incidents it missed, caught and caught later; whose approvals
  were withdrawn). Its making and approvals are
  the proposal's own; the rest are the ledger's changes whose `what` is
  the proposal's (`ProposalSteps`, an additive read by the changes' index
  on `what`),
  those older than `--keep` gone, and said so. A step is told by the
  fields its change keeps, never by its text: waiting while the rule fires
  is kept as a move from approved to approved, "proposal #N approved" as
  an approval is, but with the proposal's state where an approval has how
  many approved of how many. A reason is said under the step that gave it,
  not again under each after it, which keep it. The steps are in the
  order the ledger kept them, by the changes' ids (replicas' clocks set
  their times to the second), the latest 100 of them, with "older steps
  aren't shown" when there are more. An applied change's ends with "Ask to undo
  it": undoing is a proposal of its own, approved by the approver who asks
  as they ask, checked again and carried out the same way, which the page
  says; it is what `/proposals/{id}/undo` has done since v0.22.0, and the
  one exception to `--approvals`, as changes.md has had it since. Asking
  is that approval, so it takes the same box ticked, `required` and
  checked by the server, refused, 400, without it. On a Grafana rule the
  page says undoing puts back the whole rule as it was before the change,
  annotations edited in Grafana since too, and the undo's page says it
  again.

Dropping, closing (an admin's, with a reason), making it a task and asking
for an undo are shown disabled with why where the reader may not.

The page is cached by what it reads of its query, its diff's options, the
team and the weeks, in one order (`proposalQuery`), as a rule's page is:
another parameter, an option it doesn't take, or another order is the
same page, worked out and kept once.

### The proposals list

Facets with counts, as the rules list's, as one GET form, a chip for each
filter in effect, and the team in the bar (`team=`, a team's editor's own
team taken as on the other pages); a proposal's team is its rule's.

| Facet | Parameter | Values |
|---|---|---|
| State | `state` | `proposed`, `approved`, `doing` (being applied), `applied`, `undone`, `closed`; `open` is the first three, and a state of its own (`refused`, `opened`, …) is offered as it is once asked |
| Kind | `kind` | `for`, `threshold`, `disable`, `delete`, `says`, `undo` |
| Way | `path` | `git`, `grafana`, `ruler`, `alarm`, `none` |
| Made by | `by` | `person`, `agent`, `lapilli` |

`id=` (repeatable, twenty at most, each a positive number; anything else is
left out) picks the proposals named alone, as a bulk proposal's result
links to them: the database picks them before the facets count, as it does
the team's (`ledger.ProposalPick.IDs`, `ProposalCountsOf`, additive reads),
each is a chip, "Proposals: #12", and the form and the other chips keep
them; "Clear filters" takes them away. None of them there says so, "None of the proposals picked is
here.", with Clear, not that there are none.

`stale=1` picks those waiting for approval that are stale (backtested by
another version, their period's record changed since, or their rule changed
since), worked out as they are read: the 10,000 waiting moved last are
read, with their evidence, and counted for the facets in Go, as the database
would (`staleLines`, `countLines`). It is a flag (`pickFacet.flag`): no
checkboxes, a chip, "Proposals: stale", carried by the form and the other
links as `id=` is; none stale says so, "No proposal waiting for approval is
stale.". A row waiting to be backtested again has a badge under its state,
"⟳ stale: backtest it again", and the list links to `stale=1`, "Show the
stale ones alone", when one it shows is stale. A row of a new `for` one
event can't outlast says so under its change.

Unless `state=` is given, the open ones, as the list was, those waiting for
approval first. The form sends `state=all` with what is ticked, so none
ticked is all of them; `?state=all`, `?state=applied` and `?state=refused`
mean what they meant, and a state there is no such of (`?state=bogus`)
shows none, as it did. The database picks the list, its states, kinds,
ways and makers in the query, in brief (`ProposalList`: no document, only
the evidence of an undo, a new `for` and one waiting for approval, how many
approved), and counts the facets with
`GROUP BY` (`ProposalCounts`), both additive reads: every proposal is
counted however many there are, and newer ones, dropped by the thousand,
never push an older one waiting for approval off the list. A team's are
those of its rules, named to the database 2,000 at a time. It shows 200,
saying so past them. Each count is how many the list would
show with the value picked alone in its facet and what the others pick
(`TestProposalsFacets` checks it, on filters picked at random).

### The tasks

Their status is a facet with counts in place of the status links: open
(open and being done), done, dropped, the form sending `status=all` as the
proposals' does, the team kept. `?status=done`, `dropped` and `all` mean
what they meant, and any other, `doing` among them, is the open ones, as
it was. The counts
are the team's tasks in each status, in one read (`TasksByStatus`), and
the page says how many of them it shows.

### A quiet window's preview

The form to add a quiet window, and the one to change one, have a Preview
button: the same POST to `/windows`, with `preview=1` (and
`formaction="/windows#preview"`, so that the page lands on it), checked as
saving is (the role, a team's editor's teams, the same site, the window's
own checks, shared with `PutWindow`, and the limits on how many windows
the ledger and a team keep, for a new one and for one a team's editor
moves to another of their teams: `windowsLeft`, `movedLeft`), so that a
preview that passes is a window saving keeps, and writing nothing
(`PreviewWindow`). The page comes back, `no-store`, with the form as the
window would be kept (what was typed put in again, escaped) and, above it:

- the rules it applies to: by their own names, team and labels, as the
  rules in effect have them, and those whose firings it would have
  excused, each with how many, 50 at most, the rest counted;
- how many firings that caught nothing over the last four weeks it would
  have excused, its repeats worked out once for the span as judging does
  (`ledger.Excusing`), and of them those another window excused already:
  over the latest 20,000 firings of the four weeks, in one read, said so
  when there are more;
- when it falls in the next four weeks: each repeat that starts in them,
  31 at most, as judging covers it (`Window.Falls`, from `Covers`'s own
  rule), not by the calendar alone. A repeat every day or week is covered
  from the start of the day it falls on, and where the clocks skip
  midnight (America/Santiago, America/Havana) a repeat set at midnight
  falls, by the calendar, at 23:00 the day before, which `Covers` doesn't
  take for it: the preview lists it from that day's first time, or not at
  all when it is over by then, as judging will excuse firings. `Covers`
  isn't changed in this release. `TestWindowFallsAsCovered` holds what is
  listed to `Covers`, minute by minute, in UTC and those two zones.

Preview is the form's first button, so that Enter in a field writes
nothing.

### The Manage page

`/manage`: a card for each page of the Manage group and for the reports
(their recipients and the weekly digest), each with a line on what it is
for and, where cheap, a fact (how many teams, how many reports, how far
the firings are in): Teams, Roles (admins only, as in the sidebar),
Settings, Runs, Reports, Orphans, and where notices go (below, "First
use"). The sidebar's Manage links to it, and the breadcrumbs of its pages
start with it. When `--config` can't be used, the Settings card says so.

### What `--config` declares

A team, a quiet window, a report or a role that `--config` declares
(serveops.md, "Declared configuration") is changed in the file alone. The
Teams, Quiet windows, Reports and Roles pages, and a window's and a
report's own, mark it with a badge, "declared", beside its name, as a
rule's badges are drawn. Each action on it is drawn with the `off`
template, never hidden: on a list, the row's "Change" or "Take it away"
is the button disabled, tied (`again`) to one sentence above the table,
"Declared in config — change it in values"; on its own page, or its form
asked for (`/teams?edit=ID`, `/reports?edit=ID`), "Change it" disabled
with that sentence beside it, in place of the form and its delete. A
reader who couldn't change it anyway sees the sentence, as a line, and
none of the buttons. The server refuses the write all the same, every
path, with 409.

The Settings page has a section, "Declared configuration": where the file
is, that a file is kept to once it is read the same twice in a row, what
becomes of what it no longer declares (`--config-prune`, a role taken
away), when a new file was read once and waits for its second read, when
the ledger was last kept to it and the start of its hash, and what of the
file can't be used or kept: to an admin, why, with its line, in the
reader's language; to anyone else, the line and the kind alone ("line 23:
a role that can't be kept"), as why can name who holds a role. The Runs
page says it the same way. Without `--config`, the section says nothing is
declared. What a leader without `--config` gave back of what a file
declared, which the next file lets go unless it declares it again, is
listed, to an admin (kind, name and team), with how many to anyone, how to
keep one (a role deleted and given again), and, an admin's, "Keep as the
UI's" for each and "Keep them all as the UI's", drawn off for anyone else.
A ledger write that failed, a file read once, and files read for minutes
none of them twice in a row are each said, with when. Beside `settings.yaml`, `config.yaml` (an
admin's) gives everything as `--config` takes it. The Teams page lists
teams a ledger kept from before whose names differ only in letter case,
to be renamed.

### Orphans

`/orphans`, under Manage: a rule deleted or renamed leaves its note, its
tasks, the comments on it and its proposals not finished with under a
name no rule in effect has, which no list shows. Argo CD lists resources
no application owns as orphaned; this lists the names the ledger keeps
something of that the latest version of the rules doesn't have, in byte
order, a hundred a page ("1 to 100 of 3,000 names"), with what each kept:
whether a note, its tasks and how many are open, its comments, and its
proposals, each linked. An editor moves what a name kept to a rule in
effect, typed or picked from a `<datalist>` of the rules in effect (2,000
at most: past them, typed), for a rename (`POST /orphans/move`), or
deletes it (`POST /orphans/delete`); each is noted in the activity log,
by who did it. Moved, its tasks and comments are the rule's; its note too,
or, when the rule has one, what the rule's leaves empty is filled from
it. Its proposals stay, each made against the rule as it was: they are
listed with their links, to be dropped on their pages, and a name that
keeps nothing else says so. `/proposals?stale=1` doesn't list them: it
lists those waiting for approval that are stale for any reason, a rule
changed or a backtest from another lapilli among them, and not those
approved or being applied. A name in effect isn't moved, nor to a name
none has (400), nor one over 500 bytes or with a character that doesn't
show (400), nor one the ledger keeps nothing of (404). A viewer sees the
buttons disabled, why said once above the table. The rules in effect are
the latest version `lapilli serve` read, as `--rules-select` and `--rule`
narrow it, and the page says so: a rule they leave out is listed too.
With no version recorded (`--alerts`, firings copied in alone, or none
read yet), the page says it can't tell.

The names are read one at a time, in order, from the four tables at once
(`ledger.KeptNames`): each counted, the page's kept, the rest let go; the
page's are then counted (`ledger.KeptOf`, four reads of a hundred names).
At 4,000 rules in effect and 3,000 names gone (`TestOrphansScale`), the
page allocates some 30 MB, the 4,000 names in effect the most of it. 
### What they cost

At two thousand rules, four weeks of their firings (84,000) and a thousand
proposals (`TestChangePagesScale`), worked out afresh: the proposals list
takes 20 to 45 ms, a proposal's page 15 to 25 ms (one whose rule is 600
lines a side, shown whole, too), a quiet window's preview 35 to 80 ms
(every day, once over the four weeks, by cron), the tasks and the Manage
page about a millisecond.

What a proposal says when it is read costs, at a thousand rules that count
events and a thousand proposals of a longer `for`, two thirds waiting
(`TestProposalsNowScale`, worked out afresh, the median of seven, on a
laptop): the proposals list 14.5 ms (12.6 before), with every state 13.6
(11.7), `stale=1` 16 ms, its 667 waiting read and judged, and the API's
list 12.6 ms (3.8), which now reads the rules in effect for whether each
is stale, as the list did already. The record of the backtests' periods is
one read of the incidents around them all, and a `for_outlasts` is worked
out once a proposal. Cold, with nothing worked out before, and two hundred
proposals each made against a version of the rules of its own, 1,200
rules each (`TestProposalsColdScale`): applied, the list of every state
takes 20 ms and 21 MB, and the API's 12 ms (a second and 1.4 GB, each,
before a finished proposal said what it kept); waiting, 107 ms and 68 MB,
four older versions parsed a read, the rest worked out by the reads
after.

## Many at once, and ways in

Phase 5: bulk writes on the rules list and the firings, a firing's labels
as filters and its day by the hour, the overview's tiles and charts as ways
to what makes their numbers, and an incident's timeline.

### Bulk writes

The rules list's table is a POST form to `/rules/bulk` (the facets are a
GET form of their own, beside it, not around it): a checkbox a row, its
accessible name the rule's, and a bar over the table, "With the rules
picked": Make a task for each, a team and Set the team, a disposition and
Set the disposition, each its own button (`do=task|team|status`). There is
no "pick every row": that takes a script. The firings have the same for
what caught nothing, "These were real", a form of its own above the table
whose checkboxes join it by `form=`, so that each row's own "This was
real" form stays as it was, outside it.

Each item is the write the same form makes for one, in turn, with every
check it has: a task as a rule's "Add a task" makes it (`Tune NAME`, kind
tune, the rule's team; `ledger.PutTask`, which refuses a team's editor
another team's), a note as the rule's note form keeps it, its other fields
as they are, read just before it is written, as the form shows it just
before it is sent (`ruleInScope`, `ledger.NoteOf`, then `ledger.PutNote`,
which refuses a team's editor a rule's team changed), an incident as the firing's own "This was
real" writes it (`realOne`). So the changes log has a line for each, by
whom, as one by one would, and a refusal is counted, not written: a
team's editor's rules of another team are "not your team's", and a
team's editor's "Set the team", which only editors may, "refused: editors
only", each rule, their own team's too; what the
ledger refuses for what it is (an `ErrInvalid`, as Put's checks are: a
firing longer than an incident may be, 31 days, which the single button
answers 400), "refused"; a firing that has caught something by its turn
(an incident written from one picked before it covers it), "had caught
something already". A rule is picked by its name, the spaces around it
taken off before the picks are made unique, and a rule with a task to
tune it still open, or being done, has one already: it is counted so,
and none is made. Those go on to the next. An error of another kind (the
writers' turn waited for too long, a database that stopped answering, a
lead lost) would be the same for the rest: it is counted "failed",
logged once as an error page logs it, and the write stops there, and ends
on the list it came from, which says how many weren't tried ("1 failed:
try again; stopped there: 96 not tried"), never on an error page.
`TestBulkRules` sets each beside the same writes made one at a time on a
twin ledger: the same notes, the same tasks, the same lines in the
changes log, for an editor, an admin and a team's editor;
`TestBulkKeepsNotes` holds a note's text and runbook as they were, and
`TestBulkNoteReadEach` one changed while the rules before it were
written; `TestBulkPartial` makes an item fail.

A firing is written down as real in one write of the ledger's
(`ledger.Real`), by the single button and the bulk form alike: the firing
read again in it, and nothing written when it has caught something by
then, or an incident written here from it is there already (its title,
times and labels); a write of nothing is rolled back, not counted, so
what pages keep, and other replicas, stand. Two at once, by one person or
two, write one incident, and tell of it once: `TestRealOnce` posts a hundred four times at once,
beside each firing's own button, on SQLite, and on Postgres with
`LAPILLI_TEST_POSTGRES`; it wrote 107 to 110 incidents before.

A request takes 100 at most, each once, however often it is picked; past
that it is refused whole, 400. The writes are the same doors: same-site
POSTs (a cross-site one is refused before the handler), `--read-only`,
the role (`teamWrite` for the rules, `write` for the firings). The form
names the list it came from (`back`), which is gone back to only if it is
that list's path (`safeBack`, then the path checked), with what was done
in its query, `bulk=task&done=3&theirs=2&failed=1`, with the team and
the period it was looked at for: the page says it once, in a
`role="status"` line, read within bounds (counts of 0 to 100, a kind it
knows; anything else says nothing), and none of the links or forms the
page makes carry it on, the language links (which name the page
query-escaped), the bar's form and a comment's way back among them
(`unsaid`). A link can still be made to say a result that wasn't, as any
text in a URL can be: to whoever opens that link alone, on their own
screen, and it changes nothing. The bar is disabled for whoever may not
use it, with why (`aria-describedby`); a team's editor's "Set the team"
is, "Editors only", as moving a rule into or out of a team is an
editor's, and refused when it is sent all the same.

Enter in a form presses its first button (implicit submission): from a
row's checkbox, that would be a write for every row picked. Each bulk
form's first button is a disabled one, `hidden`, so that Enter does
nothing, and no one sees it, nor does a screen reader say it; each
action is a button pressed (`TestBulkNoEnter`).

### A proposal for the rules picked

The bar has, beside the writes, "Propose for each": a select of what, a new
`for` (as the rule's proposals tab's form, the `for` typed), the `for`
suggested for each (as its summary's "Propose it"), or retiring each (as
its proposals tab's form: turned off where it is kept so that it can be,
deleted where it can't be, and, off already, deleted once it has been off
four weeks); the new `for`; and Propose (`do=propose`). A threshold and a
fix to what a rule says, each a rule's own, are on its page alone. Each
rule picked gets the proposal its page's form would make, made by the
reader, through the same doors as `POST /proposals`: an editor's (a team's
editor's are counted, each, "refused: editors only", as the single form
refuses them); its evidence as that form gives it, from the suggestion
where the digest has one, spare where it is over the weeks the list showed
(1, 4 or 12, as the rule's page opened from the list says); read again,
written where it is kept and backtested as one is (`propose`), under the
heavy turn, taken and given back for each as `POST /proposals` takes it;
one unfinished proposal a rule; twenty an hour a person, counted as one by
one. Retiring is decided by `propose`, after it has checked the reader may
propose, from the rule as it reads it again: turned off where it can be
and isn't, deleted otherwise (`kindRetire`, MCP's `retire` the same). What
a rule's page reads for its forms (the suggestions, the spare rules), and
the rules in effect, are read once for the request, under a page's turn,
given back before any proposal waits for the heavy turn: a name no rule in
effect has is refused as not in effect, and one two alike have as
`propose` refuses it, without `propose` reading the rules for each (each
read parses every rule, and a refusal before the rules are read again
costs no try). A reader past the hour's twenty, or their tries, is refused
every rule before any of it is read. At most
twenty rules a request, refused whole past them, 400, as are a `for` that
isn't one or a kind it doesn't offer; a server with no rules of its own is
refused whole, and its bar says why; nothing picked says so first, and
reads nothing. Nothing changes until someone approves each on its page, as
one made on its rule's page is.

What is refused for what the rule is is counted by why: a proposal not
finished with already, that `for` already, no `for` suggested, not replayed
(no backtest could check it), not in effect here, and anything else,
"propose each from its page to see why". `proposeWhy` says, of every text
`propose` and what it calls refuse with, which it is, and
`TestProposeWhyKept` walks them in the source and fails on one it doesn't
say. What would be the same for every rule left stops there: the hour's
twenty made, or the reader's tries (below) used, each rule left counted so
and none read; what is the server's or the request's (nothing replayed yet,
the rules not read yet, another comparison running, a database that
stopped answering), counted failed, the rest "not tried", as the bulk
writes do. A reader past the twenty, or their tries, is refused before
waiting for the heavy turn, the single form too.

**A request's time.** A bulk proposal starts no rule after twenty seconds
(`proposeTime`), and a rule's wait for the heavy turn ends then too: the
rest are "not tried: the time for one request was up, pick them again". The
answer so comes back within what a proxy in front commonly gives a request
(30 seconds for some load balancers' backends, 60 for some ingresses),
which past it closes the request, and the reader would see an error for
what was made. A rule started just before then takes what it takes: its
backtest within `--timeout`, against Prometheus as long as that asks. A
single proposal's backtest is bounded the same way and no more: where a
proxy's timeout is shorter than `--timeout`, raise it for `POST /proposals`
and `POST /rules/bulk`, or lower `--timeout`. The answer's write deadline is
moved on before each rule, past the server's two minutes
(`TestBulkProposeWritesBack`). A request that ends (a proxy's timeout, the
reader gone, the server stopping) keeps what was made and is logged, with
how many.

**Tries.** A refusal after the rules are read again (it has that `for`
already, it isn't replayed, its history is too short to retire it) costs
that read, of Grafana, a ruler or a cloud's alarms among them, and isn't a
proposal the ledger counts. So a reader asks for 60 proposals an hour at
most that read the rules again, made or refused, three times the twenty
they may make (`triesAnHour`, kept in memory, a replica's own). A try is
counted when the rules are read again, not for a refusal before (a rule
not in effect, one waiting already). The agents acting for a person have
60 of their own, apart from theirs: where no one is named (no roles on
loopback, `--everyone-admin`) every person is "anonymous", and an agent
asking again and again would otherwise lock every person out for an hour,
with nothing in the ledger to show for it; people who share a name share
their tries, as they share the twenty. Past them the single form answers
429, and a bulk proposal counts every rule left so. Lapilli's own are bounded apart. At
four thousand rules, a bulk retire of twenty by a reader who has made the
hour's twenty took 504 ms and 641 MB, each rule parsed whole twice; it
takes 15 ms and 12 MB; one of twenty names no rule has, by an editor with
every try left, took 489 ms and 640 MB, and takes 36 ms and 42 MB, the
rules in effect read once (`TestBulkProposeAtLimitScale`,
`TestBulkProposeAbsentOneRead`). A proposal past the
twenty answers 429, whichever check finds it, the ledger's write among
them.

**The result.** The list gone back to says it once, with a link to the
proposals made, the proposals list with their `id=`. `made=` in the
result's query is read within bounds (twenty at most, as many as `done=`, a
bulk proposal's), and what it says was proposed is counted from it, read
again: those that are the reader's, made in Lapilli, not by an agent; any
other is left out, and a result that claimed some and has none of theirs
says nothing. So a link made to say "20 proposed" says what the reader
proposed. Neither is carried on by any link or form.

At two thousand rules, against the tests' stand-in history, a proposal for
twenty took 1.3 s.

`TestBulkPropose` sets each kind beside the same rules posted one at a
time, each from the form its own page gives, on a twin ledger: the same
proposals kept (rule, kind, way, who, what it was and is proposed to be,
the diff, the document, the evidence and its backtest), the same lines in
the changes log; `TestBulkProposeOff` the same for Grafana's rules, turned
off; `TestBulkProposeSpareOverWeeks` a rule spare over four weeks and not
twelve, from lists of each; `TestAgentRetires` MCP's retire;
`TestBulkProposeLimit` and `TestProposeLimitRace` the hour's twenty;
`TestProposeTries` the tries; `TestBulkProposeTurns` the page's turn and the
heavy turn; `TestBulkProposeTimeUp` the request's time, and a request that
ended; `TestBulkProposeRefused`, `TestBulkProposeBar` and
`TestBulkProposeNothingPicked` who may, why not, and nothing picked.

### The firings: labels, and the day by the hour

Each firing's labels are chips, `name=value` in `<bdi>`, each a link to the
same day's firings with that label too (`label=name=value`, repeatable,
all of them, five at most, a name of 128 bytes and a value of 256 as the
ledger keeps them); a label already picked is drawn solid and isn't a
link; with five picked, none adds one. Those picked are chips in "Filters
in effect", each taking its label away, and the form's hidden fields; the
days before and after, and the next page, keep them, and the rule and
"only ● nothing" (which the day links used to drop). A label that isn't
one is left out and not said back; past five, the page says some were.
With labels picked, the page looks through the day's latest 20,000
firings for them, and says so when there were more.

Over the table, the day's firings by the hour of the clock each started
in: 24 cells, from where the ledger's day starts (on a day whose midnight
the clocks skip, America/Santiago 2026-09-06, from 01:00), each counting
the firings the page lists, all of them, not only the hundred shown:
caught something hollow (○), caught nothing solid (●), excused dashed,
stacked from the foot. The same as a table in a `<details>`, which says of
an hour the clocks skipped "not this day" and of one they went back over
"twice". Without labels, it is one read of the day's firings counted by
the database, by the quarter of an hour (in UTC) each started in, the
caught and the excused apart (`ledger.FiringStarts`): every zone is a
whole number of quarters off UTC, so each quarter falls in one hour of
the clock; 96 rows a day, however many fired, and no labels read.
`TestHourStrip` sets it beside the ledger's firings, by the date on the
clock, in UTC, Seoul, Santiago (a skipped midnight, a repeated hour),
Berlin, New York, Kathmandu, Kolkata and Lord Howe (half an hour on), to
the second.

### The overview's ways in

Each tile links to the list that makes its number, with the same team,
over the tiles' week, last week: `week=DATE` on the rules and the
incidents is a whole week of the twelve kept, over (a date in it is its
Monday; another date, or this week, is no week, and the list is over its
period). The list says so in its lede and has a chip for it, and the
filters' form keeps it as a hidden field, so that Apply stays in the week;
the chip takes it away. Precision,
Firings and Woken for nothing go to the rules over the week; Noisy rules
to `perf=noisy`; Refired and During deploys to the order by each; the
incidents missed and caught to the incidents, `state=missed|caught`
(`state=` takes caught, noisy or missed, each a link with its count, the
counts and the median lead of every one). The rules list ends with a row
of what the rules shown come to: firings, caught, nothing (and asleep, off
hours, and the notifications for nothing while people slept, once
notifications are kept), precision, re-fires, during deploys.

"Woken for nothing", once notifications are kept, counts notifications,
by the week each came in, not firings: the overview and the rules list
both count from one table, by week and by rule, worked out once for a
generation of the ledger (`wokenBy`). So the list counts each rule's, of
the team's rules, and a rule with some is listed though no firing of it
started in the week (one at 23:59:30 the Sunday before, notified at
00:00:30; a long one begun the week before, notified again), its row
saying how many. A week over before the first notification was kept says
"–" there, as the tile does, not 0. With a team picked, the overview
judges its incidents against everyone's rules, as the incidents page
does: one caught only by another team's noisy rule is caught only by a
noisy rule on both. `TestDrillDown` holds each tile to what its list comes
to, for all teams and each, with notifications kept and without.

The firings and incidents charts mark, besides the weeks the rules
changed, the weeks with firings during deploys (a triangle) and the weeks
a quiet window was over (a bar): one excused a firing of the team's, or
one of the team's or everyone's (any team's, for all teams; one of a team
removed, none) covers some time of the week, up to now in the week under
way (a window due later in it marks nothing yet), as `windowDays` works it
out for a rule's strip; for a team, a window that applies to some rule of
the team's, by its name, team and labels (`Window.Applies`), as a rule's
strip has it. Each kind has a link to hide it and show it again
(`marks=deploys,windows`, both when not given, `none` for neither; a value
it doesn't take is none); the legend names the marks shown; the table of
the weeks says the same, a tag a mark. "What to look at" is as it was, and
its "Make it a task" is disabled, with why, for whoever may not: a team's
editor makes tasks of their team's rules, and none about an incident.

The table of the weeks fits its box on a laptop's screen. Its first
column, the week with its marks' tags kept on one line, grew to 422 px in
English and 598 in Spanish on the test pages, and Spanish's headers alone
were wider than the box (1133 px of 1100). Now the week's date is kept
whole and its tags go under it as they fit, the headers wrap between their
words (Korean's at its spaces), a little smaller, and the cells are as
close as the rules list's. `test/a11y/axe.mjs` checks it as it checks the
rules list's, at 1280, 1440, 1600 and 1920 px in each language: against
the stylesheet before, it found 47 (Spanish at 1280, 721 px too wide).

### An incident's timeline

An incident's page draws it on a time axis from `--lead` before it to
`--lead` after it ends (with no end, as long as the verdict takes one,
`UnknownLen`): its span shaded, and a row for each rule that caught it,
then each that nearly caught it (the near misses the page lists, a diamond
where it would have fired), then each that was on around it, by when it
first was, its firings drawn from their starts, the stone a circle at the
start: hollow, solid, dashed. Twenty rules at most, fifty firings each,
the rest counted; the firings read once, those on in the span, started in
it or before and not ended by its start, a day back, the earliest first,
5,000 at most (`ledger.FiringsOn`): those that caught it are read before
any that came after them, and those over before the span don't count
against the 5,000. A span of no length (`--lead 0`, and an incident that
ended as it started) draws all at its start; one that crosses midnight
says the date with each time. The rows are a table, a rule's name in a
row header and its firings an SVG on the shared axis; the same numbers,
and when each firing started, are a table in a `<details>` under it.

## First use

Bundle 20, from a real deployment used as a first-time reader would: 62
rules saying whose each is in a `team` label of four values, no team in the
ledger, no incident, one admin, and nowhere to send a notice. Each page
below said nothing of what was missing, or said it so that the reader
couldn't act on it. The blocks are templates of their own
(`templates/firstuse.html`), drawn by the pages they are on.

### Teams the rules' labels suggest

The Teams page lists, under "Teams these rules' labels suggest"
(`#suggested`), each value of a label `--team-labels` names (`team`,
`owner`, `squad` unless given; label names, eight at most; the chart's
`serve.teamLabels`) on the rules no team owns, as `key=value` in `<bdi>`,
with how many of them it would own (`suggestTeams`). Always from the rules
in effect and those recorded elsewhere, with the labels `owners` matches
them by (a rule's own over its group's); nothing is kept.

- Only the rules no team owns, by a note, a name pattern or labels, are
  counted. Each goes to the first team made, by name, whose label it has;
  one that would own nothing isn't suggested. The counts are worked out
  over every value before the fifty (`maxSuggested`) with the most are
  kept and the rest counted.
- A value under two of those labels is one team, by the label named first.
  A value longer than a team's name (64 bytes) is counted, before anything
  is made of it, and left out.
- "Make these teams" makes a value into a team only when nothing says
  not to, and each line it doesn't make says why: a team of its name is
  there, in any case (giving it the label owns these rules only when it
  owns by that label alone or by none, as a team owns the rules with all
  of its labels; otherwise the page says to name it in the rules' notes),
  with a link to change it for an admin; its label is on rules another
  team owns now, which a team made of it, first by name, would take with
  their editors, windows and digest; or it isn't a plain lowercase name
  (`^[a-z0-9][a-z0-9._-]{0,62}$`): `pаyments`, its а Cyrillic, beside
  `payments`, or `Web` beside `web`, reads as another team's, and is made
  by hand.
- "Make these teams" is a POST to `/teams/suggested`, an admin's on the
  server (`s.admin`) and from the same site, as every form. It works the
  suggestions out again, and makes those it says it makes as the team form
  does (its name the value, its labels the label), passing over one whose
  name, in any case, is a team's in the teams read in the request, not in
  the owners kept for the ledger's generation, which another request or a
  replica behind may have made meanwhile (`teams.name`'s UNIQUE is in
  case; one in any case is the schema's, for bundle 22). The page it goes
  back to names the teams made, by their ids in its query (`made`, fifty
  at most), those alone that are there and the reader made or changed in
  the last ten minutes (a team keeps who changed it last and when, not who
  made it), as a bulk proposal's result does: a link can't make it say
  more. Anyone else sees the button disabled, with why (`off`, "Admins
  only").

With no team at all, the rules list says, under its lede, why its Team
column is empty, with a link to the teams suggested, or to the form to add
one when none is; only when it has a table, not before the first run.

### Why a rule is a team's

A rule's "About this rule" says how its team was found, after whose it is:
by its note; its name matching a team's pattern (the team and the pattern
named); its labels having a team's (the labels named); or that no team's
name pattern or labels match it, with the link above when there is no team.
`ownWalk` finds it as `owners` does, the note, then the patterns, then the
labels, each team in name order; `TestTeamsSuggestedAsOwnersWould` holds it
to `owners` for every rule. The rules list's Team cell is as it was: the
reason doesn't fit the table at 1280 px, and a visually hidden one would
tell a screen reader what no one else is told.

### Who may approve

The Roles page's approver row says who may approve a change in effect
(`approversIn`): those `--approvers` names, or the admins, named, as
`--approvers` isn't given; or no one, as changes are off, lapilli serve is
read-only, or neither list is given. It said "(the admins)" before,
changes on or off.

A proposal waiting for approval says who may approve it, in its approval
panel (`mayApproveOf`), to an admin, who reads the flags on the Roles page:
those, but the one it was made by or for (an agent's proposal is the
person's it acted for) and those who approved it, named. When fewer are
left than the approvals it needs, and none of them may be anyone (a group,
a domain, everyone, or a name with neither `user:` nor `group:`, which
`listed` takes for a group of that name too), it says so as a warning, and
what to change: "No one may approve it: only the admins approve, as
--approvers isn't given, and the only admin is the one it was made by or
for. Give --approvers to name who else may." Anyone else, an anonymous
reader among them, reads only "Who may approve it: an approver other than
the one it was made by or for", or, stuck, "No one can approve this yet;
an admin sees why on the Roles page": whether `--approvers` is given, who
the admins are and whether its maker is the only one are an admin's to
read. With changes off, the disabled Approve says so already, and nothing
more is said. Who may approve is as it was (`approver`).

### Where notices go

The Manage page has a Notifications card, and the Settings page a section
of that name (`#notifications`): where what happens is told (the webhook,
Slack, or nowhere), how many places reports can go to by name and how many
teams have a digest of their own, when the weekly digest goes ("Mondays
09:00", "매주 월요일 09:00": `weeklyText`) and to how many places that are
there, set by the flags or on the Reports page, how many reports are on,
when the outbox last sent a notice, how many notices made in the last
day failed their last try and wait to be tried again, of the latest 1,000
waiting a place has (`ledger.FailedCounted`; past those, "More than 1000
notices wait to be sent", not a count it didn't read), and, apart, how
many made in the last day were given up on, after ten tries, of the
latest 1,000 given up: given up on are next tried never, and read with
the rest they came first, so that a place given up on long ago hid the
day's failures. The digest posts to Slack itself, not through the outbox:
the last week posted (`digest-posted`, set by a post alone; a week passed
over, while the digest is off or newly set, sets `digest-sent` alone, and
a ledger that knows only that says "went to Slack or was passed over")
and a later week whose digest failed, worked out or posted (a digest
run's error, `ledger.LastFailed`), are read from the ledger, the same on
every replica and after a restart (`digestPosted`). Counts and kinds,
never a URL. The weekly digest on with nowhere to go, its last post
to Slack failed, a report on whose places are all gone, and failures are
each a sentence with a sign (a notice, warn; bad for a digest that failed
and notices given up on), never a colour alone: in full on
the Settings page, with the way out (for a digest the flags set, the
flags or the Reports page, as it never goes to a `--report-dest` by
itself; for one set there, another place there); in a line on the card
("The weekly digest goes nowhere.").

It is read from the flags, the reports, the runs and the outbox
(`ledger.Standing`: a place's last send by the outbox's index on its place
and when each was sent; its waiting notices and its given up by the same
index, each a range of it read backward from its end, ordered by when each
was sent and when it is next tried, 1,001 rows at most, however many
there are, which `EXPLAIN` holds to on SQLite and on Postgres: ordered by
the next try alone, Postgres read and sorted them all), nothing kept for
it. A notice sent is written
aside, emptying no page kept, so these two
pages are worked out for each request (`freshPage`, `Cache-Control:
no-store`), the Settings page but when it tries settings on the last week.

### The firings, a day with some

`/firings` with no `day=` opens the last day with firings in the twelve
weeks the pages show, of the rule and of those that caught nothing when
asked (`lastFiringDay`: one firing read, the latest), and says once that it
is "the last day with firings"; with none, today, and says so. The labels
picked aren't looked for in choosing it. With `day=`, and the links and
canonical URLs, as before.

### Times, and Korean

A time a page shows (`dur`, `shownDur`) is rounded as a person reads one:
under a tenth of a second to the millisecond, under a minute to a tenth of
a second ("The last run took 3.4s."), longer to the second; a rule's `for`,
whole minutes, is as it was. The API and MCP answer as they did.

The Korean texts say 정탐 (○), 오탐 (●) and 면제, never the stones the
English names them by (돌, 흰 돌, 검은 돌); `TestKoreanSaysNoStones` fails
on one, read as a noun with what Korean puts after it (돌이면, 돌까지,
돌처럼, 돌이지만, 돌은요, 돌(○)), and not on the verbs that hold the
syllable (돌아가기, 되돌리기, 돌 수, 돌 때).

`TestA11yServe` serves a first-time user's deployment under `/first`
(`firstUseServer`: labelled rules and no team, changes on and the only
admin's proposal waiting, nowhere to send a notice, a report going
nowhere), so that `test/a11y/axe.mjs` checks these pages too;
`TestFirstUseAccessible` holds them to what `TestPagesAccessible` holds
every page to.

## Without a script

| Elsewhere | Here |
|---|---|
| tabs | links, one URL each (`?weeks=`, `?sort=`, `?tab=`) |
| a panel that opens | a `<details>`, or a page of its own |
| a tooltip that defines a number | a `<details>` "?" beside its label |
| a form a button elsewhere opens | a link with a parameter that opens it (`edit=note`) |
| a menu that opens | a `<details>`, drawn twice where a wide screen keeps it open |
| filters that apply as they are ticked | a GET form with Apply; the result is a URL to keep and share |
| a chip's ✕ | a link to the URL without it |
| a search that opens the one result | a 303 |
| a disabled button with a tooltip | the button disabled, why beside it in words |
| a dry run before saving | a Preview button on the same POST, which writes nothing |
| a box that enables a button | `required`, and checked again by the server |
| a diff view's toggles | links, `?diff=` and `?context=` |
| rows picked, then an action | a checkbox a row in one POST form, a button an action; each row's own write in turn; Enter presses a disabled button first |
| a toast saying what was done | the page gone back to says it once, from its query |
| annotations toggled on a chart | a link to the same page with `marks=` |

## What is left

Nothing of what the phases set out: the last two (the overview's table of
the weeks on a laptop's screen, a rule's header drawn by the `off`
template) went out in 0.40.1. Of first use, the rules list's Team cell
doesn't say how its team was found (above). New work starts from
ROADMAP.md.
