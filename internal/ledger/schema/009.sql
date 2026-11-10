-- Notifications as they were sent: what an alert manager told a receiver,
-- taken on the --notify-listen port. Each is kept as it came. The firing
-- each of its alerts tells of is worked out when read: runs replace the
-- firings they find, and a notification comes before the run that finds
-- its firing.

-- source is alertmanager, grafana, sns, azure or google; grp the group it
-- was sent for (Alertmanager's group key, or what stands for one); reason
-- why it was sent: first, new, resolved, repeat or unknown. resolved is set
-- when every alert in it was. msg is an id the source gives each message,
-- '' when it gives none: one already kept is the same message again.
CREATE TABLE notifications (
	id INTEGER PRIMARY KEY,
	at INTEGER NOT NULL,
	source TEXT NOT NULL,
	receiver TEXT NOT NULL DEFAULT '',
	grp TEXT NOT NULL DEFAULT '',
	reason TEXT NOT NULL,
	resolved INTEGER NOT NULL DEFAULT 0,
	digest TEXT NOT NULL,
	msg TEXT NOT NULL DEFAULT ''
);
CREATE INDEX notifications_at ON notifications (at);
CREATE INDEX notifications_same ON notifications (digest, at);
CREATE INDEX notifications_group ON notifications (source, receiver, grp, at);
CREATE INDEX notifications_msg ON notifications (msg) WHERE msg != '';

-- One row for each alert in a notification: its rule, its labels (as a
-- series keeps them), when it started, and whether it was still firing.
CREATE TABLE notified (
	notification_id INTEGER NOT NULL REFERENCES notifications (id) ON DELETE CASCADE,
	rule TEXT NOT NULL,
	labels TEXT NOT NULL,
	start_at INTEGER NOT NULL,
	firing INTEGER NOT NULL
);
CREATE INDEX notified_rule ON notified (rule, start_at);
CREATE INDEX notified_of ON notified (notification_id);
