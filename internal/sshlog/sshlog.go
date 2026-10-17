// Package sshlog reads the debug output of an OpenSSH client (ssh -v, -vv or -vvv)
// and turns it into a Trace: how far the connection got, which keys ssh offered,
// and what the server said about each one.
//
// The parser is line based and tolerant: it understands the message formats of
// OpenSSH 7.x through 10.x and ignores everything it does not recognise.
package sshlog

import (
	"bufio"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// Outcome is what happened to one key.
type Outcome string

const (
	Unanswered Outcome = "unanswered"  // offered, but the connection ended before the server answered
	Rejected   Outcome = "rejected"    // the server does not accept this key for this user
	Accepted   Outcome = "accepted"    // the server would accept it, but no signature followed
	Used       Outcome = "used"        // the key authenticated the session
	SignFailed Outcome = "sign-failed" // accepted, but the agent or token would not sign
	LoadFailed Outcome = "load-failed" // accepted (or tried), but ssh could not load the private key
	Missing    Outcome = "missing"     // the identity file does not exist
	Skipped    Outcome = "skipped"     // ssh did not offer it (signature algorithm not usable)
)

// Key identifies a key the way ssh describes it in its log.
type Key struct {
	Path        string `json:"path,omitempty"` // file path, or the agent's comment for agent-only keys
	Type        string `json:"type,omitempty"` // ED25519, RSA, ECDSA, ED25519-SK, ...
	Fingerprint string `json:"fingerprint,omitempty"`
	Agent       bool   `json:"agent,omitempty"`    // held by an agent
	Explicit    bool   `json:"explicit,omitempty"` // named by IdentityFile or -i
}

// Attempt is one key ssh tried, in order.
type Attempt struct {
	Key     Key     `json:"key"`
	Outcome Outcome `json:"outcome"`
	Reason  string  `json:"reason,omitempty"`
	// ServerAccepted is set when the server said it would accept the key,
	// whatever happened afterwards (signing, loading the private key).
	ServerAccepted bool `json:"server_accepted,omitempty"`
}

// IdentityFile is an identity file ssh looked for at startup.
type IdentityFile struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
}

