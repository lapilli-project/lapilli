package serve

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/ledger"
)

// A body from anyone is read only when its headers say it could prove
// itself: SNS's type and a topic taken, or Grafana's signature with a secret
// to check it by. Others are refused unread, and take no place, so they
// can't keep SNS's and Grafana's messages out.
func TestNotifyBodyOnlyWhenItCould(t *testing.T) {
	s := notifyServer(t, nil)
	h := s.NotifyHandler()
	unread := func(path string, hdr map[string]string, want int) {
		t.Helper()
		body := &readCounter{r: bytes.NewReader([]byte(`{}`))}
		r := httptest.NewRequest(http.MethodPost, path, body)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want || body.read {
			t.Errorf("%s %v: %d, body read %v; want %d unread", path, hdr, w.Code, body.read, want)
		}
	}
	unread("/v1/sns", nil, http.StatusForbidden)
	unread("/v1/sns", map[string]string{"X-Amz-Sns-Message-Type": "Notification"}, http.StatusForbidden)
	unread("/v1/sns", map[string]string{"X-Amz-Sns-Message-Type": "Notification", "X-Amz-Sns-Topic-Arn": "arn:aws:sns:us-east-1:111111111111:other"}, http.StatusForbidden)
	unread("/v1/grafana", nil, http.StatusUnauthorized)
	s2 := notifyServer(t, func(c *Config) { c.NotifyGrafanaSecret = "" })
	h = s2.NotifyHandler()
	unread("/v1/grafana", map[string]string{"X-Grafana-Alerting-Signature": "00"}, http.StatusUnauthorized)
	if n := len(s.port.preAuth) + len(s2.port.preAuth); n != 0 {
		t.Errorf("%d places held", n)
	}
	// A body whose headers name a topic other than its own isn't taken.
	g := newSNSSigner(t)
	s.port.sns.get = g.get
	subject := "[FIRING:1]"
	m := snsMessage{Type: "Notification", MessageID: "m-1", TopicArn: snsTopicARN, Subject: &subject, Message: string(notifyFixture(t, "amp-sns-message.json")),
		Timestamp: snsTime(now), SigningCertURL: snsCertURL}
	s.cfg.NotifySNS = append(s.cfg.NotifySNS, "arn:aws:sns:us-east-1:111111111111:other")
	if code, out, _ := send(t, s.NotifyHandler(), "/v1/sns", g.sign(t, m), map[string]string{"X-Amz-Sns-Topic-Arn": "arn:aws:sns:us-east-1:111111111111:other"}); code != http.StatusForbidden {
		t.Errorf("a message under another topic's header: %d %s", code, out)
	}
}

// Each source has its own alerts a day: one token holder sending on one
// path can't keep SNS's or Grafana's notifications out until tomorrow.
func TestNotifyDayBySource(t *testing.T) {
	s := notifyServer(t, nil)
	h := s.NotifyHandler()
	defer func(was int) { maxAlertsPerDay = was }(maxAlertsPerDay)
	maxAlertsPerDay = 1
	am := notifyFixture(t, "alertmanager.json")
	if code, out, _ := send(t, h, "/v1/alertmanager", am, bearer(notifyToken)); code != 200 {
		t.Fatalf("the first: %d %s", code, out)
	}
	if code, _, _ := send(t, h, "/v1/alertmanager", bytes.Replace(am, []byte(`"groupKey": "`), []byte(`"groupKey": "x`), 1), bearer(notifyToken)); code != http.StatusTooManyRequests {
		t.Errorf("past Alertmanager's alerts today: %d", code)
	}
	g := newSNSSigner(t)
	s.port.sns.get = g.get
	subject := "[FIRING:1]"
	m := snsMessage{Type: "Notification", MessageID: "m-1", TopicArn: snsTopicARN, Subject: &subject, Message: string(notifyFixture(t, "amp-sns-message.json")),
		Timestamp: snsTime(now), SigningCertURL: snsCertURL}
	if code, out, _ := send(t, h, "/v1/sns", g.sign(t, m), nil); code != 200 {
		t.Errorf("SNS after Alertmanager's day was used up: %d %s", code, out)
	}
	// Counted again from the ledger, by source, when the day begins.
	s.port.day.start = time.Time{}
	if ok, err := s.dayRoom(context.Background(), "sns", 1); ok || err != nil {
		t.Errorf("SNS's day counted again: room %v %v", ok, err)
	}
	if ok, err := s.dayRoom(context.Background(), "google", 1); !ok || err != nil {
		t.Errorf("Google's day counted again: room %v %v", ok, err)
	}
}

