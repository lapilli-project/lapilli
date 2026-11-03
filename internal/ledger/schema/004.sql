-- A quiet window can repeat: every day or every week, in the ledger's time
-- zone, until a day or for good.
ALTER TABLE windows ADD COLUMN every TEXT NOT NULL DEFAULT '';
ALTER TABLE windows ADD COLUMN until_at INTEGER;
CREATE INDEX windows_every ON windows (every, start_at);

-- A task can point at an issue in a tracker; and is told about once when
-- it falls overdue.
ALTER TABLE tasks ADD COLUMN link TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN overdue_told INTEGER;

-- What is to be told, written in the same write as what happened, and sent
-- after: a notice isn't lost if lapilli stops, and a slow receiver doesn't
-- slow a write.
CREATE TABLE outbox (
	id INTEGER PRIMARY KEY,
	dest TEXT NOT NULL,
	event TEXT NOT NULL,
	body TEXT NOT NULL,
	created INTEGER NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	next_at INTEGER NOT NULL,
	sent_at INTEGER,
	error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX outbox_due ON outbox (sent_at, next_at);