// Trace is everything the log says about one connection attempt.
type Trace struct {
	ClientVersion  string   `json:"client_version,omitempty"`
	ServerSoftware string   `json:"server_software,omitempty"`
	ConfigFiles    []string `json:"config_files,omitempty"`
	AppliedOptions []string `json:"applied_options,omitempty"` // "<file> line <n>: Applying options for <pattern>"
	Proxy          bool     `json:"proxy,omitempty"`

	// Connection stage
	ConnectHost  string `json:"connect_host,omitempty"`
	ConnectAddr  string `json:"connect_addr,omitempty"`
	ConnectPort  int    `json:"connect_port,omitempty"`
	ResolveError string `json:"resolve_error,omitempty"`
	ConnectError string `json:"connect_error,omitempty"`
	Connected    bool   `json:"connected"`
	BannerError  string `json:"banner_error,omitempty"`
	Penalized    bool   `json:"penalized,omitempty"`    // OpenSSH 9.8+ PerSourcePenalties
	NotSSH       bool   `json:"not_ssh,omitempty"`      // something other than an SSH server answered
	ConfigPerms  string `json:"config_perms,omitempty"` // "Bad owner or permissions on <path>"
	// The first line ssh refused in a config file or a -o option, which stops it
	// before it connects: "<file>: line 3: Bad configuration option: usekeychain".
	ConfigFile  string `json:"config_file,omitempty"` // "command-line" for -o
	ConfigLine  int    `json:"config_line,omitempty"`
	ConfigError string `json:"config_error,omitempty"`
	configCand  *[3]string

	// Key exchange and host key
	AuthHost        string   `json:"auth_host,omitempty"`
	AuthPort        int      `json:"auth_port,omitempty"`
	User            string   `json:"user,omitempty"`
	Hops            []string `json:"hops,omitempty"`             // every "Authenticating to" target, jump hosts first
	NegotiationWhat string   `json:"negotiation_what,omitempty"` // "host key type", "key exchange method", "cipher", "MAC"
	TheirOffer      string   `json:"their_offer,omitempty"`
	HostKeyType     string   `json:"host_key_type,omitempty"`
	HostKeyFP       string   `json:"host_key_fingerprint,omitempty"`
	HostKnown       bool     `json:"host_known,omitempty"`
	HostKeyFoundIn  string   `json:"host_key_found_in,omitempty"`
	HostKeyChanged  bool     `json:"host_key_changed,omitempty"`
	OffendingKey    string   `json:"offending_key,omitempty"` // "<file>:<line>"
	HostUnknown     bool     `json:"host_unknown,omitempty"`
	HostKeyFailed   bool     `json:"host_key_failed,omitempty"`
	ServerSigAlgs   []string `json:"server_sig_algs,omitempty"`

	// Authentication
	IdentityFiles []IdentityFile    `json:"identity_files,omitempty"`
	AgentKeys     int               `json:"agent_keys"` // -1 when the log does not say
	AgentError    string            `json:"agent_error,omitempty"`
	Methods       []string          `json:"methods,omitempty"` // first "Authentications that can continue"
	Planned       []Key             `json:"planned,omitempty"` // "Will attempt key" (OpenSSH 8.x+)
	Attempts      []Attempt         `json:"attempts,omitempty"`
	LoosePerms    map[string]string `json:"loose_perms,omitempty"` // path -> mode, from "Permissions 0644 for ..."
	TooMany       bool              `json:"too_many,omitempty"`
	Disconnect    string            `json:"disconnect,omitempty"`
	Denied        bool              `json:"denied,omitempty"`
	DeniedMethods []string          `json:"denied_methods,omitempty"`
	DeniedTarget  string            `json:"denied_target,omitempty"` // "user@host" printed before "Permission denied"
	Authenticated bool              `json:"authenticated"`
	AuthMethod    string            `json:"auth_method,omitempty"`
	Partial       bool              `json:"partial,omitempty"`

	// What a git host said after authentication (ssh -T git@github.com)
	Greeting string `json:"greeting,omitempty"`
	Account  string `json:"account,omitempty"`

	// Through jump hosts (ProxyJump, or a ProxyCommand running ssh -v) the log
	// holds one connection per host, each from its "Authenticating to" on. The
	// fields above then describe the destination, which may never have been
	// reached; Jumps are the hosts on the way, in order.
	Jumps        []*Trace `json:"jumps,omitempty"`
	JumpTarget   string   `json:"jump_target,omitempty"`   // "host:port" the first jump was asked to reach (ssh -W)
	ForwardError string   `json:"forward_error,omitempty"` // a jump host couldn't open the way on: "connect failed: Connection refused"

	Lines int `json:"lines"`
}

