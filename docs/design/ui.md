# The web UI of `lapilli serve`

Status: phases 1 and 2 built, unreleased. This is how the pages are put
together: the frame every page has, the team and period carried from page
to page, the search, the rules list and its filters, and what the later
phases will add. [serve.md](serve.md) says what each page shows;
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
(1, 4 or 12 weeks) on the rules and the incidents. The overview has no
period: its tiles are last week beside the week before, by design. The form
sends the page's other parameters again, as they were, and is named for
what it chooses: "Team", "Period", or "Team and period".

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
  or a form's POST target. A form going to another page (the search), or
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
| Under way | `✎ Proposed` (a change waits for approval), `⚠ n to check` (problems of fact in what it says) |

Noisy wins over Spare: a rule that is both is noisy first. Differs is
looked up for a rule that didn't fire in the weeks shown too: one replayed
to nothing while it really fired is the case it is most for. A badge with a
reason (Not replayed, Recorded only) has it in its `title`; the rule's page
says it in words, a rule that never fired too. The badge partial, `badges`,
is shared: the rule's page will use it. Proposed and "to check" link to the
rule's proposals and to what it says. The suggestion (a longer `for` that
would quiet it) stays as one quiet line under the badges: it is what to do.

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

## Without a script

| Elsewhere | Here |
|---|---|
| tabs | links, one URL each (`?weeks=`, `?sort=`, later `?tab=`) |
| a panel that opens | a `<details>`, or a page of its own |
| a menu that opens | a `<details>`, drawn twice where a wide screen keeps it open |
| filters that apply as they are ticked | a GET form with Apply; the result is a URL to keep and share |
| a chip's ✕ | a link to the URL without it |
| a search that opens the one result | a 303 |
| a disabled button with a tooltip | the button disabled, why beside it in words |

## Later phases

- **A rule's page**: a status panel of the four axes at its top, an
  84-day strip of its stones, and its sections as `?tab=` links.
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
