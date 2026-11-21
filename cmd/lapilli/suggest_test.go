package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
)

// TestImpactStacksDocumented holds docs/impact.md's table and impactStacks
// alike: each row a stack, in the same order, with the same name and
// --impact; each stack's metrics the metrics its --impact reads, and the
// selector counted of each one that its others narrow.
func TestImpactStacksDocumented(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "impact.md"))
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("^\\| ([^|`]+) \\| `([^`]+)` \\|$")
	var names, queries []string
	_, table, ok := strings.Cut(string(b), "| In front | `--impact` |\n|---|---|\n")
	if !ok {
		t.Fatal("docs/impact.md has no table of --impact queries")
	}
	for _, line := range strings.Split(table, "\n") {
		if !strings.HasPrefix(line, "|") {
			break
		}
		m := row.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("docs/impact.md: a row that doesn't read as a stack and its --impact: %s", line)
		}
		names, queries = append(names, m[1]), append(queries, m[2])
	}
	var listed, impacts, ids []string
	for _, s := range impactStacks {
		listed, impacts, ids = append(listed, s.Name), append(impacts, s.Impact), append(ids, s.ID)
	}
	if !slices.Equal(names, listed) {
		t.Errorf("docs/impact.md's stacks %q, impactStacks %q", names, listed)
	}
	for i := range min(len(queries), len(impacts)) {
		if queries[i] != impacts[i] {
			t.Errorf("%s: docs/impact.md's --impact\n  %s\nimpactStacks'\n  %s", names[i], queries[i], impacts[i])
		}
	}
	for i, s := range impactStacks {
		if !regexp.MustCompile(`^[a-z][a-z0-9-]*$`).MatchString(s.ID) || slices.Index(ids, s.ID) != i {
			t.Errorf("%s: id %q isn't a lowercase name of its own", s.Name, s.ID)
		}
		sels, err := rules.Selectors(s.Impact)
		if err != nil {
			t.Fatalf("%s: %v", s.Name, err)
		}
		var read []string
		for _, sel := range sels {
			if name, _, _ := strings.Cut(sel, "{"); !slices.Contains(read, name) {
				read = append(read, name)
			}
		}
		if !slices.Equal(read, s.Metrics) {
			t.Errorf("%s: its --impact reads %q, its metrics say %q", s.Name, read, s.Metrics)
		}
		for _, m := range s.Metrics {
			counted, err := s.counted(m)
			if err != nil {
				t.Fatalf("%s: %v", s.Name, err)
			}
			for _, sel := range sels {
				if name, _, _ := strings.Cut(sel, "{"); name == m && sel != counted && !strings.HasPrefix(sel, strings.TrimSuffix(counted, "}")) {
					t.Errorf("%s: %s counts %s, which %s doesn't narrow", s.Name, m, counted, sel)
				}
			}
			if strings.Contains(counted, "5") || strings.Contains(counted, "failure") {
				t.Errorf("%s: %s counts %s, the failures alone", s.Name, m, counted)
			}
		}
	}
}

// askedHistory is the verdict tests' fakeHistory, with what it was asked
// kept, and a query that names fail failing: refused as written (HTTP
// 400), or with status when it is given.
type askedHistory struct {
	asked  *[]string
	fail   string
	status int
}

func (h askedHistory) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]prom.Series, error) {
	*h.asked = append(*h.asked, expr)
	if h.fail != "" && strings.Contains(expr, h.fail) {
		return nil, &prom.APIError{Status: cmp.Or(h.status, http.StatusBadRequest), Msg: "bad_data: no"}
	}
	return fakeHistory{}.QueryRange(ctx, expr, start, end, step)
}