var (
	reVersion       = regexp.MustCompile(`^OpenSSH_([0-9][^ ,]*)`)
	reReading       = regexp.MustCompile(`^Reading configuration data (.+)$`)
	reApplying      = regexp.MustCompile(`^(.+ line \d+: Applying options for .+)$`)
	reConnecting    = regexp.MustCompile(`^Connecting to (\S+) \[([^\]]+)\] port (\d+)\.?$`)
	reConnectFail   = regexp.MustCompile(`^ssh: connect to host (\S+) port (\d+): (.+)$`)
	reResolve       = regexp.MustCompile(`^ssh: Could not resolve hostname (\S+): (.+)$`)
	reIdentity      = regexp.MustCompile(`^identity file (.+) type (-?\d+)$`)
	reRemoteVer     = regexp.MustCompile(`^Remote protocol version [0-9.]+, remote software version (.+)$`)
	reBanner        = regexp.MustCompile(`^(?:kex|ssh)_exchange_identification: (.+)$`)
	reAuthTo        = regexp.MustCompile(`^Authenticating to (.+):(\d+) as '([^']*)'$`)
	reNegotiate     = regexp.MustCompile(`^Unable to negotiate with .+: no matching (.+) found\. Their offer: (.+)$`)
	reHostKey       = regexp.MustCompile(`^Server host key: (\S+) (\S+)$`)
	reKnown         = regexp.MustCompile(`^Host '.+' is known and matches the \S+ host key\.$`)
	reFoundIn       = regexp.MustCompile(`^Found key in (.+:\d+)$`)
	reOffending     = regexp.MustCompile(`^Offending \S+ key in (.+:\d+)$`)
	reNoHostKey     = regexp.MustCompile(`^No \S+ host key is known for .+ and you have requested strict checking\.$`)
	reSigAlgs       = regexp.MustCompile(`server-sig-algs=<([^>]*)>`)
	reMethods       = regexp.MustCompile(`^Authentications that can continue: (.*)$`)
	reAgentN        = regexp.MustCompile(`get_agent_identities: agent returned (\d+) keys?`)
	reAgentSock     = regexp.MustCompile(`get_agent_identities: ssh_get_authentication_socket: (.+)$`)
	reWillAttempt   = regexp.MustCompile(`^Will attempt key: (.*)$`)
	reOffering      = regexp.MustCompile(`^Offering public key: (.*)$`)
	reOfferingOld   = regexp.MustCompile(`^Offering (\S+) public key: (.+)$`)
	reAccepts       = regexp.MustCompile(`^Server accepts key: (.*)$`)
	reTrying        = regexp.MustCompile(`^Trying private key: (.+)$`)
	reNoSuch        = regexp.MustCompile(`^no such identity: (.+): No such file or directory$`)
	reLoadKey       = regexp.MustCompile(`^Load key "(.+)": (.+)$`)
	rePerms         = regexp.MustCompile(`^Permissions (\d+) for '(.+)' are too open\.$`)
	reSkipping      = regexp.MustCompile(`^Skipping (\S+) key (.+) - (.+)$`)
	reNoMutual      = regexp.MustCompile(`no mutual signature algorithm`)
	reSignFail      = regexp.MustCompile(`^sign_and_send_pubkey: signing failed(?: for \S+ "(.+)"(?: from agent)?)?: (.+)$`)
	reSucceeded     = regexp.MustCompile(`^Authentication succeeded \((.+)\)\.$`)
	reAuthenticated = regexp.MustCompile(`^Authenticated to \S+ \(.+\)(?: using "(.+)")?\.$`)
	rePartial       = regexp.MustCompile(`^Authenticated using "(.+)" with partial success\.$`)
	reDisconnect    = regexp.MustCompile(`^Received disconnect from .+?:\s*\d+:\s*(.+)$`)
	reDenied        = regexp.MustCompile(`^(?:(\S+): )?Permission denied \((.+)\)\.$`)
	reBadOwner      = regexp.MustCompile(`^Bad owner or permissions on (.+)$`)
	reConfigLine    = regexp.MustCompile(`^(.+?):? line (\d+): (.+?)\.?$`)
	reConfigEnd     = regexp.MustCompile(`: terminating, \d+ bad configuration options$`)
	reProxyExec     = regexp.MustCompile(`^Executing proxy command: exec (.+)$`)
	reStdioTarget   = regexp.MustCompile(`\s-W\s+'?\[?([^\]'\s]+?)\]?:(\d+)'?(?:\s|$)`)
	reOpenFailed    = regexp.MustCompile(`^channel \d+: open failed: (.+?)\.?$`)
	reGitHub        = regexp.MustCompile(`^Hi (\S+)! You've successfully authenticated`)
	reGitLab        = regexp.MustCompile(`^Welcome to GitLab, @(\S+)!`)
	reForgejo       = regexp.MustCompile(`^Hi there,? (\S+)! You've successfully authenticated`)
	reBitbucket     = regexp.MustCompile(`^(?:authenticated via ssh key\.|logged in as (\S+)\.)`)
	reKeyType       = regexp.MustCompile(`^(RSA|DSA|ECDSA|ED25519|XMSS)(-SK)?(-CERT)?$`)
)

