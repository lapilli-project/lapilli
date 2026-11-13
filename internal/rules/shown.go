package rules

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// turns reports whether a character doesn't show as written or turns the
// text around it: a control character but a line break or a tab, and the
// direction marks, embeddings, overrides and isolates. A rule's name, its
// group, its labels and what it says go on pages beside other text, in
// Slack and to agents, and come from whoever writes rules (a file, a
// ruler's tenant, a Grafana editor): an override at a name's end would
// turn the text after it around ("fired 21 → 3"). As the ledger takes a
// tracker's comment (shownText).
func turns(r rune) bool {
	switch {
	case r == '\n' || r == '\r' || r == '\t':
		return false
	case unicode.IsControl(r), r == 0x061C, r == 0x200E || r == 0x200F, r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

// Turns reports whether a character doesn't show as written or turns the
// text around it, as Shown takes it.
func Turns(r rune) bool { return turns(r) }

// Shown is s with each character that turns, as turns says, as U+FFFD,
// which shows; and what isn't UTF-8 as U+FFFD too. s as it is, when it has
// none.
func Shown(s string) string {
	if utf8.ValidString(s) && strings.IndexFunc(s, turns) < 0 {
		return s
	}
	return strings.Map(func(r rune) rune {
		if turns(r) {
			return utf8.RuneError
		}
		return r
	}, strings.ToValidUTF8(s, "�"))
}

// shownRules are rules with their names, groups, labels and their groups'
// labels as Shown gives them: read so, from any source. What they say is
// (cut). An expression isn't: what it asks is the query's, and a label
// value in it may be written with a direction mark; it is shown so where
// a page shows it (the template's shown).
func shownRules(rs []Rule) []Rule {
	for i := range rs {
		r := &rs[i]
		r.Name, r.Group = Shown(r.Name), Shown(r.Group)
		r.Labels, r.GroupLabels = shownLabels(r.Labels), shownLabels(r.GroupLabels)
	}
	return rs
}

// shownLabels are labels as Shown gives them: as they are, when they show.
func shownLabels(m map[string]string) map[string]string {
	if labelsShow(m) {
		return m
	}
	l := make(map[string]string, len(m))
	for k, v := range m {
		l[Shown(k)] = Shown(v)
	}
	return l
}

func labelsShow(m map[string]string) bool {
	for k, v := range m {
		if Shown(k) != k || Shown(v) != v {
			return false
		}
	}
	return true
}
