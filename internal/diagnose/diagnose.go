// Package diagnose turns an ssh trace and what lapilli found on the client into
// one answer: where the login stopped, why, and what to do about it.
package diagnose

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/lapilli-project/lapilli/internal/local"
	"github.com/lapilli-project/lapilli/internal/sshlog"
	"github.com/lapilli-project/lapilli/internal/target"
)

// Step is one thing to do. Cmd is a command or a config snippet, shown verbatim.
type Step struct {
	Text string `json:"text"`
	Cmd  string `json:"cmd,omitempty"`
}

// Note is a finding that is not the main answer.
type Note struct {
	Level string `json:"level"` // "warn" or "info"
	Text  string `json:"text"`
	Cmd   string `json:"cmd,omitempty"`
}

// Fact is one line of what happened.
type Fact struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// KeyRow is one key in the order ssh tried it.
type KeyRow struct {
	Name        string `json:"name"`
	Type        string `json:"type,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Source      string `json:"source"`
	Result      string `json:"result"`
}

// Report is the answer.
type Report struct {
	OK       bool     `json:"ok"`
	Stage    string   `json:"stage"`
	Code     string   `json:"code"`
	Headline string   `json:"headline"`
	Facts    []Fact   `json:"facts,omitempty"`
	Keys     []KeyRow `json:"keys,omitempty"`
	Why      []string `json:"why,omitempty"`
	Fix      []Step   `json:"fix,omitempty"`
	Also     []Note   `json:"also,omitempty"`
	Account  string   `json:"account,omitempty"`
	// Hop is the jump host the answer is about, when ssh stopped on the way:
	// "alice@bastion:22".
	Hop string `json:"hop,omitempty"`
}

// Input is everything lapilli knows. Config, Files, Unused and Agent are nil when
// diagnosing a saved log, which may come from another machine.
type Input struct {
	Trace          *sshlog.Trace
	Log            []byte
	Config         *local.SSHConfig
	Files          map[string]local.KeyFile // identity files and offered key files, by path
	Unused         []local.KeyFile          // private keys in ~/.ssh that ssh did not try
	Agent          *local.Agent
	Target         *target.Target
	Home           string
	LocalUser      string
	GOOS           string
	TrustedHostKey bool
	TimedOut       bool
	Timeout        string
}

type diag struct {
	Input
	t       *sshlog.Trace
	r       *Report
	git     *GitHost
	covered map[string]bool // key paths the main answer already talks about
}

// Diagnose produces the report.
func Diagnose(in Input) *Report {
	d := &diag{Input: in, t: in.Trace, r: &Report{Stage: in.Trace.Stage()}, covered: map[string]bool{}}
	if j := in.Trace.FailedJump(); j != nil && d.r.Stage == sshlog.StageJump {
		return d.failedJump(j)
	}
	d.git = LookupGitHost(d.hostName())
	d.facts()
	d.r.Keys = d.keyRows()
	switch d.r.Stage {
	case sshlog.StageDone:
		d.success()
	case sshlog.StageConfig:
		if d.t.ConfigPerms != "" {
			d.configPerms()
		} else {
			d.configError()
		}
	case sshlog.StageResolve:
		d.resolve()
	case sshlog.StageConnect:
		d.connect()
	case sshlog.StageBanner:
		d.banner()
	case sshlog.StageKex:
		d.negotiation()
	case sshlog.StageHostKey:
		d.hostKey()
	case sshlog.StageAuth:
		d.auth()
	case sshlog.StageJump:
		d.forwardFailed()
	default:
		d.unknown()
	}
	d.stoppedAtFirstJump()
	d.localNotes()
	return d.r
}

// ---- names ----------------------------------------------------------------

// host is the name to show for the server.
func (d *diag) host() string {
	if d.git != nil {
		return d.git.Name
	}
	return d.hostName()
}

// hostName is the real host name ssh connected to (after HostName).
func (d *diag) hostName() string {
	switch {
	case d.t.AuthHost != "":
		return d.t.AuthHost
	case d.t.Proxy && d.t.ConnectHost != "":
		return d.t.ConnectHost // through a proxy, ssh stopped at the first jump host
	case d.Config != nil && d.Config.HostName != "":
		return d.Config.HostName
	case d.t.ConnectHost != "":
		return d.t.ConnectHost
	case d.Target != nil:
		return d.Target.Host
	}
	return "the server"
}

// alias is the host as the user typed it, for "Host" lines in config snippets.
func (d *diag) alias() string {
	if d.Target != nil && d.Target.Host != "" {
		return d.Target.Host
	}
	return d.hostName()
}

func (d *diag) user() string {
	if d.t.User != "" {
		return d.t.User
	}
	if d.Config != nil {
		return d.Config.User
	}
	return ""
}

func (d *diag) port() int {
	switch {
	case d.t.AuthPort != 0:
		return d.t.AuthPort
	case d.t.ConnectPort != 0:
		return d.t.ConnectPort
	case d.Config != nil && d.Config.Port != 0:
		return d.Config.Port
	}
	return 22
}

// userDefaulted reports whether ssh fell back to the local user name.
func (d *diag) userDefaulted() bool {
	return d.Target != nil && d.Target.User == "" && d.LocalUser != "" && d.user() == d.LocalUser
}

func (d *diag) show(p string) string {
	if d.Home != "" && (p == d.Home || strings.HasPrefix(p, d.Home+"/")) {
		return "~" + p[len(d.Home):]
	}
	return p
}

// rerun is the command to check again, with the user's own options.
func (d *diag) rerun(extra ...string) string {
	if d.Target == nil || d.Target.Dest == "" {
		return ""
	}
	parts := []string{"lapilli", "ssh"}
	parts = append(parts, extra...)
	for _, a := range d.Target.SSHArgs {
		parts = append(parts, shellQuote(d.show(a)))
	}
	return strings.Join(append(parts, d.Target.Dest), " ")
}

// copyID is the ssh-copy-id command that installs pub, with the options of the
// check that ssh-copy-id takes (-p, -F, -o; -J as ProxyJump).
func (d *diag) copyID(pub, user string) string {
	parts := []string{"ssh-copy-id", "-i", pub}
	if d.Target != nil {
		a := d.Target.SSHArgs
		for i := 0; i+1 < len(a); i++ {
			switch a[i] {
			case "-p", "-F", "-o":
				parts = append(parts, a[i], shellQuote(d.show(a[i+1])))
				i++
			case "-J":
				parts = append(parts, "-o", shellQuote("ProxyJump="+a[i+1]))
				i++
			case "-i", "-l":
				i++
			}
		}
	} else if p := d.port(); p != 22 {
		parts = append(parts, "-p", strconv.Itoa(p))
	}
	return strings.Join(append(parts, user+"@"+d.alias()), " ")
}

// rerunAs is rerun with a different user.
func (d *diag) rerunAs(user string, extra ...string) string {
	if d.Target == nil {
		return ""
	}
	parts := []string{"lapilli", "ssh"}
	parts = append(parts, extra...)
	for i := 0; i < len(d.Target.SSHArgs); i++ {
		if d.Target.SSHArgs[i] == "-l" {
			i++
			continue
		}
		parts = append(parts, shellQuote(d.show(d.Target.SSHArgs[i])))
	}
	return strings.Join(append(parts, user+"@"+d.Target.Host), " ")
}

func (d *diag) file(path string) (local.KeyFile, bool) {
	if d.Files == nil {
		return local.KeyFile{}, false
	}
	k, ok := d.Files[path]
	return k, ok && k.Exists
}

func (d *diag) keyName(k sshlog.Key) string {
	if k.Path == "" {
		return "a key from your agent"
	}
	if k.Agent && !strings.Contains(k.Path, "/") {
		return fmt.Sprintf("%q from your agent", k.Path)
	}
	return d.show(k.Path)
}

// ---- facts and key table ----------------------------------------------------

func (d *diag) facts() {
	t := d.t
	if t.Connected {
		v := d.hostName()
		if t.ConnectAddr != "" && t.ConnectAddr != v && !t.Proxy { // through a proxy it is the jump host's
			v += " (" + t.ConnectAddr + ")"
		}
		v += " port " + strconv.Itoa(d.port())
		if t.ServerSoftware != "" {
			v += ", " + t.ServerSoftware
		}
		d.r.Facts = append(d.r.Facts, Fact{"Server", v})
	}
	switch {
	case d.TrustedHostKey && t.HostKeyFP != "":
		d.r.Facts = append(d.r.Facts, Fact{"Host key", t.HostKeyFP + " (not checked)"})
	case t.HostKnown:
		v := t.HostKeyFP
		if t.HostKeyFoundIn != "" {
			v += ", matches " + d.show(t.HostKeyFoundIn)
		}
		d.r.Facts = append(d.r.Facts, Fact{"Host key", v})
	}
	if u := d.user(); u != "" && (t.Connected || d.r.Stage == sshlog.StageAuth) {
		v := u
		if d.userDefaulted() {
			v += " (your local user name; none was given)"
		}
		d.r.Facts = append(d.r.Facts, Fact{"User", v})
	}
	if len(t.Jumps) > 0 {
		var via []string
		for _, j := range t.Jumps {
			via = append(via, jumpName(j))
		}
		d.r.Facts = append(d.r.Facts, Fact{"Via", strings.Join(via, ", ") + " (jump host)"})
	}
	if t.AgentKeys >= 0 {
		d.r.Facts = append(d.r.Facts, Fact{"Agent", plural(t.AgentKeys, "key")})
	} else if d.Agent != nil && d.r.Stage == sshlog.StageAuth {
		switch {
		case d.Agent.Disabled:
			d.r.Facts = append(d.r.Facts, Fact{"Agent", "disabled (IdentityAgent none)"})
		case !d.Agent.Running:
			d.r.Facts = append(d.r.Facts, Fact{"Agent", "not running"})
		}
	}
}

func (d *diag) keyRows() []KeyRow {
	var rows []KeyRow
	tried := map[string]bool{}
	for _, a := range d.t.Attempts {
		tried[keyID(a.Key)] = true
		if a.Outcome == sshlog.Missing && !a.Key.Explicit {
			continue // ssh's default file names; most of them never exist
		}
		rows = append(rows, d.row(a.Key, result(a, d.t.TooMany)))
	}
	if d.t.TooMany {
		for _, k := range d.t.Planned {
			if !tried[keyID(k)] {
				rows = append(rows, d.row(k, "not reached"))
			}
		}
	}
	return rows
}

func (d *diag) row(k sshlog.Key, res string) KeyRow {
	row := KeyRow{Name: d.show(k.Path), Type: k.Type, Fingerprint: k.Fingerprint, Result: res, Source: "file"}
	if k.Agent {
		row.Source = "agent"
	}
	if f, ok := d.file(k.Path); ok {
		if row.Type == "" {
			row.Type = f.Type
		}
		if row.Fingerprint == "" {
			row.Fingerprint = f.Fingerprint
		}
		if f.Type == "RSA" && f.Bits > 0 {
			row.Type = "RSA-" + strconv.Itoa(f.Bits)
		}
	}
	return row
}

func keyID(k sshlog.Key) string {
	if k.Fingerprint != "" {
		return k.Fingerprint
	}
	return k.Path
}

func result(a sshlog.Attempt, tooMany bool) string {
	switch a.Outcome {
	case sshlog.Rejected:
		return "rejected"
	case sshlog.Unanswered:
		if tooMany {
			return "cut off"
		}
		return "no answer"
	case sshlog.Accepted:
		return "accepted, not used"
	case sshlog.Used:
		return "accepted"
	case sshlog.SignFailed:
		return "accepted, signing failed"
	case sshlog.LoadFailed:
		if a.ServerAccepted {
			return "accepted, can't load: " + a.Reason
		}
		return "can't load: " + a.Reason
	case sshlog.Missing:
		return "no such file"
	case sshlog.Skipped:
		return "not offered: " + shortReason(a.Reason)
	}
	return string(a.Outcome)
}

func shortReason(r string) string {
	switch {
	case strings.Contains(r, "not supported by server"), strings.Contains(r, "no mutual signature"):
		return "server can't verify this key type"
	case strings.Contains(r, "PubkeyAccepted"):
		return "disabled in your ssh config"
	}
	return r
}

// ---- success -----------------------------------------------------------------

func (d *diag) success() {
	r := d.r
	r.OK = true
	r.Code = "ok"
	r.Account = d.t.Account
	used := -1
	for i, a := range d.t.Attempts {
		if a.Outcome == sshlog.Used {
			used = i
		}
	}
	switch {
	case used >= 0 && d.git != nil && d.t.Account != "":
		r.Headline = fmt.Sprintf("%s accepts %s: you are %s on %s.", d.hostName(), d.keyName(d.t.Attempts[used].Key), d.t.Account, d.git.Name)
	case used >= 0:
		r.Headline = fmt.Sprintf("%s accepts %s for %s.", d.hostName(), d.keyName(d.t.Attempts[used].Key), d.user())
	default:
		r.Headline = fmt.Sprintf("Logged in to %s as %s (%s).", d.hostName(), d.user(), d.t.AuthMethod)
	}
	if used >= 0 {
		d.covered[d.t.Attempts[used].Key.Path] = true
	}
	// keys the server turned down first; missing or unloadable files cost no try
	before := 0
	for _, a := range d.t.Attempts[:max(used, 0)] {
		if a.Outcome == sshlog.Rejected || a.Outcome == sshlog.SignFailed {
			before++
		}
	}
	if before >= 2 {
		k := d.t.Attempts[used].Key
		note := Note{Level: "warn", Text: fmt.Sprintf("ssh offered %d other keys first. A server that allows fewer tries (MaxAuthTries) would disconnect you before reaching this one. Pin it in ~/.ssh/config:", before)}
		if !k.Agent || strings.Contains(k.Path, "/") {
			note.Cmd = d.pinSnippet(d.show(strings.TrimSuffix(k.Path, ".pub")))
		}
		r.Also = append(r.Also, note)
	}
	if d.git != nil && d.t.Account != "" {
		r.Also = append(r.Also, Note{Level: "info", Text: fmt.Sprintf("git over SSH acts as %s. If git says a repository is not found or access is denied, that account lacks access to it; the repository may belong to another account you have.", d.t.Account)})
	}
}

// ---- before authentication ----------------------------------------------------

func (d *diag) configPerms() {
	p := d.show(d.t.ConfigPerms)
	d.r.Code = "config-permissions"
	d.r.Headline = fmt.Sprintf("ssh refuses to read %s: other users could change it.", p)
	d.r.Why = []string{"ssh stops before connecting when its configuration file is writable by anyone but you, or owned by someone else."}
	d.r.Fix = []Step{{Text: "Make it yours and private:", Cmd: fmt.Sprintf("chown %s %s && chmod 600 %s", orUser(d.LocalUser), p, p)}}
	d.again()
}

// Options that exist only in some builds of ssh, by the lowercase name ssh
// prints, with where they come from.
var foreignOptions = map[string][2]string{
	"usekeychain": {"UseKeychain", "the ssh that comes with macOS"},
}

var reUnknownOption = regexp.MustCompile(`^Bad configuration option: (\S+)$`)

func (d *diag) configError() {
	t := d.t
	where := fmt.Sprintf("line %d of %s", t.ConfigLine, d.show(t.ConfigFile))
	if t.ConfigFile == "command-line" {
		where = "an -o option on the command line"
	}
	m := reUnknownOption.FindStringSubmatch(t.ConfigError)
	if m == nil {
		d.r.Code = "config-error"
		d.r.Headline = fmt.Sprintf("ssh stops at %s: %s.", where, t.ConfigError)
		d.r.Why = []string{"ssh reads its whole configuration before connecting, and gives up on a line it can't use."}
		d.r.Fix = []Step{{Text: "Correct or remove that line."}}
		d.again()
		return
	}
	name, from := m[1], ""
	if f, ok := foreignOptions[name]; ok {
		name, from = f[0], f[1]
	}
	d.r.Code = "config-unknown-option"
	d.r.Headline = fmt.Sprintf("ssh stops at %s: this ssh has no option %s.", where, name)
	switch {
	case from != "":
		d.r.Why = []string{fmt.Sprintf("%s exists only in %s. The configuration was probably written on, or copied from, a machine that has it.", name, from)}
	case t.ClientVersion != "":
		d.r.Why = []string{fmt.Sprintf("Either the name is misspelled, or the option comes from a newer OpenSSH than this one (%s).", t.ClientVersion)}
	default:
		d.r.Why = []string{"Either the name is misspelled, or the option comes from a newer OpenSSH than this one."}
	}
	if t.ConfigFile == "command-line" {
		d.r.Fix = []Step{{Text: fmt.Sprintf("Leave out -o %s, or check its spelling.", name)}}
		return
	}
	skip := fmt.Sprintf("If the name is right, let this ssh skip it: add this line to %s, above line %d:", d.show(t.ConfigFile), t.ConfigLine)
	if from != "" {
		skip = fmt.Sprintf("Let this ssh skip it, so the same file still works where %s is known: add this line to %s, above line %d:", name, d.show(t.ConfigFile), t.ConfigLine)
	}
	d.r.Fix = []Step{{Text: skip, Cmd: "IgnoreUnknown " + name}}
	d.again()
}

// ---- through jump hosts ----------------------------------------------------

func jumpName(j *sshlog.Trace) string {
	n := j.AuthHost + ":" + strconv.Itoa(j.AuthPort)
	if j.User != "" {
		n = j.User + "@" + n
	}
	return n
}

// jumpSSH is the ssh command line for a jump host, followed by args.
func jumpSSH(j *sshlog.Trace, args ...string) string {
	parts := []string{"ssh"}
	if j.AuthPort != 0 && j.AuthPort != 22 {
		parts = append(parts, "-p", strconv.Itoa(j.AuthPort))
	}
	dest := j.AuthHost
	if j.User != "" {
		dest = j.User + "@" + dest
	}
	parts = append(parts, dest)
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// destination is the host ssh was trying to reach through the jump hosts.
func (d *diag) destination() (string, int) {
	if h, p, ok := strings.Cut(d.t.JumpTarget, ":"); ok {
		n, _ := strconv.Atoi(p)
		return h, n
	}
	return d.t.AuthHost, d.t.AuthPort
}

// failedJump answers for the jump host ssh didn't get through, as if it had
// been checked on its own.
func (d *diag) failedJump(j *sshlog.Trace) *Report {
	args := []string{}
	if j.AuthPort != 0 && j.AuthPort != 22 {
		args = append(args, "-p", strconv.Itoa(j.AuthPort))
	}
	dest := j.AuthHost
	if j.User != "" {
		dest = j.User + "@" + dest
	}
	in := d.Input
	in.Trace, in.Config = j, nil
	in.Target, _ = target.Parse(append(args, dest))
	r := Diagnose(in)
	host, _ := d.destination()
	r.Stage = sshlog.StageJump
	r.Hop = jumpName(j)
	r.Headline = fmt.Sprintf("On the way to %s, %s", host, lowerFirst(r.Headline, j.AuthHost))
	r.Why = append([]string{fmt.Sprintf("ssh reaches %s through the jump host %s, and stopped there. Everything below is about %s.", host, j.AuthHost, j.AuthHost)}, r.Why...)
	return r
}

// forwardFailed: the jump host let you in, then couldn't connect to the next host.
func (d *diag) forwardFailed() {
	t := d.t
	host, port := d.destination()
	j := &sshlog.Trace{AuthHost: "the jump host"}
	if n := len(t.Jumps); n > 0 {
		j = t.Jumps[n-1]
	}
	d.r.Hop = jumpName(j)
	reason := strings.TrimPrefix(t.ForwardError, "connect failed: ")
	check := jumpSSH(j, "nc", "-vz", host, strconv.Itoa(port))
	d.r.Code = "jump-cannot-reach"
	d.r.Headline = fmt.Sprintf("%s let you in, but couldn't connect on to %s port %d: %s.", j.AuthHost, host, port, lowerFirst(reason, ""))
	switch r := strings.ToLower(reason); {
	case strings.Contains(r, "administratively prohibited"):
		d.r.Code = "jump-forwarding-refused"
		d.r.Headline = fmt.Sprintf("%s let you in, but won't forward a connection to %s port %d.", j.AuthHost, host, port)
		d.r.Why = []string{fmt.Sprintf("The sshd on %s forbids it: AllowTcpForwarding no, DisableForwarding yes, or a PermitOpen list without %s:%d. Only its administrator can change that.", j.AuthHost, host, port)}
		d.step(fmt.Sprintf("Ask whoever runs %s to allow forwarding to %s:%d, or which jump host is meant for it.", j.AuthHost, host, port), "")
		return
	case strings.Contains(r, "refused"):
		d.r.Why = []string{fmt.Sprintf("Nothing on %s accepts connections on port %d, as %s sees it: sshd listens on another port there, or isn't running.", host, port, j.AuthHost)}
	case strings.Contains(r, "not known"), strings.Contains(r, "resolution"), strings.Contains(r, "nodename"):
		d.r.Why = []string{fmt.Sprintf("%s can't find the address of %s. The name is looked up on the jump host, not on your machine; there it may need a different name, or an IP address in HostName.", j.AuthHost, host)}
	case strings.Contains(r, "timed out"), strings.Contains(r, "no route"), strings.Contains(r, "unreachable"):
		d.r.Why = []string{fmt.Sprintf("%s got no answer from %s:%d: a firewall between them, or the wrong address.", j.AuthHost, host, port)}
	default:
		d.r.Why = []string{fmt.Sprintf("%s logged you in, then failed to open a connection to %s:%d for ssh to go through.", j.AuthHost, host, port)}
	}
	d.step(fmt.Sprintf("Look from %s (it needs nc there):", j.AuthHost), check)
	d.again()
}

// stoppedAtFirstJump says so when ssh failed on the way to the first jump host:
// it couldn't find it, reach it, or start a handshake with it.
func (d *diag) stoppedAtFirstJump() {
	t := d.t
	host, _ := d.destination()
	if !t.Proxy || t.JumpTarget == "" || t.Connected || len(t.Jumps) > 0 || d.r.Hop != "" || host == "" || t.ConnectHost == "" || t.ConnectHost == host {
		return
	}
	switch d.r.Stage {
	case sshlog.StageResolve, sshlog.StageConnect, sshlog.StageBanner:
	default:
		return
	}
	d.r.Hop = t.ConnectHost
	d.r.Headline = fmt.Sprintf("On the way to %s, %s", host, lowerFirst(d.r.Headline, t.ConnectHost))
	d.r.Why = append([]string{fmt.Sprintf("ssh reaches %s through the jump host %s, and didn't get past it.", host, t.ConnectHost)}, d.r.Why...)
}

// lowerFirst lowercases the first letter, to go after a comma, unless the
// sentence starts with a name.
func lowerFirst(s, name string) string {
	if s == "" || (name != "" && strings.HasPrefix(s, name)) || strings.HasPrefix(s, "ssh") {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func (d *diag) resolve() {
	h := d.t.ConnectHost
	d.r.Code = "dns"
	d.r.Headline = fmt.Sprintf("ssh couldn't find the address of %s.", h)
	if d.Target != nil && d.Target.Host != "" && d.Target.Host != h {
		d.r.Why = append(d.r.Why, fmt.Sprintf("%s is the HostName your ssh config sets for %s.", h, d.Target.Host))
	} else {
		d.r.Why = append(d.r.Why, fmt.Sprintf("If %s is meant to be a short name, it needs a Host entry in ~/.ssh/config; otherwise check the spelling.", h))
	}
	d.r.Why = append(d.r.Why, "Names inside a company network often resolve only on its VPN.")
	d.r.Fix = []Step{{Text: "See what your system resolves:", Cmd: lookupCmd(d.GOOS, h)}}
}

func (d *diag) connect() {
	t := d.t
	h, p := t.ConnectHost, d.port()
	e := strings.ToLower(t.ConnectError)
	switch {
	case strings.Contains(e, "refused"):
		d.r.Code = "connection-refused"
		d.r.Headline = fmt.Sprintf("Nothing is accepting connections on %s port %d.", h, p)
		d.r.Why = []string{
			"The machine answered, but no SSH server is listening on that port, or a firewall is rejecting it.",
		}
		if p == 22 {
			d.r.Why = append(d.r.Why, "If the server uses another port, give it with -p.")
		} else {
			d.r.Why = append(d.r.Why, fmt.Sprintf("Port %d comes from your command or ssh config; SSH servers usually listen on 22.", p))
		}
		d.r.Fix = []Step{{Text: "On the server, check that sshd is running and which port it uses:", Cmd: "sudo systemctl status ssh sshd\nsudo ss -ltnp | grep ssh"}}
	case strings.Contains(e, "timed out"):
		d.r.Code = "connection-timeout"
		d.r.Headline = fmt.Sprintf("%s didn't answer on port %d.", h, p)
		d.r.Why = []string{"The connection got no reply at all: a firewall dropping it, a network that needs a VPN, a wrong address, or a machine that is off."}
		if d.git != nil && d.git.AltHost != "" && p == 22 {
			d.r.Why = append(d.r.Why, fmt.Sprintf("Some networks block port 22. %s also accepts SSH on %s port %d.", d.git.Name, d.git.AltHost, d.git.AltPort))
			d.r.Fix = []Step{
				{Text: "Try the other port:", Cmd: fmt.Sprintf("lapilli ssh -p %d git@%s", d.git.AltPort, d.git.AltHost)},
				{Text: "If that works, make it permanent in ~/.ssh/config:", Cmd: fmt.Sprintf("Host %s\n  HostName %s\n  Port %d\n  User git", d.hostName(), d.git.AltHost, d.git.AltPort)},
			}
			return
		}
		d.r.Fix = []Step{{Text: "Check whether anything answers on that port:", Cmd: fmt.Sprintf("nc -vz -w 5 %s %d", h, p)}}
	case strings.Contains(e, "no route"), strings.Contains(e, "unreachable"):
		d.r.Code = "no-route"
		d.r.Headline = fmt.Sprintf("Your machine has no network path to %s.", h)
		d.r.Why = []string{"The local network or VPN has no route to that address."}
	default:
		d.r.Code = "connect-failed"
		d.r.Headline = fmt.Sprintf("Couldn't connect to %s port %d: %s.", h, p, t.ConnectError)
	}
}

func (d *diag) banner() {
	t := d.t
	h := d.hostName()
	switch {
	case t.Penalized:
		d.r.Code = "penalized"
		d.r.Headline = fmt.Sprintf("%s is refusing connections from your address for now.", h)
		d.r.Why = []string{
			"OpenSSH 9.8 and later penalise an address after failed or abandoned logins (PerSourcePenalties). The block lifts by itself, after seconds to minutes; every new failure extends it.",
			"Something on your side may be retrying in a loop: a script, a CI job, an editor, or a git client.",
		}
		d.step("Wait a minute, then check again:", d.rerun())
		d.step("If you run the server, see the penalties and exempt trusted addresses (sshd_config):", "sudo journalctl -u ssh -u sshd --since '15 min ago' | grep -i penalt\nPerSourcePenaltyExemptList 192.0.2.0/24")
	case t.NotSSH:
		d.r.Code = "not-ssh"
		d.r.Headline = fmt.Sprintf("Something other than an SSH server answered on %s port %d.", h, d.port())
		d.r.Why = []string{"Probably the wrong port: a web server, a proxy or a load balancer is listening there."}
	default:
		d.r.Code = "closed-before-handshake"
		d.r.Headline = fmt.Sprintf("%s closed the connection before the SSH handshake.", h)
		d.r.Why = []string{
			"The server accepted the connection and hung up straight away. Usual causes: too many logins in progress (MaxStartups), a blocker such as fail2ban or DenyHosts that has listed your address, hosts.deny, or a proxy with no working server behind it.",
		}
		d.r.Fix = []Step{{Text: "The server's log says which:", Cmd: "sudo journalctl -u ssh -u sshd --since '15 min ago'"}}
	}
}

func (d *diag) negotiation() {
	t := d.t
	what, offer := t.NegotiationWhat, t.TheirOffer
	first, _, _ := strings.Cut(offer, ",")
	d.r.Code = "no-common-algorithm"
	d.r.Headline = fmt.Sprintf("%s and your ssh have no %s in common: the server only offers %s.", d.hostName(), what, offer)
	d.r.Why = []string{fmt.Sprintf("Your OpenSSH %s has turned these off as insecure. The server's SSH is old.", t.ClientVersion)}
	if strings.Contains(offer, "ssh-dss") && !strings.Contains(offer, ",") {
		d.r.Why = append(d.r.Why, "DSA was removed from OpenSSH 10.0; no setting brings it back. The server needs an update.")
		return
	}
	option := map[string]string{
		"host key type":       "HostKeyAlgorithms",
		"key exchange method": "KexAlgorithms",
		"cipher":              "Ciphers",
		"MAC":                 "MACs",
	}[what]
	d.r.Fix = []Step{{Text: "Best: update SSH on the server."}}
	if option != "" {
		snippet := fmt.Sprintf("Host %s\n  %s +%s", d.alias(), option, first)
		if option == "HostKeyAlgorithms" && strings.Contains(first, "ssh-rsa") {
			snippet += "\n  PubkeyAcceptedAlgorithms +ssh-rsa"
		}
		d.r.Fix = append(d.r.Fix, Step{Text: "To connect anyway, allow it for this host only. In ~/.ssh/config:", Cmd: snippet})
	}
	d.again()
}

func (d *diag) hostKey() {
	t := d.t
	h, p := d.hostName(), d.port()
	spec := h
	if p != 22 {
		spec = fmt.Sprintf("[%s]:%d", h, p)
	}
	published := d.git != nil && d.git.Publishes(t.HostKeyFP)
	switch {
	case t.HostKeyChanged:
		d.r.Code = "host-key-changed"
		d.r.Headline = fmt.Sprintf("%s's host key is not the one you saved.", h)
		file, line := splitFileLine(t.OffendingKey)
		d.r.Why = []string{fmt.Sprintf("You saved a different key for %s in %s (line %s). The server now presents %s %s.", spec, d.show(file), line, t.HostKeyType, t.HostKeyFP)}
		if published {
			d.r.Why = append(d.r.Why, fmt.Sprintf("The new key is one %s publishes, so the saved one is out of date (GitHub replaced its RSA key in March 2023, for example).", d.git.Name))
		} else {
			d.r.Why = append(d.r.Why, "This happens when a server is rebuilt or its keys are replaced, and also when someone is intercepting the connection. Confirm the new fingerprint with whoever runs the server before you trust it.")
		}
		rm := "ssh-keygen -R " + shellQuote(spec)
		if file != "" && !isDefaultKnownHosts(file, d.Home) {
			rm += " -f " + d.show(file)
		}
		d.r.Fix = []Step{{Text: "Remove the old key:", Cmd: rm}, {Text: "Then connect once with ssh and accept the new key after checking the fingerprint."}}
	default:
		d.r.Code = "host-key-unknown"
		d.r.Headline = fmt.Sprintf("You haven't connected to %s before, so its host key can't be checked.", spec)
		d.r.Why = []string{fmt.Sprintf("The server presents %s %s. lapilli never saves host keys for you.", t.HostKeyType, t.HostKeyFP)}
		if published {
			d.r.Why = append(d.r.Why, fmt.Sprintf("That is one of %s's published keys.", d.git.Name))
		}
		d.step("Connect once with ssh, compare the fingerprint, and accept it:", d.sshCmd())
		d.step("Or check the login without saving anything:", d.rerun("--trust-host-key"))
	}
}

// ---- authentication -----------------------------------------------------------

func (d *diag) auth() {
	t := d.t
	if t.Partial {
		d.partial()
		return
	}
	if t.Methods != nil && !contains(t.Methods, "publickey") {
		d.noPubkey()
		return
	}
	if t.TooMany {
		d.tooMany()
		return
	}
	for i := range t.Attempts {
		a := t.Attempts[i]
		if a.ServerAccepted && a.Outcome != sshlog.Used {
			d.acceptedNotUsed(a)
			return
		}
	}
	var offered, skipped []sshlog.Attempt
	for _, a := range t.Attempts {
		switch a.Outcome {
		case sshlog.Rejected, sshlog.Unanswered:
			offered = append(offered, a)
		case sshlog.Skipped:
			skipped = append(skipped, a)
		}
	}
	switch {
	case len(offered) > 0:
		d.allRejected(offered, skipped)
	case len(skipped) > 0:
		d.skipped(skipped[0])
	default:
		d.nothingOffered()
	}
}

func (d *diag) partial() {
	d.r.Code = "more-factors"
	rest := strings.Join(d.t.Methods, ", ")
	d.r.Headline = fmt.Sprintf("Your key was accepted, but %s also requires %s.", d.hostName(), rest)
	d.r.Why = []string{"The server asks for a second step (a password or a one-time code) after the key. lapilli doesn't answer prompts, so it stops here; the key part works."}
}

func (d *diag) noPubkey() {
	methods := strings.Join(d.t.Methods, ", ")
	if len(d.t.Methods) == 0 {
		methods = "nothing"
	}
	d.r.Code = "pubkey-disabled"
	d.r.Headline = fmt.Sprintf("%s doesn't take key logins for %s; it offers only %s.", d.hostName(), d.user(), methods)
	d.r.Why = []string{"ssh never got to offer a key. The server's settings turn key login off, for everyone or for this user or address: PubkeyAuthentication no, a Match block, or AuthenticationMethods asking for something else first."}
	d.r.Fix = []Step{{Text: "On the server, see the settings that apply to this user:", Cmd: fmt.Sprintf("sudo sshd -T -C user=%s | grep -Ei '^(pubkeyauthentication|authenticationmethods)'", d.user())}}
	if contains(d.t.Methods, "password") || contains(d.t.Methods, "keyboard-interactive") {
		d.r.Fix = append(d.r.Fix, Step{Text: "Meanwhile you can log in with the password:", Cmd: d.sshCmd()})
	}
}

func (d *diag) tooMany() {
	t := d.t
	n := 0
	for _, a := range t.Attempts {
		if a.Outcome == sshlog.Rejected || a.Outcome == sshlog.Unanswered {
			n++
		}
	}
	d.r.Code = "too-many-keys"
	d.r.Headline = fmt.Sprintf("%s disconnected after ssh offered %d keys (Too many authentication failures).", d.hostName(), n)
	d.r.Why = []string{"ssh offers every key in your agent before your key files, and the server allows only a few failed keys per connection (MaxAuthTries, 6 unless changed). They ran out before ssh got through its list."}

	tried := map[string]bool{}
	for _, a := range t.Attempts {
		tried[keyID(a.Key)] = true
	}
	var explicit *sshlog.Key
	pos := 0
	for i, k := range t.Planned {
		if k.Explicit && !tried[keyID(k)] {
			kk := k
			explicit, pos = &kk, i+1
			break
		}
	}
	if explicit != nil {
		name := d.show(strings.TrimSuffix(explicit.Path, ".pub"))
		d.covered[explicit.Path] = true
		d.r.Why = append(d.r.Why, fmt.Sprintf("Your configured key %s was number %d in line and never got a turn.", name, pos))
		d.step("Make ssh offer only that key to this host. In ~/.ssh/config:", d.pinSnippet(name))
		d.step("Or check it right away:", d.rerun("-i", name, "-o", "IdentitiesOnly=yes"))
		return
	}
	// key files ssh found on its own (~/.ssh/id_*) that never got a turn
	var files []string
	for _, k := range t.Planned {
		if !k.Agent && k.Fingerprint != "" && strings.Contains(k.Path, "/") && !tried[keyID(k)] {
			files = append(files, k.Path) // a missing file has no fingerprint
		}
	}
	if len(t.Planned) == 0 { // OpenSSH before 8.0 doesn't list its plan
		for _, f := range t.IdentityFiles {
			if f.Exists && !tried[f.Path] {
				files = append(files, f.Path)
			}
		}
	}
	if len(files) > 0 {
		var names []string
		for _, f := range files {
			names = append(names, d.show(f))
			d.covered[f] = true
		}
		d.r.Why = append(d.r.Why, fmt.Sprintf("Your key file %s never got a turn.", strings.Join(names, ", ")))
		pin := "If the server knows it, make ssh offer only that key to this host. In ~/.ssh/config:"
		if c := d.rerun("-i", names[0], "-o", "IdentitiesOnly=yes"); c != "" {
			d.step("Check whether the server knows it:", c)
			pin = "If it does, make ssh offer only that key to this host. In ~/.ssh/config:"
		}
		d.step(pin, d.pinSnippet(names[0]))
		return
	}
	d.step("Find out which key the server knows by testing one at a time:", d.rerun("-i", "~/.ssh/KEY", "-o", "IdentitiesOnly=yes"))
	d.step("Then pin it in ~/.ssh/config:", d.pinSnippet("~/.ssh/KEY"))
}

func (d *diag) acceptedNotUsed(a sshlog.Attempt) {
	k := a.Key
	name := d.keyName(k)
	keyPath := strings.TrimSuffix(k.Path, ".pub")
	f, haveFile := d.file(keyPath)
	d.covered[k.Path] = true
	d.covered[keyPath] = true
	h := d.hostName()
	switch {
	case a.Outcome == sshlog.LoadFailed && strings.Contains(a.Reason, "bad permissions"):
		mode := d.t.LoosePerms[k.Path]
		if mode == "" && haveFile {
			mode = f.ModeString
		}
		d.r.Code = "key-permissions"
		d.r.Headline = fmt.Sprintf("%s accepts %s, but ssh won't use it: other users can read the file (%s).", h, name, mode)
		d.r.Why = []string{"ssh refuses a private key that anyone but you can read, and carries on as if it weren't there."}
		d.r.Fix = []Step{{Text: "Make the key private to you:", Cmd: "chmod 600 " + d.show(k.Path)}}
		d.again()
	case a.Outcome == sshlog.LoadFailed:
		d.unreadable(k.Path, name, a.Reason, true)
	case a.Outcome == sshlog.SignFailed:
		d.signFailed(k, name, a.Reason)
	case isSecurityKey(k.Type) || (haveFile && isSecurityKey(f.Type)):
		d.r.Code = "security-key"
		d.r.Headline = fmt.Sprintf("%s accepts %s, a hardware security key, but it didn't sign.", h, name)
		d.r.Why = []string{"A security key needs a touch, and sometimes a PIN, for every login. lapilli can't prompt for either; plug the key in and use ssh in a terminal."}
		d.r.Fix = []Step{{Text: "Log in and touch the key when it blinks:", Cmd: d.sshCmd()}}
	case haveFile && f.Encrypted && !d.Agent.Has(f.Fingerprint):
		d.r.Code = "passphrase-not-in-agent"
		d.r.Headline = fmt.Sprintf("%s accepts %s, but the key has a passphrase and isn't loaded in your agent.", h, name)
		d.r.Why = []string{"In a terminal, ssh would ask for the passphrase on every connection. Programs that can't type it (git GUIs, IDEs, scripts, cron jobs) fail with Permission denied instead."}
		p := d.show(keyPath)
		if d.Agent != nil && !d.Agent.Running && !d.Agent.Disabled {
			d.r.Fix = append(d.r.Fix, Step{Text: "No agent is running. Start one:", Cmd: `eval "$(ssh-agent -s)"`})
		}
		if d.GOOS == "darwin" {
			d.r.Fix = append(d.r.Fix,
				Step{Text: "Load the key once and keep the passphrase in your macOS keychain:", Cmd: "ssh-add --apple-use-keychain " + p},
				Step{Text: "Let ssh do this by itself after a restart. In ~/.ssh/config:", Cmd: "Host *\n  AddKeysToAgent yes\n  UseKeychain yes"})
		} else {
			d.r.Fix = append(d.r.Fix,
				Step{Text: "Load the key into your agent (once per login session):", Cmd: "ssh-add " + p},
				Step{Text: "Or let ssh add it the first time you type the passphrase. In ~/.ssh/config:", Cmd: "Host *\n  AddKeysToAgent yes"})
		}
		d.again()
	case d.Files == nil:
		// a saved log: we can't look at the key file
		d.r.Code = "accepted-not-used"
		d.r.Headline = fmt.Sprintf("%s accepts %s, but ssh couldn't sign with it.", h, name)
		d.r.Why = []string{"The log stops after the server accepted the key. Most often the private key has a passphrase and nothing could type it (BatchMode, CI, a git GUI), or a hardware key wasn't touched."}
	case d.TimedOut:
		d.r.Code = "waiting-for-approval"
		d.r.Headline = fmt.Sprintf("%s accepts %s, but ssh was still waiting to sign when lapilli gave up after %s.", h, name, d.Timeout)
		d.r.Why = []string{"Something wanted your approval: a touch on a security key, or a confirmation in your password manager's SSH agent."}
	default:
		d.r.Code = "accepted-not-used"
		d.r.Headline = fmt.Sprintf("%s accepts %s, but ssh stopped before signing with it.", h, name)
		d.r.Why = []string{"The log doesn't say why. Run ssh -vvv yourself and look at the lines after \"Server accepts key\"."}
	}
}

func (d *diag) signFailed(k sshlog.Key, name, reason string) {
	d.r.Code = "agent-refused"
	d.r.Headline = fmt.Sprintf("%s accepts %s, but your agent wouldn't sign with it (%s).", d.hostName(), name, reason)
	kind := ""
	if d.Agent != nil {
		kind = d.Agent.Kind
	}
	switch {
	case isSecurityKey(k.Type):
		d.r.Code = "security-key"
		d.r.Why = []string{"This is a hardware security key; the agent needs you to touch it (and maybe enter a PIN). lapilli can't prompt for either."}
		d.r.Fix = []Step{{Text: "Log in and touch the key when it blinks:", Cmd: d.sshCmd()}}
	case kind == "1password":
		d.r.Why = []string{"1Password's SSH agent asks you to approve each use. It was locked, the prompt was dismissed, or the key isn't allowed for this program."}
		d.step("Unlock 1Password, run the check again and approve the prompt:", d.rerun())
	case kind == "gpg-agent":
		d.r.Why = []string{"gpg-agent couldn't show its PIN prompt: it is tied to a terminal it can no longer reach."}
		d.r.Fix = []Step{{Text: "Point gpg-agent at this terminal, then check again:", Cmd: "gpg-connect-agent updatestartuptty /bye\n" + d.rerun()}}
	case kind == "secretive" || kind == "bitwarden" || kind == "keepassxc":
		d.r.Why = []string{fmt.Sprintf("%s asks you to approve each use of the key; the request was denied or timed out.", kind)}
		d.step("Unlock it, run the check again and approve the request:", d.rerun())
	default:
		d.r.Why = []string{"The agent holds the key but refused to use it. A key added with ssh-add -c needs a confirmation program (SSH_ASKPASS) to approve each use, and a locked agent (ssh-add -x) refuses everything."}
		d.r.Fix = []Step{{Text: "If you locked the agent, unlock it:", Cmd: "ssh-add -X"}}
		if strings.Contains(k.Path, "/") {
			p := d.show(strings.TrimSuffix(k.Path, ".pub"))
			d.r.Fix = append(d.r.Fix, Step{Text: "Or add the key again without confirmation:", Cmd: fmt.Sprintf("ssh-add -d %s.pub; ssh-add %s", p, p)})
		}
	}
}

// unreadable explains a private key file ssh couldn't load.
func (d *diag) unreadable(path, name, reason string, accepted bool) {
	f, have := d.file(path)
	lead := fmt.Sprintf("ssh can't read the key file %s", name)
	if accepted {
		lead = fmt.Sprintf("%s accepts %s, but ssh can't read the private key file", d.hostName(), name)
	}
	if have && f.Format == local.FormatPuTTY {
		d.r.Code = "putty-key"
		d.r.Headline = fmt.Sprintf("%s: it is a PuTTY (.ppk) key, which OpenSSH doesn't understand.", lead)
		d.r.Fix = []Step{{Text: "Convert it with PuTTYgen:", Cmd: fmt.Sprintf("puttygen %s -O private-openssh -o %s", d.show(path), d.show(strings.TrimSuffix(path, filepath.Ext(path))+"_openssh"))}}
		return
	}
	d.r.Code = "key-unreadable"
	if reason == "" {
		reason = "ssh could not load it"
	}
	d.r.Headline = fmt.Sprintf("%s (%s).", lead, reason)
	switch {
	case strings.Contains(reason, "passphrase"):
		d.r.Why = []string{"The passphrase was wrong."}
	default:
		d.r.Why = []string{"The file isn't a private key OpenSSH can read. Usual causes: Windows line endings, a copy that was cut short, a PuTTY key, or a public key saved under the private key's name."}
		p := d.show(path)
		d.r.Fix = []Step{
			{Text: "An OpenSSH key starts with -----BEGIN OPENSSH PRIVATE KEY-----:", Cmd: "head -1 " + p},
			{Text: "If it has Windows line endings, remove them:", Cmd: fmt.Sprintf("tr -d '\\r' < %s > %s.tmp && mv %s.tmp %s && chmod 600 %s", p, p, p, p, p)},
		}
	}
}

func (d *diag) skipped(a sshlog.Attempt) {
	k := a.Key
	name := d.keyName(k)
	d.covered[k.Path] = true
	switch {
	case strings.HasPrefix(k.Type, "RSA") && (strings.Contains(a.Reason, "not supported by server") || strings.Contains(a.Reason, "no mutual signature")):
		d.r.Code = "rsa-sha1-only"
		d.r.Headline = fmt.Sprintf("%s only accepts RSA keys signed with SHA-1 (ssh-rsa), which your OpenSSH %s won't send.", d.hostName(), d.t.ClientVersion)
		why := fmt.Sprintf("OpenSSH 8.8 (2021) stopped sending ssh-rsa signatures by default because SHA-1 is broken, so your RSA key %s was never offered.", name)
		if d.t.ServerSoftware != "" {
			why += " The server runs " + d.t.ServerSoftware + "."
		}
		d.r.Why = []string{why}
		d.r.Fix = []Step{{Text: "Best: update SSH on the server. OpenSSH 7.2 (2016) and later accept SHA-2 signatures from the same key."}}
		if d.serverHasEd25519() {
			d.r.Fix = append(d.r.Fix, Step{Text: "Or use an Ed25519 key, which this server supports:", Cmd: "ssh-keygen -t ed25519"})
		}
		d.r.Fix = append(d.r.Fix, Step{Text: "To connect now, allow ssh-rsa for this host only. In ~/.ssh/config:", Cmd: fmt.Sprintf("Host %s\n  PubkeyAcceptedAlgorithms +ssh-rsa", d.alias())})
		d.again()
	case k.Type == "DSA":
		d.r.Code = "dsa-key"
		d.r.Headline = fmt.Sprintf("%s is a DSA key, which current OpenSSH doesn't use.", name)
		d.r.Why = []string{"DSA keys were turned off in OpenSSH 7.0 (2015) and removed in 10.0."}
		d.r.Fix = []Step{{Text: "Create a new key and install it where the old one was:", Cmd: "ssh-keygen -t ed25519"}}
	default:
		d.r.Code = "key-type-disabled"
		d.r.Headline = fmt.Sprintf("ssh skipped %s: your ssh configuration doesn't allow %s keys.", name, k.Type)
		d.r.Why = []string{a.Reason}
		d.r.Fix = []Step{{Text: "See the setting that applies:", Cmd: fmt.Sprintf("ssh -G %s | grep -i pubkeyacceptedalgorithms", d.alias())}}
	}
}

func (d *diag) serverHasEd25519() bool {
	for _, a := range d.t.ServerSigAlgs {
		if a == "ssh-ed25519" {
			return true
		}
	}
	v := d.t.ServerSoftware
	if strings.HasPrefix(v, "OpenSSH_") {
		major, minor := parseVersion(strings.TrimPrefix(v, "OpenSSH_"))
		return major > 6 || (major == 6 && minor >= 5)
	}
	return false
}

func (d *diag) nothingOffered() {
	t := d.t
	for _, a := range t.Attempts {
		if a.Outcome == sshlog.Missing && a.Key.Explicit {
			d.missingExplicit(a.Key.Path)
			return
		}
	}
	for _, a := range t.Attempts {
		if a.Outcome == sshlog.LoadFailed {
			d.covered[a.Key.Path] = true
			if f, ok := d.file(a.Key.Path); ok && f.Format == local.FormatPublic {
				d.publicOnly(a.Key.Path)
				return
			}
			d.unreadable(a.Key.Path, d.show(a.Key.Path), a.Reason, false)
			return
		}
	}
	d.r.Code = "no-keys"
	d.r.Headline = "ssh had no key to offer."
	var looked []string
	inDotSSH := true
	for _, f := range t.IdentityFiles {
		if !f.Exists {
			looked = append(looked, f.Path)
			inDotSSH = inDotSSH && d.Home != "" && filepath.Dir(f.Path) == filepath.Join(d.Home, ".ssh")
		}
	}
	switch {
	case len(looked) > 0 && inDotSSH:
		var names []string
		for _, p := range looked {
			names = append(names, filepath.Base(p))
		}
		d.r.Why = append(d.r.Why, fmt.Sprintf("It looked for %s in ~/.ssh; none of them exists.", strings.Join(names, ", ")))
	case len(looked) == 1:
		d.r.Why = append(d.r.Why, fmt.Sprintf("It looked for %s, which doesn't exist.", d.show(looked[0])))
	case len(looked) > 1:
		var names []string
		for _, p := range looked {
			names = append(names, d.show(p))
		}
		d.r.Why = append(d.r.Why, fmt.Sprintf("It looked for %s; none of them exists.", strings.Join(names, ", ")))
	}
	switch {
	case d.Agent != nil && d.Agent.Disabled:
		d.r.Why = append(d.r.Why, "Your configuration turns the agent off for this host (IdentityAgent none).")
	case t.AgentKeys == 0:
		d.r.Why = append(d.r.Why, "Your agent holds no keys.")
	case d.Agent != nil && !d.Agent.Running:
		d.r.Why = append(d.r.Why, "No agent is running.")
	}
	if len(d.Unused) > 0 {
		var names []string
		for _, k := range d.Unused {
			names = append(names, d.show(k.Path))
		}
		d.r.Why = append(d.r.Why, fmt.Sprintf("But ~/.ssh has keys with other names, which ssh uses only when told to: %s.", strings.Join(names, ", ")))
		first := d.show(d.Unused[0].Path)
		d.step("Check whether the server knows one of them:", d.rerun("-i", first))
		d.step("Then name it for this host in ~/.ssh/config:", d.pinSnippet(first))
		for _, k := range d.Unused {
			d.covered[k.Path] = true
		}
		return
	}
	d.r.Fix = []Step{{Text: "Create a key:", Cmd: "ssh-keygen -t ed25519"}}
	d.install("~/.ssh/id_ed25519.pub")
}

func (d *diag) missingExplicit(path string) {
	d.r.Code = "identity-file-missing"
	d.r.Headline = fmt.Sprintf("The key file your configuration names doesn't exist: %s.", d.show(path))
	d.r.Why = []string{"It comes from an IdentityFile line in your ssh config, or from -i. ssh skipped it and had nothing else to offer."}
	if len(d.Unused) > 0 {
		var names []string
		for _, k := range d.Unused {
			names = append(names, d.show(k.Path))
			d.covered[k.Path] = true
		}
		d.r.Why = append(d.r.Why, "Keys that do exist in ~/.ssh: "+strings.Join(names, ", ")+".")
	}
	var files []string
	for _, f := range d.t.ConfigFiles {
		if !strings.HasPrefix(f, "/etc/") {
			files = append(files, shellQuote(d.show(f)))
		}
	}
	if len(files) == 0 {
		files = []string{"~/.ssh/config"}
	}
	d.r.Fix = []Step{{Text: "Find the IdentityFile line (or the -i you passed) and fix the path:", Cmd: "grep -n -i identityfile " + strings.Join(files, " ")}}
}

func (d *diag) publicOnly(path string) {
	d.r.Code = "public-key-only"
	d.r.Headline = fmt.Sprintf("IdentityFile points to a public key (%s), and no agent holds its private half.", d.show(path))
	d.r.Why = []string{"That setup is meant for agents that keep the private key themselves, such as 1Password or Secretive. ssh found no such agent, or the agent doesn't have this key."}
	d.r.Fix = []Step{{Text: "Check which agent ssh uses and what it holds:", Cmd: fmt.Sprintf("ssh -G %s | grep -i identityagent\nssh-add -l", d.alias())}}
}

func (d *diag) allRejected(offered, skipped []sshlog.Attempt) {
	n := len(offered)
	user := d.user()
	g := d.git
	for _, a := range offered {
		d.covered[a.Key.Path] = true
	}
	if g != nil && user != g.User {
		d.r.Code = "git-wrong-user"
		d.r.Headline = fmt.Sprintf("You logged in to %s as %q; %s only accepts the user %q.", g.Name, user, g.Name, g.User)
		why := fmt.Sprintf("Your keys were tried for user %q, so none of them matched.", user)
		if d.userDefaulted() {
			why += " ssh used your local user name because none was given."
		}
		d.r.Why = []string{why}
		d.step("Check with the right user:", d.rerunAs(g.User))
		if d.Target != nil && d.Target.FromURL != "" {
			d.r.Fix = append(d.r.Fix, Step{Text: "And fix the remote URL, for example:", Cmd: "git remote set-url origin " + fixRemoteUser(d.Target.FromURL, g.User)})
		} else {
			d.r.Fix = append(d.r.Fix, Step{Text: "Or set it once in ~/.ssh/config:", Cmd: fmt.Sprintf("Host %s\n  User %s", d.alias(), g.User)})
		}
		return
	}
	if g != nil {
		d.r.Code = "git-key-not-registered"
		d.r.Headline = fmt.Sprintf("%s rejected every key ssh offered (%d): none of them is registered on %s.", g.Name, n, g.Name)
		if n == 1 {
			d.r.Headline = fmt.Sprintf("%s rejected the only key ssh offered: it isn't registered on %s.", g.Name, g.Name)
		}
		d.r.Why = []string{fmt.Sprintf("Each key reached %s and was turned down. %s accepts a key that is added to an account or to a repository as a deploy key; these are neither.", g.Name, g.Name)}
		if n == 1 {
			d.r.Why = []string{fmt.Sprintf("The key reached %s and was turned down. %s accepts a key that is added to an account or to a repository as a deploy key; this one is neither.", g.Name, g.Name)}
		}
		if len(d.Unused) > 0 {
			k := d.show(d.Unused[0].Path)
			d.step(fmt.Sprintf("If you already added a key to %s, it may be one ssh didn't try:", g.Name), d.rerun("-i", k))
			d.covered[d.Unused[0].Path] = true
		}
		d.install(d.pubToAdd(offered))
		d.skippedNote(skipped)
		return
	}
	d.r.Code = "all-keys-rejected"
	d.r.Headline = fmt.Sprintf("%s rejected every key ssh offered for %s (%d).", d.hostName(), user, n)
	if n == 1 {
		d.r.Headline = fmt.Sprintf("%s rejected the only key ssh offered for %s.", d.hostName(), user)
	}
	d.r.Why = []string{fmt.Sprintf("Your side works: each key reached the server and was turned down. The server doesn't find them in %s's authorized_keys, or won't read that file.", user)}
	if d.userDefaulted() {
		d.r.Why = append(d.r.Why, fmt.Sprintf("ssh logged in as %q, your local user name, because none was given. If the account on the server has another name (ubuntu, ec2-user, admin, root, ...), say so:", user))
		d.step("Check with the right user name:", d.rerunAs("NAME"))
	}
	if len(d.Unused) > 0 {
		var names []string
		for _, k := range d.Unused {
			names = append(names, d.show(k.Path))
			d.covered[k.Path] = true
		}
		text := fmt.Sprintf("ssh didn't try %s. If the server knows one of them, check it:", strings.Join(names, ", "))
		if len(names) == 1 {
			text = fmt.Sprintf("ssh didn't try %s. If the server knows it, check it:", names[0])
		}
		d.step(text, d.rerun("-i", names[0]))
	}
	fp := ""
	if len(offered) > 0 && offered[0].Key.Fingerprint != "" {
		fp = "   # is " + offered[0].Key.Fingerprint + " there?"
	}
	home := "~" + user
	d.r.Fix = append(d.r.Fix,
		Step{Text: fmt.Sprintf("On the server, look at %s's keys and their permissions:", user), Cmd: fmt.Sprintf("ssh-keygen -lf %s/.ssh/authorized_keys%s\nls -ld %s %s/.ssh %s/.ssh/authorized_keys", home, fp, home, home, home)},
		Step{Text: "sshd ignores the file when the home directory, ~/.ssh or the file can be written by others:", Cmd: fmt.Sprintf("chmod go-w %s && chmod 700 %s/.ssh && chmod 600 %s/.ssh/authorized_keys", home, home, home)},
		Step{Text: "The server's log names the check that failed:", Cmd: "sudo journalctl -u ssh -u sshd --since '15 min ago'   # or /var/log/auth.log, /var/log/secure"},
	)
	if contains(d.t.Methods, "password") || contains(d.t.Methods, "keyboard-interactive") {
		pub := d.pubToAdd(offered)
		d.r.Fix = append(d.r.Fix, Step{Text: "To install your key, if you know the password:", Cmd: d.copyID(pub, user)})
	}
	d.r.Also = append(d.r.Also, Note{Level: "info", Text: "On SELinux systems (RHEL, Fedora, Rocky), a wrong file label on ~/.ssh also hides the file from sshd:", Cmd: "restorecon -Rv " + home + "/.ssh"})
	d.skippedNote(skipped)
}

func (d *diag) skippedNote(skipped []sshlog.Attempt) {
	for _, a := range skipped {
		d.covered[a.Key.Path] = true
		d.r.Also = append(d.r.Also, Note{Level: "warn", Text: fmt.Sprintf("ssh didn't offer %s: %s.", d.keyName(a.Key), shortReason(a.Reason))})
	}
}

// install says how to put a public key where the server will find it.
func (d *diag) install(pub string) {
	if g := d.git; g != nil {
		d.r.Fix = append(d.r.Fix,
			Step{Text: "Copy the public key:", Cmd: copyCmd(d.GOOS, pub)},
		)
		if c := d.rerun(); c != "" {
			d.step("Add it at "+g.KeysURL+", then check again:", c)
		} else {
			d.step("Add it at "+g.KeysURL+".", "")
		}
		return
	}
	if contains(d.t.Methods, "password") || contains(d.t.Methods, "keyboard-interactive") {
		d.r.Fix = append(d.r.Fix, Step{Text: "Install it on the server (asks for your password once):", Cmd: d.copyID(pub, d.user())})
		return
	}
	d.r.Fix = append(d.r.Fix, Step{Text: fmt.Sprintf("Ask whoever runs the server to add %s to %s's ~/.ssh/authorized_keys.", pub, d.user())})
}

// pubToAdd picks the public key file to suggest installing.
func (d *diag) pubToAdd(offered []sshlog.Attempt) string {
	for _, a := range offered {
		if a.Key.Explicit && strings.Contains(a.Key.Path, "/") {
			return d.show(strings.TrimSuffix(a.Key.Path, ".pub")) + ".pub"
		}
	}
	for _, a := range offered {
		if f, ok := d.file(a.Key.Path); ok && f.PubExists {
			return d.show(a.Key.Path) + ".pub"
		}
	}
	for _, a := range offered {
		if strings.Contains(a.Key.Path, "/") && !a.Key.Agent {
			return d.show(a.Key.Path) + ".pub"
		}
	}
	return "~/.ssh/id_ed25519.pub"
}

func (d *diag) pinSnippet(key string) string {
	return fmt.Sprintf("Host %s\n  IdentityFile %s\n  IdentitiesOnly yes", d.alias(), key)
}

// step adds a fix step; a step that exists only to show a command is dropped
// when there is no command (a saved log has no destination to rerun).
func (d *diag) step(text, cmd string) {
	if cmd == "" && strings.HasSuffix(text, ":") {
		return
	}
	d.r.Fix = append(d.r.Fix, Step{Text: text, Cmd: cmd})
}

func (d *diag) again() {
	if c := d.rerun(); c != "" {
		d.r.Fix = append(d.r.Fix, Step{Text: "Check again:", Cmd: c})
	}
}

func (d *diag) unknown() {
	d.r.Code = "unknown"
	if d.TimedOut {
		d.r.Code = "timeout"
		d.r.Headline = fmt.Sprintf("ssh didn't finish within %s.", d.Timeout)
		d.r.Why = []string{"Something was waiting: a slow or silent network, a ProxyCommand, or a prompt (a security key touch, a password manager approval)."}
	} else {
		d.r.Headline = "lapilli couldn't tell where the connection stopped."
	}
	if tail := lastLines(d.Log, 4); len(tail) > 0 {
		d.r.Why = append(d.r.Why, "The last things ssh said:")
		d.r.Why = append(d.r.Why, tail...)
	}
}

// ---- notes from the client's files -------------------------------------------

func (d *diag) localNotes() {
	if d.t.Proxy && len(d.t.Jumps) == 0 && d.t.JumpTarget == "" && d.r.Stage != sshlog.StageDone {
		d.r.Also = append(d.r.Also, Note{Level: "info", Text: "This connection goes through a ProxyCommand that lapilli can't see into. If the proxy is what fails, check it on its own."})
	}
	if d.TrustedHostKey {
		d.r.Also = append(d.r.Also, Note{Level: "info", Text: "The host key was not checked for this test (--trust-host-key). Nothing was saved."})
	}
	if d.Files == nil {
		return
	}
	paths := make([]string, 0, len(d.Files))
	for p := range d.Files {
		paths = append(paths, p)
	}
	sortStrings(paths)
	for _, p := range paths {
		f := d.Files[p]
		if !f.Exists || d.covered[p] {
			continue
		}
		name := d.show(p)
		switch {
		case f.TooOpen():
			d.r.Also = append(d.r.Also, Note{Level: "warn", Text: fmt.Sprintf("%s can be read by other users (%s). ssh will refuse it the moment a server accepts it.", name, f.ModeString), Cmd: "chmod 600 " + name})
		case f.Format == local.FormatPuTTY:
			d.r.Also = append(d.r.Also, Note{Level: "warn", Text: fmt.Sprintf("%s is a PuTTY key; OpenSSH can't use it.", name), Cmd: fmt.Sprintf("puttygen %s -O private-openssh -o %s_openssh", name, strings.TrimSuffix(name, filepath.Ext(name)))})
		}
		if f.PubMismatch {
			d.r.Also = append(d.r.Also, Note{Level: "warn", Text: fmt.Sprintf("%s.pub belongs to a different key than %s. ssh offers the .pub but signs with the private key, so this key can never log in.", name, name), Cmd: fmt.Sprintf("ssh-keygen -y -f %s > %s.pub", name, name)})
		}
	}
}

// ---- helpers ----------------------------------------------------------------

func isSecurityKey(t string) bool { return strings.HasSuffix(t, "-SK") || strings.Contains(t, "-SK-") }

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

func orUser(u string) string {
	if u == "" {
		return "$USER"
	}
	return u
}

func splitFileLine(s string) (string, string) {
	i := strings.LastIndexByte(s, ':')
	if i < 0 {
		return s, "?"
	}
	return s[:i], s[i+1:]
}

func isDefaultKnownHosts(file, home string) bool {
	return file == filepath.Join(home, ".ssh", "known_hosts")
}

func lookupCmd(goos, host string) string {
	if goos == "darwin" {
		return "dscacheutil -q host -a name " + host
	}
	return "getent hosts " + host
}

func copyCmd(goos, pub string) string {
	if goos == "darwin" {
		return "pbcopy < " + pub
	}
	return "cat " + pub
}

func (d *diag) sshCmd() string {
	t := d.Target
	if t == nil || t.Dest == "" {
		return "ssh USER@HOST"
	}
	parts := []string{"ssh"}
	for _, a := range t.SSHArgs {
		parts = append(parts, shellQuote(d.show(a)))
	}
	return strings.Join(append(parts, t.Dest), " ")
}

// fixRemoteUser rewrites the user in a git remote URL.
func fixRemoteUser(u, user string) string {
	if strings.Contains(u, "://") {
		scheme, rest, _ := strings.Cut(u, "://")
		if _, after, ok := strings.Cut(rest, "@"); ok {
			rest = after
		}
		return scheme + "://" + user + "@" + rest
	}
	if _, after, ok := strings.Cut(u, "@"); ok {
		u = after
	}
	return user + "@" + u
}

func parseVersion(v string) (int, int) {
	majorS, rest, _ := strings.Cut(v, ".")
	major, _ := strconv.Atoi(majorS)
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	minor, _ := strconv.Atoi(rest[:end])
	return major, minor
}

func lastLines(log []byte, n int) []string {
	var out []string
	lines := strings.Split(strings.TrimRight(string(log), "\n"), "\n")
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" || strings.HasPrefix(l, "debug") {
			continue
		}
		out = append([]string{l}, out...)
	}
	return out
}

func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_./=:@~,+%", c)) {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}