// Parse reads an ssh debug log.
func Parse(r io.Reader) *Trace {
	t := &Trace{AgentKeys: -1}
	cur := -1 // index into t.Attempts of the key waiting for the server's answer
	// with a proxy, one trace per "Authenticating to", each with its own cur
	var conns []*Trace
	var curs []int
	// A disconnect message is the server's text, and OpenSSH lets newlines
	// through it: what follows may be lines of the server's making. ssh has
	// nothing true to add after it, so the rest is not read.
	disconnected := false
	rd := bufio.NewReaderSize(r, 64*1024)
	for {
		raw, ok := readLine(rd)
		if !ok {
			break
		}
		t.Lines++
		if disconnected || t.configError(raw) {
			continue
		}
		msg := stripLevel(raw)
		if t.Proxy && reAuthTo.MatchString(msg) {
			conns = append(conns, &Trace{
				AgentKeys:      -1,
				ClientVersion:  t.ClientVersion,
				ServerSoftware: t.ServerSoftware, // its banner came just before
				IdentityFiles:  append([]IdentityFile(nil), t.IdentityFiles...),
				Connected:      true,
			})
			curs = append(curs, -1)
		}
		t.line(msg, &cur)
		if n := len(conns); n > 0 && !connectLine(msg) {
			conns[n-1].line(msg, &curs[n-1])
		}
		disconnected = strings.HasPrefix(msg, "Received disconnect from ")
	}
	t.splitJumps(conns)
	// OpenSSH 7.x stops at a bad value at once ("line 2: Bad number."), without
	// the closing line; it never got as far as connecting.
	if c := t.configCand; t.ConfigError == "" && c != nil && t.ConnectHost == "" && t.ResolveError == "" {
		t.setConfigError(c[0], c[1], c[2])
	}
	return t
}

// connectLine is about opening a connection or its banner. A connection's own
// come before its "Authenticating to", so after it they belong to another one:
// the client giving up when its proxy, a jump host, went away.
func connectLine(msg string) bool {
	return reConnecting.MatchString(msg) || msg == "Connection established." ||
		reConnectFail.MatchString(msg) || reResolve.MatchString(msg) || reBanner.MatchString(msg) ||
		strings.HasPrefix(msg, "Connection timed out during banner exchange") ||
		strings.HasPrefix(msg, "Connection closed by")
}

// configError notes a config line ssh refused. Its complaints are not debug
// lines; they name a file and a line, and either end with "terminating, N bad
// configuration options" or, for a -o option, stop ssh at once.
func (t *Trace) configError(raw string) bool {
	if strings.HasPrefix(raw, "debug") || t.ConfigError != "" {
		return false
	}
	if reConfigEnd.MatchString(raw) {
		if c := t.configCand; c != nil {
			t.setConfigError(c[0], c[1], c[2])
		}
		return true
	}
	m := reConfigLine.FindStringSubmatch(raw)
	if m == nil || strings.HasPrefix(m[3], "Unsupported option") || strings.Contains(m[3], "Applying options for") {
		return false // a deprecated option is only a warning
	}
	if m[1] == "command-line" {
		t.setConfigError(m[1], m[2], m[3])
	} else if t.configCand == nil {
		t.configCand = &[3]string{m[1], m[2], m[3]}
	}
	return true
}

// splitJumps sorts the connections of a proxied log: the destination's is the
// one to the host the first jump was asked to reach (the last one, when the
// proxy isn't ssh); the others are jump hosts. The trace's own connection
// fields become the destination's, empty if ssh never got there.
func (t *Trace) splitJumps(conns []*Trace) {
	if len(conns) == 0 {
		return
	}
	dest := -1
	for i, c := range conns {
		if t.JumpTarget == "" || c.AuthHost+":"+strconv.Itoa(c.AuthPort) == t.JumpTarget {
			dest = i
		}
	}
	for i, c := range conns {
		if i != dest {
			t.Jumps = append(t.Jumps, c)
		}
	}
	d := &Trace{AgentKeys: -1}
	if dest >= 0 {
		d = conns[dest]
	} else if h, p, ok := strings.Cut(t.JumpTarget, ":"); ok {
		d.AuthHost = h
		d.AuthPort, _ = strconv.Atoi(p)
	}
	t.Connected = dest >= 0
	t.ServerSoftware = d.ServerSoftware
	t.AuthHost, t.AuthPort, t.User = d.AuthHost, d.AuthPort, d.User
	t.NegotiationWhat, t.TheirOffer = d.NegotiationWhat, d.TheirOffer
	t.HostKeyType, t.HostKeyFP, t.HostKnown, t.HostKeyFoundIn = d.HostKeyType, d.HostKeyFP, d.HostKnown, d.HostKeyFoundIn
	t.HostKeyChanged, t.OffendingKey, t.HostUnknown, t.HostKeyFailed = d.HostKeyChanged, d.OffendingKey, d.HostUnknown, d.HostKeyFailed
	t.ServerSigAlgs, t.AgentKeys, t.AgentError = d.ServerSigAlgs, d.AgentKeys, d.AgentError
	t.Methods, t.Planned, t.Attempts, t.LoosePerms = d.Methods, d.Planned, d.Attempts, d.LoosePerms
	t.TooMany, t.Disconnect, t.Denied, t.DeniedMethods, t.DeniedTarget = d.TooMany, d.Disconnect, d.Denied, d.DeniedMethods, d.DeniedTarget
	t.Authenticated, t.AuthMethod, t.Partial = d.Authenticated, d.AuthMethod, d.Partial
	t.Greeting, t.Account = d.Greeting, d.Account
}

