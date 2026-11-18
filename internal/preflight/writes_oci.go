package preflight

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/source"
)

// ---- OCI ----

// What serve's writes to OCI's alarms as --changes-oci-config's user
// need: UpdateAlarm takes ALARM_UPDATE, which only manage alarms gives, in
// the alarm's compartment, and METRIC_READ, which read metrics gives, "for
// the metric compartment and metric namespace"
// (https://docs.oracle.com/en-us/iaas/Content/Identity/Reference/monitoringpolicyreference.htm):
// the alarm's metricCompartmentId, the tenancy where its metric is read in
// the subtree (metricCompartmentIdInSubtree: "requires the user to have
// tenancy-level permissions"). serve never deletes nor makes again an OCI
// alarm.
var ociNeeds = []string{"ALARM_UPDATE", "METRIC_READ"}

// ociSays is how a policy grants what is needed, for the item.
var ociSays = map[string]string{"ALARM_UPDATE": "manage alarms", "METRIC_READ": "read metrics"}

// The bounds of what is read of OCI's policies: a compartment is six
// below the tenancy at most, so eight levels; the user's groups, each a
// read; the pages of each list; the statements, all told.
const (
	mostLevels       = 8
	mostGroups       = 20
	mostMemberships  = 1000
	mostCompartments = 10
	mostStatements   = 5000
	ociVersion       = "/20160918"
)

// ociStatement is a policy statement as far as it is read: allow, deny,
// one of the cross-tenancy kinds (define, endorse, admit) or, kind "",
// one not understood.
type ociStatement struct {
	kind     string
	subjects []ociSubject
	verb     string   // inspect, read, use, manage
	resource string   // a resource type or family, in lower case
	perms    []string // a list of permissions, {A, B}, in place of a verb and a type
	loc      ociLoc
	cond     bool // where ...
}

// ociSubject is who a statement is of: kind group (domain, name), group-id
// (name an OCID), any-user, any-group, or one that is never a user
// (dynamic-group, service).
type ociSubject struct{ kind, domain, name string }

// ociLoc is where a statement applies: the tenancy, a compartment by its
// OCID, or by its name or path (A:B) from where the policy is attached.
type ociLoc struct {
	tenancy bool
	id      string
	path    []string
}

// ociTokens splits a statement into words: a quoted name ('...' or "...")
// one word, kept quoted; , { } words of their own. ok is false for a quote
// that isn't closed.
func ociTokens(s string) (out []string, ok bool) {
	const breaks = " \t\r\n,{}'\""
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case strings.IndexByte(" \t\r\n", c) >= 0:
			i++
		case c == ',' || c == '{' || c == '}':
			out = append(out, string(c))
			i++
		case c == '\'' || c == '"':
			j := strings.IndexByte(s[i+1:], c)
			if j < 0 {
				return nil, false
			}
			out = append(out, s[i:i+j+2])
			i += j + 2
		default:
			j := i
			for j < len(s) && strings.IndexByte(breaks, s[j]) < 0 {
				j++
			}
			out = append(out, s[i:j])
			i = j
		}
	}
	return out, true
}

// unquote is a word without its quotes.
func unquote(w string) string {
	if len(w) >= 2 && (w[0] == '\'' || w[0] == '"') && w[len(w)-1] == w[0] {
		return w[1 : len(w)-1]
	}
	return w
}

