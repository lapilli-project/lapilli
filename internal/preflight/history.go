package preflight

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"syscall"
	"time"

	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/rules"
	"lapilli.dev/internal/serve"
	"lapilli.dev/internal/source"
	"lapilli.dev/internal/verdict"
)

// expressions reads --impact and --guard as Lapilli reads an expression,
// sending neither.
func (p *run) expressions() {
	s := p.c.Serve
	var read, bad []string
	for _, x := range []struct{ flag, expr string }{{"--impact", s.Impact}, {"--guard", s.Guard}} {
		if x.expr == "" {
			continue
		}
		if err := rules.ReadExpr(x.expr); err != nil {
			bad = append(bad, x.flag+": "+said(err))
			continue
		}
		read = append(read, x.flag)
	}
	switch {
	case len(read)+len(bad) == 0:
	case len(bad) > 0:
		p.add(Item{ID: "expressions", State: Problem, What: "an expression doesn't read", Why: strings.Join(bad, "; "),
			Fix: "the history refuses it as written: a run with --guard so fails each time, one with --impact reads no impact"})
	default:
		p.add(Item{ID: "expressions", State: OK, What: strings.Join(read, " and ") + " read, and not sent",
			Why: "Lapilli has no PromQL parser of its own: whether the history takes them as written, the first run says"})
	}
}

// history checks the history serve replays against: that it answers and
// lets serve in, Loki's and Grafana's data sources too, and what fired
// over the last day, for the memory estimate.
func (p *run) history(ctx context.Context, rd *read) {
	p.reach(ctx)
	p.loki(ctx)
	p.grafanaSources(ctx, rd)
	p.alertsRead(ctx, rd)
	p.intervals(ctx, rd)
}

// cloudChain names the credentials a cloud's are looked for in.
var cloudChain = map[string]string{
	"aws":    "AWS's default credential chain: the environment, the shared config, IRSA, EKS Pod Identity, the instance's role",
	"azure":  "Azure's default credential: the environment, workload identity, a managed identity, the Azure CLI",
	"google": "Google's Application Default Credentials: the environment, workload identity, the metadata server, gcloud",
}

// signedFor says how the history's requests are signed.
func (p *run) signedFor() string {
	h := p.c.History
	switch {
	case h.GCPProject != "":
		return "signed for google"
	case h.Auth != "":
		return "signed for " + h.Auth
	case h.TokenFile != "":
		return "with --bearer-token-file"
	}
	return "with no credentials"
}

// unreached says why a read failed, as one that couldn't reach an API or
// wasn't let in: what to look at.
func unreached(err error, flag, chain string) (why, fix string) {
	var dns *net.DNSError
	var ae *prom.APIError
	var unknownCA x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var invalid x509.CertificateInvalidError
	var rec tls.RecordHeaderError
	why = said(err)
	switch {
	case errors.As(err, &dns):
		return "the name doesn't resolve: " + why, "check " + flag + "'s host, and the pod's DNS"
	case errors.As(err, &unknownCA), errors.As(err, &hostErr), errors.As(err, &invalid), errors.As(err, &rec), strings.Contains(err.Error(), "tls:"):
		return "TLS: " + why, "check the server's certificate, and the CA the pod trusts"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "the connection was refused: " + why, "check " + flag + "'s port, and a NetworkPolicy between the pod and it"
	case errors.Is(err, context.DeadlineExceeded):
		return "it didn't answer within 10s", "check " + flag + ", and the network between the pod and it"
	case errors.As(err, &ae) && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden):
		fix = "check the token or the identity serve signs with"
		if chain != "" {
			fix = "check the identity " + chain + " finds for the pod, and its role"
		}
		return "not let in (HTTP " + fmt.Sprint(ae.Status) + "): " + why, fix
	case errors.As(err, &ae) && ae.Status == http.StatusTooManyRequests:
		return "HTTP 429: the quota is already in use", "give serve --max-rate, and --yield-on-429, so its runs don't take the alerts' quota (docs/design/quota.md)"
	case strings.HasPrefix(err.Error(), "credentials: "):
		if chain != "" {
			return "no credentials: " + why, "check what " + chain + " finds for the pod"
		}
		return "no credentials: " + why, "check the token's file"
	}
	return why, ""
}

