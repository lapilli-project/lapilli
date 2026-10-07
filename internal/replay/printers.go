package replay

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The rows of a table, kind by kind: what `kubectl get` prints for each. tables.go has what asks for
// them.

// printers holds the kinds whose table is built here, by the `kind` of the object. Each was written
// from the API server's own (kubernetes/pkg/printers/internalversion) and is compared with a real
// cluster's output by test/replay-diff. A kind is added here when that comparison can check it.
var printers = map[string]printer{
	"Pod": {
		columns: named(col("Ready"), col("Status"), col("Restarts"), col("Age"), wide("IP"), wide("Node"), wide("Nominated Node"), wide("Readiness Gates")),
		cells:   podCells,
	},
	"Deployment": {
		columns: named(col("Ready"), col("Up-to-date"), col("Available"), col("Age"), wide("Containers"), wide("Images"), wide("Selector")),
		cells: func(o obj, now time.Time) []any {
			names, images := containers(o.at("spec", "template", "spec"))
			return []any{o.name(), fmt.Sprintf("%d/%d", o.int("status", "readyReplicas"), o.int("spec", "replicas")), o.int("status", "updatedReplicas"), o.int("status", "availableReplicas"),
				o.age(now), names, images, labelSelector(o.at("spec", "selector"))}
		},
	},
	"ReplicaSet": {
		columns: named(col("Desired"), col("Current"), col("Ready"), col("Age"), wide("Containers"), wide("Images"), wide("Selector")),
		cells: func(o obj, now time.Time) []any {
			names, images := containers(o.at("spec", "template", "spec"))
			return []any{o.name(), o.int("spec", "replicas"), o.int("status", "replicas"), o.int("status", "readyReplicas"), o.age(now), names, images, labelSelector(o.at("spec", "selector"))}
		},
	},
	"DaemonSet": {
		columns: named(col("Desired"), col("Current"), col("Ready"), col("Up-to-date"), col("Available"), col("Node Selector"), col("Age"), wide("Containers"), wide("Images"), wide("Selector")),
		cells: func(o obj, now time.Time) []any {
			names, images := containers(o.at("spec", "template", "spec"))
			return []any{o.name(), o.int("status", "desiredNumberScheduled"), o.int("status", "currentNumberScheduled"), o.int("status", "numberReady"), o.int("status", "updatedNumberScheduled"),
				o.int("status", "numberAvailable"), labelSet(o.at("spec", "template", "spec", "nodeSelector")), o.age(now), names, images, labelSelector(o.at("spec", "selector"))}
		},
	},
	"Service": {
		columns: named(col("Type"), col("Cluster-IP"), col("External-IP"), col("Port(s)"), col("Age"), wide("Selector")),
		cells:   serviceCells,
	},
	"Endpoints": {
		columns: named(col("Endpoints"), col("Age")),
		cells:   func(o obj, now time.Time) []any { return []any{o.name(), endpoints(o), o.age(now)} },
	},
	"EndpointSlice": {
		columns: named(col("AddressType"), col("Ports"), col("Endpoints"), col("Age")),
		cells: func(o obj, now time.Time) []any {
			var ports, addresses []string
			for _, p := range o.list("ports") {
				switch {
				case p.has("port"):
					ports = append(ports, strconv.FormatInt(p.int("port"), 10))
				case p.str("name") != "":
					ports = append(ports, p.str("name"))
				default:
					ports = append(ports, "*")
				}
			}
			for _, e := range o.list("endpoints") {
				addresses = append(addresses, e.strings("addresses")...)
			}
			return []any{o.name(), o.str("addressType"), firstThree(ports, "<unset>"), firstThree(addresses, "<unset>"), o.age(now)}
		},
	},
	"Event": {
		columns: []column{col("Last Seen"), col("Type"), col("Reason"), col("Object"), wide("Subobject"), wide("Source"), col("Message"), wide("First Seen"), wide("Count"), wide("Name")},
		cells:   eventCells,
	},
	"Node": {
		columns: named(col("Status"), col("Roles"), col("Age"), col("Version"), wide("Internal-IP"), wide("External-IP"), wide("OS-Image"), wide("Kernel-Version"), wide("Container-Runtime")),
		cells:   nodeCells,
	},

	// What a cluster is made of rather than what runs on it. Short, and here because an object listed
	// as a name and an age says less than the cluster did.
	"Role":               {columns: named(col("Created At")), cells: createdAt},
	"ClusterRole":        {columns: named(col("Created At")), cells: createdAt},
	"RoleBinding":        {columns: named(col("Role"), col("Age"), wide("Users"), wide("Groups"), wide("ServiceAccounts")), cells: bindingCells},
	"ClusterRoleBinding": {columns: named(col("Role"), col("Age"), wide("Users"), wide("Groups"), wide("ServiceAccounts")), cells: bindingCells},
	"ControllerRevision": {
		columns: named(col("Controller"), col("Revision"), col("Age")),
		cells: func(o obj, now time.Time) []any {
			controller := "<none>"
			for _, ref := range o.list("metadata", "ownerReferences") {
				if ref.bool("controller") {
					group, _, _ := strings.Cut(ref.str("apiVersion"), "/")
					if !strings.Contains(ref.str("apiVersion"), "/") {
						group = ""
					}
					controller = strings.ToLower(strings.TrimSuffix(ref.str("kind")+"."+group, ".")) + "/" + ref.str("name")
				}
			}
			return []any{o.name(), controller, o.int("revision"), o.age(now)}
		},
	},
	"StorageClass": {
		columns: named(col("Provisioner"), col("ReclaimPolicy"), col("VolumeBindingMode"), col("AllowVolumeExpansion"), col("Age")),
		cells: func(o obj, now time.Time) []any {
			or := func(s, unset string) string {
				if s == "" {
					return unset
				}
				return s
			}
			return []any{o.name(), o.str("provisioner"), or(o.str("reclaimPolicy"), "Delete"), or(o.str("volumeBindingMode"), "Immediate"), o.bool("allowVolumeExpansion"), o.age(now)}
		},
		// Seen on v1.37: the default class is marked in a list, and not when it is asked for by name.
		inList: func(o obj, cells []any) {
			if a := o.at("metadata", "annotations"); a["storageclass.kubernetes.io/is-default-class"] == "true" || a["storageclass.beta.kubernetes.io/is-default-class"] == "true" {
				cells[0] = o.name() + " (default)"
			}
		},
	},
	"PriorityClass": {
		columns: named(col("Value"), col("Global-Default"), col("Age"), col("PreemptionPolicy")),
		cells: func(o obj, now time.Time) []any {
			return []any{o.name(), o.int("value"), o.bool("globalDefault"), o.age(now), o.str("preemptionPolicy")}
		},
	},
	"CSINode": {
		columns: named(col("Drivers"), col("Age")),
		cells: func(o obj, now time.Time) []any {
			return []any{o.name(), int64(len(o.list("spec", "drivers"))), o.age(now)}
		},
	},
	"IPAddress": {
		columns: named(col("ParentRef")),
		cells: func(o obj, now time.Time) []any {
			ref := o.at("spec", "parentRef")
			if ref == nil {
				return []any{o.name(), "<none>"}
			}
			var parts []string
			for _, k := range []string{"group", "resource", "namespace", "name"} {
				if v := ref.str(k); v != "" || k == "resource" || k == "name" {
					parts = append(parts, v)
				}
			}
			return []any{o.name(), strings.Join(parts, "/")}
		},
	},
	"ServiceCIDR": {
		columns: named(col("CIDRs"), col("Age")),
		cells: func(o obj, now time.Time) []any {
			return []any{o.name(), strings.Join(o.strings("spec", "cidrs"), ","), o.age(now)}
		},
	},
	"APIService": {
		columns: named(col("Service"), col("Available"), col("Age")),
		cells: func(o obj, now time.Time) []any {
			service, available := "Local", "Unknown"
			if svc := o.at("spec", "service"); svc != nil {
				service = svc.str("namespace") + "/" + svc.str("name")
			}
			for _, c := range o.list("status", "conditions") {
				if c.str("type") == "Available" {
					if available = c.str("status"); available != "True" {
						available = fmt.Sprintf("%s (%s)", c.str("status"), c.str("reason"))
					}
				}
			}
			return []any{o.name(), service, available, o.age(now)}
		},
	},
	"ClusterTrustBundle": {
		columns: named(col("SignerName")),
		cells:   func(o obj, now time.Time) []any { return []any{o.name(), orNone(o.str("spec", "signerName"))} },
	},
	"CertificateSigningRequest": {
		columns: named(col("Age"), col("SignerName"), col("Requestor"), col("RequestedDuration"), col("Condition")),
		cells: func(o obj, now time.Time) []any {
			has := map[string]bool{}
			for _, c := range o.list("status", "conditions") {
				has[c.str("type")] = true
			}
			state := "Pending"
			switch {
			case has["Denied"]:
				state = "Denied"
			case has["Approved"]:
				state = "Approved"
			}
			if has["Failed"] {
				state += ",Failed"
			}
			if o.str("status", "certificate") != "" {
				state += ",Issued"
			}
			wanted := "<none>"
			if o.has("spec", "expirationSeconds") {
				wanted = humanDuration(time.Duration(o.int("spec", "expirationSeconds")) * time.Second)
			}
			return []any{o.name(), o.age(now), o.str("spec", "signerName"), o.str("spec", "username"), wanted, state}
		},
	},
	"FlowSchema": {
		columns: named(col("PriorityLevel"), col("MatchingPrecedence"), col("DistinguisherMethod"), col("Age"), col("MissingPL")),
		cells: func(o obj, now time.Time) []any {
			dangling := "?"
			for _, c := range o.list("status", "conditions") {
				if c.str("type") == "Dangling" {
					dangling = c.str("status")
					break
				}
			}
			return []any{o.name(), o.str("spec", "priorityLevelConfiguration", "name"), o.int("spec", "matchingPrecedence"), orNone(o.str("spec", "distinguisherMethod", "type")), o.age(now), dangling}
		},
		before: func(a, b obj) bool { // a cluster lists them in the order it applies them
			if x, y := a.int("spec", "matchingPrecedence"), b.int("spec", "matchingPrecedence"); x != y {
				return x < y
			}
			return a.name() < b.name()
		},
	},
	"PriorityLevelConfiguration": {
		columns: named(col("Type"), col("NominalConcurrencyShares"), col("Queues"), col("HandSize"), col("QueueLengthLimit"), col("Age")),
		cells: func(o obj, now time.Time) []any {
			number := func(keys ...string) any {
				if o.has(keys...) {
					return o.int(keys...)
				}
				return "<none>"
			}
			return []any{o.name(), o.str("spec", "type"), number("spec", "limited", "nominalConcurrencyShares"), number("spec", "limited", "limitResponse", "queuing", "queues"),
				number("spec", "limited", "limitResponse", "queuing", "handSize"), number("spec", "limited", "limitResponse", "queuing", "queueLengthLimit"), o.age(now)}
		},
	},
}