// A body that doesn't come whole, too slowly or cut off, is told so, not
// that it was too large.
func TestNotifyBodyCut(t *testing.T) {
	s := notifyServer(t, nil)
	r := httptest.NewRequest(http.MethodPost, "/v1/alertmanager", &failing{})
	r.Header.Set("Authorization", "Bearer "+notifyToken)
	w := httptest.NewRecorder()
	s.NotifyHandler().ServeHTTP(w, r)
	if w.Code != http.StatusRequestTimeout {
		t.Errorf("a body cut off: %d %s", w.Code, w.Body)
	}
}

type failing struct{}

func (failing) Read([]byte) (int, error) { return 0, os.ErrDeadlineExceeded }

// A group key past its length, or with a character that doesn't show, is
// kept as its hash: an alert manager drops a notification it got 400 for.
func TestNotifyLongGroupKey(t *testing.T) {
	s := notifyServer(t, nil)
	am := notifyFixture(t, "alertmanager.json")
	long := `"groupKey": "{}/{namespace=~\"` + strings.Repeat("ns-a|", 300) + `\"}:{alertname=\"x\"}`
	body := bytes.Replace(am, []byte(`"groupKey": "`), []byte(long), 1)
	if code, out, _ := send(t, s.NotifyHandler(), "/v1/alertmanager", body, bearer(notifyToken)); code != 200 {
		t.Fatalf("a long group key: %d %s", code, out)
	}
	ns := kept(t, s)
	if len(ns) != 1 || !strings.HasPrefix(ns[0].Group, "sha256:") {
		t.Errorf("kept %+v", ns)
	}
}

// The firings token's 429 says how long until a write is taken: five
// seconds, as a job's retries need.
func TestFiringsRetryAfter(t *testing.T) {
	const tok = "a-firings-token-long-enough"
	s, _ := server(t, func(c *Config) { c.IncidentsListen, c.FiringsToken = "127.0.0.1:0", tokenFile(t, tok) })
	port := s.IncidentsHandler()
	for i := 0; ; i++ {
		r := httptest.NewRequest("PUT", "/api/v1/firings/cw?from=2026-11-02T00:00:00Z&to=2026-11-03T00:00:00Z", strings.NewReader(""))
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		port.ServeHTTP(w, r)
		if w.Code == http.StatusTooManyRequests {
			if got := w.Header().Get("Retry-After"); got != "5" || i != 20 {
				t.Errorf("429 after %d writes, Retry-After %q; want after 20, 5", i, got)
			}
			break
		}
		if w.Code != http.StatusOK || i > 30 {
			t.Fatalf("write %d: %d %s", i, w.Code, w.Body)
		}
	}
}

// A source that writes a period running on past now has written up to now:
// a later write still moves it on, and the digest still waits for it.
func TestRecordedPeriodToNow(t *testing.T) {
	s, _ := server(t, nil)
	ctx := context.Background()
	hour := time.Now().Truncate(time.Hour)
	o := s.cfg.Options
	if _, err := s.l.PutFirings(ctx, "cw", hour.Add(-48*time.Hour), hour.Add(24*time.Hour), nil, ledger.FiringsCopier, o); err != nil {
		t.Fatal(err)
	}
	srcs, err := s.l.Sources(ctx)
	if err != nil || len(srcs) != 1 || srcs[0].To.After(time.Now()) {
		t.Fatalf("a period past now: %+v %v", srcs, err)
	}
	was := srcs[0].To
	if _, err := s.l.PutFirings(ctx, "cw", hour, hour.Add(time.Hour), []firing.Firing{{Rule: "a", Start: hour.Add(time.Minute)}}, ledger.FiringsCopier, o); err != nil {
		t.Fatal(err)
	}
	if srcs, _ = s.l.Sources(ctx); srcs[0].Firings != 1 || srcs[0].To.Before(was) || !srcs[0].From.Equal(hour) {
		t.Errorf("a later write didn't move the source on: %+v", srcs[0])
	}
}
