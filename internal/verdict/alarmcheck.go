package verdict

import (
	"regexp"
	"slices"
	"strings"

	"lapilli.dev/internal/firing"
	"lapilli.dev/internal/rules"
)

// A cloud's own alarms keep no series of what they fired, as Prometheus's
// ALERTS is. The history the recipes in recipes/firings copy of them can
// stand in, alarm by alarm, for the ones Lapilli replays: CloudWatch PromQL
// alarms, Google Cloud policies with a PromQL condition, Azure's PromQL
// metric alerts (docs/design/inputs.md).

// AlarmIDLabel is the label a recipe gives each firing with its alarm's own
// ID.
const AlarmIDLabel = "alarm_id"

// googlePolicy is a Google policy's name, its project by ID or by number.
var googlePolicy = regexp.MustCompile(`^projects/[^/]+/(alertPolicies/[^/]+)$`)

// AlarmKey is what an alarm's ID is compared by: a Google policy's by the
// policy alone, as one place names its project by ID and another by
// number; every other ID as it is.
func AlarmKey(id string) string {
	if m := googlePolicy.FindStringSubmatch(id); m != nil {
		return m[1]
	}
	return id
}

// ReplayedAlarm reports whether a rule is a cloud's own alarm that Lapilli
// replays: its history can be set beside the replay.
func ReplayedAlarm(r rules.Rule) bool {
	return r.Cloud != "" && r.Expr != "" && r.Unsupported == ""
}

// AlarmHistory ties the firings a recipe recorded of a cloud's alarms to the
// replayed alarms of known they are of, and renames each to its alarm's
// rule, without its alarm_id: by that ID where the firing has one and the
// alarm's is known (read with --alarms, whose rules carry it as their
// Source), and otherwise by the name the recipe gives the alarm (a Google
// policy's display name, every other cloud's alarm's own). A firing that
// ties to none, or to more than one, is left out.
func AlarmHistory(known []rules.Rule, recorded []firing.Firing) []firing.Firing {
	type alarm struct {
		name  string
		id    string
		azure bool
	}
	var alarms []alarm
	byName := map[string][]int{}
	for _, r := range known {
		if !ReplayedAlarm(r) {
			continue
		}
		a := alarm{name: r.Name, azure: r.Cloud == "azure"}
		if r.Namespace != "" {
			a.id = AlarmKey(r.Source)
		}
		i := len(alarms)
		alarms = append(alarms, a)
		names := []string{r.Name}
		if r.Cloud == "google" && r.Group != "" && r.Group != r.Name {
			names = append(names, r.Group) // the policy's display name
		}
		for _, n := range names {
			if !slices.Contains(byName[n], i) {
				byName[n] = append(byName[n], i)
			}
		}
	}
	// unique is the one alarm of ids that ok takes, or -1.
	unique := func(ids []int, ok func(alarm) bool) int {
		match := -1
		for _, i := range ids {
			if ok(alarms[i]) {
				if match >= 0 {
					return -1
				}
				match = i
			}
		}
		return match
	}
	all := make([]int, len(alarms))
	for i := range all {
		all[i] = i
	}
	var out []firing.Firing
	for _, f := range recorded {
		match := -1
		if id := f.Labels[AlarmIDLabel]; id != "" {
			k := AlarmKey(id)
			byID := func(a alarm) bool { return a.id != "" && (a.id == k || a.azure && strings.EqualFold(a.id, k)) }
			if match = unique(all, byID); match < 0 && !slices.ContainsFunc(alarms, byID) {
				// No alarm known by its ID: by name, among those whose ID
				// isn't known, so an alarm's history isn't taken for
				// another's of the same name.
				match = unique(byName[f.Rule], func(a alarm) bool { return a.id == "" })
			}
		} else {
			match = unique(byName[f.Rule], func(alarm) bool { return true })
		}
		if match < 0 {
			continue
		}
		g := f
		g.Rule = alarms[match].name
		g.Labels = make(map[string]string, len(f.Labels))
		for k, v := range f.Labels {
			if k != AlarmIDLabel {
				g.Labels[k] = v
			}
		}
		out = append(out, g)
	}
	return out
}

// Whole puts together the firings of each rule named in whole into firings
// of the alarm as a whole, as CloudWatch keeps an alarm's history: one
// while any of its series fires, with no labels. Other firings are as
// they were.
func Whole(fs []firing.Firing, whole map[string]bool) []firing.Firing {
	if len(whole) == 0 {
		return fs
	}
	var out []firing.Firing
	by := map[string][]firing.Firing{}
	for _, f := range fs {
		if whole[f.Rule] {
			by[f.Rule] = append(by[f.Rule], f)
		} else {
			out = append(out, f)
		}
	}
	for _, rule := range slices.Sorted(func(yield func(string) bool) {
		for k := range by {
			if !yield(k) {
				return
			}
		}
	}) {
		group := by[rule]
		slices.SortFunc(group, func(a, b firing.Firing) int { return a.Start.Compare(b.Start) })
		var cur *firing.Firing
		for _, f := range group {
			if cur != nil && (cur.End.IsZero() || !f.Start.After(cur.End)) {
				if !cur.End.IsZero() && (f.End.IsZero() || f.End.After(cur.End)) {
					cur.End = f.End
				}
				continue
			}
			if cur != nil {
				out = append(out, *cur)
			}
			cur = &firing.Firing{Rule: rule, Labels: map[string]string{}, Start: f.Start, End: f.End}
		}
		if cur != nil {
			out = append(out, *cur)
		}
	}
	return out
}

// WholeAlarms are the names of the rules whose history is of the alarm as a
// whole: CloudWatch's.
func WholeAlarms(known []rules.Rule) map[string]bool {
	out := map[string]bool{}
	for _, r := range known {
		if r.Cloud == "cloudwatch" && ReplayedAlarm(r) {
			out[r.Name] = true
		}
	}
	return out
}

// NoHistory is why a replayed cloud alarm isn't checked without its
// history.
const NoHistory = "a cloud's alarm keeps no series of what it fired: give its history, as recipes/firings writes it, with --check-firings"
