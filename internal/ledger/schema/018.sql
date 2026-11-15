-- The incidents that keep a period's record, found without reading the
-- rest (docs/design/serve.md): those that ended at its start, two weeks
-- before its first week, or after, and those with no end that started
-- since. Every page reads them; by when they started alone, every incident
-- before the period's end was read.
CREATE INDEX incidents_end ON incidents (end_at, start_at);
