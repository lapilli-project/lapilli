package sse

import (
	"strconv"
	"strings"
)

// Canon is a math expression written out in one form: what ParseMath read
// of it, so its spacing, its parentheses that change nothing and a
// number's spelling (1, 1.0, 1e0) are gone, and two expressions that
// evaluate alike have one Canon. ok is false for an expression that
// ParseMath didn't give, which isn't written so.
func Canon(e Expr) (string, bool) {
	var b strings.Builder
	if !canon(&b, e) {
		return "", false
	}
	return b.String(), true
}

func canon(b *strings.Builder, e Expr) bool {
	switch x := e.(type) {
	case number:
		b.WriteString(strconv.FormatFloat(float64(x), 'g', -1, 64))
	case ref:
		b.WriteString("$" + strconv.Quote(string(x)))
	case unaryOp:
		b.WriteString("(" + x.op + " ")
		if !canon(b, x.e) {
			return false
		}
		b.WriteString(")")
	case call:
		b.WriteString(x.name + "(")
		if !canon(b, x.arg) {
			return false
		}
		b.WriteString(")")
	case binary:
		b.WriteString("(")
		if !canon(b, x.l) {
			return false
		}
		b.WriteString(" " + x.op + " ")
		if !canon(b, x.r) {
			return false
		}
		b.WriteString(")")
	default:
		return false
	}
	return true
}
