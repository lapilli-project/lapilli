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
	if s.ChangesGrafanaToken != "" {
		p.add(Item{ID: "grafana.write", State: Unknown, What: "whether --changes-grafana-token-file's token may write alert rules isn't asked",
			Why: "no API every Grafana serves says what a token may write without its writing; the first change approved finds out"})
	}
}

// changes takes each cloud identity serve writes as, with --changes: a
// credential is made, on a request never sent, and nothing is written.
// Whether it may write the rules or alarms named isn't known.
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
	type as struct{ cloud, flag, who, endpoint string }
	var ids []as
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
	for _, x := range []as{{"aws", "--changes-aws-role", s.ChangesAWSRole, ""}, {"azure", "--changes-azure-client-id", s.ChangesAzureClientID, ""},
		{"google", "--changes-google-service-account", s.ChangesGoogleServiceAccount, ""}, {"oci", "--changes-oci-config", s.ChangesOCIConfig, ""}} {
		if x.who != "" {
			x.endpoint = endpoint(x.cloud)
			ids = append(ids, x)
		}
	}
	if len(ids) == 0 {
		return
	}
	if p.c.Offline {
		p.offline("changes.cloud", "the identities serve writes as not taken")
		return
	}
	var took, failed []string
	for _, x := range ids {
		if err := p.take(ctx, x.cloud, x.endpoint, x.who); err != nil {
			failed = append(failed, x.flag+": "+said(err))
			continue
		}
		took = append(took, x.flag)
	}
	slices.Sort(took)
	if len(failed) > 0 {
		p.add(Item{ID: "changes.cloud", State: Problem, What: "an identity serve writes as couldn't be taken", Why: strings.Join(failed, "; "),
			Fix: "a change approved then fails as it is carried out: check the role, the federation or the key, and that the pod's identity may take it"})
		return
	}
	p.add(Item{ID: "changes.cloud", State: Unknown, What: fmt.Sprintf("%s: the identity taken, a credential made, nothing written", strings.Join(took, ", ")),
		Why: "whether it may write the rules or alarms named isn't asked in this release; the first change approved finds out"})
}

// take makes the credential serve writes as for a cloud, signing a request
// to endpoint that is never sent: AWS assumes the role, Google Cloud asks
// IAM Credentials for the service account's token, Azure asks Microsoft
// Entra for the identity's, OCI reads the config and its key.
func (p *run) take(ctx context.Context, kind, endpoint, as string) error {
	if endpoint == "" {
		return fmt.Errorf("no --ruler or --alarms of %s to write", kind)
	}
	each, cancel := context.WithTimeout(ctx, eachTime)
	defer cancel()
	sign, err := cloud.Request(each, kind, endpoint, as)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(each, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	return sign(req)
}
