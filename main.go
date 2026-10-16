// Command lapilli tells you why an SSH login fails: it runs ssh once, reads
// what happened, looks at your keys and agent, and says what to fix.
package main

import (
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
)

// version is set at release time with -ldflags "-X main.version=v0.3.0".
var version = "dev"

const usage = `lapilli tells you why an SSH login fails, and what to do about it.

Usage:
  lapilli ssh [ssh options] [user@]host   check a login
  lapilli ssh                             inside a git repository: check its origin
  lapilli ssh --log FILE                  explain a saved "ssh -vvv" log
  lapilli version

Run "lapilli ssh -h" for the options.
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
	case "ssh":
		return runSSH(rest, stdout, stderr)
	case "version", "--version":
		fmt.Fprintln(stdout, "lapilli", buildVersion())
		return 0
	case "help", "-h", "--help":
		if len(rest) > 0 && rest[0] == "ssh" {
			fmt.Fprint(stdout, sshUsage)
		} else {
			fmt.Fprint(stdout, usage)
		}
		return 0
	default:
		fmt.Fprintf(stderr, "lapilli: unknown command %q\n", cmd)
		if !strings.HasPrefix(cmd, "-") {
			fmt.Fprintf(stderr, "Did you mean: lapilli ssh %s\n", strings.Join(args, " "))
		}
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
