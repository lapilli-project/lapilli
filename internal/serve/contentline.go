package serve

import (
	"context"
	"fmt"
	"strings"

	"lapilli.dev/internal/content"
)

// contentLine sums up, in one line in lang, the problems with what the
// rules in accepts say: those of fact and those measured counted, by check,
// and the guesses apart (docs/design/content.md). It is "" when there is
// nothing.
func (s *Server) contentLine(ctx context.Context, lang string, in func(rule string) bool) (string, error) {
	cf, err := s.contentFindings(ctx)
	if err != nil {
		return "", err
	}
	by := map[string]int{}
	problems, guesses := 0, 0
	rulesWith := 0
	for name, fs := range cf {
		if in != nil && !in(name) {
			continue
		}
		had := false
		for _, f := range fs {
			if f.Class == content.Heuristic {
				guesses++
				continue
			}
			by[f.Kind]++
			problems++
			had = true
		}
		if had {
			rulesWith++
		}
	}
	if problems+guesses == 0 {
		return "", nil
	}
	var parts []string
	for _, check := range content.Checks {
		if n := by[check]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s ×%d", tr(lang, content.Message(check)), n))
		}
	}
	var line string
	if problems > 0 {
		line = tr(lang, "What the rules say: %d problems in %d rules (%s)", problems, rulesWith, strings.Join(parts, ", "))
	} else {
		line = tr(lang, "What the rules say: no problems")
	}
	if guesses > 0 {
		line += "; " + tr(lang, "%d guesses, apart", guesses)
	}
	return line + ".", nil
}
