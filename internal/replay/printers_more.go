package replay

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The kinds the three scenarios do not have, which test/replay-diff/kinds has one of each of: what
// holds state, what runs to an end, what stores, and what routes. Written, like the others, from the
// API server's printers and compared with a v1.37 cluster's output.
func init() {
	for kind, p := range map[string]printer{
		"StatefulSet": {
			group:   "apps",
			columns: named(col("Ready"), col("Age"), wide("Containers"), wide("Images")),
			cells: func(o obj, now time.Time) []any {
				names, images := containers(o.at("spec", "template", "spec"))
				return []any{o.name(), fmt.Sprintf("%d/%d", o.int("status", "readyReplicas"), o.int("spec", "replicas")), o.age(now), names, images}
			},
		},
		"ReplicationController": {
			columns: named(col("Desired"), col("Current"), col("Ready"), col("Age"), wide("Containers"), wide("Images"), wide("Selector")),
			cells: func(o obj, now time.Time) []any {
				names, images := containers(o.at("spec", "template", "spec"))
				return []any{o.name(), o.int("spec", "replicas"), o.int("status", "replicas"), o.int("status", "readyReplicas"), o.age(now), names, images, labelSet(o.at("spec", "selector"))}
			},
		},
		"Job": {
			group:   "batch",
			columns: named(col("Status"), col("Completions"), col("Duration"), col("Age"), wide("Containers"), wide("Images"), wide("Selector")),
			cells:   jobCells,
		},
		"CronJob": {
			group:   "batch",
			columns: named(col("Schedule"), col("Timezone"), col("Suspend"), col("Active"), col("Last Schedule"), col("Age"), wide("Containers"), wide("Images"), wide("Selector")),
			cells: func(o obj, now time.Time) []any {
				suspend := "<unset>"
				if o.has("spec", "suspend") {
					suspend = map[bool]string{true: "True", false: "False"}[o.bool("spec", "suspend")]
				}
				last := "<none>"
				if o.has("status", "lastScheduleTime") {
					last = since(o.time("status", "lastScheduleTime"), now)
				}
				job := o.at("spec", "jobTemplate", "spec")
				names, images := containers(job.at("template", "spec"))
				selector := "<none>"
				if job.has("selector") {
					selector = labelSelector(job.at("selector"))
				}
				return []any{o.name(), o.str("spec", "schedule"), orNone(o.str("spec", "timeZone")), suspend, int64(len(o.list("status", "active"))), last, o.age(now), names, images, selector}
			},
		},
		"PersistentVolumeClaim": {
			columns: named(col("Status"), col("Volume"), col("Capacity"), col("Access Modes"), col("StorageClass"), col("VolumeAttributesClass"), col("Age"), wide("VolumeMode")),
			cells: func(o obj, now time.Time) []any {
				phase, capacity, modes := o.str("status", "phase"), "", ""
				if o.has("metadata", "deletionTimestamp") {
					phase = "Terminating"
				}
				if o.str("spec", "volumeName") != "" { // what it got, once it has got something: an amount, if only none
					capacity, modes = quantity(o.get("status", "capacity", "storage")), accessModes(o.strings("status", "accessModes"))
				}
				return []any{o.name(), phase, o.str("spec", "volumeName"), capacity, modes, storageClass(o), unset(o.str("spec", "volumeAttributesClassName")), o.age(now), unset(o.str("spec", "volumeMode"))}
			},
		},
		"PersistentVolume": {
			columns: named(col("Capacity"), col("Access Modes"), col("Reclaim Policy"), col("Status"), col("Claim"), col("StorageClass"), col("VolumeAttributesClass"), col("Reason"), col("Age"), wide("VolumeMode")),
			cells: func(o obj, now time.Time) []any {
				phase, claim := o.str("status", "phase"), ""
				if o.has("metadata", "deletionTimestamp") {
					phase = "Terminating"
				}
				if ref := o.at("spec", "claimRef"); ref != nil {
					claim = ref.str("namespace") + "/" + ref.str("name")
				}
				return []any{o.name(), o.str("spec", "capacity", "storage"), accessModes(o.strings("spec", "accessModes")), o.str("spec", "persistentVolumeReclaimPolicy"), phase, claim,
					storageClass(o), unset(o.str("spec", "volumeAttributesClassName")), o.str("status", "reason"), o.age(now), unset(o.str("spec", "volumeMode"))}
			},
		},
		"HorizontalPodAutoscaler": {
			group:   "autoscaling",
			columns: named(col("Reference"), col("Targets"), col("MinPods"), col("MaxPods"), col("Replicas"), col("Age")),
			cells: func(o obj, now time.Time) []any {
				min := "<unset>"
				if o.has("spec", "minReplicas") {
					min = strconv.FormatInt(o.int("spec", "minReplicas"), 10)
				}
				return []any{o.name(), o.str("spec", "scaleTargetRef", "kind") + "/" + o.str("spec", "scaleTargetRef", "name"), autoscalerTargets(o), min,
					o.int("spec", "maxReplicas"), o.int("status", "currentReplicas"), o.age(now)}
			},
		},
		"Ingress": {
			group:   "networking.k8s.io",
			columns: named(col("Class"), col("Hosts"), col("Address"), col("Ports"), col("Age")),
			cells: func(o obj, now time.Time) []any {
				seen, address := map[string]bool{}, []string{}
				for _, in := range o.list("status", "loadBalancer", "ingress") {
					at := in.str("ip")
					if at == "" {
						at = in.str("hostname")
					}
					if at != "" && !seen[at] {
						seen[at], address = true, append(address, at)
					}
				}
				sort.Strings(address)
				ports := "80"
				if len(o.list("spec", "tls")) > 0 {
					ports = "80, 443"
				}
				return []any{o.name(), orNone(o.str("spec", "ingressClassName")), ingressHosts(o.list("spec", "rules")), strings.Join(address, ","), ports, o.age(now)}
			},
		},
		"IngressClass": {
			group:   "networking.k8s.io",
			columns: named(col("Controller"), col("Parameters"), col("Age")),
			cells: func(o obj, now time.Time) []any {
				parameters := "<none>"
				if p := o.at("spec", "parameters"); p != nil {
					parameters = p.str("kind")
					if p.has("apiGroup") {
						parameters += "." + p.str("apiGroup")
					}
					parameters += "/" + p.str("name")
				}
				name := o.name()
				if o.str("metadata", "annotations", "ingressclass.kubernetes.io/is-default-class") == "true" {
					name += " (default)"
				}
				return []any{name, o.str("spec", "controller"), parameters, o.age(now)}
			},
			as: func(minor int, p printer) printer {
				if minor < 36 { // the default class is marked from v1.36
					cells := p.cells
					p.cells = func(o obj, now time.Time) []any {
						c := cells(o, now)
						c[0] = o.name()
						return c
					}
				}
				return p
			},
		},
		"ResourceQuota": {
			columns: named(col("Request"), col("Limit"), col("Age")),
			cells: func(o obj, now time.Time) []any {
				hard := o.at("status", "hard")
				names := make([]string, 0, len(hard))
				for name := range hard {
					names = append(names, name)
				}
				sort.Strings(names)
				var request, limit []string
				for _, name := range names {
					entry := fmt.Sprintf("%s: %s/%s", name, quantity(o.get("status", "used", name)), quantity(hard[name]))
					if strings.HasPrefix(name, "limits.") {
						limit = append(limit, entry)
					} else {
						request = append(request, entry)
					}
				}
				return []any{o.name(), strings.Join(request, ", "), strings.Join(limit, ", "), o.age(now)}
			},
			as: func(minor int, p printer) printer {
				if minor < 33 { // the age stood before the amounts up to v1.32
					cells := p.cells
					p.columns = named(col("Age"), col("Request"), col("Limit"))
					p.cells = func(o obj, now time.Time) []any {
						c := cells(o, now)
						return []any{c[0], c[3], c[1], c[2]}
					}
				}
				return p
			},
		},
		"LimitRange": {columns: named(col("Created At")), cells: createdAt},
		"RuntimeClass": {
			group:   "node.k8s.io",
			columns: named(col("Handler"), col("Age")),
			cells:   func(o obj, now time.Time) []any { return []any{o.name(), o.str("handler"), o.age(now)} },
		},
		"CustomResourceDefinition": {
			group:   "apiextensions.k8s.io",
			columns: named(col("Scope"), col("Versions"), col("Created At"), wide("Group"), wide("Kind"), wide("ShortNames"), wide("Established")),
			cells: func(o obj, _ time.Time) []any {
				var versions []string
				for _, v := range o.list("spec", "versions") {
					if !v.bool("served") {
						continue
					}
					name := v.str("name")
					if v.bool("storage") {
						name += "(storage)"
					}
					versions = append(versions, name)
				}
				sort.Strings(versions) // as letters, so v1 before v1beta1 and v10 before v2
				return []any{o.name(), o.str("spec", "scope"), strings.Join(versions, ","), o.time("metadata", "creationTimestamp").UTC().Format(time.RFC3339),
					o.str("spec", "group"), o.str("spec", "names", "kind"), strings.Join(o.strings("spec", "names", "shortNames"), ","), condition(o, "Established") == "True"}
			},
			as: func(minor int, p printer) printer {
				if minor < 37 { // a name and when it was created, and no more, up to v1.36
					p.columns, p.cells = named(col("Created At")), createdAt
				}
				return p
			},
		},
	} {
		printers[kind] = p
	}
}