// reach asks the history vector(1), signed as serve signs it.
func (p *run) reach(ctx context.Context) {
	if p.historyBase() == "" {
		return
	}
	if p.c.Offline {
		p.offline("history.reach", "[--prometheus] not asked")
		return
	}
	chain := cloudChain[p.c.History.Auth]
	if p.c.History.GCPProject != "" {
		chain = cloudChain["google"]
	}
	h, err := p.historyClient(ctx)
	if err != nil {
		why, fix := unreached(err, "--prometheus", chain)
		p.add(Item{ID: "history.reach", State: Problem, What: "[--prometheus]'s client couldn't be made", Why: why, Fix: fix})
		return
	}
	var counts prom.Counts
	each, cancel := context.WithTimeout(prom.Counting(ctx, &counts), eachTime)
	defer cancel()
	t := p.now.Truncate(time.Second)
	began := time.Now()
	_, err = h.QueryRange(each, vectorOne, t, t, p.c.Serve.Step)
	took := time.Since(began).Round(time.Millisecond).String()
	if took == "0s" {
		took = "under a millisecond"
	}
	if err != nil {
		if counts.TooMany.Load() > 0 && errors.Is(err, context.DeadlineExceeded) {
			err = &prom.APIError{Status: http.StatusTooManyRequests}
		}
		why, fix := unreached(err, "--prometheus", chain)
		p.add(Item{ID: "history.reach", State: Problem, What: "[--prometheus] didn't answer vector(1), " + p.signedFor(), Why: why, Fix: fix})
		return
	}
	p.add(Item{ID: "history.reach", State: OK, What: fmt.Sprintf("[--prometheus] answered vector(1) in %s, %s", took, p.signedFor())})
}

// loki asks --loki for its labels over the last five minutes, as serve
// signs its requests.
func (p *run) loki(ctx context.Context) {
	if p.c.Serve.Loki == "" {
		return
	}
	if p.c.Offline {
		p.offline("history.loki", "[--loki] not asked")
		return
	}
	each, cancel := context.WithTimeout(ctx, eachTime)
	defer cancel()
	_, err := p.c.Loki.History(p.agent()).Get(each, p.lokiLabels(), 1<<20)
	if err != nil {
		why, fix := unreached(err, "--loki", "")
		p.add(Item{ID: "history.loki", State: Problem, What: "[--loki] didn't answer its labels", Why: why, Fix: fix})
		return
	}
	p.add(Item{ID: "history.loki", State: OK, What: "[--loki] answered its labels"})
}

// grafanaSources says which of the data sources Grafana's rules use the
// token reads, and, of Loki's, which it can query through Grafana's data
// source proxy, as a replay of them asks.
func (p *run) grafanaSources(ctx context.Context, rd *read) {
	if p.c.Serve.Grafana == "" || rd == nil {
		return
	}
	if p.c.Offline {
		p.offline("history.grafana", "Grafana's data sources not asked")
		return
	}
	if len(rd.dataSources) == 0 {
		return
	}
	var bad, lokis []string
	for _, uid := range slices.Sorted(maps.Keys(rd.dataSources)) {
		if why := rd.dataSources[uid]; why != "" {
			bad = append(bad, verdict.Clean(uid)+" ("+why+")")
			continue
		}
		if rd.kinds[uid] == "loki" {
			lokis = append(lokis, uid)
		}
	}
	host, path, _ := at(p.c.Serve.Grafana)
	var proxied []string
	for _, uid := range lokis {
		p.al.add(allow{api: "grafana", method: http.MethodGet, host: host, path: path + "/api/datasources/proxy/uid/" + uid + "/loki/api/v1/labels", query: p.al.lokiRecent})
		each, cancel := context.WithTimeout(ctx, eachTime)
		_, err := source.GrafanaLoki(p.c.Serve.Grafana, p.c.Serve.GrafanaToken, p.agent(), uid, nil).Get(each, p.lokiLabels(), 1<<20)
		cancel()
		if err != nil {
			why, _ := unreached(err, "--grafana", "")
			proxied = append(proxied, verdict.Clean(uid)+" ("+why+")")
		}
	}
	it := Item{ID: "history.grafana", State: OK, What: fmt.Sprintf("%s read", plural(len(rd.dataSources)-len(bad), "data source"))}
	if rd.unread > 0 {
		it.What += fmt.Sprintf("; %d more not read here, of the %d the rules name", rd.unread, rd.unread+len(rd.dataSources))
	}
	switch {
	case len(bad) > 0:
		it.State, it.Why = Problem, "the token can't read "+listed(bad, 5)
		it.Fix = "their queries are taken as PromQL, and $__rate_interval put in with 15s: give the token Viewer, or a role that reads data sources"
	case len(proxied) > 0:
		it.State, it.Why = Problem, "the token can't query "+listed(proxied, 5)+" through Grafana's data source proxy"
		it.Fix = "their rules aren't replayed: give the token a role that queries the data source"
	}
	p.add(it)
}

