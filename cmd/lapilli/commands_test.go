package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var writeDocs = flag.Bool("write-docs", false, "write docs/commands.md from the help")

// docs/commands.md is every command's help, as -h prints it: written from
// the help itself, so an option added to the help and not to the page
// fails here. go test ./cmd/lapilli -run TestCommandsDocumented -write-docs
// writes it again.
func TestCommandsDocumented(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "commands.md")
	want := commandsPage()
	if *writeDocs {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("docs/commands.md isn't the help as it is now: go test ./cmd/lapilli -run TestCommandsDocumented -write-docs")
	}
}

func commandsPage() string {
	var b strings.Builder
	b.WriteString(`# Commands

Each command's options, as ` + "`lapilli COMMAND -h`" + ` prints them. The pages linked
under each say what the options are for; this page is all of them in one
place, to look one up. These pages follow the main branch: ` + "`lapilli version`" + `
says which version you have, and its own ` + "`-h`" + ` is the one to go by.
`)
	for _, c := range []struct{ name, about, help string }{
		{"lapilli", "", usage},
		{"lapilli verdict", "[Use `lapilli verdict`](verdict.md), [How it judges](how-it-judges.md)", verdictUsage},
		{"lapilli serve", "[Keep judging](serve.md)", serveUsage},
		{"lapilli preflight", "[Before it is deployed](serve.md#before-it-is-deployed)", preflightUsage},
		{"lapilli ledger", "[The ledger in Postgres](serve.md#the-ledger-in-postgres)", ledgerUsage},
	} {
		b.WriteString("\n## `" + c.name + "`\n\n")
		if c.about != "" {
			b.WriteString("What it's for: " + c.about + ".\n\n")
		}
		b.WriteString("```text\n" + strings.TrimRight(c.help, "\n") + "\n```\n")
	}
	return b.String()
}
