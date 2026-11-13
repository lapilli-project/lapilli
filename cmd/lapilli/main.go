// Command lapilli judges alert rules against what actually went wrong: which
// firings caught an incident, which were noise, and which incidents no alert
// caught.
package main

import (
	"fmt"
	"io"
	"os"
	"runtime/debug"
)

// version is set at release time with -ldflags "-X main.version=v0.4.0".
var version = "dev"

const usage = `lapilli judges your alert rules against what actually went wrong.

Each firing gets a stone, the way Roman juries voted: white when it caught an
incident, black when nothing was wrong. Count a rule's stones over a week and
you know whether it earns its page; an incident with no stone is one your
alerts missed.

Usage:
  lapilli verdict --rules FILE --prometheus URL --incidents FILE
                                     replay rules against history
  lapilli verdict --alerts --prometheus URL --incidents FILE
                                     judge what Prometheus fired
  lapilli serve --rules DIR --prometheus URL
                                     keep judging, every hour, into a
                                     ledger with a web UI
  lapilli ledger copy --ledger DIR --to-url-file FILE
                                     copy a ledger into Postgres
  lapilli version

Run "lapilli verdict -h", "lapilli serve -h" or "lapilli ledger -h" for the
options.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch cmd, rest := args[0], args[1:]; cmd {
	case "verdict":
		return runVerdict(rest, stdout, stderr)
	case "serve":
		return runServe(rest, stdout, stderr)
	case "ledger":
		return runLedger(rest, stdout, stderr)
	case "version", "--version":
		fmt.Fprintln(stdout, "lapilli", buildVersion())
		return 0
	case "help", "-h", "--help":
		switch {
		case len(rest) > 0 && rest[0] == "verdict":
			fmt.Fprint(stdout, verdictUsage)
		case len(rest) > 0 && rest[0] == "serve":
			fmt.Fprint(stdout, serveUsage)
		case len(rest) > 0 && rest[0] == "ledger":
			fmt.Fprint(stdout, ledgerUsage)
		default:
			fmt.Fprint(stdout, usage)
		}
		return 0
	default:
		fmt.Fprintf(stderr, "lapilli: unknown command %q\n", cmd)
		return 2
	}
}

// buildVersion is the release's version, or the module version when installed
// with "go install ...@vX.Y.Z".
func buildVersion() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return version
}
