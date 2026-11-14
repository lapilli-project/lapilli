# The web UI of `lapilli serve`

Status: phases 1 and 2 in v0.35.0; phase 3, a rule's page, in v0.37.0;
phase 4, the way to change things, in v0.38.0; phase 5, many at once and
ways in, built, unreleased. This is how the pages are put together: the
frame every page has, the team and period carried from page to page, the
search, the rules list and its filters, a rule's page, the way to change
things (a proposal's page, the lists' facets, a quiet window's preview,
the Manage page), what can be done to many at once and the ways from a
number to what makes it, and what the later phases will add.
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
  sideways; a wide table scrolls in its own box.
- **Bounds.** A page asked with a query of more than 8192 bytes is
  refused, 414, before anything is read for it. A search (`q=`, the bar's
  or the rules') of more than 500 characters is left out whole, and the
  page says so; of its words, the first 20 are used, and the rest named,
  "Left out: more than 20 words". A facet takes 20 values, a value given
  twice once. Several `q=` are one search, joined. What a page does with
  what it is asked doesn't grow past these.

## The frame

### The sidebar

The pages are in four groups, each named, not a link:

| Group | Pages |
|---|---|
| See | Overview `/`, Rules `/rules`, Incidents `/incidents`, Firings `/firings` |
| Fix | Tasks `/tasks`, Proposals `/proposals`, Quiet windows `/windows` |
| Report | Reports `/reports`, Outcomes `/outcomes` |
| Manage | Teams `/teams`, Roles `/roles`, Settings `/settings`, Runs `/runs` |

Roles is listed for admins, who alone may open it. A group's name isn't a
link, but Manage's, which goes to `/manage`, the page of its own (below),
`aria-current` there. Tasks has the open
tasks beside it, and Proposals those waiting for an approval, each the
team's in effect (picked, or the reader's own), as their lists have them,
or all teams': two counts the ledger gives once each time it changes
(`Server.waiting`: the open tasks by team, the proposals waiting by
rule, counted by the database), not once a page.

On a screen 960 CSS px wide or more, the sidebar stays on the left as the
page scrolls, with the languages at its foot. Narrower, or zoomed in, it is
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

An error page (403, 404, 414, 500), and the page after signing out, are
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
editors"): a quiet window's and a task's change, a team's (`?edit=`), the
settings' save, the switch that pauses changes (Admins only), a report's
change, an incident's (its change, whether it was real, a task about it),
a rule's (what to do first, its note, a fix to what it says, proposing a
change, a task about it) and a proposal's (below). Comments are as before.
The server checks every write as it did: a disabled button is never the
only check.

### Empty states

One partial, `empty`, in three kinds, as Grafana has them:

- `first`: nothing judged yet; the first run is replaying the weeks before
  it. The overview and the rules.
- `notfound`: what was asked for matches nothing. It says so, and gives the
  ways out: clear the filters; with a team picked, the same filters in all
  teams. The rules, and the search.
