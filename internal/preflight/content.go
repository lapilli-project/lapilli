package preflight

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"lapilli.dev/internal/content"
	"lapilli.dev/internal/source"
)

// mostPassed is how many of what serve would pass over in Alertmanager's
// configuration an item names.
const mostPassed = 5

// alertmanager reads --alertmanager-config as serve reads it, once: a
// file and the template files beside it, or Mimir's or Cortex's
// configuration API, with the ruler's token and tenant, through the
// allowlist (allowAll). It says how many receivers and templates it
// found; what serve would pass over, and say in its log, is a problem. It is
// read when the content check is on, or the notifications worked out are (who
// a firing reached), which read it too.
func (p *run) alertmanager(ctx context.Context) {
	s := p.c.Serve
	// Read by the content check, and by the notifications worked out (who a
	// firing reached) with it off, each hour.
	if !(s.CheckContent || s.Notifications) || s.AlertmanagerConfig == "" {
		return
	}
	api := source.AlertmanagerURL(s.AlertmanagerConfig)
	if api && p.c.Offline {
		p.offline("content.alertmanager", "the Alertmanager's configuration not asked")
		return
	}
	var cfg []byte
	var files []source.TemplateFile
	var passed []string
	var err error
	what := "--alertmanager-config read, as serve reads it"
	if api {
		each, cancel := context.WithTimeout(ctx, eachTime)
		var found bool
		cfg, files, found, err = source.AlertmanagerAPI(each, s.AlertmanagerConfig, s.RulerTenant, source.Creds{TokenFile: s.RulerToken}, p.agent(), nil)
		cancel()
		if err == nil && !found {
			err = errNoAlertmanager
		}
		what = "[--alertmanager-config] answered, as serve asks it"
	} else {
		cfg, files, passed, err = source.AlertmanagerFiles(s.AlertmanagerConfig)
	}
	var n *content.Notifications
	var skipped []string
	// Whether the configuration was read at all (the routing's own read
	// doesn't depend on the content check's).
	got := err == nil
	// Who a firing reached reads the routing as serve does, from the
	// configuration alone (no templates): a route to a receiver that isn't
	// there, a matcher that doesn't parse, is a configuration serve can't use.
	routingProblem := func() {
		if !got || !s.Notifications {
			return
		}
		if _, rerr := content.ReadAlertmanagerRouting(cfg); rerr != nil {
			p.add(Item{ID: "notifications.routing", State: Problem,
				What: "Alertmanager's routing couldn't be read: serve works out no routing, so who each firing reached isn't known",
				Why:  said(rerr), Fix: "fix the configuration's routes and receivers so that Alertmanager loads it: a route's receiver defined, its matchers and regular expressions parsing"})
		}
	}
	if err == nil && s.CheckContent {
		in := make([]content.TemplateFile, len(files))
		for i, f := range files {
			in[i] = content.TemplateFile(f)
		}
		n, skipped, err = content.ReadAlertmanager(cfg, in)
	}
	if err != nil {
		it := Item{ID: "content.alertmanager", State: Problem,
			What: "Alertmanager's configuration couldn't be read: the content check goes on without its templates", Why: said(err)}
		if !s.CheckContent {
			it.What = "Alertmanager's configuration couldn't be read: serve works out no routing, so who each firing reached isn't known"
		} else if !got && s.Notifications {
			// The source itself failed: the routing is lost with the templates.
			it.What = "Alertmanager's configuration couldn't be read: the content check goes on without its templates, and serve works out no routing, so who each firing reached isn't known"
		}
		switch {
		case errors.Is(err, errNoAlertmanager):
			it.Fix = "give --ruler-tenant the tenant whose Alertmanager sends the rules"
		case api && (strings.Contains(err.Error(), "HTTP 401") || strings.Contains(err.Error(), "HTTP 403")):
			it.Fix = "give --ruler-token-file a token that reads the tenant's Alertmanager configuration, and --ruler-tenant its tenant"
		case api:
			it.Fix = "give Mimir's or Cortex's /api/v1/alerts, reachable from where serve runs"
		default:
			it.Fix = "give Alertmanager's configuration file, with a route and its receivers, or one in Mimir's format"
		}
		p.add(it)
		routingProblem()
		return
	}
	if !s.CheckContent {
		// Read for the routing alone: serve's routing ignores the templates.
		p.add(Item{ID: "content.alertmanager", State: OK, What: what + ", for the routing"})
		routingProblem()
		return
	}
	defer routingProblem()
	r := n.Read()
	it := Item{ID: "content.alertmanager", State: OK, What: fmt.Sprintf("%s: %s; %s, defining %s", what, plural(r.Receivers, "receiver"),
		plural(r.Files, "template file"), plural(r.Templates, "template"))}
	if over := append(passed, skipped...); len(over) > 0 {
		told := make([]string, 0, min(len(over), mostPassed))
		for _, x := range over[:min(len(over), mostPassed)] {
			told = append(told, saidText(x))
		}
		if len(over) > mostPassed {
			told = append(told, fmt.Sprintf("and %d more", len(over)-mostPassed))
		}
		it.State, it.Why = Problem, strings.Join(told, "; ")
		it.Fix = "serve reads the rest, and says the same once in its log"
		if len(passed) > 0 {
			it.Fix += ": mount the template files beside the configuration, as its templates: globs are read there (an absolute one by its last element)"
		} else {
			it.Fix += "; a template file that doesn't parse keeps Alertmanager from loading the configuration too"
		}
	}
	p.add(it)
}

// errNoAlertmanager is a tenant with no Alertmanager configuration: Mimir's
// 404, as serve says it.
var errNoAlertmanager = errors.New("the tenant has no Alertmanager configuration (HTTP 404)")
