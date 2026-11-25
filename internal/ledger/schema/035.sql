-- Who a firing reached, worked out (docs/design/routing.md, "In lapilli
-- serve"), after 034: the alert manager's routing, run by each run over the
-- firings of the hours it judges and the hours before them that groups and
-- repeats reach back to, from the firings in this ledger alone. These are
-- worked out, not recorded: the notifications really sent stay in
-- notifications, and every page that shows these says which kind it shows.

-- Each notification the model worked out, one for each integration of the
-- receiver it went to: when, the receiver, the integration's kind, whether
-- it pages (an integration of a paging kind, or a receiver --paging-receivers
-- matched), why (first, new, resolved or repeat), the group it was for as
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

-- What each firing reached, one row a firing worked out (a firing with no
-- row wasn't): notified are the notifications that told of it firing and
-- paged the pages among them; first_page when the first went (Unix seconds,
-- 0 for none); page_ats when each page went (a JSON list, at most 200); and
-- receivers the receivers it reached, as a JSON list of {receiver, kind,
-- notified, paged} for each receiver and integration kind, with those that
-- send nothing as {receiver, empty: true}; muted what kept it out of a
-- notification, as a JSON list of {kind, by, source_rule}. Gone with its
-- firing.
CREATE TABLE firing_routed (
	firing_id INTEGER NOT NULL PRIMARY KEY REFERENCES firings (id) ON DELETE CASCADE,
	notified INTEGER NOT NULL DEFAULT 0,
	paged INTEGER NOT NULL DEFAULT 0,
	first_page INTEGER NOT NULL DEFAULT 0,
	page_ats TEXT NOT NULL DEFAULT '[]',
	receivers TEXT NOT NULL DEFAULT '[]',
	muted TEXT NOT NULL DEFAULT '[]'
);
