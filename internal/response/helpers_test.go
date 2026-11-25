package response

import "lapilli.dev/internal/incident"

func alert(rule string, kv ...string) incident.Alert {
	a := incident.Alert{Rule: rule}
	for i := 0; i+1 < len(kv); i += 2 {
		if a.Labels == nil {
			a.Labels = map[string]string{}
		}
		a.Labels[kv[i]] = kv[i+1]
	}
	return a
}