func createdAt(o obj, _ time.Time) []any {
	return []any{o.name(), o.time("metadata", "creationTimestamp").UTC().Format(time.RFC3339)}
}

// bindingCells is a RoleBinding or a ClusterRoleBinding: what it grants, and to whom.
func bindingCells(o obj, now time.Time) []any {
	var users, groups, accounts []string
	for _, s := range o.list("subjects") {
		switch s.str("kind") {
		case "User":
			users = append(users, s.str("name"))
		case "Group":
			groups = append(groups, s.str("name"))
		case "ServiceAccount":
			accounts = append(accounts, s.str("namespace")+"/"+s.str("name"))
		}
	}
	return []any{o.name(), o.str("roleRef", "kind") + "/" + o.str("roleRef", "name"), o.age(now), strings.Join(users, ", "), strings.Join(groups, ", "), strings.Join(accounts, ", ")}
}

// nodeCells is printNode: Ready or NotReady from the one condition, the roles from the labels.
func nodeCells(o obj, now time.Time) []any {
	status := []string{"Unknown"}
	switch condition(o, "Ready") {
	case "True":
		status = []string{"Ready"}
	case "False", "Unknown":
		status = []string{"NotReady"}
	}
	if o.bool("spec", "unschedulable") {
		status = append(status, "SchedulingDisabled")
	}
	var roles []string
	for k, v := range o.at("metadata", "labels") {
		if role, ok := strings.CutPrefix(k, "node-role.kubernetes.io/"); ok && role != "" {
			roles = append(roles, role)
		} else if s, _ := v.(string); k == "kubernetes.io/role" && s != "" {
			roles = append(roles, s)
		}
	}
	sort.Strings(roles)
	address := func(kind string) string {
		for _, a := range o.list("status", "addresses") {
			if a.str("type") == kind {
				return a.str("address")
			}
		}
		return "<none>"
	}
	unknown := func(s string) string {
		if s == "" {
			return "<unknown>"
		}
		return s
	}
	info := o.at("status", "nodeInfo")
	kernel := unknown(info.str("kernelVersion"))
	if arch := info.str("architecture"); arch != "" { // seen on v1.37: the architecture beside the kernel
		kernel += " (" + arch + ")"
	}
	return []any{o.name(), strings.Join(status, ","), orNone(strings.Join(roles, ",")), o.age(now), info.str("kubeletVersion"),
		address("InternalIP"), address("ExternalIP"), unknown(info.str("osImage")), kernel, unknown(info.str("containerRuntimeVersion"))}
}