// parseOCI reads a policy statement, conservatively: Allow SUBJECTS to
// VERB RESOURCE-TYPE in LOCATION [where CONDITIONS], as OCI's policy
// syntax has it
// (https://docs.oracle.com/en-us/iaas/Content/Identity/Concepts/policysyntax.htm).
// Anything else is of kind "", not understood, which is never a grant.
func parseOCI(st string) ociStatement {
	toks, ok := ociTokens(st)
	if !ok || len(toks) == 0 {
		return ociStatement{}
	}
	var x ociStatement
	switch strings.ToLower(toks[0]) {
	case "define", "endorse", "admit":
		return ociStatement{kind: "other"}
	case "allow", "deny":
		x.kind = strings.ToLower(toks[0])
	default:
		return ociStatement{}
	}
	word := func(i int) string {
		if i < len(toks) {
			return strings.ToLower(toks[i])
		}
		return ""
	}
	i, last := 1, ""
	for {
		switch w := word(i); w {
		case "any-user", "any-group":
			x.subjects = append(x.subjects, ociSubject{kind: w})
			i++
		case "group", "dynamic-group", "service":
			last = w
			i++
			if word(i) == "id" {
				last = w + "-id"
				i++
			}
			fallthrough
		default:
			if last == "" || i >= len(toks) {
				return ociStatement{}
			}
			sub, n := ociName(toks[i:])
			if n == 0 {
				return ociStatement{}
			}
			sub.kind = last
			x.subjects = append(x.subjects, sub)
			i += n
		}
		if word(i) == "," {
			i++
			continue
		}
		if word(i) != "to" {
			return ociStatement{}
		}
		i++
		break
	}
	if word(i) == "{" {
		for i++; word(i) != "}"; i++ {
			if i >= len(toks) {
				return ociStatement{}
			}
			if word(i) != "," {
				x.perms = append(x.perms, strings.ToUpper(toks[i]))
			}
		}
		i++
	} else {
		switch x.verb = word(i); x.verb {
		case "inspect", "read", "use", "manage":
		default:
			return ociStatement{}
		}
		x.resource = word(i + 1)
		if x.resource == "" || x.resource == "in" {
			return ociStatement{}
		}
		i += 2
	}
	if word(i) != "in" {
		return ociStatement{}
	}
	switch word(i + 1) {
	case "tenancy":
		x.loc.tenancy = true
		i += 2
	case "compartment":
		if word(i+2) == "id" {
			if i+3 >= len(toks) {
				return ociStatement{}
			}
			x.loc.id = unquote(toks[i+3])
			i += 4
		} else {
			if i+2 >= len(toks) {
				return ociStatement{}
			}
			for _, c := range strings.Split(toks[i+2], ":") {
				c = unquote(c)
				if c == "" {
					return ociStatement{}
				}
				x.loc.path = append(x.loc.path, c)
			}
			i += 3
		}
	default:
		return ociStatement{}
	}
	switch {
	case i == len(toks):
	case word(i) == "where":
		x.cond = true
	default:
		return ociStatement{}
	}
	return x
}

// ociName reads a subject's name: NAME, 'NAME', DOMAIN/NAME or
// 'DOMAIN'/'NAME'; n is how many words it took, 0 for none.
func ociName(toks []string) (ociSubject, int) {
	switch {
	case len(toks) >= 3 && toks[1] == "/" && strings.ContainsAny(toks[0][:1], `'"`) && strings.ContainsAny(toks[2][:1], `'"`):
		return ociSubject{domain: unquote(toks[0]), name: unquote(toks[2])}, 3
	case strings.ContainsAny(toks[0][:1], `'"`):
		return ociSubject{name: unquote(toks[0])}, 1
	case toks[0] == "," || toks[0] == "{" || toks[0] == "}" || strings.EqualFold(toks[0], "to"):
		return ociSubject{}, 0
	}
	if d, n, ok := strings.Cut(toks[0], "/"); ok {
		return ociSubject{domain: d, name: n}, 1
	}
	return ociSubject{name: toks[0]}, 1
}

// tri is yes, no or maybe.
type tri int

const (
	no tri = iota
	maybe
	yes
)

// ociGroups are the user's groups: the OCIDs and the names of those read
// and ACTIVE; more, the OCIDs of those not read, past mostGroups; and
// whether some weren't read, those or past mostMemberships.
type ociGroups struct {
	ids, names, more []string
	unread           bool
}

// applies says whether a statement is of the user: any user; any group,
// for a user in one; a group of theirs by OCID or by name, in the Default
// identity domain. A group named in another domain is maybe: the
// groups read are the Default domain's.
func (x ociStatement) applies(g ociGroups) tri {
	best := no
	for _, s := range x.subjects {
		t := no
		switch s.kind {
		case "any-user":
			t = yes
		case "any-group":
			switch {
			case len(g.ids) > 0:
				t = yes
			case len(g.more) > 0 || g.unread:
				t = maybe
			}
		case "group-id":
			switch {
			case containsFold(g.ids, s.name):
				t = yes
			case containsFold(g.more, s.name) || g.unread:
				t = maybe
			}
		case "group":
			switch {
			case s.domain != "" && !strings.EqualFold(s.domain, "Default"):
				t = maybe
			case containsFold(g.names, s.name):
				t = yes
			case g.unread:
				t = maybe
			}
		}
		best = max(best, t)
	}
	return best
}