func TestImpactSuggest(t *testing.T) {
	now = func() time.Time { return t0 }
	var asked []string
	fail, status := "", 0
	newHistory = func(context.Context, *verdictFlags) (history, error) { return askedHistory{&asked, fail, status}, nil }
	var stdout, stderr bytes.Buffer
	args := []string{"impact", "suggest", "--prometheus", "http://prometheus:9090", "--tz", "UTC"}
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"Lapilli impact suggest  2026-10-26 00:00 → 2026-11-02 00:00 UTC\n",
		"2 of the 6 stacks have their metrics in the history: SERIES, the most at once, and WINDOWS,\n",
		"those its --impact finds, 5m or longer, each an incident nobody wrote down.\n\n",
		"STACK          SERIES  WINDOWS\nIstio          412     2\ningress-nginx  36      0\n",
		"\nIstio:\n  --impact '" + impactStacks[0].Impact + "'\n",
		"\ningress-nginx:\n  --impact 'sum by (ingress) (rate(nginx_ingress_controller_requests{status=~\"5..\"}[5m]))",
		"Not found: Linkerd, Envoy or Envoy Gateway, Traefik, OpenTelemetry HTTP server metrics.\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("want %q in:\n%s", want, out)
		}
	}
	// One count a stack, and the --impact of the two found alone.
	var counts, impacts int
	for _, q := range asked {
		if strings.HasPrefix(q, "count(") {
			counts++
		} else if q == impactStacks[0].Impact || q == impactStacks[3].Impact {
			impacts++
		} else {
			t.Errorf("asked %s", q)
		}
	}
	if counts != len(impactStacks) || impacts != 2 {
		t.Errorf("asked %d counts and %d --impact, want %d and 2", counts, impacts, len(impactStacks))
	}

	// --json says the same.
	stdout.Reset()
	if code := run(append(args, "--json"), &stdout, &stderr); code != 0 {
		t.Fatalf("--json: exit %d: %s", code, stderr.String())
	}
	var rep suggestReport
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Found) != 2 || rep.Found[0].ID != "istio" || rep.Found[0].Series != 412 || rep.Found[0].Windows == nil || *rep.Found[0].Windows != 2 ||
		rep.Found[0].Metrics[0].Selector != `istio_requests_total{reporter="destination"}` || rep.Found[0].Argument != "--impact '"+impactStacks[0].Impact+"'" ||
		rep.Found[1].ID != "ingress-nginx" || rep.Found[1].Windows == nil || *rep.Found[1].Windows != 0 {
		t.Errorf("found %+v", rep.Found)
	}
	var notFound []string
	for _, s := range rep.NotFound {
		notFound = append(notFound, s.ID)
	}
	if !slices.Equal(notFound, []string{"linkerd", "envoy", "traefik", "otel-http"}) || len(rep.NotKnown) != 0 ||
		!rep.From.Equal(t0.Add(-7*24*time.Hour)) || !rep.To.Equal(t0) || rep.ImpactFor != "5m" || rep.Step != "1m" {
		t.Errorf("not found %q, not known %+v, %s to %s, for %s at %s", notFound, rep.NotKnown, rep.From, rep.To, rep.ImpactFor, rep.Step)
	}

	// A query refused as written leaves its stack not known, and the
	// others are asked: exit 1.
	fail = "traefik"
	stdout.Reset()
	if code := run(args, &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), "Not known: Traefik, which couldn't be asked: HTTP 400: bad_data: no\n") ||
		!strings.Contains(stdout.String(), "Not found: Linkerd, Envoy or Envoy Gateway, OpenTelemetry HTTP server metrics.\n") ||
		!strings.Contains(stdout.String(), "Istio          412     2") {
		t.Errorf("exit %d:\n%s", code, stdout.String())
	}
	// Istio's windows refused: found, its windows not known.
	fail = "rate(istio_requests_total"
	stdout.Reset()
	if code := run(args, &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), "Istio          412     ?") ||
		!strings.Contains(stdout.String(), "  its windows couldn't be read: HTTP 400") || !strings.Contains(stdout.String(), "ingress-nginx  36      0") {
		t.Errorf("exit %d:\n%s", code, stdout.String())
	}
	// A history turned away (HTTP 503) is asked no more: the stacks from
	// it on are not known, on one line, unasked.
	fail, status, asked = "nginx", http.StatusServiceUnavailable, nil
	stdout.Reset()
	if code := run(args, &stdout, &stderr); code != 1 ||
		!strings.Contains(stdout.String(), "Not known: ingress-nginx, Traefik, OpenTelemetry HTTP server metrics, which couldn't be asked: HTTP 503: bad_data: no\n") ||
		!strings.Contains(stdout.String(), "Not found: Linkerd, Envoy or Envoy Gateway.\n") || len(asked) != 5 {
		t.Errorf("exit %d, asked %d:\n%s", code, len(asked), stdout.String())
	}
	// Nothing could be asked.
	fail, status = "count(", 0
	stdout.Reset()
	if code := run(args, &stdout, &stderr); code != 1 || strings.Contains(stdout.String(), "STACK") {
		t.Errorf("exit %d:\n%s", code, stdout.String())
	}
}

func TestImpactSuggestNoneFound(t *testing.T) {
	now = func() time.Time { return t0 }
	newHistory = func(context.Context, *verdictFlags) (history, error) { return unreachableNone{}, nil }
	var stdout, stderr bytes.Buffer
	if code := run([]string{"impact", "suggest", "--prometheus", "http://p", "--since", "1d"}, &stdout, &stderr); code != 0 ||
		!strings.Contains(stdout.String(), "None of the 6 stacks have their metrics in the history.\n") ||
		!strings.Contains(stdout.String(), "Write an --impact of your own") {
		t.Errorf("exit %d:\n%s%s", code, stdout.String(), stderr.String())
	}
}

// unreachableNone is a history with no series of anything.
type unreachableNone struct{}

func (unreachableNone) QueryRange(context.Context, string, time.Time, time.Time, time.Duration) ([]prom.Series, error) {
	return nil, nil
}

