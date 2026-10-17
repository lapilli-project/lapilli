package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/lapilli-project/lapilli/internal/diagnose"
	"github.com/lapilli-project/lapilli/internal/local"
	"github.com/lapilli-project/lapilli/internal/probe"
	"github.com/lapilli-project/lapilli/internal/render"
	"github.com/lapilli-project/lapilli/internal/sshlog"
	"github.com/lapilli-project/lapilli/internal/target"
)

const sshUsage = `lapilli ssh tells you why an SSH login fails, and what to do about it.

Usage:
  lapilli ssh [ssh options] [user@]host
  lapilli ssh git@github.com:owner/repo.git   a git remote works too
  lapilli ssh                                 inside a git repository: its origin
  lapilli ssh --log FILE                      read a saved "ssh -vvv" log ("-" for stdin)

It runs ssh once with BatchMode: it never prompts, runs no remote command,
forwards nothing and saves no host key.

ssh options passed through:
  -i FILE  -p PORT  -l USER  -F FILE  -o OPTION  -J JUMP  -4  -6

Flags:
  --json             print the report as JSON
  --show-log         print ssh's debug log after the report
  --trust-host-key   don't check the host key for this test (nothing is saved)
  --timeout DUR      give up after this long (default 30s)
`

type flags struct {
	json, showLog, trust bool
	logFile              string
	timeout              time.Duration
	rest                 []string
}

func parseFlags(args []string) (*flags, error) {
	f := &flags{timeout: 30 * time.Second}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := strings.Cut(a, "=")
		value := func() (string, error) {
			if hasVal {
				return val, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", name)
			}
			i++
			return args[i], nil
		}
		switch name {
		case "--json":
			f.json = true
		case "--show-log":
			f.showLog = true
		case "--trust-host-key":
			f.trust = true
		case "--log":
			v, err := value()
			if err != nil {
				return nil, err
			}
			f.logFile = v
		case "--timeout":
			v, err := value()
			if err != nil {
				return nil, err
			}
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				return nil, fmt.Errorf("bad --timeout %q", v)
			}
			f.timeout = d
		case "-h", "--help":
			return nil, errors.New(name)
		default:
			if strings.HasPrefix(a, "--") && a != "--" {
				return nil, fmt.Errorf("unknown flag %s", a)
			}
			f.rest = append(f.rest, a)
		}
	}
	return f, nil
}

// runSSH is "lapilli ssh".
func runSSH(args []string, stdout, stderr io.Writer) int {
	f, err := parseFlags(args)
	if err != nil {
		if err.Error() == "-h" || err.Error() == "--help" {
			fmt.Fprint(stdout, sshUsage)
			return 0
		}
		fmt.Fprintln(stderr, "lapilli ssh:", err)
		return 2
	}
	tg, err := target.Parse(f.rest)
	if err != nil {
		fmt.Fprintln(stderr, "lapilli ssh:", err)
		return 2
	}
	in := diagnose.Input{GOOS: runtime.GOOS, Target: tg, Timeout: f.timeout.String()}
	if f.logFile != "" {
		return fromLog(f, in, stdout, stderr)
	}

	if tg.Dest == "" {
		remote, err := originRemote()
		if err != nil {
			fmt.Fprint(stderr, sshUsage)
			return 2
		}
		if err := tg.SetDest(remote); err != nil {
			fmt.Fprintf(stderr, "lapilli ssh: origin is %s: %v\n", remote, err)
			return 2
		}
		fmt.Fprintf(stderr, "Checking origin (%s)\n\n", remote)
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		fmt.Fprintln(stderr, "lapilli ssh: ssh is not installed or not on PATH")
		return 2
	}
	in.Home, _ = os.UserHomeDir()
	if u, err := user.Current(); err == nil {
		in.LocalUser = u.Username
	}

	ctx := context.Background()
	cfg, err := local.ResolveConfig(ctx, tg.SSHArgs, tg.Dest, in.Home, in.LocalUser)
	if err != nil {
		// ssh -G reads the configuration the way ssh would; when it can't, that is the answer
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if trace := sshlog.Parse(bytes.NewReader(exit.Stderr)); trace.Stage() == sshlog.StageConfig {
				in.Trace, in.Log = trace, exit.Stderr
				rep := diagnose.Diagnose(in)
				if f.json {
					render.JSON(stdout, rep, trace)
				} else {
					render.Text(stdout, rep, useColor(stdout))
					if f.showLog {
						fmt.Fprintf(stdout, "\n--- ssh -G %s\n%s", strings.Join(append(tg.SSHArgs, tg.Dest), " "), exit.Stderr)
					}
				}
				return 1
			}
		}
		fmt.Fprintln(stderr, "lapilli ssh: ssh -G failed:", err)
		return 2
	}
	in.Config = cfg
	opts := probe.Options{
		SSHArgs:      tg.SSHArgs,
		Dest:         tg.Dest,
		Session:      diagnose.LookupGitHost(cfg.HostName) != nil,
		TrustHostKey: f.trust,
		Timeout:      f.timeout,
	}
	res, err := probe.Run(ctx, opts)
	if err != nil {
		fmt.Fprintln(stderr, "lapilli ssh: running ssh:", err)
		return 2
	}
	trace := sshlog.Parse(bytes.NewReader(res.Log))

	// The user's own config would accept a new host key without asking; do the
	// same for this test, without saving it, so the login itself gets checked.
	if !f.trust && trace.Stage() == sshlog.StageHostKey && !trace.HostKeyChanged && acceptsNewHosts(cfg.StrictHostKeyChecking) {
		opts.TrustHostKey = true
		if res2, err := probe.Run(ctx, opts); err == nil {
			res, trace = res2, sshlog.Parse(bytes.NewReader(res2.Log))
		}
	}
	in.TrustedHostKey = opts.TrustHostKey
	in.Trace, in.Log, in.TimedOut = trace, res.Log, res.TimedOut

	in.Files, in.Unused = inspectKeys(cfg, tg, trace, in.Home)
	in.Agent = local.ListAgent(ctx, cfg.IdentityAgent)

	rep := diagnose.Diagnose(in)
	if f.json {
		render.JSON(stdout, rep, trace)
	} else {
		render.Text(stdout, rep, useColor(stdout))
		if f.showLog {
			fmt.Fprintf(stdout, "\n--- ssh %s\n%s", strings.Join(res.Args, " "), res.Log)
		}
	}
	if rep.OK {
		return 0
	}
	return 1
}