- `done`: nothing left to do. The overview's "What to look at": "Nothing to
  look at now."

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
| How it did, over the weeks shown (one of) | `● Noisy` ("Noisy, 3 wk" when it has been noisy weeks running), `○ OK`, `– Silent` (no firing in the weeks shown), `⧉ Spare` (another rule caught every incident it caught, as soon) |
| Its backtest, only when it can't be taken for granted | `≠ Differs` (its replay differed from what really fired over the last week checked), `◌ Not replayed`, `⇣ Recorded only` (copied in from where it fired; its condition unknown) |
| What people mean to do (its note's status) | `✓ keep`, `± tune`, `⊘ retire` |
| Under way | `✎ Proposed` (a change waits for approval), `⚠ n to check` (what its page lists to check, below) |

Noisy wins over Spare: a rule that is both is noisy first. Differs is
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
beside it, "Clear filters". On a screen 1200 px wide or more it is a panel
beside the table; narrower, a `<details>` above it, open when a filter is
in effect. Like the sidebar, it is drawn twice and shown once.

| Facet | Parameter | Values |
|---|---|---|
| Performance | `perf` | `noisy`, `ok`, `silent`, `spare` |
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

### The search box

Words are parts of a rule's name, in any case, each one in it; a phrase in
quotes is one, and is how to search for a name with a colon (`"job:rate5m"`).
Filters are written `key:value`, and `key:"a value"` when the value has
spaces (`label:"a=b c"`, `team:"a b"`):

| Filter | Is |
|---|---|
| `team:NAME` | the team |
| `severity:VALUE` | the severity facet |
| `is:noisy`, `is:ok`, `is:silent`, `is:spare` | Performance |
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
  fix to what a rule says has nothing to backtest, and says so.
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
  asked for, linked to it. Its making and approvals are the proposal's
  own; the rest are the ledger's changes whose `what` is the proposal's
  (`ProposalSteps`, an additive read by the changes' index on `what`),
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

Unless `state=` is given, the open ones, as the list was, those waiting for
approval first. The form sends `state=all` with what is ticked, so none
ticked is all of them; `?state=all`, `?state=applied` and `?state=refused`
mean what they meant, and a state there is no such of (`?state=bogus`)
shows none, as it did. The database picks the list, its states, kinds,
ways and makers in the query, in brief (`ProposalList`: no document, only
an undo's evidence, how many approved), and counts the facets with
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
Settings, Runs, Reports. The sidebar's Manage links to it, and the
breadcrumbs of its pages start with it.

### What they cost

At two thousand rules, four weeks of their firings (84,000) and a thousand
proposals (`TestChangePagesScale`), worked out afresh: the proposals list
takes 20 to 45 ms, a proposal's page 15 to 25 ms (one whose rule is 600
lines a side, shown whole, too), a quiet window's preview 35 to 80 ms
(every day, once over the four weeks, by cron), the tasks and the Manage
page about a millisecond.

## Many at once, and ways in

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
as they are (`ruleInScope`, then `ledger.PutNote`, which refuses a team's
editor a rule's team changed), an incident as the firing's own "This was
real" writes it (`realOne`). So the changes log has a line for each, by
whom, as one by one would, and a refusal is counted, not written: a
team's editor's rules of another team are "not your team's"; what the
ledger refuses for what it is, "refused"; a firing that has caught
something by its turn (an incident written from one picked before it
covers it), "had caught something already". An error of another kind
stops there, as the write alone would. `TestBulkRules` sets each beside
the same writes made one at a time on a twin ledger: the same notes, the
same tasks, the same lines in the changes log, for an editor, an admin and
a team's editor.

A request takes 100 at most, each once, however often it is picked; past
that it is refused whole, 400. The writes are the same doors: same-site
POSTs (a cross-site one is refused before the handler), `--read-only`,
the role (`teamWrite` for the rules, `write` for the firings). The form
names the list it came from (`back`), which is gone back to only if it is
that list's path (`safeBack`, then the path checked), with what was done
in its query, `bulk=task&done=3&theirs=2`: the page says it once, in a
`role="status"` line, read within bounds (counts of 0 to 100, a kind it
knows; anything else says nothing), and none of the links the page makes
carry it on. The bar is disabled for whoever may not use it, with why
(`aria-describedby`); a team's editor's "Set the team" is, "Editors only",
as moving a rule into or out of a team is an editor's.

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
"twice". Without labels, it is one read of the day's firings' starts and
stones (`ledger.FiringStarts`), their labels not read. `TestHourStrip`
sets it beside the ledger's firings, by the date on the clock, in UTC,
Seoul, Santiago (a skipped midnight, a repeated hour), Berlin and New York.

### The overview's ways in

Each tile links to the list that makes its number, with the same team,
over the tiles' week, last week: `week=DATE` on the rules and the
incidents is a whole week of the twelve kept, over (a date in it is its
Monday; another date, or this week, is no week, and the list is over its
period). The list says so in its lede and has a chip for it. Precision,
Firings and Woken for nothing go to the rules over the week; Noisy rules
to `perf=noisy`; Refired and During deploys to the order by each; the
incidents missed and caught to the incidents, `state=missed|caught`
(`state=` takes caught, noisy or missed, each a link with its count, the
counts and the median lead of every one). The rules list ends with a row
of what the rules shown come to: firings, caught, nothing (and asleep, off
hours, and the notifications for nothing while people slept, as the
overview counts them, once notifications are kept), precision, re-fires,
during deploys. `TestDrillDown` holds each tile to what its list comes
to, for all teams and each, with notifications kept and without.

The firings and incidents charts mark, besides the weeks the rules
changed, the weeks with firings during deploys (a triangle) and the weeks
a quiet window was over (a bar): one excused a firing of the team's, or
one of the team's or everyone's (any team's, for all teams; one of a team
removed, none) covers some time of the week, as `windowDays` works it out
for a rule's strip. Each kind has a link to hide it and show it again
(`marks=deploys,windows`, both when not given, `none` for neither; a value
it doesn't take is none); the legend names the marks shown; the table of
the weeks says the same, a tag a mark. "What to look at" is as it was, and
its "Make it a task" is disabled, with why, for whoever may not: a team's
editor makes tasks of their team's rules, and none about an incident.

### An incident's timeline

An incident's page draws it on a time axis from `--lead` before it to
`--lead` after it ends (with no end, as long as the verdict takes one,
`UnknownLen`): its span shaded, and a row for each rule that caught it,
then each that nearly caught it (the near misses the page lists, a diamond
where it would have fired), then each that was on around it, by when it
first was, its firings drawn from their starts, the stone a circle at the
start: hollow, solid, dashed. Twenty rules at most, fifty firings each,
the rest counted; the firings read once, those on in the span, started in
it or before and not ended by its start, a day back, 5,000 at most. The
rows are a table, a rule's name in a row header and its firings an SVG on
the shared axis; the same numbers, and when each firing started, are a
table in a `<details>` under it.

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
| rows picked, then an action | a checkbox a row in one POST form, a button an action; each row's own write in turn |
| a toast saying what was done | the page gone back to says it once, from its query |
| annotations toggled on a chart | a link to the same page with `marks=` |

## Later phases

- **A proposal for the rules picked**, beside the bulk writes.
- **Label chips on the rules list**: the labels a rule fires with as chips
  on its row, each a `label:` filter.
