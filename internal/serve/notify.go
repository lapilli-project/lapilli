package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/report"
)

// The notifier sends what the outbox holds: to a webhook, as JSON with a
// link to the page; to Slack, as a line of text. It runs apart from the
// worker, so a slow receiver holds up neither runs nor pages.

// dests are where what happens is told.
func (s *Server) dests() []string {
	var out []string
	if s.cfg.NotifyWebhook != "" {
		out = append(out, "webhook")
	}
	if s.cfg.NotifySlack && s.cfg.Slack != "" {
		out = append(out, "slack")
	}
	return out
}

// sendsTo are the places the outbox has anything for: where what happens is
// told, teams, whom only their digests go to, and the destinations reports
// are sent to by name.
func (s *Server) sendsTo() []string {
	out := s.dests()
	if len(s.cfg.TeamDigests) > 0 {
		out = append(out, teamDest)
	}
	if len(s.cfg.ReportDests) > 0 || s.cfg.Slack != "" || len(s.cfg.TeamDigests) > 0 {
		out = append(out, reportDest)
	}
	return out
}

// teamDest is a team's own digest, in the outbox.
const teamDest = "team"

// notify sends what the outbox holds, a loop for each place it goes, so a
// receiver that is down doesn't hold up the others.
func (s *Server) notify(ctx context.Context) {
	var wg sync.WaitGroup
	for _, dest := range s.sendsTo() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if err := s.sendDue(ctx, dest, time.Now()); err != nil {
					s.fail("notifying", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
			}
		}()
	}
	wg.Wait()
}

// sendDue sends the notices due to dest at now, the oldest first: the
// outbox keeps the wall clock's time, not the runs'. At the first that
// fails, the rest wait for the next round, rather than each wait out a
// receiver that is down; except teams' and reports', each its own receiver.
func (s *Server) sendDue(ctx context.Context, dest string, now time.Time) error {
	due, err := s.l.Due(ctx, dest, now, 50)
	if err != nil {
		return err
	}
	for _, n := range due {
		// Read through a reader: sent only while this process still holds
		// the ledger, or a process that took it over sends it too.
		if err := s.l.Hold(ctx); err != nil {
			return err
		}
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := s.send(sctx, n)
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		result := "sent"
		if err != nil {
			result = "failed"
		}
		s.status.Lock()
		s.status.notices[n.Dest+" "+result]++
		s.status.Unlock()
		if err != nil {
			s.log.Warn("a notice wasn't sent; it is tried again later", "event", n.Event, "to", n.Dest, "attempts", n.Attempts+1, "err", err)
			if err := s.l.Failed(ctx, n.ID, err.Error()); err != nil || (dest != teamDest && dest != reportDest) {
				return err
			}
			continue
		}
		if err := s.l.Sent(ctx, n.ID); err != nil {
			return err
		}
	}
	return nil
}

// notice is what a notice says, read back from the outbox.
type notice struct {
	Event    string           `json:"event"`
	At       time.Time        `json:"at"`
	By       string           `json:"by,omitempty"`
	Was      string           `json:"was,omitempty"`
	Task     *ledger.Task     `json:"task,omitempty"`
	Incident *json.RawMessage `json:"incident,omitempty"`
	Window   *ledger.Window   `json:"window,omitempty"`
	Comment  *ledger.Comment  `json:"comment,omitempty"`
	// A source firings are copied in from, gone quiet.
	Source *ledger.Source `json:"source,omitempty"`
	// A digest: the team it is for, when it is for one, and the report.
	Team string `json:"team,omitempty"`
	Text string `json:"text,omitempty"`
	// A report people scheduled, and for its text, the destination it goes
	// to by name.
	ReportID int64  `json:"report_id,omitempty"`
	To       string `json:"to,omitempty"`
	// A proposal to change a rule, and what became of it.
	Proposal *proposalNotice `json:"proposal,omitempty"`
}

// proposalNotice is what a notice says of a proposal.
type proposalNotice struct {
	ID     int64  `json:"id"`
	Rule   string `json:"rule"`
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	State  string `json:"state"`
	Link   string `json:"link"`
	Reason string `json:"reason"`
	MadeBy string `json:"made_by"`
	Agent  bool   `json:"agent"`
}

// page is the page a notice is about, under --external-url.
func (s *Server) pageOf(n notice) string {
	path := "/"
	switch {
	case n.ReportID != 0:
		path = fmt.Sprintf("/reports/%d", n.ReportID)
	case n.Team != "":
		path = "/?team=" + url.QueryEscape(n.Team)
	case n.Source != nil:
		path = "/runs"
	case n.Proposal != nil:
		path = fmt.Sprintf("/proposals/%d", n.Proposal.ID)
	case n.Task != nil:
		path = fmt.Sprintf("/tasks/%d", n.Task.ID)
	case n.Window != nil:
		path = fmt.Sprintf("/windows/%d", n.Window.ID)
	case n.Comment != nil && n.Comment.On == "task":
		path = "/tasks/" + url.PathEscape(n.Comment.Key)
	case n.Comment != nil:
		path = "/" + n.Comment.On + "s/" + url.PathEscape(n.Comment.Key)
	case n.Incident != nil:
		var in struct {
			ID string `json:"id"`
		}
		json.Unmarshal(*n.Incident, &in)
		path = "/incidents/" + url.PathEscape(in.ID)
	}
	if s.cfg.ExternalURL == "" {
		return ""
	}
	return strings.TrimRight(s.cfg.ExternalURL, "/") + path
}

