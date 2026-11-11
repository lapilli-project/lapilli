-- Proposals (docs/design/changes.md): a change to one rule, with the
-- evidence for it, and what became of it. rule is the name the ledger
-- judges it under; kind is for, threshold, disable, delete, says (one
-- annotation) or undo; path is git (a pull request a recipe opens),
-- grafana (Grafana's provisioning API) or none (a change for a person to
-- make, its diff to copy). state is
-- proposed, approved, ready (checked, waiting for a recipe), opened (a pull
-- request is open), merged, applied, refused, failed, undone or dropped.
--
-- before is the rule as it was read when the proposal was made, and
-- before_hash its hash: a rule read again that differs makes the proposal
-- stale. after is the rule as proposed, and after_hash what a version of
-- the rules holding it hashes it to: the change is applied once one does. source is the document it was read from, doc_hash that
-- document's hash, and document the document as changed, which a recipe
-- writes in place of the file with doc_hash. backup is the rule as kept
-- where it is, read just before it was changed, for undo. follows is the
-- proposal this one follows: a delete, the disable before it; an undo, the
-- change it undoes.
CREATE TABLE proposals (
	id INTEGER PRIMARY KEY,
	rule TEXT NOT NULL,
	kind TEXT NOT NULL,
	path TEXT NOT NULL,
	state TEXT NOT NULL,
	made INTEGER NOT NULL,
	made_by TEXT NOT NULL,
	agent INTEGER NOT NULL DEFAULT 0,
	source TEXT NOT NULL DEFAULT '',
	doc_hash TEXT NOT NULL DEFAULT '',
	before TEXT NOT NULL,
	before_hash TEXT NOT NULL,
	after TEXT NOT NULL,
	after_hash TEXT NOT NULL DEFAULT '',
	document TEXT NOT NULL DEFAULT '',
	diff TEXT NOT NULL,
	evidence TEXT NOT NULL,
	reason TEXT NOT NULL DEFAULT '',
	link TEXT NOT NULL DEFAULT '',
	backup TEXT NOT NULL DEFAULT '',
	follows INTEGER,
	task_id INTEGER,
	updated INTEGER NOT NULL,
	applied INTEGER
);
CREATE INDEX proposals_rule ON proposals (rule, state);
CREATE INDEX proposals_state ON proposals (state, updated);

-- Who approved a proposal: one row a person, so one can't approve twice.
CREATE TABLE approvals (
	proposal_id INTEGER NOT NULL REFERENCES proposals (id) ON DELETE CASCADE,
	by TEXT NOT NULL,
	at INTEGER NOT NULL,
	PRIMARY KEY (proposal_id, by)
) WITHOUT ROWID;
