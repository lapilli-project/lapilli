package sshlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func load(t *testing.T, name string) *Trace {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "testdata", "logs", name+".log"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	return Parse(f)
}

// attempts renders the attempts as "name:outcome" for compact comparison.
func attempts(tr *Trace) string {
	var out []string
	for _, a := range tr.Attempts {
		out = append(out, filepath.Base(a.Key.Path)+":"+string(a.Outcome))
	}
	return strings.Join(out, " ")
}

func TestFixtures(t *testing.T) {
	cases := []struct {
		name     string
		stage    string
		attempts string
		check    func(*testing.T, *Trace)
	}{
		{"ok_file", StageDone, "good:used", func(t *testing.T, tr *Trace) {
			want(t, tr.AuthMethod, "publickey")
			want(t, tr.User, "alice")
			want(t, tr.HostKnown, true)
			want(t, tr.ClientVersion, "10.0p2")
		}},
		{"rejected", StageAuth, "other:rejected", func(t *testing.T, tr *Trace) {
			want(t, strings.Join(tr.Methods, ","), "publickey,password")
			want(t, tr.DeniedTarget, "alice@127.0.0.1")
		}},
		{"rejected_then_ok", StageDone, "other:rejected good:used", nil},
		{"no_keys", StageAuth, "missing:missing", nil},
		{"enc_accepted", StageAuth, "enc:accepted", nil},
		{"enc_rejected", StageAuth, "enc2:rejected", nil},
		{"nopub_ok", StageDone, "nopub:used", nil},
		{"bad_perms", StageAuth, "loose:rejected", func(t *testing.T, tr *Trace) {
			want(t, len(tr.LoosePerms), 0) // a rejected key is never loaded, so ssh never sees its mode
		}},
		{"bad_perms_good", StageAuth, "loosegood:load-failed", func(t *testing.T, tr *Trace) {
			want(t, tr.Attempts[0].Reason, "bad permissions")
			want(t, tr.LoosePerms["/home/alice/.ssh/loosegood"], "0644")
		}},
		{"pubkey_disabled", StageAuth, "", func(t *testing.T, tr *Trace) {
			want(t, strings.Join(tr.Methods, ","), "password")
		}},
		{"rsa_sha1_only", StageAuth, "rsa:skipped", func(t *testing.T, tr *Trace) {
			want(t, tr.Attempts[0].Reason, "corresponding algorithm not supported by server")
			want(t, strings.Join(tr.ServerSigAlgs, ","), "ssh-rsa")
		}},
		{"hostkey_ssh_rsa", StageKex, "", func(t *testing.T, tr *Trace) {
			want(t, tr.NegotiationWhat, "host key type")
			want(t, tr.TheirOffer, "ssh-rsa")
		}},
		{"hostkey_changed", StageHostKey, "", func(t *testing.T, tr *Trace) {
			want(t, tr.HostKeyChanged, true)
			want(t, tr.OffendingKey, "/home/alice/.ssh/known_hosts:1")
		}},
		{"hostkey_unknown", StageHostKey, "", func(t *testing.T, tr *Trace) {
			want(t, tr.HostKeyFailed, true)
			want(t, tr.HostKnown, false)
		}},
		{"conn_refused", StageConnect, "", func(t *testing.T, tr *Trace) {
			want(t, tr.ConnectError, "Connection refused")
			want(t, tr.ConnectPort, 2329)
		}},
		{"conn_timeout", StageConnect, "", func(t *testing.T, tr *Trace) {
			want(t, tr.ConnectError, "Operation timed out")
		}},
		{"dns_fail", StageResolve, "", func(t *testing.T, tr *Trace) {
			want(t, tr.ConnectHost, "no-such-host.invalid")
		}},
		{"kex_closed_penalty", StageBanner, "", func(t *testing.T, tr *Trace) {
			want(t, tr.Penalized, true)
		}},
		{"too_many", StageAuth, "flood1:rejected flood2:rejected flood3:unanswered", func(t *testing.T, tr *Trace) {
			want(t, tr.TooMany, true)
			want(t, len(tr.Planned), 7)
			want(t, tr.Planned[6].Explicit, true)
			want(t, tr.Planned[0].Agent, true)
			want(t, tr.AgentKeys, 6)
		}},
		{"too_many_noid", StageAuth, "flood1:rejected flood2:rejected flood3:unanswered", nil},
		{"agent_ok_identitiesonly", StageDone, "good.pub:used", func(t *testing.T, tr *Trace) {
			k := tr.Attempts[0].Key
			want(t, k.Agent && k.Explicit, true)
			want(t, k.Type, "ED25519")
		}},
		{"agent_refused", StageAuth, "good.pub:sign-failed", func(t *testing.T, tr *Trace) {
			want(t, tr.Attempts[0].Reason, "agent refused operation")
		}},
		{"agent_empty_enc", StageAuth, "enc:accepted", func(t *testing.T, tr *Trace) {
			want(t, tr.AgentKeys, 0)
		}},
		{"wrong_user", StageAuth, "good:rejected", func(t *testing.T, tr *Trace) {
			want(t, tr.User, "nosuchuser")
		}},
		{"config_unknown_option", StageConfig, "", func(t *testing.T, tr *Trace) {
			want(t, tr.ConfigFile, "/home/alice/.ssh/config")
			want(t, tr.ConfigLine, 3)
			want(t, tr.ConfigError, "Bad configuration option: usekeychain")
		}},
		{"config_bad_value", StageConfig, "", func(t *testing.T, tr *Trace) {
			want(t, tr.ConfigError, "Bad port 'twentytwo'")
		}},
		{"config_perms", StageConfig, "", func(t *testing.T, tr *Trace) {
			want(t, tr.ConfigPerms, "/home/alice/.ssh/config")
		}},
		// OpenSSH 7.4 (Amazon Linux 2): no "Will attempt key", the type before "public key"
		{"openssh74_too_many", StageAuth, "flood1:rejected flood2:rejected flood3:unanswered", func(t *testing.T, tr *Trace) {
			want(t, tr.ClientVersion, "7.4p1")
			want(t, len(tr.Planned), 0)
			want(t, tr.TooMany, true)
			want(t, tr.Attempts[0].Key.Type, "ED25519")
		}},
		{"openssh74_agent_refused", StageAuth, "good:sign-failed id_rsa:missing id_dsa:missing id_ecdsa:missing id_ed25519:missing", func(t *testing.T, tr *Trace) {
			want(t, tr.Attempts[0].Reason, "agent refused operation")
		}},
		{"openssh74_enc_accepted", StageAuth, "id_rsa:missing id_dsa:missing id_ecdsa:missing id_ed25519:accepted", nil},
		{"openssh74_bad_perms_good", StageAuth, "id_rsa:missing id_dsa:missing id_ecdsa:missing id_ed25519:load-failed", func(t *testing.T, tr *Trace) {
			want(t, tr.LoosePerms["/home/alice/.ssh/id_ed25519"], "0644")
		}},
		{"openssh74_config_bad_value", StageConfig, "", func(t *testing.T, tr *Trace) {
			want(t, tr.ConfigLine, 2)
			want(t, tr.ConfigError, "Bad number")
		}},
		{"openssh80_agent_refused", StageAuth, "good:sign-failed id_rsa:missing id_dsa:missing id_ecdsa:missing id_ed25519:missing id_xmss:missing", func(t *testing.T, tr *Trace) {
			want(t, tr.ClientVersion, "8.0p1")
			want(t, tr.Attempts[0].Reason, "agent refused operation")
		}},
		{"openssh89_rsa_sha1_only", StageAuth, "id_rsa:skipped id_ecdsa:missing id_ecdsa_sk:missing id_ed25519:missing id_ed25519_sk:missing id_xmss:missing id_dsa:missing", func(t *testing.T, tr *Trace) {
			want(t, tr.Attempts[0].Reason, "no mutual signature algorithm")
		}},
		{"openssh89_hostkey_ssh_rsa", StageKex, "", func(t *testing.T, tr *Trace) {
			want(t, tr.NegotiationWhat, "host key type")
			want(t, tr.TheirOffer, "ssh-rsa")
		}},
		// Rocky Linux 9: the crypto policy leaves ssh-rsa out before anything is offered
		{"openssh99_rhel_rsa_sha1_only", StageAuth, "id_rsa:skipped id_ecdsa:missing id_ecdsa_sk:missing id_ed25519:missing id_ed25519_sk:missing id_xmss:missing id_dsa:missing", func(t *testing.T, tr *Trace) {
			want(t, tr.ClientVersion, "9.9p1")
			want(t, tr.Attempts[0].Reason, "corresponding algorithm not supported by server")
		}},
		// through a jump host (jump:2322 or :2327) to lab; 2327 knows a different key
		{"jump_ok", StageDone, "id_rsa:missing id_ecdsa:missing id_ecdsa_sk:missing id_ed25519:used", func(t *testing.T, tr *Trace) {
			want(t, tr.JumpTarget, "lab:2322")
			want(t, len(tr.Jumps), 1)
			want(t, tr.Jumps[0].AuthHost, "jump")
			want(t, tr.Jumps[0].Authenticated, true)
			want(t, tr.AuthHost, "lab")
		}},
		{"jump_refused", StageConnect, "", func(t *testing.T, tr *Trace) {
			want(t, tr.ConnectHost, "jump")
			want(t, tr.JumpTarget, "lab:2322")
		}},
		{"jump_rejected", StageJump, "", func(t *testing.T, tr *Trace) {
			j := tr.FailedJump()
			want(t, j.AuthPort, 2327)
			want(t, attempts(j), "id_rsa:missing id_ecdsa:missing id_ecdsa_sk:missing id_ed25519:rejected id_ed25519_sk:missing id_xmss:missing")
			want(t, tr.Authenticated, false)
		}},
		{"jump_target_hostkey_unknown", StageHostKey, "", func(t *testing.T, tr *Trace) {
			want(t, tr.HostKeyFailed, true)
			want(t, tr.AuthHost, "lab")
			want(t, tr.Jumps[0].Authenticated, true)
		}},
		{"jump_target_refused", StageJump, "", func(t *testing.T, tr *Trace) {
			want(t, tr.ForwardError, "connect failed: Connection refused")
			want(t, tr.JumpTarget, "lab:2329")
			want(t, tr.FailedJump() == nil, true)
		}},
		{"jump_target_rejected", StageAuth, "id_rsa:missing id_ecdsa:missing id_ecdsa_sk:missing id_ed25519:rejected id_ed25519_sk:missing id_xmss:missing", func(t *testing.T, tr *Trace) {
			want(t, tr.DeniedTarget, "alice@lab")
			want(t, tr.Jumps[0].Authenticated, true)
		}},
		// OpenSSH 7.4 doesn't name the host in "Permission denied"
		{"openssh74_jump_rejected", StageJump, "", func(t *testing.T, tr *Trace) {
			want(t, tr.FailedJump().AuthPort, 2327)
		}},
		{"openssh74_jump_target_rejected", StageAuth, "id_rsa:missing id_dsa:missing id_ecdsa:missing id_ed25519:rejected", func(t *testing.T, tr *Trace) {
			want(t, tr.AuthPort, 2327)
			want(t, tr.Jumps[0].Authenticated, true)
		}},
		{"synthetic_openssh74_rejected", StageAuth, "id_rsa:rejected id_dsa:missing id_ecdsa:missing id_ed25519:missing", func(t *testing.T, tr *Trace) {
			want(t, tr.ClientVersion, "7.4p1")
			want(t, tr.Attempts[0].Key.Type, "RSA")
			want(t, tr.User, "deploy")
			want(t, strings.Join(tr.DeniedMethods, ","), "publickey,gssapi-keyex,gssapi-with-mic,password")
		}},
		{"synthetic_openssh89_nomutual", StageAuth, "id_rsa:skipped id_ed25519:missing", func(t *testing.T, tr *Trace) {
			want(t, tr.Attempts[0].Reason, "no mutual signature algorithm")
			want(t, tr.ServerSoftware, "OpenSSH_6.6.1p1 Ubuntu-2ubuntu2.13")
		}},
		{"synthetic_github_rejected", StageAuth, "alice@laptop:rejected id_rsa:rejected", nil},
		{"synthetic_github_wrong_user", StageAuth, "alice@laptop:rejected id_rsa:rejected", func(t *testing.T, tr *Trace) {
			want(t, tr.User, "alice")
		}},
		{"synthetic_github_ok", StageDone, "id_ed25519_work:used", func(t *testing.T, tr *Trace) {
			want(t, tr.Account, "alice-work")
			want(t, tr.Planned[1].Path, "alice@laptop")
			want(t, tr.Planned[1].Agent, true)
		}},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		seen[c.name] = true
		t.Run(c.name, func(t *testing.T) {
			tr := load(t, c.name)
			want(t, tr.Stage(), c.stage)
			want(t, attempts(tr), c.attempts)
			if c.check != nil {
				c.check(t, tr)
			}
		})
	}
	// every fixture must be covered
	files, _ := filepath.Glob(filepath.Join("..", "..", "testdata", "logs", "*.log"))
	for _, f := range files {
		if n := strings.TrimSuffix(filepath.Base(f), ".log"); !seen[n] {
			t.Errorf("fixture %s has no test case", n)
		}
	}
}

