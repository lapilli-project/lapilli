package main

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// chartDocs are the documents a rendering holds, by kind.
func chartDocs(t *testing.T, out string) map[string][]map[string]any {
	t.Helper()
	docs := map[string][]map[string]any{}
	dec := yaml.NewDecoder(strings.NewReader(out))
	for {
		var d map[string]any
		if err := dec.Decode(&d); err != nil {
			break
		}
		if k, ok := d["kind"].(string); ok {
			docs[k] = append(docs[k], d)
		}
	}
	return docs
}

// Several replicas share a ledger in Postgres alone: the chart refuses
// them on SQLite's, with a sentence that says why, rolls them one at a
// time, keeps a PodDisruptionBudget, and signs people in with one key.
// One replica is as it was: Recreate, and no budget.
func TestChartReplicas(t *testing.T) {
	pg := append(serveValues, "--set", "serve.ledger.postgres.secret.name=ledger-db")
	for _, c := range []struct {
		values []string
		says   string
	}{
		{append(serveValues, "--set", "serve.replicas=3"), "3 replicas share one ledger only in Postgres (serve.ledger.postgres.secret)"},
		{append(serveValues, "--set", "serve.replicas=2", "--set", "serve.ledger.persistence=false"), "share one ledger only in Postgres"},
		{append(serveValues, "--set", "serve.replicas=0"), "serve.replicas: 1 or more"},
		{append(pg, "--set", "serve.replicas=3", "--set", "serve.podDisruptionBudget.maxUnavailable=3"), "so that one replica at least stays"},
		{append(pg, "--set", "serve.replicas=3", "--set", "serve.podDisruptionBudget.maxUnavailable=100%"), "so that one replica at least stays"},
		{append(pg, "--set", "serve.replicas=2", "--set", "serve.signIn.oidc.issuer=https://idp.example.com", "--set", "serve.signIn.oidc.clientId=lapilli",
			"--set", "serve.externalURL=https://lapilli.example.com", "--set", "serve.roles.viewers={ana@example.com}",
			"--set", "serve.signIn.sessionKeySecret.create=false"), "replicas sign people in with one session key"},
	} {
		out, err := helmTemplate(t, c.values...)
		if err == nil || !strings.Contains(out, c.says) {
			t.Errorf("%v: %v, want refused saying %q\n%s", c.values[len(serveValues):], err, c.says, out)
		}
	}
	for _, one := range [][]string{serveValues, pg} {
		out, err := helmTemplate(t, one...)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		docs := chartDocs(t, out)
		d := docs["Deployment"][0]["spec"].(map[string]any)
		if d["replicas"] != 1 || d["strategy"].(map[string]any)["type"] != "Recreate" || len(docs["PodDisruptionBudget"]) != 0 {
			t.Errorf("one replica: %v %v, %d budgets", d["replicas"], d["strategy"], len(docs["PodDisruptionBudget"]))
		}
	}
	out, err := helmTemplate(t, append(pg, "--set", "serve.replicas=3", "--set", "serve.serviceMonitor.enabled=true")...)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	docs := chartDocs(t, out)
	d := docs["Deployment"][0]["spec"].(map[string]any)
	st := d["strategy"].(map[string]any)
	if d["replicas"] != 3 || st["type"] != "RollingUpdate" || st["rollingUpdate"].(map[string]any)["maxUnavailable"] != 0 {
		t.Errorf("three replicas: %v %v", d["replicas"], st)
	}
	if len(docs["PodDisruptionBudget"]) != 1 {
		t.Fatalf("budgets: %d", len(docs["PodDisruptionBudget"]))
	}
	pdb := docs["PodDisruptionBudget"][0]["spec"].(map[string]any)
	sel := pdb["selector"].(map[string]any)["matchLabels"].(map[string]any)
	pods := d["template"].(map[string]any)["metadata"].(map[string]any)["labels"].(map[string]any)
	for k, v := range sel {
		if pods[k] != v {
			t.Errorf("the budget's %s=%v isn't the pods' (%v)", k, v, pods[k])
		}
	}
	if pdb["maxUnavailable"] != 1 {
		t.Errorf("maxUnavailable: %v", pdb["maxUnavailable"])
	}
	// Each pod's /metrics, its own leader gauge in it: the ServiceMonitor
	// scrapes every endpoint of the Service, as before.
	if sm := docs["ServiceMonitor"]; len(sm) != 1 || !strings.Contains(out, "port: metrics") {
		t.Errorf("ServiceMonitor: %v", sm)
	}
	// No pod needs to be the one a person came to: sessions are signed
	// with the key every pod mounts.
	if strings.Contains(out, "sessionAffinity: ClientIP") {
		t.Error("the Service sticks people to a pod")
	}
}