// since is how the API server writes "how long ago": <unknown> for no time at all.
func since(t, now time.Time) string {
	if t.IsZero() {
		return "<unknown>"
	}
	return humanDuration(now.Sub(t))
}

// humanDuration is k8s.io/apimachinery/pkg/util/duration.HumanDuration: two units while the larger
// one is small, one after that.
func humanDuration(d time.Duration) string {
	seconds := int(d.Seconds())
	switch {
	case seconds < -1:
		return "<invalid>"
	case seconds < 0:
		return "0s"
	case seconds < 60*2:
		return fmt.Sprintf("%ds", seconds)
	}
	two := func(big int, bigUnit string, small int, smallUnit string) string {
		if small == 0 {
			return fmt.Sprintf("%d%s", big, bigUnit)
		}
		return fmt.Sprintf("%d%s%d%s", big, bigUnit, small, smallUnit)
	}
	minutes, hours := int(d/time.Minute), int(d/time.Hour)
	switch {
	case minutes < 10:
		return two(minutes, "m", int(d/time.Second)%60, "s")
	case minutes < 60*3:
		return fmt.Sprintf("%dm", minutes)
	case hours < 8:
		return two(hours, "h", minutes%60, "m")
	case hours < 48:
		return fmt.Sprintf("%dh", hours)
	case hours < 24*8:
		return two(hours/24, "d", hours%24, "h")
	case hours < 24*365*2:
		return fmt.Sprintf("%dd", hours/24)
	case hours < 24*365*8:
		return two(hours/24/365, "y", (hours/24)%365, "d")
	}
	return fmt.Sprintf("%dy", hours/24/365)
}

