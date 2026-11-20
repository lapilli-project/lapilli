# Ask it from an agent

`lapilli serve` speaks [MCP](https://modelcontextprotocol.io) at `/mcp`
(Streamable HTTP), so an agent can ask it what the pages show, and check a
change to the rules before anyone makes it. The agent can be Claude Code,
Cursor, VS Code, or one of your own:

```sh
kubectl -n monitoring port-forward svc/lapilli 8080
claude mcp add --transport http lapilli http://localhost:8080/mcp
```

`compare` is off until `--mcp-compare editors` (or `admins`) turns it on,
with roles set.

Six tools read what the pages show, each answer as JSON with a schema:

- `overview`
- `rules`
- `rule`: a rule's definition and history, and what it is held to (`expectations`: budgets, catches and scenarios, [design/expectations.md](design/expectations.md))
- `incidents`
- `firings`: with their labels
- `outcomes`

The seventh, `compare`, is the one an agent can't do by itself. The agent
writes the changed rules as a Prometheus rule file. Lapilli replays them,
as they are and as proposed, against the metrics' history over the days
before (up to 14), and judges both against the ledger's incidents. A
comparison takes 20 rules at most and answers within 25 seconds. It
answers with how often each rule would have fired, and which incidents it
would have caught, sooner or later or not at all. The agent proposes and
Lapilli measures. Once the change is merged, `outcomes` says whether it
did what `compare` said.

`--mcp-compare` also gives `test_expectation`: it checks what a rule is
held to (an expectations text, such as a pull request's, or serve's own),
or a rewrite of the rule, against the rule replayed over the twelve weeks
to now, and says, per item, met, broken or not known, as it is and
rewritten ([design/expectations.md](design/expectations.md), step 3). A
scenario's promtool test file, given in `tests` by its path, is run by
serve's `--promtool`, once it is read as promtool would expand it and is
within bounds, and within `--promtool-memory` on Linux (a rewrite's
annotation template can ask promtool for more memory than a test file
says: past it, the scenario is not known), one promtool at a time across
serve: one waiting for serve's own scenarios longer than the check's time
is not known, saying so. Given a rewrite that meets
every expectation, losing no incident the rule catches, the rule as it is
and rewritten each replayed to the end, it answers with `post`: Markdown,
marked as an agent's rewrite backtested by Lapilli, saying whose
expectations it meets (serve's own for the rule, or the ones the agent
gave, which aren't necessarily what the rule is held to), with the
numbers before and after, which the agent or a person may post on the
pull request. Lapilli posts nothing; without `post`, the items and the note say
why. One check runs at a time, with `compare`'s.

Nothing an agent asks for changes anything: `compare` and
`test_expectation` replay and keep nothing. With `--mcp-propose editors`
(or `admins`), `propose` keeps a change to one rule, or to what it says, or a new rule (kind `add`, where `places` says one may go, telling only whom the rules there tell, one of their contact points where every Grafana rule there names one (`places`' `must_tell`), its labels plain text, held to the expectations given: no one approves it while one is broken), as a proposal ([Changing rules](changing-rules.md)),
marked as an agent's, for someone other than the person it acts for to
approve in the UI, and `backtest_again` backtests a stale proposal again,
as an editor does from its page. No tool approves or applies one. With it
too, `propose_expectation` keeps a draft of what one rule is held to,
checked as `test_expectation` checks it, for someone other than the person
the agent acts for to accept or drop on the rule's Expectations tab; it
counts toward that person's 20 proposals an hour.

- **Access.** Agents come through the same sign-in as the pages (with
  `--oidc-issuer`, an ID token as a bearer token), and
  everyone gets the tools that read. `compare` runs PromQL the agent wrote
  with Lapilli's access to Prometheus, and, for a rule kept in Loki, LogQL
  the agent wrote, sent to the Loki (or through Grafana's data source
  proxy) with Lapilli's token, so only those `--mcp-compare` names get it,
  and one comparison runs at a time.
- **Browsers.** Requests from web pages are refused: a browser sends an
  `Origin`, an agent doesn't. The one exception is a request from
  `--external-url`.
- **Text people wrote.** Incident titles, notes and labels reach the agent
  as people wrote them, and the server tells it to treat them as data.
