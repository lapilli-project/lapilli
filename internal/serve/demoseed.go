package serve

import (
	"context"

	"lapilli.dev/internal/ledger"
)

// ProposeSuggested keeps, as by, a proposal for the first of the last
// digest's suggestions: its rule's longer "for", backtested as a person's
// from the rule's page is. It is what lapilli demo --serve seeds, from
// Config.Seed, and says it made one with ok; ok is false, and nothing is
// made, while the digest has no suggestion or the rule has a proposal
// already.
func (s *Server) ProposeSuggested(ctx context.Context, by string) (id int64, ok bool, err error) {
	ss, _, err := s.suggestionsNow(ctx, nil)
	if err != nil || len(ss) == 0 {
		return 0, false, err
	}
	x := ss[0]
	id, err = s.propose(ctx, ask{Rule: x.Rule, Kind: ledger.KindFor, For: x.NewFor, From: "suggestion"}, by, false, true)
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}
