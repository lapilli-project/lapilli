// Package probe runs ssh once against the destination, in a mode that cannot
// change anything: no prompts, no remote command, no forwarding, nothing written
// to known_hosts, on the destination and on every jump host on the way. It
// returns ssh's debug log.
package probe

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
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
	// Jump is the ProxyJump in effect, as ssh -G prints it. The probe makes
	// the ProxyCommand ssh would make of it, with its own options on every hop.
	Jump string
	// Host is the destination's HostName. With a ProxyCommand of the user's,
	// only "Authenticated to Host" ends the probe early.
	Host    string
	Proxied bool // a ProxyCommand of the user's is in effect
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
	for _, opt := range known(o, options(o)) {
		args = append(args, "-o", opt)
	}
	userArgs := o.SSHArgs
	if hops := o.hops(); len(hops) > 0 {
		args = append(args, "-o", "ProxyCommand="+jumpCommand(o, hops))
		userArgs = withoutJ(userArgs) // ssh won't take -J beside a ProxyCommand
	}
	args = append(args, userArgs...)
	if o.Session {
		args = append(args, "-T")
	} else {
		args = append(args, "-N")
	}
	return append(args, "--", o.Dest)
}

// known leaves out the options this ssh doesn't know.
func known(o Options, opts []string) []string {
	var out []string
	for _, opt := range opts {
		name, _, _ := strings.Cut(opt, "=")
		if !slices.Contains(o.Unknown, strings.ToLower(name)) {
			out = append(out, opt)
		}
	}
	return out
}

func (o Options) hops() []string {
	if o.Jump == "" || strings.EqualFold(o.Jump, "none") {
		return nil
	}
	return strings.Split(o.Jump, ",")
}

// jumpCommand is the ProxyCommand ssh makes of ProxyJump (ssh -l USER -p PORT
// -W '[%h]:%p' HOST, with -F and -v), plus the probe's options, which ssh
// doesn't pass on: without BatchMode a jump host's ssh would ask for a
// password. Earlier hops become a ProxyCommand of its own rather than -J, so
// they get the options too.
func jumpCommand(o Options, hops []string) string {
	user, host, port := splitHop(hops[len(hops)-1])
	oj := o
	oj.TrustHostKey = false // that is for the destination only
	// the ssh that runs this expands %h and %p in it; every other % must
	// come through, the next hop's %h included
	arg := func(s string) string { return shellQuote(strings.ReplaceAll(s, "%", "%%")) }
	parts := []string{"ssh"}
	for _, opt := range known(o, options(oj)) {
		parts = append(parts, "-o", arg(opt))
	}
	parts = append(parts, "-vvv")
	if user != "" {
		parts = append(parts, "-l", arg(user))
	}
	if port != "" {
		parts = append(parts, "-p", arg(port))
	}
	if f := configFile(o.SSHArgs); f != "" {
		parts = append(parts, "-F", arg(f))
	}
	if len(hops) > 1 {
		parts = append(parts, "-o", arg("ProxyCommand="+jumpCommand(o, hops[:len(hops)-1])))
	}
	parts = append(parts, "-W", "'[%h]:%p'", "--", arg(host))
	return strings.Join(parts, " ")
}

// splitHop reads one ProxyJump hop: [ssh://][user@]host[:port], host maybe [v6].
func splitHop(s string) (user, host, port string) {
	s = strings.TrimPrefix(s, "ssh://")
	if i := strings.LastIndex(s, "@"); i >= 0 {
		user, s = s[:i], s[i+1:]
	}
	if strings.HasPrefix(s, "[") {
		if i := strings.Index(s, "]"); i > 0 {
			return user, s[1:i], strings.TrimPrefix(s[i+1:], ":")
		}
	}
	if strings.Count(s, ":") == 1 {
		host, port, _ = strings.Cut(s, ":")
		return user, host, port
	}
	return user, s, ""
}

