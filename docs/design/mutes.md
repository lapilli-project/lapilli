# Mutes: why a firing paged no one

Status: built (schema 029), and counted in the digest and the reports
later, with no schema change ("Shown"). Under
[GOVERNANCE.md](../../GOVERNANCE.md) this is a new kind of data in the
ledger, so it is written down here, with the build, for review as a pull
request. It changes no stone: a firing is judged as it is now. The
companion to [inputs.md](inputs.md), section 1, whose "What it doesn't
do: Mutes" it settles, and to
[notifications.md](notifications.md), beside whose notifications it shows.

## Why

A firing's page says how many notifications told of it, or that it paged
no one. Why it paged no one, Lapilli couldn't say: silenced, inhibited and
routed nowhere look the same from the receiving end. Alertmanager's event
recorder (0.33.0 on, `--enable-feature=event-recorder`), which Lapilli
reads every delivery from since 0.27, also says when a silence or an
inhibition kept an alert from a notification. Kept, a firing that paged no
one says it was muted, by which silence, or that another alert inhibited
it. What is left, a firing that paged no one with nothing muting it, is
one no route sent anywhere, or one the alert manager never had.

## What the recorder says

Read from the source (`proto/eventrecorder/events/v2/events.proto` in
0.35.0, `eventrecorder/eventrecorderpb/eventrecorder.proto` in 0.34.2,
`silence/silencer.go`, `inhibit/inhibit.go`, `notify/mute.go`) and
recorded from real Alertmanagers, 0.34.2 and 0.35.0, with a silence over
one alert of a group (`internal/serve/testdata/events/*-muted.jsonl`):

- **`silenceMutedAlert`**: `silence` (its `id`, matchers, times,
  `createdBy`, `comment`) and `mutedAlert` (`labels`, `fingerprint`). It is
  recorded each time a silence mutes an alert as Alertmanager asks: at each
  flush of its group, for each route it takes, and each time its API lists
  alerts. The recordings have one every five seconds, two at once at
  times.
- **`inhibitionMutedAlert`**: `inhibitRules` (matchers, `equal`, a
  `name`), `mutedAlert`, and `inhibitingFingerprints`, the alerts that
  inhibit it; Alertmanager gives one. A 64-bit number, which protojson
  writes as a string.
- **A notification's `mutedAlerts`**: the alerts of its group a silence or
  an inhibition kept out of the delivery, which doesn't say which. 0.35.0
  lists them among `alerts` too, with their labels; 0.33 and 0.34 name
  them by a hash alone, which nothing else in the event ties to labels.
- Labels come in the two formats a notification's do: pairs in 0.33 and
  0.34, a map in 0.35.

## What is kept

Of each such event, a mute is read, and nothing else of it:

- **when**: the event's `@timestamp`, within the hour before now and five
  minutes after, as a notification's; otherwise now;
- **the alert**: its `alertname`, as the rule, and its labels, bounded as
  a notification's alert's are (512 characters of rule name, 64 labels, 4
  KB of them, none that don't show), its labels 4 KB at most as kept too
  (JSON writes `<`, `>`, `&` and controls in six bytes each);
- **the kind**: `silence`, `inhibition`, or `either`, for a notification's
  `mutedAlerts` (0.35.0's, which carry labels), by what it doesn't say;
- **what muted it**: the silence's `id` (letters, digits and dashes, 64 at
  most), the first inhibiting alert's fingerprint (a 64-bit number), or
  nothing for `either`.

Not kept: a silence's comment and who made it, which can name people; its
matchers; the inhibition rule; the muted alert's own fingerprint. An event
that can't be read as the recorder's (a silence with no ID, an ID with
other characters, a mute with no alert or no `alertname`, a fingerprint
that isn't a number, labels that aren't strings) is refused, that event
alone, as a notification's is.

### Told of again

The same mute comes every few seconds, after a failure (the recorder tries
an event three times), and from each replica of an Alertmanager in high
availability. The ledger keeps it a row an alert, a kind, what muted it,
and an hour (UTC), from the first time in the hour to the last: one told
of again changes nothing, or makes its row longer. One that comes back the
next hour is a row of that hour. A replica of Lapilli writes a mute again
only when it would make the row more than a minute longer: told of every
five seconds, a muted alert takes a write a minute, not twelve. It
remembers 20,000 rows at most, then forgets them and writes each again.

## Shown

A firing is muted by the mutes of its rule whose labels are all of the
firing's and more (the alert carries external labels the replay doesn't),
told of from two evaluations before its start to two after its end, or
now while it is on: the span within which a notification's alert ties to
it. Only the hours about its start and its end are read (each within two
evaluations of it, six hours at most), not those between: a silence that
keeps a firing from paging anyone is told of as the firing starts, and
goes on being told of until it ends. A silence made in the middle of a
long firing isn't found, and such a firing was told of by its first
notification anyway. A silence comes first, then an inhibition, each the
earliest first, 20 at most, counted after the labels are matched;
`either` is left out where one that says which is there.