func orNone(s string) string {
	if s == "" {
		return "<none>"
	}
	return s
}

func containers(podSpec obj) (names, images string) {
	var n, i []string
	for _, c := range podSpec.list("containers") {
		n, i = append(n, c.str("name")), append(i, c.str("image"))
	}
	return strings.Join(n, ","), strings.Join(i, ",")
}

// labelSet writes a map of labels as k=v,k=v in key order, and <none> for an empty one.
func labelSet(m obj) string {
	parts := make([]string, 0, len(m))
	for k, v := range m {
		s, _ := v.(string)
		parts = append(parts, k+"="+s)
	}
	sort.Strings(parts)
	return orNone(strings.Join(parts, ","))
}

// labelSelector writes a selector the way a cluster does: its requirements in key order.
func labelSelector(sel obj) string {
	type requirement struct{ key, text string }
	var reqs []requirement
	for k, v := range sel.at("matchLabels") {
		s, _ := v.(string)
		reqs = append(reqs, requirement{k, k + "=" + s})
	}
	for _, e := range sel.list("matchExpressions") {
		key, values := e.str("key"), e.strings("values")
		sort.Strings(values)
		switch e.str("operator") {
		case "In":
			reqs = append(reqs, requirement{key, key + " in (" + strings.Join(values, ",") + ")"})
		case "NotIn":
			reqs = append(reqs, requirement{key, key + " notin (" + strings.Join(values, ",") + ")"})
		case "Exists":
			reqs = append(reqs, requirement{key, key})
		case "DoesNotExist":
			reqs = append(reqs, requirement{key, "!" + key})
		}
	}
	sort.SliceStable(reqs, func(i, j int) bool { return reqs[i].key < reqs[j].key })
	parts := make([]string, len(reqs))
	for i, r := range reqs {
		parts[i] = r.text
	}
	return strings.Join(parts, ",")
}

// firstThree joins at most three items and says how many more there are.
func firstThree(items []string, empty string) string {
	if len(items) == 0 {
		return empty
	}
	if len(items) > 3 {
		return fmt.Sprintf("%s + %d more...", strings.Join(items[:3], ","), len(items)-3)
	}
	return strings.Join(items, ",")
}

