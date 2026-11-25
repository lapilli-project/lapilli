-- Who a firing reached, worked out (docs/design/routing.md, "In lapilli
-- serve"), after 034: the alert manager's routing, run by each run over the
-- firings of the hours it judges and the hours before them that groups and
-- repeats reach back to, from the firings in this ledger alone. These are
-- worked out, not recorded: the notifications really sent stay in
-- notifications, and every page that shows these says which kind it shows.

-- Each notification the model worked out, one for each integration of the
-- receiver it went to: when, the receiver, the integration's kind, whether
-- it is a page (page 1: it rings a phone: on a paging integration, with an
-- alert firing in it, and, on a kind that keeps an open incident for the
-- group, the notification that opens it, reason first; a receiver
-- --paging-receivers matched pages for each notification), why (first, new,
-- resolved or repeat), the group it was for as
-- Alertmanager writes a group's key, and how many firings it told of (0 for
-- one that only told of alerts resolving). Replaced, from the hours a run
-- worked out on, by the next run that works them out.
CREATE TABLE routed (
	id INTEGER PRIMARY KEY,
	at INTEGER NOT NULL,
	receiver TEXT NOT NULL,
	kind TEXT NOT NULL,
	page INTEGER NOT NULL DEFAULT 0,
	reason TEXT NOT NULL,
	grp TEXT NOT NULL DEFAULT '',
	firings INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX routed_at ON routed (at);

-- What a run said of each firing besides the notifications that told of it
-- (those are firing_routed_sent's, and every count of a firing's
-- notifications and pages is made from them, for the hours worked out, when
-- it is read: a firing has no count here that a missing row could be read
-- as none): grouped 1 when a notification that told of it told of another
-- rule's alert too; receivers the receivers it reached that send nothing, as
-- a JSON list of {receiver, empty: true}; muted what kept it out of a
-- notification, as a JSON list of {kind, by, source_rule}. One row for each
-- firing a run saw; kept when hours of its life aren't worked out (what
-- those hours reached is then not known, which the cover says, not this
-- table). Gone with its firing.
CREATE TABLE firing_routed (
	firing_id INTEGER NOT NULL PRIMARY KEY REFERENCES firings (id) ON DELETE CASCADE,
	grouped INTEGER NOT NULL DEFAULT 0,
	receivers TEXT NOT NULL DEFAULT '[]',
	muted TEXT NOT NULL DEFAULT '[]'
);

-- The notifications that told of each firing, one row for each firing and
-- notification: when, the receiver, the integration's kind, and whether it
-- was a page. They are what a firing's counts are made of, so that a
-- firing that goes on across runs has the notifications of all of them, each
-- run replacing those of the hours it worked out (as it replaces routed's)
-- and none of the others: a run's view of a firing that has been on for
-- days is of the hours it looked at, and never rewrites the rest. Gone with
-- its firing.
CREATE TABLE firing_routed_sent (
	firing_id INTEGER NOT NULL REFERENCES firings (id) ON DELETE CASCADE,
	at INTEGER NOT NULL,
	receiver TEXT NOT NULL,
	kind TEXT NOT NULL,
	page INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX firing_routed_sent_firing ON firing_routed_sent (firing_id, at);
CREATE INDEX firing_routed_sent_at ON firing_routed_sent (at);

-- Which hours were worked out: one row for each stretch of time, they do
-- not overlap, the latest work over an earlier. ok is 1 for a stretch worked
-- out and 0 for one that was not (the work stopped, failed, the routing
-- configuration couldn't be read, or its firings were recorded again): its
-- notifications and pages are not known, not none. why is, for ok 0, what
-- went wrong, and for ok 1 a caveat ("" for none), kept whole. A stretch
-- that isn't here wasn't tried.
CREATE TABLE routed_cover (
	from_at INTEGER NOT NULL PRIMARY KEY,
	to_at INTEGER NOT NULL,
	ok INTEGER NOT NULL DEFAULT 1,
	why TEXT NOT NULL DEFAULT ''
);

-- The lookups of the cover for what overlaps a stretch (a run writing its
-- hours, a firing's hours): stretches not worked out pile up while a
-- configuration can't be read, and from_at alone serves half of them.
CREATE INDEX routed_cover_to ON routed_cover (to_at);