// FailedJump is the first jump host ssh didn't get through, or nil.
func (t *Trace) FailedJump() *Trace {
	for _, j := range t.Jumps {
		if !j.Authenticated || j.Partial {
			return j
		}
	}
	return nil
}

func (t *Trace) setConfigError(file, line, msg string) {
	t.ConfigFile, t.ConfigError = file, msg
	t.ConfigLine, _ = strconv.Atoi(line)
}

// maxLine is the longest line read whole; the rest of a longer one is skipped.
const maxLine = 1 << 20

// readLine reads one line without its line ending, cut at maxLine.
func readLine(rd *bufio.Reader) (string, bool) {
	var b []byte
	for {
		part, more, err := rd.ReadLine()
		if len(b) < maxLine {
			b = append(b, part[:min(len(part), maxLine-len(b))]...)
		}
		if err != nil {
			return string(b), len(b) > 0
		}
		if !more {
			return strings.TrimRight(string(b), "\r"), true
		}
	}
}

func stripLevel(s string) string {
	for _, p := range []string{"debug1: ", "debug2: ", "debug3: "} {
		if strings.HasPrefix(s, p) {
			return s[len(p):]
		}
	}
	return s
}

func (t *Trace) line(msg string, cur *int) {
	var m []string
	switch {
	case t.ClientVersion == "" && reVersion.MatchString(msg):
		t.ClientVersion = reVersion.FindStringSubmatch(msg)[1]

	case match(reReading, msg, &m):
		t.ConfigFiles = appendUnique(t.ConfigFiles, m[1])
	case match(reApplying, msg, &m):
		t.AppliedOptions = append(t.AppliedOptions, m[1])
	case strings.HasPrefix(msg, "Setting implicit ProxyCommand from ProxyJump"):
		t.Proxy = true
	case match(reProxyExec, msg, &m):
		t.Proxy = true
		// the first one is the client's own: ssh ... -W '[dest]:port' jump
		if w := reStdioTarget.FindStringSubmatch(" " + m[1]); w != nil && t.JumpTarget == "" {
			t.JumpTarget = w[1] + ":" + w[2]
		}
	case match(reOpenFailed, msg, &m):
		if t.ForwardError == "" {
			t.ForwardError = m[1]
		}

	case match(reConnecting, msg, &m):
		t.ConnectHost, t.ConnectAddr = m[1], m[2]
		t.ConnectPort, _ = strconv.Atoi(m[3])
	case msg == "Connection established.":
		t.Connected = true
	case match(reConnectFail, msg, &m):
		t.ConnectError = m[3]
		if t.ConnectHost == "" {
			t.ConnectHost = m[1]
		}
		t.ConnectPort, _ = strconv.Atoi(m[2])
	case match(reResolve, msg, &m):
		t.ResolveError = m[2]
		t.ConnectHost = m[1]
	case match(reBadOwner, msg, &m):
		t.ConfigPerms = m[1]
	case match(reBanner, msg, &m):
		switch {
		case strings.Contains(m[1], "Not allowed at this time"):
			t.Penalized = true
		case strings.Contains(m[1], "invalid characters"), strings.Contains(m[1], "banner line contains"):
			t.NotSSH = true
			t.BannerError = m[1]
		case strings.HasPrefix(m[1], "banner line"):
			// a pre-banner line the server printed; only its content matters above
		default:
			t.BannerError = m[1]
		}
	case strings.HasPrefix(msg, "Bad remote protocol version identification"),
		strings.HasPrefix(msg, "Protocol major versions differ"):
		t.NotSSH = true
	case strings.HasPrefix(msg, "Connection timed out during banner exchange"):
		t.BannerError = "Connection timed out during banner exchange"

	case match(reIdentity, msg, &m):
		if !strings.HasSuffix(m[1], "-cert") {
			n, _ := strconv.Atoi(m[2])
			t.IdentityFiles = append(t.IdentityFiles, IdentityFile{Path: m[1], Exists: n >= 0})
		}
	case match(reRemoteVer, msg, &m):
		t.ServerSoftware = m[1]
	case match(reAuthTo, msg, &m):
		t.AuthHost, t.User = m[1], m[3]
		t.AuthPort, _ = strconv.Atoi(m[2])
		t.Hops = append(t.Hops, m[3]+"@"+m[1])
	case match(reNegotiate, msg, &m):
		t.NegotiationWhat, t.TheirOffer = m[1], m[2]

	case match(reHostKey, msg, &m):
		t.HostKeyType, t.HostKeyFP = m[1], m[2]
	case reKnown.MatchString(msg):
		t.HostKnown = true
	case match(reFoundIn, msg, &m):
		t.HostKeyFoundIn = m[1]
	case strings.Contains(msg, "REMOTE HOST IDENTIFICATION HAS CHANGED"):
		t.HostKeyChanged = true
	case match(reOffending, msg, &m):
		t.OffendingKey = m[1]
	case reNoHostKey.MatchString(msg), strings.HasPrefix(msg, "The authenticity of host"):
		t.HostUnknown = true
	case msg == "Host key verification failed.":
		t.HostKeyFailed = true
	case match(reSigAlgs, msg, &m):
		t.ServerSigAlgs = strings.Split(m[1], ",")

	case match(reMethods, msg, &m):
		if t.Methods == nil {
			t.Methods = splitMethods(m[1])
		}
		if *cur >= 0 {
			a := &t.Attempts[*cur]
			switch a.Outcome {
			case Unanswered:
				a.Outcome = Rejected
			case Accepted:
				// accepted, then nothing worked: the signature never reached the server
				if a.Reason == "" {
					a.Reason = "no signature was sent"
				}
			}
			*cur = -1
		}
	case match(reAgentN, msg, &m):
		t.AgentKeys, _ = strconv.Atoi(m[1])
	case strings.Contains(msg, "agent contains no identities"):
		t.AgentKeys = 0
	case match(reAgentSock, msg, &m):
		t.AgentError = m[1]
	case strings.HasPrefix(msg, "Error connecting to agent"),
		strings.HasPrefix(msg, "Could not open a connection to your authentication agent"):
		t.AgentError = msg

	case match(reWillAttempt, msg, &m):
		t.Planned = append(t.Planned, parseKey(m[1]))
	case match(reOffering, msg, &m):
		t.settleSilent(cur)
		t.Attempts = append(t.Attempts, Attempt{Key: parseKey(m[1]), Outcome: Unanswered})
		*cur = len(t.Attempts) - 1
	case match(reOfferingOld, msg, &m):
		t.settleSilent(cur)
		k := parseKey(m[2])
		if k.Type == "" {
			k.Type = m[1]
		}
		t.Attempts = append(t.Attempts, Attempt{Key: k, Outcome: Unanswered})
		*cur = len(t.Attempts) - 1
	case match(reTrying, msg, &m):
		t.settleSilent(cur)
		k := Key{Path: m[1]}
		for _, p := range t.Planned {
			if p.Path == k.Path {
				k = p
			}
		}
		t.Attempts = append(t.Attempts, Attempt{Key: k, Outcome: Unanswered})
		*cur = len(t.Attempts) - 1
	case match(reAccepts, msg, &m):
		if *cur >= 0 {
			t.Attempts[*cur].Outcome = Accepted
			t.Attempts[*cur].ServerAccepted = true
			if k := parseKey(m[1]); k.Fingerprint != "" && t.Attempts[*cur].Key.Fingerprint == "" {
				t.Attempts[*cur].Key = k
			}
		}
	case match(reNoSuch, msg, &m):
		if i := t.lastAttempt(m[1]); i >= 0 {
			t.Attempts[i].Outcome = Missing
			if i == *cur {
				*cur = -1
			}
		}
	case match(rePerms, msg, &m):
		if t.LoosePerms == nil {
			t.LoosePerms = map[string]string{}
		}
		t.LoosePerms[m[2]] = m[1]
	case match(reLoadKey, msg, &m):
		i := t.lastAttempt(m[1])
		if i < 0 {
			t.Attempts = append(t.Attempts, Attempt{Key: Key{Path: m[1]}})
			i = len(t.Attempts) - 1
		}
		t.Attempts[i].Outcome = LoadFailed
		t.Attempts[i].Reason = m[2]
		if i == *cur {
			*cur = -1
		}
	case match(reSkipping, msg, &m):
		t.Attempts = append(t.Attempts, Attempt{
			Key:     Key{Path: m[2], Type: strings.ToUpper(strings.TrimPrefix(m[1], "ssh-"))},
			Outcome: Skipped,
			Reason:  m[3],
		})
	case reNoMutual.MatchString(msg):
		if *cur >= 0 {
			t.Attempts[*cur].Outcome = Skipped
			t.Attempts[*cur].Reason = "no mutual signature algorithm"
			*cur = -1
		}
	case match(reSignFail, msg, &m):
		i := *cur
		if m[1] != "" {
			if j := t.lastAttempt(m[1]); j >= 0 {
				i = j
			}
		}
		if i < 0 {
			i = t.lastWith(Accepted)
		}
		if i >= 0 {
			t.Attempts[i].Outcome = SignFailed
			t.Attempts[i].Reason = m[2]
		}

	case msg == "No more authentication methods to try.",
		strings.HasPrefix(msg, "Next authentication method: "):
		t.settleSilent(cur)

	case match(rePartial, msg, &m):
		t.Partial = true
		t.markUsed(m[1], cur)
	case match(reSucceeded, msg, &m):
		t.Authenticated = true
		t.AuthMethod = m[1]
		t.markUsed(m[1], cur)
	case match(reAuthenticated, msg, &m):
		t.Authenticated = true
		if m[1] != "" {
			t.AuthMethod = m[1]
		}
		t.markUsed(t.AuthMethod, cur)
	case match(reDisconnect, msg, &m):
		t.Disconnect = m[1]
		if strings.Contains(m[1], "Too many authentication failures") {
			t.TooMany = true
		}
	case strings.Contains(msg, "Too many authentication failures"):
		t.TooMany = true
		if t.Disconnect == "" {
			t.Disconnect = "Too many authentication failures"
		}
	case match(reDenied, msg, &m):
		// the server's word is final: an earlier "Authenticated to" was banner text
		t.Authenticated, t.AuthMethod = false, ""
		t.Denied = true
		t.DeniedTarget = m[1]
		t.DeniedMethods = splitMethods(m[2])

	case match(reGitHub, msg, &m), match(reForgejo, msg, &m), match(reGitLab, msg, &m):
		t.Greeting, t.Account = msg, m[1]
	case match(reBitbucket, msg, &m):
		t.Greeting = msg
		if m[1] != "" {
			t.Account = m[1]
		}
	}
}