// ingressHosts is formatHosts: the first three hosts, and how many rules there are beyond three —
// rules, whether or not they name a host, which is how the API server counts them.
func ingressHosts(rules []obj) string {
	var list []string
	more := false
	for _, rule := range rules {
		if len(list) == 3 {
			more = true
		}
		if host := rule.str("host"); !more && host != "" {
			list = append(list, host)
		}
	}
	if len(list) == 0 {
		return "*"
	}
	if more {
		return fmt.Sprintf("%s + %d more...", strings.Join(list, ","), len(rules)-3)
	}
	return strings.Join(list, ",")
}

func unset(s string) string {
	if s == "" {
		return "<unset>"
	}
	return s
}

// quantity is a count or an amount as the API writes it: a string, or a bare number.
func quantity(v any) string {
	switch q := v.(type) {
	case string:
		return q
	case float64:
		return strconv.FormatFloat(q, 'f', -1, 64)
	}
	return "0"
}

func accessModes(modes []string) string {
	short := map[string]string{"ReadWriteOnce": "RWO", "ReadOnlyMany": "ROX", "ReadWriteMany": "RWX", "ReadWriteOncePod": "RWOP"}
	var out []string
	for _, want := range []string{"ReadWriteOnce", "ReadOnlyMany", "ReadWriteMany", "ReadWriteOncePod"} { // in this order, whatever the object's
		for _, m := range modes {
			if m == want {
				out = append(out, short[m])
				break
			}
		}
	}
	return strings.Join(out, ",")
}