// What ssh prints when it won't use its configuration, as OpenSSH 10.0 words it.
func TestConfigError(t *testing.T) {
	cases := []struct {
		log, file string
		line      int
		err       string
	}{
		{"/home/alice/.ssh/config: line 3: Bad configuration option: usekeychain\n/home/alice/.ssh/config line 4: Bad port 'abc'.\n/home/alice/.ssh/config: terminating, 2 bad configuration options\n",
			"/home/alice/.ssh/config", 3, "Bad configuration option: usekeychain"},
		{"/home/alice/.ssh/config line 2: unsupported option \"maybe\".\n/home/alice/.ssh/config: terminating, 1 bad configuration options\n",
			"/home/alice/.ssh/config", 2, `unsupported option "maybe"`},
		{"command-line: line 0: Bad configuration option: remotecommand\n",
			"command-line", 0, "Bad configuration option: remotecommand"},
		// a deprecated option is only a warning, and debug lines are never errors
		{"/home/alice/.ssh/config line 2: Unsupported option \"rsaauthentication\"\ndebug1: /etc/ssh/ssh_config line 21: include /etc/ssh/ssh_config.d/*.conf matched no files\n",
			"", 0, ""},
		// OpenSSH 7.4 stops at a bad value without the closing line
		{"OpenSSH_7.4p1, OpenSSL 1.0.2k-fips  26 Jan 2017\n/home/alice/.ssh/config line 2: Bad number.\n",
			"/home/alice/.ssh/config", 2, "Bad number"},
		// a complaint that ssh got past is not a stop
		{"/home/alice/.ssh/config line 4: Bad port 'abc'.\ndebug1: Connecting to lab [172.21.0.2] port 22.\n", "", 0, ""},
	}
	for _, c := range cases {
		tr := Parse(strings.NewReader(c.log))
		want(t, tr.ConfigFile, c.file)
		want(t, tr.ConfigLine, c.line)
		want(t, tr.ConfigError, c.err)
		want(t, tr.Stage() == StageConfig, c.err != "")
	}
}