// settleSilent resolves a key ssh started on but never sent: old clients print
// "Trying private key" and then move on without a word when the file is missing
// or cannot be read.
func (t *Trace) settleSilent(cur *int) {
	if *cur < 0 || t.Attempts[*cur].Outcome != Unanswered {
		return
	}
	a := &t.Attempts[*cur]
	a.Outcome = LoadFailed
	a.Reason = "ssh could not load it"
	for _, f := range t.IdentityFiles {
		if f.Path == a.Key.Path && !f.Exists {
			a.Outcome = Missing
			a.Reason = ""
		}
	}
	*cur = -1
}

// markUsed records which key authenticated, when the method was publickey.
func (t *Trace) markUsed(method string, cur *int) {
	if method != "" && method != "publickey" {
		return
	}
	if i := t.lastWith(Accepted); i >= 0 {
		t.Attempts[i].Outcome = Used
		t.Attempts[i].Reason = ""
	} else if *cur >= 0 {
		// "Trying private key" in old clients goes straight to a signature
		t.Attempts[*cur].Outcome = Used
	}
	*cur = -1
}

func (t *Trace) lastAttempt(path string) int {
	for i := len(t.Attempts) - 1; i >= 0; i-- {
		if t.Attempts[i].Key.Path == path {
			return i
		}
	}
	return -1
}