func condition(o obj, kind string) string {
	for _, c := range o.list("status", "conditions") {
		if c.str("type") == kind {
			return c.str("status")
		}
	}
	return ""
}

// podCells is printPod. The STATUS a cluster prints is not the pod's phase: it is the reason of the
// first container that is waiting or has terminated, with the init containers read first.
func podCells(o obj, now time.Time) []any {
	reason := o.str("status", "phase")
	if r := o.str("status", "reason"); r != "" {
		reason = r
	}
	for _, c := range o.list("status", "conditions") {
		if c.str("type") == "PodScheduled" && c.str("reason") == "SchedulingGated" {
			reason = "SchedulingGated"
		}
	}
	restartable := map[string]bool{} // init containers that keep running beside the others
	total := len(o.list("spec", "containers"))
	for _, c := range o.list("spec", "initContainers") {
		if c.str("restartPolicy") == "Always" {
			restartable[c.str("name")] = true
			total++
		}
	}
	lastTermination := func(c obj, latest time.Time) time.Time {
		if t := c.time("lastState", "terminated", "finishedAt"); c.has("lastState", "terminated") && latest.Before(t) {
			return t
		}
		return latest
	}

	var restarts, sidecarRestarts, ready int64
	var lastRestart, lastSidecarRestart time.Time
	initializing := false
	initStatuses := o.list("status", "initContainerStatuses")
	for i, c := range initStatuses {
		restarts += c.int("restartCount")
		lastRestart = lastTermination(c, lastRestart)
		if restartable[c.str("name")] {
			sidecarRestarts += c.int("restartCount")
			lastSidecarRestart = lastTermination(c, lastSidecarRestart)
		}
		terminated, waiting := c.at("state", "terminated"), c.at("state", "waiting")
		switch {
		case terminated != nil && terminated.int("exitCode") == 0:
			continue
		case restartable[c.str("name")] && c.bool("started"):
			if c.bool("ready") {
				ready++
			}
			continue
		case terminated != nil:
			switch {
			case terminated.str("reason") != "":
				reason = "Init:" + terminated.str("reason")
			case terminated.int("signal") != 0:
				reason = fmt.Sprintf("Init:Signal:%d", terminated.int("signal"))
			default:
				reason = fmt.Sprintf("Init:ExitCode:%d", terminated.int("exitCode"))
			}
			initializing = true
		case waiting != nil && waiting.str("reason") != "" && waiting.str("reason") != "PodInitializing":
			reason = "Init:" + waiting.str("reason")
			initializing = true
		default:
			reason = fmt.Sprintf("Init:%d/%d", i, len(o.list("spec", "initContainers")))
			initializing = true
		}
		break
	}

	if !initializing || condition(o, "Initialized") == "True" {
		restarts, lastRestart = sidecarRestarts, lastSidecarRestart
		running := false
		statuses := o.list("status", "containerStatuses")
		for i := len(statuses) - 1; i >= 0; i-- {
			c := statuses[i]
			restarts += c.int("restartCount")
			lastRestart = lastTermination(c, lastRestart)
			terminated, waiting := c.at("state", "terminated"), c.at("state", "waiting")
			switch {
			case waiting != nil && waiting.str("reason") != "":
				reason = waiting.str("reason")
			case terminated != nil && terminated.str("reason") != "":
				reason = terminated.str("reason")
			case terminated != nil && terminated.int("signal") != 0:
				reason = fmt.Sprintf("Signal:%d", terminated.int("signal"))
			case terminated != nil:
				reason = fmt.Sprintf("ExitCode:%d", terminated.int("exitCode"))
			case c.bool("ready") && c.has("state", "running"):
				running = true
				ready++
			}
		}
		if reason == "Completed" && running {
			if condition(o, "Ready") == "True" {
				reason = "Running"
			} else {
				reason = "NotReady"
			}
		}
	}
	if o.has("metadata", "deletionTimestamp") {
		switch phase := o.str("status", "phase"); {
		case o.str("status", "reason") == "NodeLost":
			reason = "Unknown"
		case phase != "Succeeded" && phase != "Failed":
			reason = "Terminating"
		}
	}

	restartCell := strconv.FormatInt(restarts, 10)
	if restarts != 0 && !lastRestart.IsZero() {
		restartCell = fmt.Sprintf("%d (%s ago)", restarts, since(lastRestart, now))
	}
	ip := ""
	if ips := o.list("status", "podIPs"); len(ips) > 0 {
		ip = ips[0].str("ip")
	}
	gates := "<none>"
	if wanted := o.list("spec", "readinessGates"); len(wanted) > 0 {
		met := 0
		for _, g := range wanted {
			if condition(o, g.str("conditionType")) == "True" {
				met++
			}
		}
		gates = fmt.Sprintf("%d/%d", met, len(wanted))
	}
	return []any{o.name(), fmt.Sprintf("%d/%d", ready, total), reason, restartCell, o.age(now),
		orNone(ip), orNone(o.str("spec", "nodeName")), orNone(o.str("status", "nominatedNodeName")), gates}
}