func (s *Server) send(ctx context.Context, x ledger.Notice) error {
	var n notice
	if err := json.Unmarshal(x.Body, &n); err != nil {
		return err
	}
	link := s.pageOf(n)
	switch x.Dest {
	case "webhook":
		var body map[string]any
		if err := json.Unmarshal(x.Body, &body); err != nil {
			return err
		}
		if link != "" {
			body["url"] = link
		}
		// The same each time it is tried: sent twice (a receiver that took
		// it, but answered too late), it can be told apart.
		body["id"] = fmt.Sprintf("lapilli-%d", x.ID)
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		return report.Webhook(ctx, s.cfg.NotifyWebhook, b, body["id"].(string))
	case "slack":
		text := s.line(n)
		if link != "" {
			text += " " + link
		}
		return report.SlackMessage(ctx, s.cfg.Slack, text)
	case teamDest:
		file, ok := s.cfg.TeamDigests[n.Team]
		if !ok {
			return fmt.Errorf("no --team-digest for %q any more", n.Team)
		}
		return report.SlackFrom(ctx, file, "--team-digest "+n.Team, n.Text)
	case reportDest:
		file, flag, ok := s.destFile(n.To)
		if !ok {
			return fmt.Errorf("no destination called %q any more", n.To)
		}
		return report.SlackFrom(ctx, file, flag, n.Text)
	}
	return fmt.Errorf("nowhere called %q to send to", x.Dest)
}

// line is a notice in a sentence, in --lang's language.
func (s *Server) line(n notice) string {
	l := s.cfg.Lang
	who := n.By
	if who == "" || who == "anonymous" {
		who = tr(l, "anonymous")
	}
	switch {
	case n.Task != nil:
		t := n.Task
		switch n.Event {
		case "task.created":
			return tr(l, "lapilli: %s made task #%d, %q", who, t.ID, t.Title)
		case "task.assigned":
			return tr(l, "lapilli: task #%d, %q, is now %q's", t.ID, t.Title, t.Assignee)
		case "task.status":
			return tr(l, "lapilli: %s marked task #%d, %q, %s", who, t.ID, t.Title, tr(l, t.Status))
		case "task.overdue":
			return tr(l, "lapilli: task #%d, %q, was due %s", t.ID, t.Title, t.Due.In(s.cfg.Loc).Format("2006-01-02"))
		}
	case n.Window != nil:
		return tr(l, "lapilli: %s kept a quiet window, %q, from %s", who, n.Window.Title, n.Window.Start.In(s.cfg.Loc).Format("2006-01-02 15:04"))
	case n.Source != nil:
		return tr(l, "lapilli: nothing has been copied in from %s since %s; the job copying its firings may have stopped", n.Source.Name, n.Source.Written.In(s.cfg.Loc).Format("2006-01-02 15:04"))
	case n.Comment != nil:
		return tr(l, "lapilli: %s commented on %s %q", who, tr(l, n.Comment.On), n.Comment.Key)
	case n.Proposal != nil:
		p := n.Proposal
		kind := tr(l, kindText(p.Kind))
		switch n.Event {
		case "proposal.made":
			if p.Agent {
				return tr(l, "lapilli: an agent, for %s, proposed #%d, %s for %s", who, p.ID, kind, p.Rule)
			}
			return tr(l, "lapilli: %s proposed #%d, %s for %s", who, p.ID, kind, p.Rule)
		case "proposal.approved":
			return tr(l, "lapilli: %s approved #%d, %s for %s", who, p.ID, kind, p.Rule)
		case "change.opened":
			return tr(l, "lapilli: a pull request is open for #%d, %s for %s: %s", p.ID, kind, p.Rule, p.Link)
		case "change.applied":
			return tr(l, "lapilli: #%d, %s for %s, is applied", p.ID, kind, p.Rule)
		case "change.refused":
			return tr(l, "lapilli: #%d, %s for %s, was refused when checked again: %s", p.ID, kind, p.Rule, p.Reason)
		case "change.failed":
			return tr(l, "lapilli: #%d, %s for %s, failed: %s", p.ID, kind, p.Rule, p.Reason)
		case "change.undone":
			return tr(l, "lapilli: #%d, %s for %s, is undone", p.ID, kind, p.Rule)
		case "change.amended":
			return tr(l, "lapilli: #%d, %s for %s, was merged changed in review: the rule is neither as it was nor as proposed", p.ID, kind, p.Rule)
		}
	case n.Incident != nil:
		var in struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		}
		json.Unmarshal(*n.Incident, &in)
		return tr(l, "lapilli: %s wrote down incident %s, %q", who, in.ID, in.Title)
	}
	return "lapilli: " + n.Event
}
