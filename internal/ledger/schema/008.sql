-- Firings recorded elsewhere, such as a cloud's own alarm history, copied in
-- by a job a source and a period at a time (docs/design/recorded.md). Such a
-- firing is a row of firings like any other; what is new is where its rule
-- comes from. A recorded rule has its source, and the name and severity the
-- source sent it under: its own name can differ, told apart from a replayed
-- rule's. A replayed rule has neither.
ALTER TABLE rules ADD COLUMN source TEXT NOT NULL DEFAULT '';
ALTER TABLE rules ADD COLUMN sent TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX rules_recorded ON rules (source, sent, severity) WHERE source != '';

-- Each source firings are recorded from: when it was last written, by
-- whom, the last period sent and how many firings it held, and when a write
-- last changed what the ledger had.
CREATE TABLE sources (
	name TEXT PRIMARY KEY,
	written INTEGER NOT NULL,
	written_by TEXT NOT NULL DEFAULT '',
	period_from INTEGER NOT NULL,
	period_to INTEGER NOT NULL,
	firings INTEGER NOT NULL DEFAULT 0,
	changed INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
