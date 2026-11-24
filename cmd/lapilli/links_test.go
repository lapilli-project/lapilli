package main

import "testing"

// --linked-only is read into what judges, and the report says so.
func TestLinkedOnlyFlag(t *testing.T) {
	parallel(t)
	f, err := parseVerdictFlags([]string{"--linked-only", "--rules", "x.yaml", "--prometheus", "http://prometheus:9090", "--incidents", "i.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if !f.opts.LinkedOnly || !f.opts.Settings().LinkedOnly {
		t.Errorf("--linked-only: %+v", f.opts)
	}
	if f, _ := parseVerdictFlags([]string{"--rules", "x.yaml", "--prometheus", "http://prometheus:9090", "--incidents", "i.yaml"}); f.opts.LinkedOnly {
		t.Error("linked only without the flag")
	}
}