- **The rule's page** (its latest firings) and **the day's firings**: in
  the Notified column, a firing no notification told of says "muted by a
  silence" with the silence's ID, "inhibited by another alert", or "muted
  by a silence or an inhibition", before "paged no one". A firing told of
  says how many notifications did, as before. Mutes show from the first
  kept, whether or not notifications are counted for the rule yet.
- **MCP's `rule`**: each of its latest firings has `muted`, a list of
  `{by, silence, inhibited_by, from, to}`, beside `notified`.
- They are read for the firings shown, each hour of a rule once, its
  rows through `mutes_rule`, never cached: a page kept for the ledger's
  generation shows a mute kept after it from the next change to the
  ledger, a new mute's row among them; a row only made longer isn't one.
  An hour of a rule holds 1,000 rows at most (below), so a firing reads
  some thousands of rows at most, however many other alerts of its rule
  were muted, and its own mute is among them
  (`TestMutedOfBounded`, by the rows read).

- **The digest and the reports**, where they say what the notifications
  sent came to (the weekly digest, a team's, a report's `notifications`,
  and `digest.ready`'s and `report.ready`'s `notifications.paged_no_one`):
  of the firings of their rules that started in the span and that no
  notification told of, how many a silence muted, an inhibition, or
  either, by what their pages say muted them first; and how many nothing
  was told of muting, of rules whose notifications were counted by then,
  those their pages say paged no one. Given once the ledger keeps a mute,
  over the twelve weeks of notifications the pages tie, as the
  notifications' own numbers are. Worked out when read, as the pages are:
  no schema, no tally kept.

## In the ledger: schema 029

```sql
CREATE TABLE mutes (
	id INTEGER PRIMARY KEY,
	key TEXT NOT NULL,
	hour INTEGER NOT NULL,
	rule TEXT NOT NULL,
	labels TEXT NOT NULL,
	kind TEXT NOT NULL,
	ref TEXT NOT NULL DEFAULT '',
	first_at INTEGER NOT NULL,
	last_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX mutes_key ON mutes (key, hour);
CREATE INDEX mutes_rule ON mutes (rule, hour);
CREATE INDEX mutes_last ON mutes (last_at);
```

`key` is the first 32 hex digits of the SHA-256 of the rule, the labels,
the kind and the ref; `hour`, Unix seconds over 3,600. Postgres has the
same, its texts `COLLATE "C"`: a rule's name is bounded where it is taken
in, so `(rule, hour)` is a btree, as `notified_rule` is. Mutes are kept
for `--keep`, by `last_at`, as notifications are. Numbered 029: another
change takes 028, and until it is merged, 028 here is a file that changes
nothing, so that the migrator, which applies the files in their order,
reaches 029. A ledger at 027 migrates to 029 with nothing else changed
(`TestMigrate029`, SQLite and Postgres).

## Bounds

- A request is what it was: 1 MB, 1,000 events, 1,000 alerts a
  notification. A mute's alert is bounded as a notification's.
- **Rate.** A request of mutes alone is taken at the events' rate (200 a
  second, bursts of 1,000), within the events' budget of bytes read (8 MB
  a second), as every event that carries no notification is: a burst of
  mutes doesn't use up the rate deliveries are kept at. Mutes that come
  with a notification come at its rate.
- **An hour of a rule.** 1,000 rows at most: a thousand alerts each muted
  by one silence, or five hundred a notification told of too. Past that,
  a new row of that rule's hour is passed over, and that is logged once a
  day; a row kept goes on being made longer. It bounds what a firing's
  page reads, with the disk.
