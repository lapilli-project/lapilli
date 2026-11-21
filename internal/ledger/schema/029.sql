-- Mutes (docs/design/mutes.md), after 028: the alerts a silence or an
-- inhibition kept from a notification, as Alertmanager's event recorder
-- tells of them (silenceMutedAlert, inhibitionMutedAlert, and a
-- notification's mutedAlerts). The recorder tells of a mute each time
-- Alertmanager asks whether an alert is muted, every few seconds, and
-- again after a failure, from each replica: they are kept a row an alert,
-- what muted it and an hour, from the first of them in the hour to the
-- last. The firing each tells of is worked out when read, as a
-- notification's is.
--
-- key is the first 32 hex digits of the SHA-256 of the rule, the labels,
-- the kind and the ref; hour, the hour (Unix seconds / 3600, UTC) they
-- came in. rule is the alert's alertname; labels its labels, as a
-- notification's alert keeps them. kind is silence, inhibition or either
-- (a notification's mutedAlerts, which don't say which); ref, the
-- silence's ID or the inhibiting alert's fingerprint, '' where the event
-- doesn't say. first_at and last_at are Unix seconds. Nothing else of a
-- silence is kept: its comment and who made it can name people. Kept for
-- --keep, by last_at.
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
-- The same mute again in its hour; a firing's, by its rule and hours; the
-- day's count and --keep.
CREATE UNIQUE INDEX mutes_key ON mutes (key, hour);
CREATE INDEX mutes_rule ON mutes (rule, hour);
CREATE INDEX mutes_last ON mutes (last_at);