func storageClass(o obj) string {
	if class := o.str("metadata", "annotations", "volume.beta.kubernetes.io/storage-class"); class != "" {
		return class
	}
	return o.str("spec", "storageClassName")
}

// jobCells is printJob: what it came to, how much of it is done, and how long it took or has taken.
func jobCells(o obj, now time.Time) []any {
	is := func(kind string) bool { return condition(o, kind) == "True" }
	status := "Running"
	switch {
	case is("Complete"):
		status = "Complete"
	case is("Failed"):
		status = "Failed"
	case o.has("metadata", "deletionTimestamp"):
		status = "Terminating"
	case is("Suspended"):
		status = "Suspended"
	case is("FailureTarget"):
		status = "FailureTarget"
	case is("SuccessCriteriaMet"):
		status = "SuccessCriteriaMet"
	}
	done := fmt.Sprintf("%d/1", o.int("status", "succeeded"))
	switch {
	case o.has("spec", "completions"):
		done = fmt.Sprintf("%d/%d", o.int("status", "succeeded"), o.int("spec", "completions"))
	case o.int("spec", "parallelism") > 1:
		done = fmt.Sprintf("%d/1 of %d", o.int("status", "succeeded"), o.int("spec", "parallelism"))
	}
	took := ""
	switch {
	case !o.has("status", "startTime"):
	case !o.has("status", "completionTime"):
		took = since(o.time("status", "startTime"), now)
	default:
		took = humanDuration(o.time("status", "completionTime").Sub(o.time("status", "startTime")))
	}
	names, images := containers(o.at("spec", "template", "spec"))
	return []any{o.name(), status, done, took, o.age(now), names, images, labelSelector(o.at("spec", "selector"))}
}

