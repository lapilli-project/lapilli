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
