// Package routing works out where an alert manager sends what it is given:
// which receivers a firing reaches, in which notifications, when, and which
// of them page someone (docs/design/routing.md). It is a model, not a
// record: it reads an Alertmanager's or a Grafana's routing configuration,
// takes alerts (a label set, when it started, when it resolved), and plays
// them through the route tree, the groups, the inhibition rules, the
// silences, the time intervals and the notification log the way
// Alertmanager's dispatcher and notification pipeline do
// (dispatch/dispatch.go, notify/notify.go, notify/dedup_stage.go,
// inhibit/inhibit.go, timeinterval/timeinterval.go, config/notifiers.go in
// github.com/prometheus/alertmanager), without importing any of them.
//
// The pieces:
//
//   - Config is the routing configuration: the route tree with each
//     route's options inherited as Alertmanager inherits them, the receivers'
//     integrations, the named time intervals and the inhibition rules.
//     content reads one from Alertmanager's configuration file, or from
//     Grafana's provisioning API; tests and other readers can build one with
//     NewConfig.
//   - Silence is a silence as Alertmanager's API lists it (ReadSilences).
//   - Config.Run plays Alerts through the configuration, with Options, and
//     gives a Result: for each alert what reached which receiver and what
//     muted it, and every Notification, worked out.
//
// A run is worked out event by event: a group is touched when one of its
// alerts starts or resolves, when something that could mute or unmute one
// of them changes, and when its repeat interval falls due, never at each of
// its flushes. It is bounded (MaxGroups) and deterministic: the same
// configuration, alerts and options give the same result, in the same order.
//
// Nothing here reads a secret or a connection's address; a configuration
// holds names, kinds, matchers, durations and booleans only.
package routing