// Text a server sends can hold newlines; lines it forges must not change the
// outcome.
func TestServerTextIsNotTrusted(t *testing.T) {
	// a disconnect message with lines of its own
	tr := Parse(strings.NewReader("OpenSSH_10.0p2, OpenSSL 3.5.0\n" +
		"debug1: Authenticating to lab:22 as 'alice'\n" +
		"debug1: Offering public key: /home/alice/.ssh/id_ed25519 ED25519 SHA256:abc\n" +
		"Received disconnect from 172.21.0.2 port 22:2: bye\n" +
		"debug1: Server accepts key: /home/alice/.ssh/id_ed25519 ED25519 SHA256:abc\n" +
		"Authenticated to lab ([172.21.0.2]:22) using \"publickey\".\n" +
		"Load key \"/tmp/k;touch /tmp/pwned\": bad permissions\n"))
	want(t, tr.Authenticated, false)
	want(t, tr.Disconnect, "bye")
	want(t, attempts(tr), "id_ed25519:unanswered")

	// a banner claiming success before the server turns the key down
	tr = Parse(strings.NewReader("debug1: Authenticating to lab:22 as 'alice'\n" +
		"Authenticated to lab ([172.21.0.2]:22) using \"publickey\".\n" +
		"alice@lab: Permission denied (publickey).\n"))
	want(t, tr.Authenticated, false)
	want(t, tr.Stage(), StageAuth)
}