func fromLog(f *flags, in diagnose.Input, stdout, stderr io.Writer) int {
	var data []byte
	var err error
	if f.logFile == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(f.logFile)
	}
	if err != nil {
		fmt.Fprintln(stderr, "lapilli ssh:", err)
		return 2
	}
	trace := sshlog.Parse(bytes.NewReader(data))
	if trace.Lines == 0 {
		fmt.Fprintln(stderr, "lapilli ssh: the log is empty")
		return 2
	}
	if in.Target != nil && in.Target.Dest == "" {
		in.Target = nil
	}
	in.Trace, in.Log = trace, data
	rep := diagnose.Diagnose(in)
	if f.json {
		render.JSON(stdout, rep, trace)
	} else {
		render.Text(stdout, rep, useColor(stdout))
	}
	if rep.OK {
		return 0
	}
	return 1
}

// inspectKeys looks at every key file ssh considered, and lists private keys in
// ~/.ssh that it didn't try.
func inspectKeys(cfg *local.SSHConfig, tg *target.Target, tr *sshlog.Trace, home string) (map[string]local.KeyFile, []local.KeyFile) {
	files := map[string]local.KeyFile{}
	var add func(string)
	add = func(p string) {
		if p == "" || !strings.Contains(p, "/") {
			return
		}
		if _, ok := files[p]; !ok {
			files[p] = local.InspectKey(p)
		}
		if strings.HasSuffix(p, ".pub") {
			add(strings.TrimSuffix(p, ".pub"))
		}
	}
	for _, p := range cfg.IdentityFiles {
		add(p)
	}
	for _, p := range tg.IdentityF {
		add(local.ExpandPath(p, home, "", "", ""))
	}
	for _, f := range tr.IdentityFiles {
		add(f.Path)
	}
	for _, a := range tr.Attempts {
		add(a.Key.Path)
	}

	seen := map[string]bool{}
	for p, k := range files {
		seen[p] = true
		if k.Fingerprint != "" {
			seen[k.Fingerprint] = true
		}
	}
	for _, k := range tr.Planned {
		seen[k.Fingerprint] = true
	}
	for _, a := range tr.Attempts {
		seen[a.Key.Fingerprint] = true
	}
	var unused []local.KeyFile
	for _, k := range local.ScanKeys(filepath.Join(home, ".ssh")) {
		if seen[k.Path] || (k.Fingerprint != "" && seen[k.Fingerprint]) {
			continue
		}
		unused = append(unused, k)
	}
	return files, unused
}

func originRemote() (string, error) {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func acceptsNewHosts(v string) bool {
	switch strings.ToLower(v) {
	case "no", "off", "false", "accept-new":
		return true
	}
	return false
}

func useColor(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
