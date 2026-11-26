# The HTTP API

`lapilli serve` answers on up to four ports, each opened by a flag of its
own, so a NetworkPolicy can let each client reach the one it needs and no
other:

| Port | What it answers | Who gets in |
|---|---|---|
| `--listen` (default `127.0.0.1:8080`) | the pages, the JSON API under `/api/v1`, MCP at `/mcp`, `/metrics`, `/healthz`, `/readyz` | as the sign-in and the roles say (below) |
| `--metrics-listen` | `/metrics`, `/healthz` and `/readyz`, nothing else | anyone who reaches it: no sign-in |
| `--jobs-listen` | what jobs write: incidents, firings recorded elsewhere, tasks kept in step with a tracker, what became of a change's pull request | a bearer token for each kind of job ([The jobs port](#the-jobs-port)) |
| `--notifications-listen` | notifications from alert managers, a path for each | each path's own proof ([The notifications port](#the-notifications-port)) |
| `--backtest-listen` | pull request backtests from CI jobs | a CI's ID token or a plain token an entry of `--backtest-trust` takes, then the backtest's key ([The backtest port](#the-backtest-port)) |

A path below is on `--listen` unless it says otherwise.

```sh
# as a script, behind a sign-in of lapilli's own
curl -H "Authorization: Bearer $(cat id-token)" \
  'https://lapilli.example.com/api/v1/summary?from=2026-10-26&team=payments'

# as a job copying an incident in, on --jobs-listen
curl -X PUT -H "Authorization: Bearer $(cat incidents-token)" \
  http://lapilli:8082/api/v1/incidents/INC-1042 \
  -d '{"title": "checkout errors", "start": "2026-11-16T10:00:00Z", "end": "2026-11-16T11:00:00Z", "labels": {"service": ["checkout"]}}'
```

## Signing in

A request on `--listen` says who it comes from in one of four ways
([docs/design/signin.md](design/signin.md)):

- **None.** Without the flags below, no request names anyone: each is
  `anonymous`.
- **`--user-header NAME`** (and `--groups-header NAME`, the groups
  separated by commas): a proxy in front signs people in and sets the
  header, which lapilli takes as it is. A header sent twice names no one.
  The proxy must be the only way in, and strip both headers from callers.
- **`--oidc-issuer`**: lapilli signs people in itself. A browser keeps a
  session in a signed cookie; a script or an agent sends an ID token the
  provider gave this client, as `Authorization: Bearer TOKEN`
  ([API clients and agents](design/signin.md#api-clients-and-agents)). A
  token issued longer ago than `--session-max-age` (12h), or before its
  person signed out, is refused, whatever its own `exp` says.
  `--oidc-bearer=false` takes no token.
- **`--proxy-jwt KIND`**: the JWT a sign-in in front puts in a header,
  checked against its keys on every request.

With `--oidc-issuer` or `--proxy-jwt`, every path but `/healthz`,
`/readyz`, `/static/`, `/lang`, `/auth/` and
`/.well-known/oauth-protected-resource` needs someone signed in. A request
under `/api/` or to `/mcp` from no one gets 401 and
`{"error": "..."}`. With `--oidc-issuer` the 401 carries
`WWW-Authenticate: Bearer realm="lapilli", resource_metadata="..."`,
naming `/.well-known/oauth-protected-resource` (which, with
`--oidc-bearer=false`, answers 404), and a token refused adds
`error="invalid_token"` and an `error_description` of a few fixed words
("the token has expired"); a browser asking for a page is sent to sign in
instead.

With `--oidc-issuer`, `--proxy-jwt` or `--viewers`, only those a role
names come in at all: `--viewers`, `--editors`, `--admins`, `--approvers`,
or a role an admin gave. Anyone else gets 403, on every path but those
above.

## Roles

| Role | May |
|---|---|
| viewer | read every `GET` but `GET /api/v1/roles`, `/roles` and `/config.yaml` |
| a team's editor | what an editor may, of what is their team's alone: its tasks, quiet windows, marked windows and reports, notes on the rules it owns, comments on what is its |
| editor | write incidents, tasks, comments, notes on rules, quiet and marked windows, and a team's reports |
| admin | also the settings (`POST /settings` and its forms), the teams, the roles, any report and the weekly digest |

A role is given by:

- `--editors` and `--admins`: people (as the sign-in names them), groups,
  `user:NAME`, `group:NAME`, `*@DOMAIN` or `*`. Whom no list and no role
  in the ledger names is a viewer, or, with `--oidc-issuer`, `--proxy-jwt`
  or `--viewers`, isn't let in (403). The ledger checks a team's editor's
  writes in the write itself.
- `POST /api/v1/roles`, the Roles page, or `--config`'s `grants:`: an
  editor's role, of every team (an editor) or of one (a team's editor),
  kept in the ledger. Only with
  `--admins` and a sign-in that names people (`--user-header`,
  `--oidc-issuer`, `--proxy-jwt`). An admin's and an approver's stay the
  flags'.
- Neither `--editors` nor `--admins`: listening on loopback alone,
  everyone is an admin; beyond it, everyone a viewer, unless
  `--everyone-admin`. With `--oidc-issuer` or `--proxy-jwt`, loopback
  makes no one an admin.

With roles, `anonymous`, a request whose header names no one, is a
viewer, and with `--viewers` isn't let in at all. `--read-only` refuses
every write, 403.
`GET /api/v1/me` says who the request comes from (`user`, `groups`), its
`role`, and, for a team's editor, `teams`. [Keep judging](serve.md) says
more of each flag.

Approving a change, or asking for one to be undone, is no role of the
API's: it is a form on the proposal's page, sent by a browser from that
page (`Sec-Fetch-Site: same-origin`), not with a bearer token, by someone
`--approvers` names, or, without it, an admin
([Changing rules](changing-rules.md)). No API route, token or MCP tool
approves.

## From a browser

`--listen` serves a web site, and keeps pages elsewhere from using a
browser that reaches it:

- A `POST`, `PUT`, `PATCH` or `DELETE` a browser sends from another site
  is refused, 403: by its `Sec-Fetch-Site`, or else its `Origin` beside
  `Host` (Go's `http.CrossOriginProtection`). A request with neither, as
  a script's is, goes on.
- A request to a host name lapilli doesn't answer to gets 421 (DNS
  rebinding). It answers an address, `localhost`, names under
  `.localhost`, `--external-url`'s name and those `--allowed-hosts` adds;
  and, from another machine, a name without dots and names under `.svc`,
  `.cluster.local` or `.internal`. Over loopback (this machine, a
  port-forward, a sidecar) those are refused: a port-forward asked as
  `lapilli.monitoring.svc` gets 421. `/healthz` and `/readyz` answer any
  name. The same holds on `--metrics-listen` and `--jobs-listen`.
- Every answer past that check, on every port, has
  `Content-Security-Policy: default-src 'none'; ...`,
  `X-Frame-Options: DENY` and `Referrer-Policy: no-referrer`; a 421 has
  none of them.

## Requests and answers

- A body is JSON; its `Content-Type` isn't read. Times are RFC 3339.
  `from` and `to` are the last seven days to now when not given (but
  `/api/v1/summary`'s), and `to` must be after `from`.
- A `POST` that makes something answers 201, with `Location` and what it
  made; a `PUT` or a `PATCH`, 200 with what was kept; a `DELETE`, 204 and
  no body. A `PUT` to an id that isn't there gets 404, but an incident's,
  which makes it.
- A `GET` under `/api/v1` on `--listen` (but `/api/v1/me` and
  `/api/v1/report` with its firings) answers with an `ETag`: sent back in
  `If-None-Match`, it gets 304 and no body while nothing has changed.
- Most answers name their fields in snake case. `GET /api/v1/rules`,
  `GET /api/v1/runs`, and the `Settings` and `Changes` of
  `GET /api/v1/settings`, are the pages' views as Go names them (`Lines`,
  `Firings`, `Precision`): see `rulesView` and `ruleLine` in
  [internal/serve/views.go](../internal/serve/views.go).
- What `--config` declares (`"declared": true`) is changed in the file
  alone: a write to it gets 409.

An error from a route under `/api/` is JSON, its words in English:

```json
{"error": "only an editor can change this"}
```

What is refused before a route is reached is plain text: `--read-only`,
a cross-origin write, a host name, a path that isn't there (404), a
method a path doesn't take (405, with `Allow`), and, on the incidents
port, a token missing or wrong and a token's rate. On `--listen`, a
request under `/api/` or to `/mcp` from no one is JSON, as above. The
notifications port answers its refusals in plain text.

| Code | When |
|---|---|
| 400 | a body, a query or a value that can't be taken, and why; a body past its bound, but those under 413 |
| 401 | no one signed in; a token missing or wrong |
| 403 | a role that doesn't allow it; someone no role lets in; `--read-only`; a cross-origin write; another's comment; another team's row |
| 404 | no such row |
| 408 | a notification that came too slowly, or was cut off |
| 409 | what `--config` declares; an incident that comes from elsewhere; an id taken; a proposal that moved on |
| 413 | firings past 16 MB; `POST /api/v1/tasks/{id}/comments` or a tracker job's `PATCH /api/v1/tasks/{id}` past 64 KB; a notification past 1 MB or of too many alerts |
| 414 | a page's query past 8,192 bytes (more for a try on the Settings page) |
| 421 | a host name lapilli doesn't answer to |
| 429 | on the incidents and notifications ports, with `Retry-After`: a token's or a source's rate, another request with the same token, the notifications' turn at the ledger after five seconds, a source's alerts for the day. A page's form past the proposals, tries or backtests an hour gets it without `Retry-After` |
| 503 | busy: a heavy request waited 30 seconds for its turn, a page 10 (with `Retry-After`), signed notifications arriving too many at once, or OCI's after two seconds; no run recorded yet, or the ledger's database out of reach (`/readyz`) |

Bounds:

- A write takes a body of 1 MB at most on `--listen`.
- `/api/v1/report` is 31 days and 100,000 firings at most, or 366 days
  with `firings=false`; `GET /api/v1/verdicts` 31 days and 20,000
  verdicts; `/api/v1/incidents` 366 days;
  `/api/v1/windows` and `/api/v1/marked` 400 days; `/api/v1/summary` 53
  weeks.
- `GET /api/v1/tasks` gives 500 tasks at most, the latest changed first;
  `GET /api/v1/proposals` 200; `GET /api/v1/runs` the last 100;
  `GET /api/v1/activity` 1,000, or 8 MiB of their text, and, for a
  `team`'s, looks through 20,000 changes and looks up 200 tasks and
  proposals at most, then says where it stopped (`next`, `cut`); it is
  written out four at a time, one a person, each holding its turn until it
  is read, and one of 50 changes at most, held to a mebibyte, takes a
  turn of the small ones', 16 at once.
  Only a
  tracker job's `GET /api/v1/tasks` goes on from where it stopped
  (`after`, `next`).
- A heavy request takes the one turn there is for one: a report with its
  firings, a summary of every team or of more than 53 rows, and
  `GET /api/v1/verdicts`. Another waits up to 30 seconds for it, then gets
  503; the answer has 30 seconds to be written.
- `--listen` reads a request in 30 seconds and its headers up to 64 KB,
  and writes an answer in two minutes; the other ports, in 30 seconds,
  16 KB and one minute.

## What was judged

| Route | Role | Takes | Gives |
|---|---|---|---|
| `GET /api/v1/report` | viewer | `from`, `to`; `firings=false`; `format=csv`; `redact=true` | `lapilli verdict --json`'s report for the period ([fields](numbers.md#the-reports-fields); `white_by_dependency` on a rule and `settings.depends`, with `too_many` when a judgement of the period was made with no graph, [dependencies.md](dependencies.md#in-lapilli-serve)); with `format=csv`, its firings a row each ([columns](numbers.md#the-csvs-columns)), those quiet windows excused among them; with `redact=true`, every name an alias, as `--redact` gives ([redact.md](redact.md)); with `--alertmanager-config` or `--grafana`, the `notifications` fields of each firing, rule and the report, `lapilli verdict`'s (a rule's and the report's with `known` and `stopped`, false and why when some hour of its firings, or of the period, wasn't worked out; the report's `groups` counts each group once over the period), worked out (`worked_out: true`) from the ledger's firings, and the `worked_out_*` columns of the CSV ([notifications-worked-out.md](notifications-worked-out.md#in-lapilli-serve)) |
| `GET /api/v1/summary` | viewer | `from`, `to` (a day, `2026-11-02`, or an RFC 3339 time; the last four whole weeks and this one by default), `team` (repeatable, or `*` alone for everyone and every team), `critical` (the severities counted as critical, comma-separated) | each week, for everyone or each team: precision, critical firings caught, re-fires, firings off hours and during deploys, incidents and impact windows caught and missed, the firings counted as `episodes` ([numbers.md](numbers.md#episodes); `null` until the ledger has counted the week's), `people`'s verdicts, `judged_by_nothing` and `not_judged` (its old name, beside it until the last 0.x minor release) ([numbers.md](numbers.md#a-persons-verdict-lapilli-serve)), `responses` (the firings counted by what was done about them: `changed`, `routine`, `acknowledged`, `unanswered`, `none`) and `actionable` (`useful`, `not_useful` and `share`, the useful over both, `null` with none known), both `null` until pages, changes or deploys are kept ([numbers.md](numbers.md#what-people-did)), `paged` (the pages that went in the week, `pages`, of them `recorded` and `worked_out`, `asleep`, `off_hours`, `for_nothing` and `for_nothing_asleep`, and `basis`: `recorded`, `worked_out` or `recorded_and_worked_out`; `unknown: true` (with no count) for a week some hour of which wasn't worked out or whose work stopped or failed, `counted_from` (the day) for a week counted from part-way into it, and omitted without `--alertmanager-config` or `--grafana`, and for a week before pages are counted from; [notifications-worked-out.md](notifications-worked-out.md#in-lapilli-serve)), and `goals` with goals declared; with `schema` 1 ([docs/design/noc.md](design/noc.md)) |
| `GET /api/v1/outcomes` | viewer | | what the Outcomes page shows: `effect` (what the rule changes did, beside what their backtests said), `record` (whether incidents are being written down), `use` |
| `GET /api/v1/expectations` | viewer | `rule` (one rule's; every rule's when left out) | `checks`, what checking the expectations held now found ([design/expectations.md](design/expectations.md)): each rule's budgets by week (`week_of`, the week's Monday) over the week so far and the twelve whole weeks before, its catches (`week_of` the week the incident began, `""` while it isn't written down), and its scenarios (`what` `scenario PATH`, `week_of` the week what it found was found in, `detail` what promtool said of it failing, or why it couldn't be run), each with `what`, `held`, `selector`, `source`, `result` (`met`, `broken`, `not known`, or `open`: the week under way, not over its budget yet), `why`, `n` and `value`, and `task`, the task made of it; `this_week` |
| `GET /api/v1/expectations/draft` | viewer | `rule` (one rule's), or `team` (a team's rules'); every rule's when neither | the history's draft of what the rules are held to ([design/expectations.md](design/expectations.md), step 3): `yaml`, an expectations file, as `/expectations/draft.yaml` gives it, of the twelve whole weeks before this one (`from`, `to`), each rule's most in a week, while people slept and re-fired, rounded up, and the incidents it caught; `rules`, how many it holds; `left_out`, those it doesn't, each with `why` (`no_week`, `unreplayed`, `past_bound`); `team` |
| `GET /api/v1/expectations/drafts` | viewer | `rule`, `state` (`open`, `accepted`, `dropped`; comma-separated) | `drafts`, 200 at most, the latest first: each draft of what a rule is held to, an agent's or the history's, with `made_by`, `agent`, `made`, `text` (the expectations, as Lapilli wrote them), `tests`, `reason` (the proposer's words), `items` (what checking it found: `what`, `held`, `as_it_is`, `as_it_is_state`, `as_it_is_why`), `target` (`file`, `config`, `none`), `state`, `decided_by`, `decided`, and, by the git path, `pull_request` (`ready`, `opened`, `merged`, `closed`, `failed`), `link` and `pull_request_reason` |
| `GET /api/v1/expectations/drafts/{id}` | viewer | | the draft. Both were `GET /api/v1/expectation-drafts` and `GET /api/v1/expectation-drafts/{id}`, taken still under those paths, with `Deprecation: @1795651200` and a `Link` to the new one, until the next major release ([Renamed names](upgrading.md#renamed-names)) |
| `GET /api/v1/rules` | viewer | `weeks` (`1`, `4` or `12`; 4 by default), `team`, `sort` (`refired`, `deploys`), `during` or `outside` (a marked window's kind) | the rules list: `Lines`, a rule each, with `Firings`, `White`, `Black`, `Precision`, `Noisy`, `Team`, `Suggestion`, ...; `Silent`, the rules that didn't fire in the twelve weeks kept |
| `GET /api/v1/runs` | viewer | | the last 100 runs, the latest first: `Kind`, `From`, `To`, `Firings`, `Error`, `Failed`; to anyone but an admin, without the paths of serve's files |
| `GET /api/v1/sources` | viewer | `all` (`true` for every source) | the sources firings are copied in from: `name`, `written`, `by`, `from`, `to`, `firings`, `rules`; with `all=true`, `sources`, every source serve reads as the Sources page shows it: `id`, `kind`, `name`, `from` (its flag), `state` (`ok`, `failing`, `quiet`, `pending`), `down`, `last_ok`, `last_failed`, `last_error` (as `GET /api/v1/runs` says an error, and without a credential to anyone), `failures` (reads in a row, -1 where not counted), `rules`, `lag_seconds`, `took_seconds`, and, to an admin, `where`; never kept ([docs/design/serveops.md](design/serveops.md#sources)) |
| `GET /api/v1/activity` | viewer | `who`, `kind` (`setting`, `incident`, `verdict`, `proposal`, `task`, `comment`, `note`, `window`, `marked`, `report`, `team`, `role`, `goal`, `firings`, `cleanup`, `other`), `team`, `from`, `to` (a day, `2026-11-02`, in `--tz`, `to` taking it whole, or an RFC 3339 time; `since` and `until` are their old names, still taken, and a name with its old one is a 400 naming both), `before` (where a page ended), `limit` (1 to 1,000; 1,000 unless given, and as many as 8 MiB of their text holds), `format` (`json`, `csv`) | `activity`, the changes kept, the latest first: `id`, `at`, `by`, `kind`, `what`, `before`, `after`; `next`, the `before` to go on from; `cut`, that it stopped looking. With `format=csv`, the same as CSV (`id,at,by,kind,what,before,after`), the next page in `Link`, no cell a formula. The roles given and taken to an admin alone, and to anyone else the paths of serve's files as their flags; never kept ([docs/design/serveops.md](design/serveops.md#the-activity-log)) |
| `GET /api/v1/settings` | viewer | | the judging settings, each with its flag and value (`Settings`: `lead`, `match`, `refire`, `noisy-firings`, `noisy-precision` and `noisy-by`, `caught` or `useful`, what rules are found noisy by: [responses.md](responses.md); `--linked-only` is one, its value `on` or `off`), whether `--linked-only` and `--match-strict` are in effect (`linked_only`, `match_strict`), what a firing's record is kept by, `"day"` or, with `--record-by-week`, `"week"` (`record_by`), and whether every firing is still being judged again with them after a start that gave them or took them away (`linked_only_judging`), the incident criteria (`criteria`), and what `--config` came to (`config`; to anyone but an admin, without the file's name) |
| `GET /api/v1/me` | anyone let in | | `user`, `groups`, `role`, `teams` |

## Incidents

| Route | Role | Takes | Gives |
|---|---|---|---|
| `GET /api/v1/incidents` | viewer | `from`, `to` | the incidents that started in the period, the earliest first, impact windows among them (`origin` `impact`, `state`, `counts`), each with `caught_by` (`linked` of a catch by an alert it lists, `dependency` of one by the service graph alone: the services in call order, [dependencies.md](dependencies.md)), and `unreplayed` when nothing caught it and it started before the ledger's first replayed time, so no rule could have fired for it: it is not missed |
| `POST /api/v1/incidents` | editor | `id` (the next `L-n` when left out), `title`, `start`, `end`, `labels` (`{"service": ["checkout"]}`), `alerts`, those its tool linked to it (`[{"rule": "CheckoutErrors", "labels": {"service": "checkout"}}]`, 100 at most, docs/design/links.md) | 201, the incident |
| `PUT /api/v1/incidents/{id}` | editor | as `POST` | the incident with that id, made or replaced |
| `DELETE /api/v1/incidents/{id}` | editor | | 204 |
| `POST /api/v1/incidents/{id}/decide` | editor | `state` (`confirmed`, `dismissed`, or `""` to reopen), `title` (of the incident a window confirmed becomes; its own when left out), `together` (`true` dismisses the other named signals' windows at that time not decided yet) | `id`, `state`, and `incident`, the one it was confirmed as. A window reopened and confirmed again is the incident it was confirmed as before, while that stands for it (kept, not deleted and written again under its id, its hours meeting the window's): no second is written; it is given the hours and labels the window has grown to since, and `title` when one is given |

A write that changes an incident gives the firings their stones again at
once; one written again as it was changes nothing. An incident from the
incidents file, one a job copied in, or an impact window is changed where
it comes from: 409. An impact window is decided as its form decides it:
a window only (400 for an incident), its title 500 bytes at most, noted in
the activity log as the editor's.

## People's verdicts on firings

What a person says of one firing ([docs/design/judging.md](design/judging.md),
item 3): that it is **unrelated** to the incidents it caught, which turns
that firing alone black, and whether it **needed action**, which changes no
stone. A firing is picked by its alert and start, as a report gives it
(`rule`, `labels`, `start`): a report's firings have no id.

| Route | Role | Takes | Gives |
|---|---|---|---|
| `GET /api/v1/verdicts` | viewer | `from`, `to` (RFC 3339; the last seven days by default; 31 days and 20,000 verdicts at most: past them, 400, to ask a shorter period) | `verdicts`, those of the firings that started in the period, each with its firing (`rule`, `labels`, `start`, `end` as it was when said), `unrelated` (`incidents`, each `{key, start, end}` as it was then, `by`, `at`, `note`) and `action` (`answer`, `by`, `at`, `note`), each `null` when not said; `moved`, why it is to be looked at again, and `on_firing` |
| `PUT /api/v1/verdicts` | editor; a team's editor, `action` and `action_note` of the rules it owns | `rule`, `labels`, `start`; `unrelated` (`true` or `false`), `unrelated_note`, `action` (`yes`, `no`, or `""` for none), `action_note`: a part left out is left as it is | the verdict |
| `PUT /api/v1/verdicts/bulk` | editor; a team's editor, of the rules it owns | `firings` (each `rule`, `labels`, `start`; 100 at most), `action`, `action_note` | `firings`, `changed` |
| `DELETE /api/v1/verdicts` | editor; a team's editor, `action` of the rules it owns | `rule`, `labels`, `start`, `part` (`unrelated`, `action`, or both when left out) | 204 |
| `POST /api/v1/firings/real` | editor | `rule`, `labels`, `start` | the firing written down as an incident it caught, as "This was real" writes it: 201 and `incident`, with `Location`; 200 and no `incident` once one was written from it, or it caught one since. An editor's of the last 14 days' firings, an admin's of any (older, 403); 409 on a firing said unrelated; 404 with no such firing |

- **Part by part.** A team's editor's write leaves `unrelated` as it is
  stored: a `PUT` that gives it otherwise, or its note, is 403, and their
  `DELETE` of both undoes `action` alone. `unrelated` is an editor's of
  every team and an admin's: the incident it takes a catch from may be
  another team's.
- **The last 14 days.** One who isn't an admin writes of firings that
  started in the last 14 days alone: an older one is 403, an admin's.
- **What it needs.** `unrelated` keeps the incidents the firing catches
  now, 16 at most: 409 on a firing that caught nothing, or one "This was
  real" wrote an incident from. 404 with no such firing.
- **30 an hour.** Each person writes 30 verdicts an hour, a `PUT`, a bulk
  `PUT` (one, however many firings) and a `DELETE` alike, the forms
  among them: past them, 429 with `Retry-After`, and nothing written.
- **Who wrote it.** A part's `by` and `at` are of whoever last wrote its
  answer or its note; another answer given with no note drops the note of
  the one before.
- **All or nothing.** A bulk `PUT` with a firing not found, of another
  team's for a team's editor, or older than 14 days for one who isn't an
  admin refuses all of it, saying which; `unrelated` isn't taken in bulk.
- Every write is noted in the activity log, with its before and after; a
  bulk one as one change; and each person's verdicts of an hour shown as
  one line. MCP reads verdicts (`firings`, `rule`, `summary`) and sets
  none.

## Teams, notes and roles

| Route | Role | Takes | Gives |
|---|---|---|---|
| `GET /api/v1/teams` | viewer | | every team: `id`, `name`, `rules` (a regular expression of rule names), `labels`, `note`, `work_hours`, `sleep_hours`, `tz`, `shifts`, `declared` |
| `GET /api/v1/teams/{id}` | viewer | | the team |
| `POST /api/v1/teams` | admin | the fields above | 201, the team |
| `PUT /api/v1/teams/{id}` | admin | as `POST` | the team |
| `DELETE /api/v1/teams/{id}` | admin | | 204: the roles of the team's editors go with it |
| `GET /api/v1/notes` | viewer | | the notes on rules, as an object by rule name |
| `PUT /api/v1/notes` | editor; a team's editor, on the rules it owns | `rule`, `team_id`, `status` (`keep`, `tune`, `retire`), `runbook` (an http or https URL), `note` | the note; one with nothing in it is taken away. A team's editor can't move a rule to another team |
| `GET /api/v1/roles` | admin | | `kept` (whether roles can be given), `flags` (`admins`, `editors`, `approvers`), `given` |
| `POST /api/v1/roles` | admin | `who` (`user:NAME` or `group:NAME`), `role` (`editor`, the only one kept), `team_id` (left out for every team) | 201, the role, with its `team` |
| `DELETE /api/v1/roles/{id}` | admin | | 204 |

How teams own rules, and their hours, are in
[docs/design/work.md](design/work.md).

## Quiet windows and marked windows

| Route | Role | Takes | Gives |
|---|---|---|---|
| `GET /api/v1/windows` | viewer | `from`, `to` | the quiet windows that meet the period, the earliest first |
| `GET /api/v1/windows/{id}` | viewer | | the window |
| `POST /api/v1/windows` | editor; a team's editor, for its team | `title`, `start`, `end`, `every` (`day`, `week`, `month`, `cron`), `repeat_on` (when, for `month` and `cron`: `15`, `last`, `first sat`; `on` is its old name, still taken, and both is a 400 naming both), `until`, `team_id`, `rules`, `labels` | 201, the window, with `repeat_on` and, beside it for one minor release, `on` (as every window the API gives) |
| `PUT /api/v1/windows/{id}` | as `POST` | as `POST` | the window |
| `DELETE /api/v1/windows/{id}` | as `POST` | | 204 |
| `GET /api/v1/marked` | viewer | `from`, `to` | the marked windows with an occurrence in the period, each with its `occurrences` |
| `GET /api/v1/marked/{id}` | viewer | | the window |
| `POST /api/v1/marked` | editor; a team's editor, for its team | `kind`, `title`, `start`, `end`, `every`, `repeat_on` (`on`, its old name, still taken, not both), `until`, `team_id`, `labels` (`{"service": "search"}`) | 201, the window, with `repeat_on` and `on` beside it as a window's |
| `PUT /api/v1/marked/{id}` | as `POST` | as `POST` | the window |
| `DELETE /api/v1/marked/{id}` | as `POST` | | 204 |

A quiet window's repeats are in [Keep judging](serve.md) and
[docs/design/work.md](design/work.md); a marked window's in
[docs/design/marked.md](design/marked.md).

## Tasks and comments

| Route | Role | Takes | Gives |
|---|---|---|---|
| `GET /api/v1/tasks` | viewer | `status` (some of `open`, `doing`, `done`, `dropped`, comma-separated), `rule`, `incident`, `team` (a team's name) | the tasks |
| `GET /api/v1/tasks/{id}` | viewer | | the task |
| `POST /api/v1/tasks` | editor; a team's editor, for its team | `title`, `kind` (`tune`, `write`, `retire`, `decide`, `other`), `rule`, `incident`, `team_id`, `assignee`, `status`, `due`, `body`, `link` | 201, the task |
| `PUT /api/v1/tasks/{id}` | as `POST` | the whole task | the task |
| `PATCH /api/v1/tasks/{id}` | as `POST` | the fields to change alone | the task |
| `DELETE /api/v1/tasks/{id}` | as `POST` | | 204 |
| `GET /api/v1/comments` | viewer | `on` (`rule`, `incident` or `task`) and `key` (its name or id) | its comments, the earliest first |
| `GET /api/v1/comments/{id}` | viewer | | the comment |
| `POST /api/v1/comments` | editor; a team's editor, on what is its | `on`, `key`, `body` | 201, the comment |
| `PUT /api/v1/comments/{id}` | as `POST`, and only its author, but for a comment `anonymous` wrote | `body` | the comment |
| `DELETE /api/v1/comments/{id}` | as `PUT` | | 204 |

## Reports

| Route | Role | Takes | Gives |
|---|---|---|---|
| `GET /api/v1/reports` | viewer | | the reports people scheduled |
| `GET /api/v1/reports/{id}` | viewer | | the report: `name`, `team_id`, `schedule`, `span`, `zone`, `sections`, `dests`, `enabled`, `last_span_end`, `stopped` |
| `GET /api/v1/reports/{id}/preview` | viewer | | the report for its last span, sent nowhere: what `report.ready` would say |
| `POST /api/v1/reports` | admin; an editor or a team's editor, a team's report to `team:NAME` | `name`, `team_id`, `schedule`, `span`, `zone` (`ledger` or `team`), `sections`, `dests` (1 to 10), `enabled` (true unless given) | 201, the report |
| `PUT /api/v1/reports/{id}` | as `POST` | as `POST` | the report |
| `DELETE /api/v1/reports/{id}` | as `POST` | | 204 |
| `GET /api/v1/reports/digest` | viewer | | the weekly digest: `schedule`, `dests`, `enabled`, `set_by` (`flags` until an admin sets it) |
| `GET /api/v1/reports/digest/preview` | viewer | | the digest as it would be sent now |
| `PUT /api/v1/reports/digest` | admin | `schedule` (`weekly DAY@HH:MM`), `dests`, `enabled` | the digest |
| `DELETE /api/v1/reports/digest` | admin | | 204: the flags define it again |

The schedules, the sections and the destinations are in
[Keep judging](serve.md) and [docs/design/reports.md](design/reports.md).

## Proposals

| Route | Role | Takes | Gives |
|---|---|---|---|
| `GET /api/v1/proposals` | viewer | `rule`, `state` (comma-separated) | `proposals`, 200 at most, each with its `diff`, `evidence`, `approvals`, and, when it is stale, `stale`, why; `changes` (whether `--changes` is on) and `paused` |
| `GET /api/v1/proposals/{id}` | viewer | | the proposal, with the `document` as changed; of a new rule (kind `add`), its `evidence.add` (where it goes, the rule as Lapilli wrote it, each expectation's `items`), `held` (why no one may approve it yet) and `expectations` and `tests`, what it is held to |

None of the API makes, approves or applies a proposal: a person proposes
on a rule's page (`POST /proposals`), or a new rule on its form
(`/proposals/new`, `POST /proposals` with `kind=add`), an agent with MCP's
`propose` (kind `add` for a new rule, where MCP's `places` says it may
go), and a
person approves in a browser ([Changing rules](changing-rules.md)). Nor a
draft of expectations: an agent drafts with MCP's `propose_expectation`, a
person keeps the history's on the rule's tab, and an approver accepts in a
browser.

## The jobs port

`--jobs-listen` takes what jobs write, each kind with a token of its
own, and nothing else. A job sends its token as
`Authorization: Bearer TOKEN`. Each token's file is read for each request,
so it can be rotated; it holds 16 characters at least. Serve doesn't
start while two token files, `--notifications-token-file` among them, hold the
same token, and one rotated to another's is taken from no one until they
differ. The headers that name people mean nothing here; host names are
checked as on `--listen`.

| Token | Routes | Rate | Body |
|---|---|---|---|
| `--incidents-token-file` | `POST /api/v1/incidents`, `PUT /api/v1/incidents/{id}`, `DELETE /api/v1/incidents/{id}` | 20 a second, bursts of 40 | 1 MB |
| `--firings-token-file` | `PUT /api/v1/firings/{source}` | 20 at once, then one each 5 seconds | 16 MB |
| `--responses-token-file SOURCE=FILE` | `PUT /api/v1/responses/{source}`, for that source alone | 20 at once, then one each 5 seconds | 16 MB |
| `--tasks-token-file` | `GET /api/v1/tasks`, `PATCH /api/v1/tasks/{id}`, `GET /api/v1/tasks/{id}/comments`, `POST /api/v1/tasks/{id}/comments`, `PATCH /api/v1/tasks/{id}/comments/{comment}` | 5 a second, bursts of 100 | 64 KB |
| `--changes-recipe-token-file` | `GET /api/v1/changes`, `PATCH /api/v1/changes/{id}`, `PATCH /api/v1/changes/expectations/{id}` | 1 a second, bursts of 30 | 64 KB |

A token makes one request at a time on each replica: another gets 429
and `Retry-After: 1`. Past its rate, 429 with how long to wait. With
several replicas the rates are the ledger's, shared by every replica;
the one request at a time stays each replica's
([Several replicas](serve.md#several-replicas)).

- **Incidents.** As on `--listen`, writing alone: an incident a job writes
  (`origin` `copy`) is the job's, which the UI doesn't change, and the job
  changes none of the UI's (409). [Incidents kept elsewhere](incidents.md)
  has the recipes.
- **Firings.** `PUT /api/v1/firings/{source}?from=&to=` takes the firings
  that start in `[from, to)`, one JSON object a line as `--firings` reads
  them, in place of those the ledger has from that source then. The source
  is lowercase letters, digits and dashes, 40 at most, neither starting
  nor ending with a dash. It gives `source`,
  `from`, `to`, `firings`, `added`, `removed` and `changed`
  ([Firings recorded elsewhere](serve.md#firings-recorded-elsewhere)).
- **Responses.** `PUT /api/v1/responses/{source}?from=&to=` takes what a
  paging tool says of the pages sent in `[from, to)`, `{"responses": [...]}`
  as `--responses` reads it, JSON only (a YAML body is refused), in place of
  the pages the ledger has from that source sent then; the pages it has from
  other times stay. The body is 16 MB at most (413 past it), read a page at a
  time, as a write of firings is. Each source has a token of its own
  (`--responses-token-file SOURCE=FILE`), which writes that source's pages
  and no other's: a request with another source's token, or for a source no
  token is for, is 401. The source is named as a firings' is, and is not
  `file`, which `--responses` owns, and whose pages outrank an API source's for
  a firing. A page sent outside the period is refused (400), as is a period
  over 31 days or one that starts, or a page sent, more than an hour ahead of
  the clock, a 51st source, a source with 500,000 pages or a week with
  200,000 (the write is refused whole), and a bound broken in the pages (an id
  twice, a person named, a value that isn't text). It gives `source`, `from`, `to`, `pages`, `added`, `changed`,
  `removed` and `changed_ledger`: sent again as it is, it changes nothing.
  The firings the pages answer are judged again. The token writes its
  source's pages alone, which can make a firing acknowledged or unanswered;
  an acknowledged page can turn an unanswered firing, not useful, into one
  of unknown use, which raises a rule's actionable share, within what its
  source's token and the file's precedence allow ([Responses](responses.md),
  [docs/design/responses.md](design/responses.md)).
- **Tasks.** `GET /api/v1/tasks` takes `linked` (`false` unless given),
  `status` (`open,doing` unless given), `updated_after` (an RFC 3339
  time), `after` (a task's id) and `limit` (1 to 200, 200 unless given),
  and gives `tasks`, each with `status_by`, `status_at`, `team` and, with
  `--external-url`, `url`, and `next`, the `after` to go on from while
  there may be more. `PATCH /api/v1/tasks/{id}` sets `link` (an https URL,
  set once) and `status`, and refuses any other field.
  `GET /api/v1/tasks/{id}/comments` gives `comments`, the earliest first,
  those copied in from the issue with `origin` `tracker`;
  `POST /api/v1/tasks/{id}/comments` copies one in (`text`, `author`,
  `ref`, the tracker's id for it, and `at`) and gives `kept` and
  `as_sent`, with 201 when it was kept;
  `PATCH /api/v1/tasks/{id}/comments/{comment}` takes `{"posted": REF}`,
  the tracker's id for a comment of people's the job posted, set once, or,
  for a comment copied in from the issue, `{"text": TEXT, "edited": TIME}`,
  its edit there, kept when later than the one kept, and gives `changed`
  and `as_sent`
  ([Tasks kept in step with a tracker](serve.md#tasks-kept-in-step-with-a-tracker),
  [docs/design/trackers.md](design/trackers.md)).
- **Changes.** `GET /api/v1/changes` gives `changes`, 50 at most, of rules
  kept in git: those approved and ready for a pull request (`state`
  `ready`, with the `document` as changed), while `--changes` is on and
  not paused, and those with one open (`opened`), each with its `branch`
  (`lapilli/LEDGER/proposal-ID`), `title`, `body`, `source`, `doc_hash`
  and `link`. `PATCH /api/v1/changes/{id}` takes `state`: `opened` with
  the pull request's `link`, `merged`, `closed`, or `failed` with a
  `reason` of 500 characters at most
  ([recipes/changes](../recipes/changes/README.md)). It also gives
  `expectations`, 50 at most: the drafts of expectations accepted by the
  git path, ready (with the `document`, the expectations file with the
  draft taken in, and `doc_hash`, the sha256 of the file as lapilli serve
  read it) or with a pull request open, each with its `branch`
  (`lapilli/LEDGER/expectations-ID`), `rule`, `title`, `body` and `link`;
  `PATCH /api/v1/changes/expectations/{id}` takes the same `state`s
  ([design/expectations.md](design/expectations.md), step 3).

## The notifications port

`--notifications-listen` takes notifications as alert managers sent them, and
nothing else, each source on a path of its own:

| Route | Proved by |
|---|---|
| `POST /v1/alertmanager` | the token in `--notifications-token-file`, as a bearer token |
| `POST /v1/alertmanager/events` | the same, from Alertmanager's event recorder |
| `POST /v1/grafana` | the same token, or Grafana's HMAC signature with the secret in `--notifications-grafana-secret-file` |
| `POST /v1/sns` | Amazon SNS's signature, from a topic `--notifications-sns` names |
| `POST /v1/azure` | the token as the password in the URL |
| `POST /v1/google` | the token as the password, sent after a 401 asks for it |
| `POST /v1/oci/{source}` | the token as the password in the URL |

A notification is 1 MB at most; each source is taken at 20 a second,
bursts of 100, and up to 200,000 alerts a day (UTC). It answers
`{"kept": N, "same_as_kept": N}`, with `refused` when some couldn't be
kept; `/v1/alertmanager/events` adds `mutes`, the silences' and
inhibitions' mutes it read and kept ([notifications.md](notifications.md#why-a-firing-paged-no-one)). Host names aren't checked here: each request carries its own proof.
[Notifications as they were sent](notifications.md) says what each
source sends and how.

## The backtest port

`--backtest-listen` takes pull request backtests from CI jobs, and nothing
else ([serve.md](serve.md#pull-request-backtests),
[design/backtests.md](design/backtests.md)):

| Route | Who may call it |
|---|---|
| `POST /v1/backtests` | a job holding a token an entry of `--backtest-trust` takes, as a bearer token: a CI's ID token for the entry's audience, with its pins, or the entry's plain token |
| `GET /v1/backtests/{id}` | whoever holds the backtest's key, given once in the POST's answer, as a bearer token |
| `DELETE /v1/backtests/{id}` | the same |

A body is JSON of 4 MiB at most, read strictly, only once the token
verified, its entry's hour taken, and the queue's room for the entry and
the token's once seen (409 for a token that started one, 429 with
`Retry-After` for no room, the body unread). The POST answers 202 with the id, the
key, the place in the queue and the requests it will ask; the GET 202
while it waits or runs, 200 with the comment and its JSON once done.
Host names aren't checked here, and a request with an `Origin` is
refused.

A file whose content is the JSON `terraform show -json` writes of a plan or
a state is kept in the ledger, until the backtest ends, as only its
`grafana_rule_group` resources: a plan holds every resource's values, the
sensitive ones among them, and serve keeps nothing else of it, whatever the
job sent.

## Health, metrics and the rest

| Route | What it is |
|---|---|
| `GET /healthz` | `ok`, to anyone, at any host name |
| `GET /readyz` | `ok` once the ledger has a run recorded, or, on the replica that leads, once its first round of work is done, whatever it recorded; 503 before. On a Postgres ledger, 503 while its database is out of reach, and, on `--metrics-listen`, which replica leads |
| `GET /metrics` | the metrics, over the last seven days ([Metrics](metrics.md)). On `--listen` it is behind the sign-in, as a page is; `--metrics-listen` serves it with none |
| `/mcp` | MCP, over Streamable HTTP, any method, behind the same sign-in ([Ask it from an agent](mcp.md)) |
| `GET /.well-known/oauth-protected-resource` | with `--oidc-issuer`, to anyone: this server as an OAuth 2.0 protected resource (RFC 9728), with the provider to get a token from (`authorization_servers`); with `--oidc-bearer=false`, 404 |
| `GET /auth/login`, `GET /auth/callback`, `GET /auth/signed-out` | with `--oidc-issuer`: the sign-in, sent to the provider and back |
| `POST /auth/logout` | with `--oidc-issuer`: signs the person out, ending every session of theirs, and every bearer token issued to them, up to then |
| `GET /settings.yaml` | viewer: the judging settings changed in the UI, as Helm values to commit |
| `GET /config.yaml` | admin: what the ledger holds, as `--config` YAML ([docs/design/serveops.md](design/serveops.md)) |
| `GET /static/`, `GET /lang` | the UI's files, and the choice of its language |

Lapilli serves no ruler's API: `/loki/api/v1/rules` and
`/prometheus/api/v1/rules` are a Loki ruler's, which
`--loki-loaded-rules` reads ([Rules on logs: Loki](loki.md)).

## The pages

The UI's pages are HTML for people: `/` (the overview), `/rules` and
`/rules/{name}`, `/incidents` and `/incidents/{id}`, `/firings`, `/tasks`
and `/tasks/{id}`, `/proposals` and `/proposals/{id}`, `/windows` and
`/windows/{id}`, `/marked` and `/marked/{id}`, `/reports`,
`/reports/digest` and `/reports/{id}`, `/outcomes`, `/goals`, `/teams`,
`/roles`, `/settings`, `/sources`, `/runs`, `/activity`, `/orphans`,
`/manage` and `/search`. Their
forms post to paths of their own (`POST /incidents`, `POST /rules/bulk`,
`POST /settings`, ...), and need the roles the API's writes of the same
things do. `POST /settings` (an admin's) changes the judging settings it
names (`lead`, `match`, `refire`, `noisy-firings`, `noisy-precision`,
`linked-only` as `on` or `off`) and leaves the others as they are; an
empty value, or `reset-KEY=1`, gives one back to the flag's. Some writes are there alone, with no route of the API:
changing the settings, proposing a change (an agent proposes through MCP),
approving one, making a task of a goal
missed or an expectation broken (`POST /goals/task`, `POST
/expectations/task`, the latter of a rule's `rule`, `expect` and `week`,
refused unless it is broken as last checked), keeping the history's draft
of a rule's expectations (`POST /expectations/drafts`, its `rule`: a
team's editor's, of their own team's rule), accepting a draft (`POST
/expectations/drafts/{id}/accept`, an approver's, from the rule's page in
a browser, never its proposer's) or dropping one (`POST
/expectations/drafts/{id}/drop`, an editor's or an approver's), and the
changes to many rules or firings at once. `GET /expectations/draft.yaml`
(`rule` or `team`) gives the history's draft as a file to take. Deciding
an impact window and writing a firing down as real have routes of their
own too (above), with the forms' roles and checks. A form about a firing (`POST /firings/{id}/real`,
`POST /firings/{id}/verdict`, and their bulk `POST /firings/real` and
`POST /firings/verdict`) carries the firing's series and start beside its
id, and is refused, 409, when the firing is another by then: a run gives
its firings again, and an id with them.

## What stays the same

While the version is 0.x, a minor release may change the API, and its
changelog entry says how ([GOVERNANCE.md](../GOVERNANCE.md#releases)).
Beside that, the report's fields (`/api/v1/report`, as
`lapilli verdict --json`), the CSV's columns and the metrics keep their
names and meanings as [The numbers](numbers.md#what-stays-the-same) says;
and `/api/v1/summary` keeps its own `schema`, 1: a field may be added
without changing it, one changed in meaning or taken away changes it, and
the path stays ([docs/design/noc.md](design/noc.md)).
