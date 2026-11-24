package source

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Where a Grafana sends its alerts (docs/design/content.md): its own
// Alertmanager, external ones, or both. Read from Grafana's alerting API
// (its admin configuration and its active Alertmanagers) and its data
// sources API (the Alertmanagers that take Grafana's alerts). It only
// reads.

// maxSendNames is how many data source names are kept.
const maxSendNames = 20

// GrafanaSends is where a Grafana sends the alerts of its own rules.
type GrafanaSends struct {
	// Choice is Grafana's alertmanagersChoice: "internal", "external" or
	// "all". "internal" when Grafana has no configuration, which is its
	// default.
	Choice string
	// Active is how many external Alertmanagers Grafana is sending to now.
	Active int
	// Sources are the names of the Alertmanager data sources set to take
	// Grafana's alerts; SourcesRead is false when data sources couldn't be
	// read, and they are then not known.
	Sources     []string
	SourcesRead bool
}

// External reports whether Grafana's alerts go to an external
// Alertmanager.
func (g GrafanaSends) External() bool { return g.Choice != "internal" && g.Active > 0 }

// Internal reports whether Grafana's alerts go through its own
// Alertmanager. "external" with no external Alertmanager sends nowhere:
// that is checked as an internal one is, as before.
func (g GrafanaSends) Internal() bool { return g.Choice != "external" || g.Active == 0 }

// GrafanaAlertmanagers asks a Grafana where it sends its alerts:
// GET /api/v1/ngalert/admin_config (alertmanagersChoice; 404 is a Grafana
// with none, which sends to its own), GET /api/v1/ngalert/alertmanagers
// (the external Alertmanagers it is sending to) and GET /api/datasources
// (the Alertmanager data sources that take its alerts, whose
// jsonData.handleGrafanaManagedAlerts is true; not reading them isn't an
// error). A Grafana that answers otherwise, or not at all, is an error:
// the caller checks as it did.
func GrafanaAlertmanagers(ctx context.Context, base, tokenFile, agent string) (GrafanaSends, error) {
	get, err := grafanaGetter(ctx, base, tokenFile, agent)
	if err != nil {
		return GrafanaSends{}, err
	}
	g := GrafanaSends{Choice: "internal"}
	body, code, err := get("/api/v1/ngalert/admin_config")
	switch {
	case code == 404:
		return g, nil
	case err != nil:
		return GrafanaSends{}, err
	}
	var cfg struct {
		Choice *string `json:"alertmanagersChoice"`
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		return GrafanaSends{}, fmt.Errorf("/api/v1/ngalert/admin_config: not the answer of Grafana's alerting API: %w", err)
	}
	// An unset choice is "all", as Grafana's API says when none is sent.
	c := "all"
	if cfg.Choice != nil && *cfg.Choice != "" {
		c = strings.ToLower(*cfg.Choice)
	}
	if c != "all" && c != "internal" && c != "external" {
		return GrafanaSends{}, fmt.Errorf("/api/v1/ngalert/admin_config: alertmanagersChoice %q isn't one of internal, external, all", clip(c))
	}
	g.Choice = c
	if g.Choice == "internal" {
		return g, nil
	}
	if body, _, err = get("/api/v1/ngalert/alertmanagers"); err != nil {
		return GrafanaSends{}, err
	}
	var ams struct {
		Data struct {
			Active []json.RawMessage `json:"activeAlertManagers"`
		} `json:"data"`
		Active []json.RawMessage `json:"activeAlertManagers"`
	}
	if err := json.Unmarshal(body, &ams); err != nil {
		return GrafanaSends{}, fmt.Errorf("/api/v1/ngalert/alertmanagers: not the answer of Grafana's alerting API: %w", err)
	}
	g.Active = len(ams.Active) + len(ams.Data.Active)
	if body, _, err = get("/api/datasources"); err == nil {
		var ds []struct {
			Name     string `json:"name"`
			Type     string `json:"type"`
			JSONData struct {
				Handle bool `json:"handleGrafanaManagedAlerts"`
			} `json:"jsonData"`
		}
		if json.Unmarshal(body, &ds) == nil {
			g.SourcesRead = true
			for _, d := range ds {
				if d.Type == "alertmanager" && d.JSONData.Handle && len(g.Sources) < maxSendNames {
					g.Sources = append(g.Sources, clip(d.Name))
				}
			}
			sort.Strings(g.Sources)
		}
	}
	return g, nil
}
