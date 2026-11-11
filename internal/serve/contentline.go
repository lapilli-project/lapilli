package serve

import (
	"context"
	"fmt"
	"strings"

	"lapilli.dev/internal/content"
)

// contentSum is what the content check finds now in the rules a report
// covers (docs/design/content.md): the problems of fact and those measured,
// by check, in how many rules; the guesses apart, by check. It is what the
// rules say now, not what changed since the report before.
type contentSum struct {
	Problems int            `json:"problems"`
	Rules    int            `json:"rules"`
	ByCheck  map[string]int `json:"by_check"`
	Guesses  int            `json:"guesses"`
	// GuessesByCheck are the heuristics', by check.
	GuessesByCheck map[string]int `json:"guesses_by_check"`
	// Text is the line a report shows, in its language; "" for nothing.
	Text string `json:"text"`
}

// contentSummary sums up the problems with what the rules in accepts say,
// with its line in lang: those of fact and those measured counted, by
// check, and the guesses apart.
func (s *Server) contentSummary(ctx context.Context, lang string, in func(rule string) bool) (*contentSum, error) {
	cf, err := s.contentFindings(ctx)
	if err != nil {
		return nil, err
	}
	sum := &contentSum{ByCheck: map[string]int{}, GuessesByCheck: map[string]int{}}
	for name, fs := range cf {
		if in != nil && !in(name) {
			continue
		}
		had := false
		for _, f := range fs {
			if f.Class == content.Heuristic {
				sum.Guesses++
				sum.GuessesByCheck[f.Kind]++
				continue
			}
			sum.ByCheck[f.Kind]++
			sum.Problems++
			had = true
		}
		if had {
			sum.Rules++
		}
	}
	if sum.Problems+sum.Guesses == 0 {
		return sum, nil
	}
	var parts []string
	for _, check := range content.Checks {
		if n := sum.ByCheck[check]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s ×%d", tr(lang, content.Message(check)), n))
		}
	}
	if sum.Problems > 0 {
		sum.Text = tr(lang, "What the rules say: %d problems in %d rules (%s)", sum.Problems, sum.Rules, strings.Join(parts, ", "))
	} else {
		sum.Text = tr(lang, "What the rules say: no problems")
	}
	if sum.Guesses > 0 {
		sum.Text += "; " + tr(lang, "%d guesses, apart", sum.Guesses)
	}
	sum.Text += "."
	return sum, nil
}

// contentLine is contentSummary's line: "" when there is nothing.
func (s *Server) contentLine(ctx context.Context, lang string, in func(rule string) bool) (string, error) {
	sum, err := s.contentSummary(ctx, lang, in)
	if err != nil {
		return "", err
	}
	return sum.Text, nil
}