func configFile(args []string) string {
	for i, a := range args {
		switch {
		case a == "-F" && i+1 < len(args):
			return args[i+1]
		case strings.HasPrefix(a, "-F") && len(a) > 2:
			return a[2:]
		}
	}
	return ""
}

func withoutJ(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-J":
			i++
		case strings.HasPrefix(args[i], "-J"):
		default:
			out = append(out, args[i])
		}
	}
	return out
}

func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789@%+=:,./_-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// options are the probe's own -o options.
func options(o Options) []string {
	opts := []string{
		"BatchMode=yes", // never prompt
		"ConnectTimeout=" + seconds(o.Timeout/2),
		"ControlMaster=no", "ControlPath=none", // a shared connection would skip authentication
		"UpdateHostKeys=no",
		"CheckHostIP=no",    // before 8.5 ssh would add the server's address to known_hosts
		"AddKeysToAgent=no", // a key the probe loads stays out of your agent
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

// newer lists the probe's options that an older ssh may not know, and that the
// probe can do without; nothing else is ever left out.
var newer = []string{"remotecommand"} // OpenSSH 7.6

var reBadOption = regexp.MustCompile(`^command-line: line \d+: Bad configuration option: (\S+)$`)

// rejectedOption is the option ssh refused before doing anything else. ssh
// checks command-line options first, so its complaint follows only the version
// line; text a server sends comes later and can't count.
func rejectedOption(log []byte) string {
	for i, line := range strings.SplitN(string(log), "\n", 3) {
		line = strings.TrimRight(line, "\r")
		switch m := reBadOption.FindStringSubmatch(line); {
		case m != nil:
			return strings.ToLower(m[1])
		case i == 0 && strings.HasPrefix(line, "OpenSSH_"):
			continue
		}
		return ""
	}
	return ""
}

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
		name := rejectedOption(res.Log)
		if name == "" || !slices.Contains(newer, name) || slices.Contains(o.Unknown, name) {
			return res, nil
		}
		o.Unknown = append(o.Unknown, name)
	}
}

// maxLog bounds the log kept from one run; ssh -vvv writes far less.
const maxLog = 8 << 20

// env is the environment for ssh, without a way to ask: no askpass program,
// even where a ProxyCommand runs an ssh without BatchMode.
func env() []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "SSH_ASKPASS") {
			out = append(out, kv)
		}
	}
	return append(out, "SSH_ASKPASS_REQUIRE=never")
}

func run(ctx context.Context, o Options) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()

	res := &Result{Args: Args(o)}
	cmd := exec.CommandContext(ctx, "ssh", res.Args...)
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = env()
	detach(cmd)
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
	// jump hosts log in first; only the destination's login ends the probe
	need, seen, reached := 1+len(o.hops()), 0, false
	// a disconnect message is the server's text and may hold lines of its own
	// making; ssh says nothing true about logging in after it
	disconnected := false
	// read in pieces, so no line, however long, is held whole; only a piece
	// that starts a line can say anything
	rd := bufio.NewReaderSize(pr, 64*1024)
	lineStart := true
	for {
		chunk, err := rd.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			err = nil
		}
		line := string(chunk)
		if buf.Len() < maxLog {
			buf.WriteString(line)
		}
		start := lineStart
		lineStart = strings.HasSuffix(line, "\n")
		if start && strings.HasPrefix(line, "Received disconnect from ") {
			disconnected = true
		}
		if start && strings.HasPrefix(line, "Authenticated to ") && !disconnected {
			seen++
			if f := strings.Fields(line); seen >= need && (!o.Proxied || o.Host == "" || len(f) > 2 && strings.EqualFold(f[2], o.Host)) {
				reached = true
			}
		}
		if reached && authAt.IsZero() {
			if !o.Session {
				cancel() // -N would hold the connection open; we have what we need
			} else {
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
	if ctx.Err() == context.DeadlineExceeded && !reached {
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
