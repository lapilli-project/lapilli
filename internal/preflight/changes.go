package preflight

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"lapilli.dev/internal/cloud"
	"lapilli.dev/internal/source"
)

// grafana checks what the Grafana token is asked for beside the rules: the
// notifications the content check reads, and, with --changes, writes.
func (p *run) grafana(ctx context.Context) {
	s := p.c.Serve
	if s.CheckContent && s.Grafana != "" {
		if p.c.Offline {
			p.offline("grafana.notifications", "Grafana's templates, contact points and policies not asked")
		} else {
			each, cancel := context.WithTimeout(ctx, eachTime)
			_, _, _, err := source.GrafanaNotifications(each, s.Grafana, s.GrafanaToken, p.agent())
			cancel()
			if err != nil {
				fix := "check --grafana and its token"
				if strings.Contains(err.Error(), "HTTP 403") {
					fix = "give the token a role that reads notification templates, contact points and policies"
				}
				p.add(Item{ID: "grafana.notifications", State: Problem, What: "Grafana's notifications couldn't be read: the template part of the content check stays as it was",
					Why: said(err), Fix: fix})
			} else {
				p.add(Item{ID: "grafana.notifications", State: OK, What: "Grafana's templates, contact points and policies read, as the content check reads them"})
			}
		}
	}
	p.grafanaSends(ctx)
	p.grafanaWrite(ctx)
}

// grafanaSends asks, as serve does, where Grafana sends its alerts, with
// --grafana and --alertmanager-config: to an external Alertmanager, its
// rules are checked against the configuration given.
func (p *run) grafanaSends(ctx context.Context) {
	s := p.c.Serve
	if !s.CheckContent || s.Grafana == "" || s.AlertmanagerConfig == "" {
		return
	}
	if p.c.Offline {
		p.offline("grafana.alertmanagers", "where Grafana sends its alerts not asked")
		return
	}
	each, cancel := context.WithTimeout(ctx, eachTime)
	g, err := source.GrafanaAlertmanagers(each, s.Grafana, s.GrafanaToken, p.agent())
	cancel()
	switch {
	case err != nil:
		fix := "check --grafana and its token; Grafana's alerting API gives this at /api/v1/ngalert/admin_config, which older versions don't have"
		if strings.Contains(err.Error(), "HTTP 403") {
			fix = "give the token a role that reads Grafana's alerting settings"
		}
		p.add(Item{ID: "grafana.alertmanagers", State: Problem, What: "where Grafana sends its alerts couldn't be read: its rules are checked against its own notification templates only",
			Why: said(err), Fix: fix})
	case g.Choice != "internal" && g.Active == 0:
		p.add(Item{ID: "grafana.alertmanagers", State: Problem, What: fmt.Sprintf("Grafana sends to %s Alertmanagers, and has no external one active: its rules are checked against its own templates", g.Choice),
			Fix: "turn on Receive Grafana Alerts on the Alertmanager data source, and enable it in Grafana's alerting settings"})
	case g.External():
		what := fmt.Sprintf("Grafana sends its alerts to %s: its rules are checked against --alertmanager-config", plural(g.Active, "external Alertmanager"))
		if g.Internal() {
			what += ", and against its own templates"
		}
		p.add(Item{ID: "grafana.alertmanagers", State: OK, What: what})
	default:
		p.add(Item{ID: "grafana.alertmanagers", State: OK, What: "Grafana sends its alerts to its own Alertmanager: its rules are checked against its own templates"})
	}
}

