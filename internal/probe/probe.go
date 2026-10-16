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
	args := []string{
		"-vvv",
		"-o", "BatchMode=yes", // never prompt
		"-o", "ConnectTimeout=" + seconds(o.Timeout/2),
		"-o", "ControlMaster=no", "-o", "ControlPath=none", // a shared connection would skip authentication
		"-o", "UpdateHostKeys=no",
		"-o", "ClearAllForwardings=yes",
		"-o", "ForwardAgent=no", "-o", "ForwardX11=no",
		"-o", "PermitLocalCommand=no",
		"-o", "RemoteCommand=none",
		"-o", "RequestTTY=no",
	}
	if o.TrustHostKey {
		args = append(args, "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "GlobalKnownHostsFile=/dev/null")
	} else {
		args = append(args, "-o", "StrictHostKeyChecking=yes")
	}
	args = append(args, o.SSHArgs...)
	if o.Session {
		args = append(args, "-T")
	} else {
		args = append(args, "-N")
	}
	return append(args, "--", o.Dest)
}

// Run starts ssh and reads its output until authentication finishes, the
// connection fails, or the timeout passes.
func Run(ctx context.Context, o Options) (*Result, error) {
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Second
	}
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