// A line longer than maxLine is cut, and reading goes on.
func TestLongLine(t *testing.T) {
	log := "OpenSSH_10.0p2, OpenSSL 3.5.0\n" + strings.Repeat("x", 3<<20) + "\nAuthenticated to lab ([172.21.0.2]:22) using \"publickey\".\n"
	tr := Parse(strings.NewReader(log))
	want(t, tr.Lines, 3)
	want(t, tr.Authenticated, true)
}

func TestParseKey(t *testing.T) {
	cases := []struct {
		in   string
		want Key
	}{
		{"/h/.ssh/id_ed25519 ED25519 SHA256:abc explicit agent", Key{Path: "/h/.ssh/id_ed25519", Type: "ED25519", Fingerprint: "SHA256:abc", Agent: true, Explicit: true}},
		{"RSA SHA256:abc /h/.ssh/id_rsa", Key{Path: "/h/.ssh/id_rsa", Type: "RSA", Fingerprint: "SHA256:abc"}},
		{"/h/.ssh/missing  explicit", Key{Path: "/h/.ssh/missing", Explicit: true}},
		{"John Doe key ED25519 SHA256:abc agent", Key{Path: "John Doe key", Type: "ED25519", Fingerprint: "SHA256:abc", Agent: true}},
		{"/h/.ssh/id_ecdsa_sk ECDSA-SK SHA256:abc explicit", Key{Path: "/h/.ssh/id_ecdsa_sk", Type: "ECDSA-SK", Fingerprint: "SHA256:abc", Explicit: true}},
	}
	for _, c := range cases {
		if got := parseKey(c.in); got != c.want {
			t.Errorf("parseKey(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func want[T comparable](t *testing.T, got, w T) {
	t.Helper()
	if got != w {
		t.Errorf("got %v, want %v", got, w)
	}
}
