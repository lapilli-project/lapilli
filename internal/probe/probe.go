// Package probe runs ssh once against the destination, in a mode that cannot
// change anything: no prompts, no remote command, no forwarding, nothing written
// to known_hosts. It returns ssh's debug log.
package probe

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Options for one probe.
type Options struct {
	SSHArgs []string // options passed through from the command line (-i, -p, -l, -F, -o, -J, -4, -6)
	Dest    string
	// Session asks for a session with no command (ssh -T), which git hosts answer
	// with a greeting naming the account. Without it the probe stops right after
	// authentication (ssh -N).
	Session bool
	// TrustHostKey skips host key checking for this one connection, without
	// saving the key anywhere.
	TrustHostKey bool
	Timeout      time.Duration
	// Unknown lists probe options (lowercase names) this ssh doesn't know, which
	// Args leaves out. Run fills it in when ssh rejects one.
	Unknown []string
}

// Result of a probe.
type Result struct {
	Log      []byte
	Args     []string
	ExitCode int
	TimedOut bool
}

// Args are the ssh arguments for a probe. Options given here come before the
// user's, and ssh keeps the first value it sees, so these win.
func Args(o Options) []string {
	args := []string{"-vvv"}
	for _, opt := range options(o) {
		name, _, _ := strings.Cut(opt, "=")
		if !slices.Contains(o.Unknown, strings.ToLower(name)) {
			args = append(args, "-o", opt)
		}
	}
	args = append(args, o.SSHArgs...)
	if o.Session {
		args = append(args, "-T")
	} else {
		args = append(args, "-N")
	}
	return append(args, "--", o.Dest)
}

// options are the probe's own -o options.
func options(o Options) []string {
	opts := []string{
		"BatchMode=yes", // never prompt
		"ConnectTimeout=" + seconds(o.Timeout/2),
		"ControlMaster=no", "ControlPath=none", // a shared connection would skip authentication
		"UpdateHostKeys=no",
		"CheckHostIP=no", // before 8.5 ssh would add the server's address to known_hosts
		"ClearAllForwardings=yes",
		"ForwardAgent=no", "ForwardX11=no",
		"PermitLocalCommand=no",
		"RemoteCommand=none", // 7.6+
		"RequestTTY=no",
	}
	if o.TrustHostKey {
		opts = append(opts, "StrictHostKeyChecking=no", "UserKnownHostsFile=/dev/null", "GlobalKnownHostsFile=/dev/null")
	} else {
		opts = append(opts, "StrictHostKeyChecking=yes")
	}
	return opts
}

var reBadOption = regexp.MustCompile(`(?m)^command-line: line \d+: Bad configuration option: (\S+)`)

// Run starts ssh and reads its output until authentication finishes, the
// connection fails, or the timeout passes. An ssh that doesn't know one of the
// probe's own options rejects it before connecting; Run then goes again
// without it. Leaving it out is safe: an option ssh doesn't know can't be set
// in the user's config either.
func Run(ctx context.Context, o Options) (*Result, error) {
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Second
	}
	for {
		res, err := run(ctx, o)
		if err != nil {
			return nil, err
		}
		m := reBadOption.FindSubmatch(res.Log)
		if m == nil {
			return res, nil
		}
		name := strings.ToLower(string(m[1]))
		if slices.Contains(o.Unknown, name) || !slices.ContainsFunc(options(o), func(opt string) bool {
			return strings.EqualFold(strings.SplitN(opt, "=", 2)[0], name)
		}) {
			return res, nil
		}
		o.Unknown = append(o.Unknown, name)
	}
}

func run(ctx context.Context, o Options) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()

	res := &Result{Args: Args(o)}
	cmd := exec.CommandContext(ctx, "ssh", res.Args...)
	cmd.WaitDelay = 2 * time.Second
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		pw.Close()
		done <- err
	}()

	var buf bytes.Buffer
	var authAt time.Time
	rd := bufio.NewReader(pr)
	for {
		line, err := rd.ReadString('\n')
		buf.WriteString(line)
		if strings.HasPrefix(line, "Authenticated to ") || strings.Contains(line, "Authentication succeeded (") {
			if !o.Session {
				cancel() // -N would hold the connection open; we have what we need
			} else if authAt.IsZero() {
				authAt = time.Now()
				// a git host answers within a moment; give it a few seconds
				go func() {
					select {
					case <-time.After(5 * time.Second):
						cancel()
					case <-ctx.Done():
					}
				}()
			}
		}
		if err != nil {
			break
		}
	}
	werr := <-done
	res.Log = buf.Bytes()
	var exit *exec.ExitError
	switch {
	case werr == nil:
	case errors.As(werr, &exit):
		res.ExitCode = exit.ExitCode()
	default:
		res.ExitCode = -1
	}
	if ctx.Err() == context.DeadlineExceeded && authAt.IsZero() && !bytes.Contains(res.Log, []byte("Authenticated to ")) {
		res.TimedOut = true
	}
	return res, nil
}

func seconds(d time.Duration) string {
	s := int(d.Seconds())
	if s < 3 {
		s = 3
	}
	return itoa(s)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