func (t *Trace) lastWith(o Outcome) int {
	for i := len(t.Attempts) - 1; i >= 0; i-- {
		if t.Attempts[i].Outcome == o {
			return i
		}
	}
	return -1
}

// parseKey reads ssh's description of a key. The formats vary by version:
//
//	/home/a/.ssh/id_ed25519 ED25519 SHA256:abc explicit agent   (8.x+)
//	RSA SHA256:abc /home/a/.ssh/id_rsa                           (7.6-7.9)
//	/home/a/.ssh/id_rsa                                          (older; type in the prefix)
//	/home/a/.ssh/missing  explicit                               (file absent)
//
// For agent-only keys the "path" is the key's comment, which may contain spaces.
func parseKey(desc string) Key {
	var k Key
	toks := strings.Fields(desc)
	end := len(toks)
	// flags trail the description
	for end > 0 {
		switch toks[end-1] {
		case "explicit":
			k.Explicit = true
		case "agent":
			k.Agent = true
		case "token", "authenticator":
		default:
			if strings.HasPrefix(toks[end-1], "(") || strings.HasSuffix(toks[end-1], ")") {
				break // sk flags like "(user presence required)"
			}
			goto flagsDone
		}
		end--
	}
flagsDone:
	toks = toks[:end]
	fp, typ := -1, -1
	for i, s := range toks {
		if fp < 0 && (strings.HasPrefix(s, "SHA256:") || strings.HasPrefix(s, "MD5:")) {
			fp = i
		}
		if typ < 0 && reKeyType.MatchString(s) {
			typ = i
		}
	}
	// certificates repeat the CA's type and fingerprint; the first ones are the key's
	if typ >= 0 {
		k.Type = toks[typ]
	}
	if fp >= 0 {
		k.Fingerprint = toks[fp]
	}
	var rest []string
	for i, s := range toks {
		if i == fp || i == typ {
			continue
		}
		if typ >= 0 && i > typ && fp >= 0 && i > fp {
			continue // certificate details after the fingerprint
		}
		rest = append(rest, s)
	}
	switch {
	case typ < 0 || fp < 0:
		k.Path = strings.Join(rest, " ")
	case typ < fp && typ > 0:
		// path comes first: "path TYPE fp"
		k.Path = strings.Join(toks[:typ], " ")
	default:
		// "TYPE fp path"
		k.Path = strings.Join(toks[fp+1:], " ")
	}
	return k
}