// autoscalerTargets is formatHPAMetrics: for each metric, where it stands against where it should,
// two of them and a count of the rest. A metric's status is the one at the same place in the list,
// if it is of the same kind. Only the resource metrics were compared with a cluster; the rest follow
// the API server's code, down to the <nil> it prints for an amount a status does not give.
func autoscalerTargets(o obj) string {
	specs, statuses := o.list("spec", "metrics"), o.list("status", "currentMetrics")
	if len(specs) == 0 {
		return "<none>"
	}
	var list []string
	for i, spec := range specs {
		var status obj
		if i < len(statuses) {
			status = statuses[i]
		}
		// amount is what the status says of one field: unknown if it has no status of this kind, and
		// otherwise the amount — or, where the API server would print a pointer it never checked, <nil>.
		amount := func(source, field string, checked bool) string {
			switch v := status.str(source, "current", field); {
			case !status.has(source), v == "" && checked:
				return "<unknown>"
			case v == "":
				return "<nil>"
			default:
				return v
			}
		}
		switch kind := spec.str("type"); kind {
		case "Resource", "ContainerResource":
			source := map[string]string{"Resource": "resource", "ContainerResource": "containerResource"}[kind]
			name := spec.str(source, "name")
			if target := spec.str(source, "target", "averageValue"); target != "" {
				list = append(list, fmt.Sprintf("%s: %s/%s", name, amount(source, "averageValue", false), target))
				break
			}
			current, target := "<unknown>", "<auto>"
			if status.has(source, "current", "averageUtilization") {
				current = fmt.Sprintf("%d%%", status.int(source, "current", "averageUtilization"))
			}
			if spec.has(source, "target", "averageUtilization") {
				target = fmt.Sprintf("%d%%", spec.int(source, "target", "averageUtilization"))
			}
			list = append(list, fmt.Sprintf("%s: %s/%s", name, current, target))
		case "Pods":
			list = append(list, fmt.Sprintf("%s/%s", amount("pods", "averageValue", false), spec.str("pods", "target", "averageValue")))
		case "Object", "External":
			source := strings.ToLower(kind)
			if target := spec.str(source, "target", "averageValue"); target != "" {
				list = append(list, fmt.Sprintf("%s/%s (avg)", amount(source, "averageValue", true), target))
			} else {
				list = append(list, fmt.Sprintf("%s/%s", amount(source, "value", false), spec.str(source, "target", "value")))
			}
		default:
			list = append(list, "<unknown type>")
		}
	}
	if len(list) > 2 {
		return fmt.Sprintf("%s + %d more...", strings.Join(list[:2], ", "), len(list)-2)
	}
	return strings.Join(list, ", ")
}
