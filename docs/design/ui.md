# The web UI of `lapilli serve`

Status: phases 1 and 2 in v0.35.0; phase 3, a rule's page, built,
unreleased. This is how the pages are put together: the frame every page
has, the team and period carried from page to page, the search, the rules
list and its filters, a rule's page, and what the later phases will add. [serve.md](serve.md) says what each page shows;
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
  a template or the frame names that the pages it renders don't show.
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

Roles is listed for admins, who alone may open it. Tasks has the open
tasks beside it (the team's, when a team is picked), and Proposals those
waiting for an approval: two counts the ledger gives once each time it
changes (`Server.waiting`), not once a page.

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
LatencyP99; Fix › Proposals › #12 …. The group isn't a link; the page is,
with the page context.

### The bar

Under the breadcrumbs, every page has a search (below), and the pages that
show one team's or a period, a GET form to pick them: the team (all teams,
or one) on the overview, the rules, the incidents and the tasks; the period
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
The forms elsewhere on a page (to change a task, to comment) are as before;
a later phase does the same for them.

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
gap (`Coverage`); for a cloud's alarm, its history from when its tie says;
the notifications from the first kept, and only in the pages (the twelve
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

## Later phases

- **Proposals**: the diff with options (unified, side by side, the whole
  rule), an approval panel that says who may approve and what is left, and
  each proposal's history.
- **Bulk actions** on the rules list: a task, a note's status, a proposal
  for the rules picked, as one POST form.
- **Label chips**: the labels a rule fires with as chips on its row, each a
  `label:` filter.
- **The overview's drill-down**: each tile and chart a link to the rules or
  incidents it counts, with the filters that give them.
- The page actions and disabled-with-reason for every form, not only each
  page's main one.
