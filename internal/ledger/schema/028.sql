-- The alerts an incident's tool linked to it (docs/design/links.md),
-- after expectations' 027. alerts is, of an incident written down or
-- copied in, those it lists: a JSON list of {"rule": NAME, "labels":
-- {...}}, sorted, each once (incident.SortAlerts), '' for none, every
-- incident's before. A firing of one of them catches it whatever its
-- labels say on the labels matched on.
--
-- linked is, of a catch, 1 when a firing of the rule that caught the
-- incident is one of the alerts it lists, 0 else: every catch's before,
-- none of whose incidents listed any.
ALTER TABLE incidents ADD COLUMN alerts TEXT NOT NULL DEFAULT '';
ALTER TABLE catches ADD COLUMN linked INTEGER NOT NULL DEFAULT 0;
