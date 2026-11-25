package rules

import "strconv"

// parsedKind reads a rule's expression as what it is, for Parsed: its
// expression in one form, and what it is written in, which Parsed has in
// its result so that a Loki ruler's rule, or one cloud's alarm, is never
// another's nor a Prometheus rule's of the same text. ok is false for a
// rule whose expression isn't read: a Grafana rule's, an alarm that isn't
// PromQL (it has no expression), or one parsedExpr or parsedLogQL refuse.
func (r Rule) parsedKind() (expr, kind string, ok bool) {
	switch {
	case r.Logs != "":
		expr, ok = parsedLogQL(r.Expr)
		return expr, " logql " + strconv.Quote(r.Logs), ok
	case r.Cloud != "":
		expr, ok = parsedExpr(r.Expr)
		return expr, " cloud " + strconv.Quote(r.Cloud), ok
	}
	expr, ok = parsedExpr(r.Expr)
	return expr, "", ok
}