func splitMethods(s string) []string {
	var out []string
	for _, m := range strings.Split(s, ",") {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	return out
}

func appendUnique(xs []string, x string) []string {
	for _, y := range xs {
		if y == x {
			return xs
		}
	}
	return append(xs, x)
}

func match(re *regexp.Regexp, s string, m *[]string) bool {
	*m = re.FindStringSubmatch(s)
	return *m != nil
}

// Stage names where a connection attempt stopped.
const (
	StageResolve = "resolve"
	StageConnect = "connect"
	StageConfig  = "config"
	StageJump    = "jump-host" // a jump host refused, or couldn't reach the next host
	StageBanner  = "banner"
	StageKex     = "key-exchange"
	StageHostKey = "host-key"
	StageAuth    = "authentication"
	StageDone    = "authenticated"
	StageUnknown = "unknown"
)

// Stage reports how far the connection got.
func (t *Trace) Stage() string {
	switch {
	case t.Authenticated && !t.Partial:
		return StageDone
	case t.ConfigPerms != "", t.ConfigError != "":
		return StageConfig
	case t.ResolveError != "":
		return StageResolve
	case t.ConnectError != "" && !t.Connected:
		return StageConnect
	case t.FailedJump() != nil, t.ForwardError != "" && !t.Connected:
		return StageJump
	case t.Penalized || t.NotSSH || t.BannerError != "":
		return StageBanner
	case t.NegotiationWhat != "":
		return StageKex
	case t.HostKeyFailed || t.HostKeyChanged || t.HostUnknown:
		return StageHostKey
	case t.Denied || t.TooMany || len(t.Attempts) > 0 || t.Methods != nil:
		return StageAuth
	}
	return StageUnknown
}