// alertsRead reads how many samples each alert name fired over the last
// day, of ALERTS and, for Grafana's rules, GRAFANA_ALERTS: for memory alone,
// never shown by rule. None is unknown: a quiet day and a history keeping
// no ALERTS answer alike.
func (p *run) alertsRead(ctx context.Context, rd *read) {
	if rd == nil || p.historyBase() == "" || len(replayable(rd.all)) == 0 {
		return
	}
	if p.c.Offline {
		p.offline("history.alerts", "not asked: memory is the caps' bound")
		return
	}
	h, err := p.historyClient(ctx)
	if err != nil {
		p.add(Item{ID: "history.alerts", State: Unknown, What: "not read: memory is the caps' bound", Why: said(err)})
		return
	}
	series := []string{serve.AlertsSeries}
	if slices.ContainsFunc(rd.all, func(r rules.Rule) bool { return r.Graph != nil }) {
		series = append(series, serve.GrafanaAlertsSeries)
	}
	got := map[string]float64{}
	var failed []string
	t := p.now.Truncate(time.Second)
	for _, sel := range series {
		each, cancel := context.WithTimeout(ctx, eachTime)
		ss, err := h.QueryRange(each, alertsCount(sel), t, t, p.c.Serve.Step)
		cancel()
		if err != nil {
			failed = append(failed, said(err))
			continue
		}
		for _, s := range ss {
			if name := s.Labels["alertname"]; name != "" && len(s.Samples) > 0 {
				got[name] += s.Samples[len(s.Samples)-1].V
			}
		}
	}
	switch {
	case len(got) > 0:
		p.alerts = got
		total := 0.0
		for _, v := range got {
			total += v
		}
		p.add(Item{ID: "history.alerts", State: OK, What: fmt.Sprintf("%s fired in the last day, %.0f samples in all: for the memory estimate",
			plural(len(got), "alert name"), total)})
	case len(failed) > 0:
		p.add(Item{ID: "history.alerts", State: Unknown, What: "not read: memory is the caps' bound", Why: strings.Join(failed, "; ")})
	default:
		p.add(Item{ID: "history.alerts", State: Unknown, What: "no firing alert in the last day, or no ALERTS kept: memory is the caps' bound",
			Why: "ALERTS has a series only while an alert is active: a quiet day and a history that keeps none can't be told apart"})
	}
}

// intervals asks Prometheus for its configuration and its active targets,
// as the content check does without --scrape-interval.
func (p *run) intervals(ctx context.Context, rd *read) {
	s := p.c.Serve
	if !s.CheckContent || s.ScrapeInterval > 0 || p.historyBase() == "" || rd == nil {
		return
	}
	if p.c.Offline {
		p.offline("history.intervals", "the scrape intervals not asked")
		return
	}
	h, err := p.historyClient(ctx)
	if err != nil {
		p.add(Item{ID: "history.intervals", State: Unknown, What: "not read: the range check counts with the global interval, as serve does", Why: said(err)})
		return
	}
	each, cancel := context.WithTimeout(ctx, eachTime)
	defer cancel()
	var failed []string
	if _, err := h.StatusConfig(each); err != nil {
		failed = append(failed, "/api/v1/status/config: "+said(err))
	}
	if _, err := h.Targets(each); err != nil {
		failed = append(failed, "/api/v1/targets: "+said(err))
	}
	if len(failed) > 0 {
		p.add(Item{ID: "history.intervals", State: Unknown, What: "not served: the range check counts with the global interval, as serve does",
			Why: strings.Join(failed, "; "), Fix: "--scrape-interval says it, where the history doesn't"})
		return
	}
	p.add(Item{ID: "history.intervals", State: OK, What: "the configuration and the active targets answered: each job's interval is known"})
}