func serviceCells(o obj, now time.Time) []any {
	kind := o.str("spec", "type")
	clusterIP := "<none>"
	if ips := o.strings("spec", "clusterIPs"); len(ips) > 0 {
		clusterIP = ips[0]
	}
	external := strings.Join(o.strings("spec", "externalIPs"), ",")
	switch kind {
	case "LoadBalancer":
		var at []string
		for _, in := range o.list("status", "loadBalancer", "ingress") {
			if ip := in.str("ip"); ip != "" {
				at = append(at, ip)
			} else if host := in.str("hostname"); host != "" {
				at = append(at, host)
			}
		}
		at = append(at, o.strings("spec", "externalIPs")...)
		if external = strings.Join(at, ","); external == "" {
			external = "<pending>"
		}
	case "ExternalName":
		external = o.str("spec", "externalName")
	default:
		external = orNone(external)
	}
	var ports []string
	for _, p := range o.list("spec", "ports") {
		s := strconv.FormatInt(p.int("port"), 10)
		if p.int("nodePort") > 0 {
			s += ":" + strconv.FormatInt(p.int("nodePort"), 10)
		}
		ports = append(ports, s+"/"+p.str("protocol"))
	}
	return []any{o.name(), kind, clusterIP, external, orNone(strings.Join(ports, ",")), o.age(now), labelSet(o.at("spec", "selector"))}
}

// endpoints is formatEndpoints: address:port for the first three, and how many more.
func endpoints(o obj) string {
	subsets := o.list("subsets")
	if len(subsets) == 0 {
		return "<none>"
	}
	var shown []string
	count := 0
	add := func(s string) {
		if count++; len(shown) < 3 {
			shown = append(shown, s)
		}
	}
	for _, ss := range subsets {
		if len(ss.list("ports")) == 0 { // a headless service may have none
			for _, a := range ss.list("addresses") {
				add(a.str("ip"))
			}
			continue
		}
		for _, p := range ss.list("ports") {
			for _, a := range ss.list("addresses") {
				add(net.JoinHostPort(a.str("ip"), strconv.FormatInt(p.int("port"), 10)))
			}
		}
	}
	if count > 3 {
		return fmt.Sprintf("%s + %d more...", strings.Join(shown, ","), count-3)
	}
	return strings.Join(shown, ",")
}

// eventCells is printEvent for a core/v1 Event.
func eventCells(o obj, now time.Time) []any {
	first := since(o.time("firstTimestamp"), now)
	if o.time("firstTimestamp").IsZero() {
		first = since(o.time("eventTime"), now)
	}
	last := since(o.time("lastTimestamp"), now)
	if o.time("lastTimestamp").IsZero() {
		last = first
	}
	count := o.int("count")
	if o.has("series") {
		last, count = since(o.time("series", "lastObservedTime"), now), o.int("series", "count")
	} else if count == 0 {
		count = 1
	}
	target := strings.ToLower(o.str("involvedObject", "kind"))
	if name := o.str("involvedObject", "name"); name != "" {
		target += "/" + name
	}
	source, instance := o.str("source", "component"), o.str("source", "host")
	if source == "" {
		source = o.str("reportingComponent")
	}
	if instance == "" {
		instance = o.str("reportingInstance")
	}
	if instance != "" {
		source += ", " + instance
	}
	return []any{last, o.str("type"), o.str("reason"), target, o.str("involvedObject", "fieldPath"), source, strings.TrimSpace(o.str("message")), first, count, o.name()}
}