- **A day.** 200,000 rows of mutes a day (UTC) at most. A row of an
  ordinary alert (a short rule name, a few labels) is some 450 bytes with
  its indexes, about 90 MB a day; at most, a rule's name of 512
  characters (up to 2 KB, kept in the row and in `mutes_rule`) and 4 KB
  of labels as kept, some 8.5 KB, 1.7 GB a day. Past it, mutes are passed
  over until the next day, and that is logged once. A notification with
  them is still kept. A muted alert makes 24 rows a day at most, for each
  silence or inhibiting alert.
- **Writes.** One write at a time, with the notifications' turn; a mute a
  replica kept already within a minute takes none.
- **The count in the digest and the reports.** The span's firings are
  read without their labels, in the order they started, 20,000 a
  statement and 2,000,000 at most, and those a notification told of
  passed over; firings past the 2,000,000 aren't read, are in none of the
  numbers, and the line says some weren't counted (`partial`), as below
  (`TestUnpagedSpanBounded`). Before 0.69.2 one statement read them all,
  however many the span held. For each rule left, one row of `mutes_rule`
  says whether a mute of it was told of about them. Only a rule with one
  has its firings read with their labels, one rule at a time, and its
  mutes read as a firing's page reads them: the hours about each firing's
  start and end, each of a rule once, 1,000 rows an hour at most. In all,
  200,000 such rows are read at most: of firings read with their labels (a
  statement's `LIMIT`, so many
  firings beside one forged mute read no more), and of mutes looked at, a
  row counted once for each firing it is looked at for, and an hour not
  read yet as the most it can hold. Past that, the firings not read or
  looked at yet are in none of the numbers, and the line says some
  weren't counted (`partial`). A rule's mutes with no such firing, and its
  hours between a firing's start and end, aren't read
  (`TestUnpagedBounded`, by the rows of firings and of mutes read).

## Attacker's look

- Nothing is read before the token, as on every token path of the port:
  a request without it is answered 401 before its body is read.
- The bounds above hold the disk, the rate and what a page reads; a body
  of mutes is read within the same budget of bytes as every event.
  Someone who fills a rule's hour with mutes of other alerts (the token's
  holder, or anyone who can send alerts to an Alertmanager that takes them
  unchecked, under a silence that matches) can keep a real mute of that
  hour from being kept, not hide one kept: the page then says "paged no
  one", as it did before mutes were kept, and the log says so once a
  day.
- Someone with the notifications token can post mutes that never were,
  within those bounds. What a forged mute can claim is only that a firing
  was muted: the page and MCP say so beside a firing no notification told
  of. It changes no stone, no count, no notification, no catch, and
  nothing is sent because of it. It can't hide a notification: a firing
  told of shows its count first. The same holder can already post
  deliveries that never were.
- Labels and a silence's ID are shown on pages and to agents: they are
  bounded and held to characters that show; an ID is letters, digits and
  dashes alone. The page escapes them as it does every label.
- Nothing that can name a person is kept: a silence's comment and creator
  are never read into the ledger.
- In the digest and the reports, a forged mute counts its firing as
  muted, as its page says it was, and changes no other number. Mutes that
  fill the hours of rules with firings that paged no one make the count
  stop at its most: the line then says some weren't counted, rather than
  read on.

## Later

- **The file and Kafka outputs**, as for notifications: since sent on by
  [recipes/events](../../recipes/events), every event, mutes among them
  ([inputs.md](inputs.md#the-file-and-kafka-outputs)).
- **0.33's and 0.34's `mutedAlerts`**: by hash alone, they can't be tied
  to labels without the recorder's `alertGrouped` events, which aren't
  kept; their own mute events say the same, by what.

## Sources

- Alertmanager's event recorder:
  https://github.com/prometheus/alertmanager/blob/v0.35.0/proto/eventrecorder/events/v2/events.proto,
  https://github.com/prometheus/alertmanager/blob/v0.34.2/eventrecorder/eventrecorderpb/eventrecorder.proto,
  https://github.com/prometheus/alertmanager/blob/v0.35.0/silence/silencer.go,
  https://github.com/prometheus/alertmanager/blob/v0.35.0/inhibit/inhibit.go,
  https://github.com/prometheus/alertmanager/blob/v0.35.0/notify/mute.go