func containsFold(xs []string, s string) bool {
	for _, x := range xs {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// gives are the permissions needed that a statement gives, and those it
// may give: a list of permissions in braces, which this reads as written
// but doesn't count as a grant.
func (x ociStatement) gives() (sure, perhaps []string) {
	for _, n := range ociNeeds {
		if containsFold(x.perms, n) {
			perhaps = append(perhaps, n)
		}
	}
	all := x.resource == "all-resources"
	if x.verb == "manage" && (all || x.resource == "alarms") {
		sure = append(sure, "ALARM_UPDATE")
	}
	if (x.verb == "read" || x.verb == "use" || x.verb == "manage") && (all || x.resource == "metrics") {
		sure = append(sure, "METRIC_READ")
	}
	return sure, perhaps
}

// ociCompartment is one of the compartments from the tenancy down to the
// one the alarms are in.
type ociCompartment struct{ id, name string }

// covers says whether a statement's location covers the last compartment
// of chain (from the tenancy, chain[0], down), in a policy attached to
// chain[k]. Access given in a compartment is given in those under it. A
// name is from where the policy is attached: the compartment itself, or a
// path from one under it (policies.htm, "If you attach the policy to the
// compartment or its parent, you can simply specify the compartment name.
// If you attach the policy further up the hierarchy, you must specify the
// path"); the compartment itself by its own name is maybe (below). With
// a metric read in every compartment of the tenancy (subtree), only the
// tenancy covers them all; another is maybe.
func (l ociLoc) covers(chain []ociCompartment, k int, subtree bool) tri {
	switch {
	case l.tenancy:
		if k == 0 {
			return yes
		}
		return no
	case subtree:
		return maybe
	case l.id != "":
		for _, c := range chain[k:] {
			if c.id == l.id {
				return yes
			}
		}
		return no
	}
	if k+len(l.path) < len(chain) {
		match := true
		for j, n := range l.path {
			if !strings.EqualFold(chain[k+1+j].name, n) {
				match = false
			}
		}
		if match {
			return yes
		}
	}
	// The compartment the policy is attached to, by its own name: a
	// compartment under it of the same name, which the name would read as
	// as well, can't be ruled out without listing its children, which
	// isn't asked.
	if len(l.path) == 1 && k > 0 && strings.EqualFold(chain[k].name, l.path[0]) {
		return maybe
	}
	return no
}

// ociJudged is what the policies give the user, in a compartment.
type ociJudged struct {
	granted, perhaps map[string]bool
	// why the perhaps: a condition, a deny, a statement not understood, a
	// group not read or of another domain, a location partly covered.
	conds, denies, unread, others int
}

// ociJudge reads the statements of the policies in each compartment of
// chain, by where they are attached (policies[k], chain[k]'s), for what
// they give the user in chain's last.
func ociJudge(chain []ociCompartment, policies [][]string, g ociGroups, subtree bool) ociJudged {
	j := ociJudged{granted: map[string]bool{}, perhaps: map[string]bool{}}
	relevant := func(s string) bool {
		s = strings.ToLower(s)
		return strings.Contains(s, "alarm") || strings.Contains(s, "metric") || strings.Contains(s, "all-resources")
	}
	for k, stmts := range policies {
		for _, st := range stmts {
			x := parseOCI(st)
			switch x.kind {
			case "other":
				continue
			case "":
				if relevant(st) {
					j.unread++
					for _, n := range ociNeeds {
						j.perhaps[n] = true
					}
				}
				continue
			case "deny":
				if relevant(st) || len(x.perms) > 0 {
					j.denies++
				}
				continue
			}
			app := x.applies(g)
			sure, perhaps := x.gives()
			if app == no || len(sure)+len(perhaps) == 0 {
				continue
			}
			cov := x.loc.covers(chain, k, subtree)
			if cov == no {
				continue
			}
			for _, n := range sure {
				if app == yes && cov == yes && !x.cond {
					j.granted[n] = true
				} else {
					j.perhaps[n] = true
				}
			}
			for _, n := range perhaps {
				j.perhaps[n] = true
			}
			switch {
			case x.cond:
				j.conds++
			case len(perhaps) > 0 || app == maybe || cov == maybe:
				j.others++
			}
		}
	}
	return j
}

// ociReader reads OCI's Identity API as the user, allowing each request
// as it learns what it is.
type ociReader struct {
	p        *run
	sign     func(*http.Request) error
	host     string
	tenancy  string
	compared int // the statements read
	// read are the statements of each compartment's policies, read once.
	read map[string][]string
}

// get asks Identity for path, with query, once allowed; gives the answer
// and the next page's token.
func (o *ociReader) get(ctx context.Context, path string, query url.Values, fixed []string) ([]byte, string, error) {
	h, _, ok := at(o.host)
	if !ok {
		return nil, "", fmt.Errorf("no Identity endpoint for the config's region")
	}
	// Allowed as it is learned: this path, the query's keys with the
	// values asked, and a page's token beside them.
	x := allow{api: "iam:oci", method: http.MethodGet, host: h, path: ociVersion + path}
	if len(fixed) > 0 {
		x.query = func(q url.Values) bool {
			if len(q) != len(fixed) && !(len(q) == len(fixed)+1 && len(q["page"]) == 1 && q.Get("page") != "") {
				return false
			}
			for _, k := range fixed {
				if len(q[k]) != 1 || q.Get(k) != query.Get(k) {
					return false
				}
			}
			return true
		}
	}
	o.p.al.first(x)
	u := o.host + ociVersion + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", o.p.agent())
	if err := o.sign(req); err != nil {
		return nil, "", err
	}
	hd, b, err := ask(req)
	if err != nil {
		return nil, "", err
	}
	return b, hd.Get("opc-next-page"), nil
}

// list reads every page of a list, mostPages at most.
func (o *ociReader) list(ctx context.Context, path string, q url.Values, each func([]byte) error) error {
	fixed := make([]string, 0, len(q))
	for k := range q {
		fixed = append(fixed, k)
	}
	page := ""
	for pages := 0; ; pages++ {
		if pages >= mostPages {
			return fmt.Errorf("more than %d pages", mostPages)
		}
		qq := url.Values{}
		for k, v := range q {
			qq[k] = v
		}
		if page != "" {
			qq.Set("page", page)
		}
		b, next, err := o.get(ctx, path, qq, fixed)
		if err != nil {
			return err
		}
		if err := each(b); err != nil {
			return err
		}
		if next == "" {
			return nil
		}
		page = next
	}
}

// groups reads the user's groups: its ACTIVE memberships in the tenancy,
// mostMemberships at most, then each group, mostGroups at most, for its
// name, if it is ACTIVE. A membership or a group in another state gives
// the user nothing.
func (o *ociReader) groups(ctx context.Context, user string) (ociGroups, error) {
	var g ociGroups
	seen := map[string]bool{}
	err := o.list(ctx, "/userGroupMemberships", url.Values{"compartmentId": {o.tenancy}, "userId": {user}, "limit": {"1000"}}, func(b []byte) error {
		var ms []struct {
			GroupID string `json:"groupId"`
			State   string `json:"lifecycleState"`
		}
		if err := json.Unmarshal(b, &ms); err != nil {
			return fmt.Errorf("not a list of group memberships: %w", err)
		}
		for _, m := range ms {
			switch {
			case m.GroupID == "" || m.State != "ACTIVE" || seen[m.GroupID]:
			case len(g.ids) >= mostMemberships:
				g.unread = true
			default:
				seen[m.GroupID] = true
				g.ids = append(g.ids, m.GroupID)
			}
		}
		return nil
	})
	if err != nil {
		return g, err
	}
	var active []string
	for i, id := range g.ids {
		if i >= mostGroups {
			g.unread, g.more = true, g.ids[i:]
			break
		}
		o.p.hide("--changes-oci-config's group", id)
		b, _, err := o.get(ctx, "/groups/"+url.PathEscape(id), nil, nil)
		if err != nil {
			return g, err
		}
		var grp struct {
			Name  string `json:"name"`
			State string `json:"lifecycleState"`
		}
		if err := json.Unmarshal(b, &grp); err != nil {
			return g, fmt.Errorf("not a group: %w", err)
		}
		o.p.hideSaid("--changes-oci-config's group's name", grp.Name)
		if grp.State == "ACTIVE" {
			active = append(active, id)
			g.names = append(g.names, grp.Name)
		}
	}
	g.ids = active
	return g, nil
}

// chain reads the compartments from the tenancy down to c, each by its
// parent (GetCompartment's compartmentId), mostLevels at most.
func (o *ociReader) chain(ctx context.Context, c string) ([]ociCompartment, error) {
	var up []ociCompartment
	for id := c; id != o.tenancy; {
		if len(up) >= mostLevels {
			return nil, fmt.Errorf("more than %d compartments above the alarms'", mostLevels)
		}
		o.p.hide("an OCI compartment", id)
		b, _, err := o.get(ctx, "/compartments/"+url.PathEscape(id), nil, nil)
		if err != nil {
			return nil, err
		}
		var cm struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Parent string `json:"compartmentId"`
		}
		if err := json.Unmarshal(b, &cm); err != nil || cm.ID != id || cm.Parent == "" {
			return nil, fmt.Errorf("not the compartment asked for")
		}
		o.p.hideSaid("an OCI compartment's name", cm.Name)
		up = append(up, ociCompartment{id: cm.ID, name: cm.Name})
		id = cm.Parent
	}
	out := []ociCompartment{{id: o.tenancy}}
	for i := len(up) - 1; i >= 0; i-- {
		out = append(out, up[i])
	}
	return out, nil
}

// policies reads the statements of the policies attached to each
// compartment of chain, mostStatements all told.
func (o *ociReader) policies(ctx context.Context, chain []ociCompartment) ([][]string, error) {
	out := make([][]string, len(chain))
	if o.read == nil {
		o.read = map[string][]string{}
	}
	for k, c := range chain {
		if st, ok := o.read[c.id]; ok {
			out[k] = st
			continue
		}
		err := o.list(ctx, "/policies", url.Values{"compartmentId": {c.id}, "limit": {"1000"}}, func(b []byte) error {
			var ps []struct {
				Statements []string `json:"statements"`
				State      string   `json:"lifecycleState"`
			}
			if err := json.Unmarshal(b, &ps); err != nil {
				return fmt.Errorf("not a list of policies: %w", err)
			}
			for _, p := range ps {
				if p.State != "ACTIVE" {
					continue // one being made, or deleted, gives nothing
				}
				o.compared += len(p.Statements)
				if o.compared > mostStatements {
					return fmt.Errorf("more than %d statements", mostStatements)
				}
				out[k] = append(out[k], p.Statements...)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		o.read[c.id] = out[k]
	}
	return out, nil
}

// ociAlarm is an alarm --changes-in names, as this run read it: its
// compartment, its metric's, and whether its metric is read in every
// compartment of the tenancy.
type ociAlarm struct {
	compartment, metric string
	metricSubtree       bool
}

// ociAlarms are the alarms of OCI's --alarms that --changes-in names, as
// read in this run (rules.read): an alarm is written where it is, its
// metric read where it is, and serve writes no other. unnamed says of an
// entry that names none read.
func (p *run) ociAlarms() (alarms []ociAlarm, unnamed []string) {
	s := p.c.Serve
	for _, a := range s.Alarms {
		if k, _ := source.AlarmsKind(a.URL); k != source.AlarmsOCI {
			continue
		}
		for _, pat := range s.ChangesIn {
			src, name, ok := strings.Cut(pat, ":")
			if !ok || src != a.Source {
				continue
			}
			found := 0
			for _, d := range p.docs["alarms:"+a.Source] {
				if !namedBy(name, rules.AlarmName(d.Data, "oci")) {
					continue
				}
				var doc struct {
					Data []struct {
						Compartment   string `json:"compartmentId"`
						Metric        string `json:"metricCompartmentId"`
						MetricSubtree bool   `json:"metricCompartmentIdInSubtree"`
					} `json:"data"`
				}
				if json.Unmarshal(d.Data, &doc) != nil || len(doc.Data) != 1 || doc.Data[0].Compartment == "" || doc.Data[0].Metric == "" {
					unnamed = append(unnamed, "--alarms "+a.Source+": an alarm read without its compartments")
					continue
				}
				found++
				x := ociAlarm{doc.Data[0].Compartment, doc.Data[0].Metric, doc.Data[0].MetricSubtree}
				if !slices.Contains(alarms, x) {
					alarms = append(alarms, x)
				}
			}
			if found == 0 {
				unnamed = append(unnamed, "--alarms "+a.Source+": no alarm read that an entry of --changes-in names")
			}
		}
	}
	slices.Sort(unnamed)
	return alarms, slices.Compact(unnamed)
}

// namedBy reports whether an entry of --changes-in names a name, as serve
// reads one: the name, or a prefix ending in *.
func namedBy(pat, name string) bool {
	if p, ok := strings.CutSuffix(pat, "*"); ok {
		return strings.HasPrefix(name, p)
	}
	return pat == name
}

// ociWrites reads the user's groups and, for each alarm --changes-in
// names, the policies of its compartment and of each above it, and of its
// metric's, and judges what they give the user: manage alarms where the
// alarm is, read metrics where its metric is, as UpdateAlarm takes. A
// statement with a condition, or one not understood, is never a grant.
// Reading them takes inspect groups, users, compartments and policies in
// the tenancy (iampolicyreference.htm): without it, the item is unknown.
func (p *run) ociWrites(ctx context.Context, w writer) Item {
	id := w.id
	alarms, unnamed := p.ociAlarms()
	var oks, problems, unknowns []string
	unknowns = append(unknowns, unnamed...)
	if len(alarms) == 0 {
		return verdictOf(id, w.flag, nil, nil, unknowns, "", "")
	}
	u, err := ociConfigUser(w.who)
	if err != nil {
		return Item{ID: id, State: Unknown, What: "whether " + w.flag + " may write isn't asked", Why: p.cloudSaid(err)}
	}
	each, cancel := context.WithTimeout(ctx, eachTime)
	defer cancel()
	o := &ociReader{p: p, sign: w.sign, host: u.Identity, tenancy: u.Tenancy}
	cantRead := func(err error) Item {
		return Item{ID: id, State: Unknown, What: "whether " + w.flag + " may write isn't known: its user's groups and the policies couldn't be read", Why: p.cloudSaid(err),
			Fix: "let the user inspect groups, users, compartments and policies in the tenancy, which reads them and changes nothing, and this is known; serve itself never needs it"}
	}
	g, err := o.groups(each, u.User)
	if err != nil {
		return cantRead(err)
	}
	// Each compartment asked once, for what is needed there: ALARM_UPDATE
	// where an alarm is, METRIC_READ where its metric is (the tenancy, in
	// the subtree, where only a grant in the tenancy covers it).
	type need struct {
		compartment, perm string
		subtree           bool
	}
	var needs []need
	for _, a := range alarms {
		m := need{a.metric, "METRIC_READ", a.metricSubtree}
		if a.metricSubtree {
			m.compartment = u.Tenancy
		}
		for _, n := range []need{{a.compartment, "ALARM_UPDATE", false}, m} {
			if !slices.Contains(needs, n) {
				needs = append(needs, n)
			}
		}
	}
	type read struct {
		chain    []ociCompartment
		policies [][]string
	}
	cache := map[string]read{}
	for _, n := range needs {
		where := "where an alarm --changes-in names is"
		if n.perm == "METRIC_READ" {
			where = "where its metric is"
			if n.subtree {
				where = "in every compartment of the tenancy, where its metric is read"
			}
		}
		r, ok := cache[n.compartment]
		if !ok {
			if len(cache) >= mostCompartments {
				unknowns = append(unknowns, where+": past the first "+fmt.Sprint(mostCompartments)+" compartments, not asked")
				continue
			}
			chain, err := o.chain(each, n.compartment)
			if err != nil {
				return cantRead(err)
			}
			pols, err := o.policies(each, chain)
			if err != nil {
				return cantRead(err)
			}
			r = read{chain, pols}
			cache[n.compartment] = r
		}
		j := ociJudge(r.chain, r.policies, g, n.subtree)
		switch says := ociSays[n.perm]; {
		case j.granted[n.perm] && j.denies == 0:
			oks = append(oks, where+": "+says)
		case !j.granted[n.perm] && !j.perhaps[n.perm] && n.subtree:
			unknowns = append(unknowns, where+": no policy of the tenancy's gives the user "+says+" in the tenancy, and those in its compartments aren't read")
		case !j.granted[n.perm] && !j.perhaps[n.perm]:
			problems = append(problems, where+": no policy gives the user "+says)
		default:
			var why []string
			if j.conds > 0 {
				why = append(why, "a policy with a condition")
			}
			if j.denies > 0 {
				why = append(why, "a deny statement")
			}
			if j.unread > 0 {
				why = append(why, "a statement not understood")
			}
			if j.others > 0 {
				why = append(why, "a group not read, a permission list, a compartment named by its own name or a location only partly covering")
			}
			unknowns = append(unknowns, where+": "+says+" given only by "+strings.Join(why, ", ")+", which isn't counted as a grant")
		}
	}
	return verdictOf(id, w.flag, oks, problems, unknowns, fmt.Sprintf("as %s of the policies above the alarms and their metrics say, read and judged here", plural(o.compared, "statement")),
		"give the user's group a policy to manage alarms where each alarm is, and read metrics where its metric is (charts/lapilli/README.md has the least privilege)")
}