// changes takes each cloud identity serve writes as, with --changes: a
// credential is made, on a request never sent, and nothing is written.
// Then each is asked what it may write, of an API that says so without
// writing (writes.go).
func (p *run) changes(ctx context.Context) {
	s := p.c.Serve
	if !s.Changes {
		return
	}
	if s.Ruler != "" {
		if k, _ := source.RulerKind(s.Ruler); k == source.RulerConfig && len(s.ChangesIn) > 0 {
			p.add(Item{ID: "changes.ruler", State: Unknown, What: "whether the ruler takes writes (rules:write) isn't known",
				Why: "a Mimir's or a Cortex's can't say without writing"})
		}
	}
	if s.Loki != "" && s.LokiLoaded && slices.ContainsFunc(s.ChangesIn, func(x string) bool { return strings.HasPrefix(x, "loki:") }) {
		p.add(Item{ID: "changes.loki", State: Unknown, What: "whether Loki's ruler takes writes isn't known",
			Why: "Loki's ruler can't say without writing, and one whose rules are local files (ruler.storage.type: local) takes none"})
	}
	endpoint := func(kind string) string {
		if k, _ := source.RulerKind(s.Ruler); s.Ruler != "" && (kind == "aws" && k == source.RulerAmazon || kind == "azure" && k == source.RulerAzure) {
			return s.Ruler
		}
		want := map[string]string{"aws": source.AlarmsAWS, "azure": source.AlarmsAzure, "google": source.AlarmsGoogle, "oci": source.AlarmsOCI}[kind]
		for _, a := range s.Alarms {
			if k, _ := source.AlarmsKind(a.URL); k == want {
				return a.URL
			}
		}
		return ""
	}
	var ws []writer
	for _, x := range []writer{{cloud: "aws", id: "changes.aws", flag: "--changes-aws-role", who: s.ChangesAWSRole},
		{cloud: "azure", id: "changes.azure", flag: "--changes-azure-client-id", who: s.ChangesAzureClientID},
		{cloud: "google", id: "changes.google", flag: "--changes-google-service-account", who: s.ChangesGoogleServiceAccount},
		{cloud: "oci", id: "changes.oci", flag: "--changes-oci-config", who: s.ChangesOCIConfig}} {
		// Without --changes-google-service-account, the pod's own identity
		// writes Google's alerting policies, with a token of the scope
		// that writes (cloud.Writes): it is asked about as serve's other
		// write identities are.
		own := x.cloud == "google" && x.who == "" && len(p.googleProjects()) > 0
		if own {
			x.flag = "the pod's own Google identity (no --changes-google-service-account)"
		}
		if x.who != "" || own {
			x.endpoint = endpoint(x.cloud)
			ws = append(ws, x)
		}
	}
	if len(ws) == 0 {
		return
	}
	if p.c.Offline {
		p.offline("changes.cloud", "the identities serve writes as not taken")
		for _, w := range ws {
			p.offline(w.id, "what "+w.flag+" may write not asked")
		}
		return
	}
	for _, w := range ws {
		p.hideWho(w.cloud, w.flag, w.who)
	}
	var took, failed, ids []string
	var taken, untaken []writer
	for _, w := range ws {
		var err error
		ids = append(ids, w.id)
		if w.sign, w.iam, err = take(ctx, w.cloud, w.endpoint, w.who); err != nil {
			failed = append(failed, w.flag+": "+p.cloudSaid(err))
			untaken = append(untaken, w)
			continue
		}
		took = append(took, w.flag)
		taken = append(taken, w)
	}
	slices.Sort(took)
	if len(failed) > 0 {
		p.add(Item{ID: "changes.cloud", State: Problem, What: "an identity serve writes as couldn't be taken", Why: strings.Join(failed, "; "),
			Fix: "a change approved then fails as it is carried out: check the role, the federation or the key, and that the pod's identity may take it"})
	} else {
		p.add(Item{ID: "changes.cloud", State: OK, What: fmt.Sprintf("%s: the identity taken, a credential made, nothing written", strings.Join(took, ", ")),
			Why: "what each may write is said apart: " + strings.Join(ids, ", ")})
	}
	for _, w := range untaken {
		p.add(Item{ID: w.id, State: Unknown, What: "what " + w.flag + " may write isn't asked: the identity couldn't be taken (changes.cloud)"})
	}
	p.cloudWrites(ctx, taken)
}

// writeCredential is the credential serve writes with, as serve makes it
// (cloud.Writes: Google's, without a service account, of the scope that
// writes); tests replace it.
var writeCredential = cloud.Writes

// take makes the credential serve writes as for a cloud, signing a request
// to endpoint that is never sent: AWS assumes the role, Google Cloud asks
// IAM Credentials for the service account's token, Azure asks Microsoft
// Entra for the identity's, OCI reads the config and its key. It gives
// the signer, and, for AWS, one for IAM's API with the same credentials.
// Tests replace it.
var take = func(ctx context.Context, kind, endpoint, as string) (sign, iam func(*http.Request) error, err error) {
	if endpoint == "" {
		return nil, nil, fmt.Errorf("no --ruler or --alarms of %s to write", kind)
	}
	each, cancel := context.WithTimeout(ctx, eachTime)
	defer cancel()
	if _, region, ok := iamOf(as); kind == "aws" && ok {
		sign, iam, err = cloud.AWSAs(each, endpoint, as, "iam", region)
	} else {
		sign, err = writeCredential(each, kind, endpoint, as)
	}
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(each, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, err
	}
	return sign, iam, sign(req)
}
