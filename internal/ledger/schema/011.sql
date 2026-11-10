-- The source a notification's alerts were copied in as, through PUT
-- /api/v1/firings (docs/design/recorded.md), when its URL names one: a
-- cloud's alarm, told of on the notifications port, ties to that source's
-- rule that has the name the alarm sent, whatever name the ledger gives the
-- rule. '' for none: its alerts tie by their rule's name, as before.
ALTER TABLE notifications ADD COLUMN copied TEXT NOT NULL DEFAULT '';
