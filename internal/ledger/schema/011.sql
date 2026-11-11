-- The source a notification's alerts were copied in as, through PUT
-- /api/v1/firings (docs/design/recorded.md), when its URL names one: a
-- cloud's alarm, told of on the notifications port, ties to that source's
-- rule that has the name the alarm sent, whatever name the ledger gives the
-- rule. '' for none: its alerts tie by their rule's name, as before.
ALTER TABLE notifications ADD COLUMN copied TEXT NOT NULL DEFAULT '';

-- The kind of message a notification came as, where a path takes more than
-- one: cloudwatch, for CloudWatch's own messages to SNS (directly or through
-- EventBridge), oci for OCI Notifications'; '' for an alert manager's. A
-- message that doesn't say when its alert started takes the start of one of
-- the same shape, group and labels.
ALTER TABLE notifications ADD COLUMN shape TEXT NOT NULL DEFAULT '';

-- An alert of a cloud's alarm names the alarm by id (alarm_id, as OCI's do
-- and recipes/firings/oci.jq labels the alarm's firings): read when a
-- notification is, so one that came before its firing was copied in still
-- ties once it is. These find them without reading every label.
CREATE INDEX series_alarm ON series (json_extract(labels, '$.alarm_id')) WHERE json_extract(labels, '$.alarm_id') IS NOT NULL;
CREATE INDEX notified_alarm ON notified (json_extract(labels, '$.alarm_id'), start_at) WHERE json_extract(labels, '$.alarm_id') IS NOT NULL;
