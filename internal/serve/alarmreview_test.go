package serve

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"lapilli.dev/internal/alarmtest"
	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/source"
)

// What two reviews of the alarm path found (docs/design/alarms.md, "As
// built"), each held by a test that fails without its fix.

// undo asks for an applied change to be undone, as ana, and carries it out.
func undo(t *testing.T, s *Server, id int64) ledger.Proposal {
	t.Helper()
	h := s.Handler()
	key := keyFor(t, h, id, ana, "undo")
	_, _, hd := ui(t, h, http.MethodPost, "/proposals/"+itoa(id)+"/undo", ana, "same-origin", url.Values{"key": {key}})
	if err := s.carryOut(context.Background()); err != nil {
		t.Fatal(err)
	}
	return state(t, s, locID(hd))
}

// restart is lapilli serve started again: its alarms not read yet.
func restart(t *testing.T, s *Server, raw string) {
	t.Helper()
	fresh, err := source.NewAlarms(s.alarms[0].Source, raw, "lapilli/test")
	if err != nil {
		t.Fatal(err)
	}
	fresh.Transport, fresh.Reads, fresh.Now = s.alarms[0].Transport, s.alarms[0].Reads, s.alarms[0].Now
	s.alarms[0] = fresh
}

// deleted retires a PromQL alarm: off, then deleted four weeks later.
func deleted(t *testing.T, s *Server, rule string) int64 {
	t.Helper()
	ctx := context.Background()
	h := s.Handler()
	var off int64
	if ps, _ := s.l.Proposals(ctx, ledger.ProposalQuery{Rule: rule, States: []string{ledger.StateProposed}}); len(ps) == 1 && ps[0].Kind == ledger.KindDisable {
		off = ps[0].ID
	} else {
		off = proposeRule(t, h, rule, "disable", nil)
	}
	approve(t, h, off, ana)
	s.jobs(ctx, false)
	if p := state(t, s, off); p.State != ledger.StateApplied {
		t.Fatalf("%s off: %s %q", rule, p.State, p.Reason)
	}
	raw := ""
	s.look(ctx, &raw)
	was := s.now
	s.now = func() time.Time { return time.Now().Add(5 * weekLen) }
	defer func() { s.now = was }()
	if err := s.proposeFound(ctx); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.l.Proposals(ctx, ledger.ProposalQuery{Rule: rule, States: []string{ledger.StateProposed}})
	if len(ps) != 1 || ps[0].Kind != ledger.KindDelete {
		t.Fatalf("deleting %s: %+v", rule, ps)
	}
	approve(t, h, ps[0].ID, cy)
	s.jobs(ctx, false)
	if p := state(t, s, ps[0].ID); p.State != ledger.StateApplied {
		t.Fatalf("%s deleted: %s %q", rule, p.State, p.Reason)
	}
	return ps[0].ID
}

// M1: an undo is a write as any other. It is refused where the change it
// undoes would be, now: an alarm since made automation, or given a
// warm-up; one --changes-in no longer names, after a restart too; and a
// deleted alarm isn't made again once --changes-in no longer names it, or
// its tags, as kept, are another tool's, on CloudWatch and Google alike.
func TestAlarmUndoAsAWrite(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		set  func(*alarmtest.CloudWatch, *Server)
		want string
	}{
		"made automation since": {func(cw *alarmtest.CloudWatch, _ *Server) {
			cw.Metric["checkout-5xx"]["AlarmActions"] = []any{"arn:aws:autoscaling:eu-west-1:111122223333:scalingPolicy:x:autoScalingGroupName/web:policyName/up"}
		}, noPathAutomation},
		"given a warm-up since": {func(cw *alarmtest.CloudWatch, _ *Server) {
			cw.Metric["checkout-5xx"]["WarmUpConfiguration"] = map[string]any{"WarmUpPeriodDurationInMinutes": 10}
		}, noPathWarmUp},
		"not named since, restarted": {func(_ *alarmtest.CloudWatch, s *Server) {
			s.cfg.ChangesIn = []string{"cw:latency"}
			restart(t, s, cwURL)
		}, noPathAlarmIn},
	} {
		t.Run(name, func(t *testing.T) {
			cw := cloudWatchAlarms()
			s := cwServer(t, cw, nil)
			id := proposeRule(t, s.Handler(), "checkout-5xx", "for", url.Values{"for": {"15m"}})
			approve(t, s.Handler(), id, ana)
			s.jobs(ctx, false)
			cw.Lock()
			c.set(cw, s)
			cw.Unlock()
			if u := undo(t, s, id); u.State != ledger.StateRefused || u.Reason != c.want || len(cw.Took()) != 1 {
				t.Errorf("%s %q, written %v", u.State, u.Reason, cw.Took())
			}
		})
	}
	t.Run("deleted, then not named or another tool's", func(t *testing.T) {
		for _, set := range []func(*Server){
			func(s *Server) { s.cfg.ChangesIn = []string{"cw:checkout-*"} },
			func(s *Server) { s.cfg.ChangesManaged = []string{"team=shop"} },
		} {
			cw := cloudWatchAlarms()
			s := cwServer(t, cw, nil)
			s.now = func() time.Time { return now.Add(13 * weekLen) }
			s.proposeFound(ctx)
			del := deleted(t, s, "latency")
			set(s)
			if u := undo(t, s, del); u.State != ledger.StateRefused || cw.Alarm("latency") != nil {
				t.Errorf("made again: %s %q", u.State, u.Reason)
			}
		}
	})
	t.Run("a Google policy deleted, then not named", func(t *testing.T) {
		g, _ := googlePolicies()
		cloudErrs(t, &g.Common)
		s := alarmServer(t, "gcp", "https://monitoring.googleapis.com/v3/projects/shop/alertPolicies", g, []string{"gcp:Down", "gcp:Shop"}, nil)
		del := deleted(t, s, "Down")
		s.cfg.ChangesIn = []string{"gcp:Shop"}
		if u := undo(t, s, del); u.State != ledger.StateRefused || u.Reason != noPathAlarmIn || g.Took()[len(g.Took())-1] == "POST create" {
			t.Errorf("made again: %s %q %v", u.State, u.Reason, g.Took())
		}
	})
}

// M2: an alarm's parents are asked with ParentsOfAlarmName alone, as AWS
// takes it (the stand-in refuses any other parameter with it): one a
// composite alarm watches, or two do, isn't deleted, one none watches is;
// and an error, a throttle, is the cloud's, not "a composite alarm watches
// it".
func TestAlarmCompositeParents(t *testing.T) {
	ctx := context.Background()
	cw := cloudWatchAlarms()
	cw.AddComposite("shop-slow", "ALARM(latency) OR ALARM(cpu-high)")
	s := cwServer(t, cw, nil)
	tg := alarmTarget{s}
	for name, want := range map[string]string{"checkout-5xx": noPathWatched, "cpu-high": noPathWatched, "scale-out": noPathAutomation, "warm": ""} {
		if why, err := tg.offered(ctx, mustUnit(t, s, "cw", cw.ARN(name)), ledgerRule(t, s, name), ledger.KindDelete); why != want || err != nil {
			t.Errorf("%s: %q %v, want %q", name, why, err, want)
		}
	}
	cw.FailParents = true
	if why, err := tg.offered(ctx, mustUnit(t, s, "cw", cw.ARN("warm")), ledgerRule(t, s, "warm"), ledger.KindDelete); err == nil || why != "" {
		t.Errorf("throttled: %q %v", why, err)
	}
}
