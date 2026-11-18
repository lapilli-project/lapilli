# Showing a report outside

To show a vendor, a community or a talk what your alerting looked like
without its names, `--redact` gives every name in the report an alias,
in the text, `--json`, `--csv`, `--markdown` and `--metrics`, and in
what `--push` and `--slack-webhook-file` send:

- rules are `rule-01`, `rule-02` and on, and their groups, teams and the
  sources firings were copied from the same way (`group-01`, `team-01`,
  `source-01`);
- a label's values are aliased by the label, `service-01`, `instance-02`
  (`label-team-01` for a label named `team`, so it isn't taken for a
  team's alias), the same value the same alias on firings and incidents,
  so what `--match` agreed on still agrees; label names, and
  `severity`'s values, stay;
- incidents are `incident-01` on, by when they started, their titles left
  out, and those only what `--expect` holds a rule to names after them;
- named impact signals are `signal-01` on, and a rule entry's pattern of
  rule names is `[redacted]`;
- why a rule couldn't be replayed (a query, a host, the history's own
  words), an expression a near miss quotes, shifts naming an on-call
  schedule and the time zones hours were counted in are `[redacted]`, and
  a recording rule's name aliased (`record-01`); whether a replay failed,
  and so the exit status, is as it was.

Numbers, severities and durations stay, and times, in UTC whatever `--tz`
says: an offset says where a report was made. Each list keeps its order
(the noisiest rules first, incidents by when they started), and where
names or a file's order broke a tie, the aliases do. Aliases are the
output's own, numbered in an order no name decides: run it again and
`rule-01` may be another rule. With `--redact-key FILE`, a random key in
hex, 32 digits or more (`openssl rand -hex 32`), a name's alias is a keyed
hash of it, `rule-3f9a1c04e2`, the same in every output made with the
key; without the key, it can't be worked back to the name by trying
names, as an unkeyed hash of a short name can, nor a key a person thought
of be found by trying keys. `lapilli serve` gives the same with
`/api/v1/report?from=&to=&redact=true`, its aliases that answer's own,
and `&format=csv&redact=true` the CSV, the firings quiet windows excused
and the team column given aliases with the rest.
What goes to stderr, the job's log, isn't redacted, which the run says
there.
