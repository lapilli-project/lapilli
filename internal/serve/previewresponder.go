package serve

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/response"
	"lapilli.dev/internal/verdict"
)

// previewResponder gives a preview's judging the responses the ledger finds
// for the firings it judges, so that with rules found noisy by what was done
// about their firings (--noisy-by useful) a preview asks the test every other
// surface does (verdict.RuleNoisy): a firing's usefulness rests on whether it
// caught anything and was judged, which the criterion tried changes, so it is
// worked out from the response found (response.Useful) and not read from
// what the ledger kept.
type previewResponder struct {
	by map[string]response.Response
}

var _ verdict.Responder = (*previewResponder)(nil)

func previewKey(rule string, start time.Time, labels map[string]string) string {
	b, _ := json.Marshal(labels) // keys sorted
	return rule + "\x00" + strconv.FormatInt(start.Unix(), 10) + "\x00" + string(b)
}

func (p *previewResponder) Respond(f firing.Firing, _ time.Duration, _ time.Time, white, judged bool, answer string) verdict.FiringResponse {
	resp, ok := p.by[previewKey(f.Rule, f.Start, f.Labels)]
	if !ok {
		return verdict.FiringResponse{Kind: verdict.ResponseNone}
	}
	return ledger.ResponseOfJudged(resp, white, judged, answer)
}

func (p *previewResponder) Totals() verdict.ResponseTotals { return verdict.ResponseTotals{} }

// previewResponses is the responder a preview judges fs with (the firings it
// judges, none excused), or nil when rules are found noisy by what they
// caught, which asks for none.
func (s *Server) previewResponses(ctx context.Context, fs []ledger.Firing, o verdict.Options) (verdict.Responder, error) {
	if o.NoisyBy != verdict.NoisyByUseful {
		return nil, nil
	}
	got, err := s.l.ResponsesOf(ctx, fs, o, s.now())
	if err != nil {
		return nil, err
	}
	p := &previewResponder{by: make(map[string]response.Response, len(fs))}
	for _, f := range fs {
		if r, ok := got[f.ID]; ok {
			p.by[previewKey(f.Rule, f.Start, f.Labels)] = r
		}
	}
	return p, nil
}