func TestImpactSuggestFlags(t *testing.T) {
	newHistory = func(context.Context, *verdictFlags) (history, error) { return unreachableNone{}, nil }
	token := write(t, "token", "secret\n")
	for args, want := range map[string]string{
		"impact suggest": "say where the history is",
		"impact suggest --prometheus http://p --rules r.yaml":                                        "unknown flag --rules",
		"impact suggest --prometheus http://p --impact-for 10m":                                      "unknown flag --impact-for",
		"impact suggest --prometheus http://p --incidents i.yml":                                     "unknown flag --incidents",
		"impact suggest --prometheus http://p extra":                                                 "unknown flag extra",
		"impact suggest --prometheus http://p --from 2026-11-01T00:00:00Z":                           "--from and --to go together",
		"impact suggest --prometheus http://p --from 2026-11-02T00:00:00Z --to 2026-11-01T00:00:00Z": "--to must be after --from",
		"impact suggest --prometheus http://p --auth aws --bearer-token-file " + token:               "--auth and --bearer-token-file don't go together",
		"impact suggest --prometheus http://p --gcp-project shop":                                    "--prometheus and --gcp-project don't go together",
		"impact suggest --prometheus http://p --auth oci":                                            "Oracle Cloud has no managed Prometheus",
		"impact suggest --prometheus http://p --since x":                                             "--since x: want a duration",
		"impact suggest --prometheus":                                                                "--prometheus needs a value",
		"impact other":                                                                               "unknown command \"other\"",
	} {
		var stdout, stderr bytes.Buffer
		if code := run(strings.Fields(args), &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), want) {
			t.Errorf("%s: exit %d, stderr %q, want 2 and %q", args, code, stderr.String(), want)
		}
	}
	for args, code := range map[string]int{"impact": 2, "impact -h": 0, "impact suggest -h": 0, "help impact": 0} {
		var stdout, stderr bytes.Buffer
		if got := run(strings.Fields(args), &stdout, &stderr); got != code || stdout.String() != impactUsage {
			t.Errorf("%s: exit %d, want %d and the help", args, got, code)
		}
	}
}

// TestImpactSuggestReads runs it against a Prometheus over HTTP, through
// the client lapilli verdict asks with: it only asks query_range, with the
// token, one count a stack and the --impact of each stack found, a day a
// request; and it says nothing of the token.
func TestImpactSuggestReads(t *testing.T) {
	newHistory = realHistory
	now = func() time.Time { return t0 }
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		q, _ := url.ParseQuery(string(b))
		asked = append(asked, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+q.Get("query"))
		start, _ := strconv.ParseInt(q.Get("start"), 10, 64)
		end, _ := strconv.ParseInt(q.Get("end"), 10, 64)
		step, _ := strconv.ParseFloat(q.Get("step"), 64)
		var result string
		switch expr := q.Get("query"); {
		case strings.HasPrefix(expr, "count(traefik_service_requests_total"):
			result = fmt.Sprintf(`{"metric":{},"values":[[%d,"7"]]}`, start)
		case expr == impactStacks[4].Impact:
			// Its 5xx share above 1% for 30m from the first point of each
			// day asked: a window a day.
			var vs []string
			for t := start; t <= end && t < start+1800; t += int64(step) {
				vs = append(vs, fmt.Sprintf(`[%d,"0.5"]`, t))
			}
			result = `{"metric":{"service":"shop"},"values":[` + strings.Join(vs, ",") + `]}`
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[%s]}}`, result)
	}))
	defer srv.Close()
	token := write(t, "token", "s3cret-token\n")
	var stdout, stderr bytes.Buffer
	args := []string{"impact", "suggest", "--prometheus", srv.URL, "--bearer-token-file", token, "--from", "2026-10-26T00:00:00Z", "--to", "2026-10-29T00:00:00Z", "--json"}
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var rep suggestReport
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Found) != 1 || rep.Found[0].ID != "traefik" || rep.Found[0].Series != 7 || rep.Found[0].Windows == nil || *rep.Found[0].Windows != 3 {
		t.Errorf("found %+v", rep.Found)
	}
	var counts, impacts int
	for _, a := range asked {
		method, rest, _ := strings.Cut(a, " ")
		path, rest, _ := strings.Cut(rest, " ")
		if method != http.MethodPost || path != "/api/v1/query_range" || !strings.HasPrefix(rest, "Bearer s3cret-token ") {
			t.Errorf("asked %s", a)
		}
		switch q := strings.TrimPrefix(rest, "Bearer s3cret-token "); {
		case strings.HasPrefix(q, "count("):
			counts++
		case q == impactStacks[4].Impact:
			impacts++
		default:
			t.Errorf("asked %s", q)
		}
	}
	// Three days at a minute: three requests of a day.
	if counts != len(impactStacks) || impacts != 3 {
		t.Errorf("asked %d counts and %d of --impact, want %d and 3", counts, impacts, len(impactStacks))
	}
	if strings.Contains(stdout.String()+stderr.String(), "s3cret") {
		t.Error("the token was said")
	}
}
