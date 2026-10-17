package diagnose

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lapilli-project/lapilli/internal/local"
	"github.com/lapilli-project/lapilli/internal/sshlog"
	"github.com/lapilli-project/lapilli/internal/target"
)

func trace(t *testing.T, name string) (*sshlog.Trace, []byte) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "logs", name+".log"))
	if err != nil {
		t.Fatal(err)
	}
	return sshlog.Parse(strings.NewReader(string(data))), data
}

// TestSavedLogs pins the main finding for every captured log, read the way
// "lapilli ssh --log" reads it: with no access to the machine it came from.
func TestSavedLogs(t *testing.T) {
	want := map[string]string{
		"ok_file":                      "ok",
		"rejected":                     "all-keys-rejected",
		"rejected_then_ok":             "ok",
		"no_keys":                      "identity-file-missing",
		"enc_accepted":                 "accepted-not-used",
		"enc_rejected":                 "all-keys-rejected",
		"nopub_ok":                     "ok",
		"bad_perms":                    "all-keys-rejected",
		"bad_perms_good":               "key-permissions",
		"pubkey_disabled":              "pubkey-disabled",
		"rsa_sha1_only":                "rsa-sha1-only",
		"hostkey_ssh_rsa":              "no-common-algorithm",
		"hostkey_changed":              "host-key-changed",
		"hostkey_unknown":              "host-key-unknown",
		"conn_refused":                 "connection-refused",
		"conn_timeout":                 "connection-timeout",
		"dns_fail":                     "dns",
		"kex_closed_penalty":           "penalized",
		"too_many":                     "too-many-keys",
		"too_many_noid":                "too-many-keys",
		"agent_ok_identitiesonly":      "ok",
		"agent_refused":                "agent-refused",
		"agent_empty_enc":              "accepted-not-used",
		"wrong_user":                   "all-keys-rejected",
		"config_unknown_option":        "config-unknown-option",
		"config_bad_value":             "config-error",
		"config_perms":                 "config-permissions",
		"openssh74_too_many":           "too-many-keys",
		"openssh74_agent_refused":      "agent-refused",
		"openssh74_enc_accepted":       "accepted-not-used",
		"openssh74_bad_perms_good":     "key-permissions",
		"openssh74_config_bad_value":   "config-error",
		"openssh80_agent_refused":      "agent-refused",
		"openssh89_rsa_sha1_only":      "rsa-sha1-only",
		"openssh89_hostkey_ssh_rsa":    "no-common-algorithm",
		"openssh99_rhel_rsa_sha1_only": "rsa-sha1-only",
		"synthetic_openssh74_rejected": "all-keys-rejected",
		"synthetic_openssh89_nomutual": "rsa-sha1-only",
		"synthetic_github_ok":          "ok",
		"synthetic_github_rejected":    "git-key-not-registered",
		"synthetic_github_wrong_user":  "git-wrong-user",
	}
	files, _ := filepath.Glob(filepath.Join("..", "..", "testdata", "logs", "*.log"))
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".log")
		code, ok := want[name]
		if !ok {
			t.Errorf("%s: no expected finding", name)
			continue
		}
		tr, log := trace(t, name)
		r := Diagnose(Input{Trace: tr, Log: log, GOOS: "linux"})
		if r.Code != code {
			t.Errorf("%s: code %q, want %q (%s)", name, r.Code, code, r.Headline)
		}
		if r.Headline == "" {
			t.Errorf("%s: no headline", name)
		}
		if r.OK != (code == "ok") {
			t.Errorf("%s: ok=%v", name, r.OK)
		}
		for _, s := range r.Fix {
			if s.Cmd == "" && strings.HasSuffix(s.Text, ":") {
				t.Errorf("%s: step %q has no command", name, s.Text)
			}
		}
	}
}

func TestGitHub(t *testing.T) {
	tr, _ := trace(t, "synthetic_github_ok")
	r := Diagnose(Input{Trace: tr})
	if r.Account != "alice-work" || !strings.Contains(r.Headline, "you are alice-work on GitHub") {
		t.Errorf("success: %q", r.Headline)
	}

	tr, _ = trace(t, "synthetic_github_wrong_user")
	tg, _ := target.Parse([]string{"github.com:owner/repo.git"})
	r = Diagnose(Input{Trace: tr, Target: tg, LocalUser: "alice"})
	if !strings.Contains(r.Headline, `only accepts the user "git"`) {
		t.Errorf("wrong user: %q", r.Headline)
	}
	if got := r.Fix[len(r.Fix)-1].Cmd; got != "git remote set-url origin git@github.com:owner/repo.git" {
		t.Errorf("remote fix: %q", got)
	}

	tr, _ = trace(t, "synthetic_github_rejected")
	tg, _ = target.Parse([]string{"git@github.com"})
	r = Diagnose(Input{Trace: tr, Target: tg, GOOS: "darwin", Home: "/home/alice", Unused: []local.KeyFile{{Path: "/home/alice/.ssh/work", Exists: true}}})
	var cmds []string
	for _, s := range r.Fix {
		cmds = append(cmds, s.Cmd)
	}
	got := strings.Join(cmds, " | ")
	for _, w := range []string{"lapilli ssh -i ~/.ssh/work git@github.com", "pbcopy < ~/.ssh/id_rsa.pub", "lapilli ssh git@github.com"} {
		if !strings.Contains(got, w) {
			t.Errorf("fix steps %q lack %q", got, w)
		}
	}
}

// With the key file at hand, an accepted key that never signed is explained.
func TestPassphraseNotInAgent(t *testing.T) {
	tr, _ := trace(t, "enc_accepted")
	key := tr.Attempts[0].Key
	files := map[string]local.KeyFile{key.Path: {Path: key.Path, Exists: true, Encrypted: true, Fingerprint: key.Fingerprint}}
	for _, c := range []struct {
		goos, cmd string
	}{{"darwin", "ssh-add --apple-use-keychain"}, {"linux", "ssh-add /home/alice/.ssh/enc"}} {
		r := Diagnose(Input{Trace: tr, Files: files, Agent: &local.Agent{Running: true}, GOOS: c.goos})
		if r.Code != "passphrase-not-in-agent" {
			t.Fatalf("code %q", r.Code)
		}
		if !strings.Contains(r.Fix[0].Cmd, c.cmd) {
			t.Errorf("%s: first step %q", c.goos, r.Fix[0].Cmd)
		}
	}
	// the same key already in the agent is not the explanation
	agent := &local.Agent{Running: true, Keys: []local.AgentKey{{Fingerprint: key.Fingerprint}}}
	if r := Diagnose(Input{Trace: tr, Files: files, Agent: agent}); r.Code == "passphrase-not-in-agent" {
		t.Errorf("blamed the passphrase although the agent holds the key")
	}
}

// Agent keys used up the tries before ssh reached a key file it found itself.
func TestTooManyNamesTheFile(t *testing.T) {
	tr, _ := trace(t, "too_many_noid")
	tg, _ := target.Parse([]string{"-p", "2323", "alice@127.0.0.1"})
	r := Diagnose(Input{Trace: tr, Target: tg, Home: "/Users/alice"})
	if why := strings.Join(r.Why, " "); !strings.Contains(why, "Your key file ~/.ssh/id_rsa never got a turn.") {
		t.Errorf("why: %s", why)
	}
	if got := r.Fix[0].Cmd; got != "lapilli ssh -i ~/.ssh/id_rsa -o IdentitiesOnly=yes -p 2323 alice@127.0.0.1" {
		t.Errorf("first step: %q", got)
	}
}

// A config copied from a Mac to Linux: ssh stops at UseKeychain.
func TestConfigUnknownOption(t *testing.T) {
	tr, _ := trace(t, "config_unknown_option")
	r := Diagnose(Input{Trace: tr, Home: "/home/alice"})
	if r.Headline != "ssh stops at line 3 of ~/.ssh/config: this ssh has no option UseKeychain." {
		t.Errorf("headline: %q", r.Headline)
	}
	if !strings.Contains(r.Why[0], "macOS") {
		t.Errorf("why: %q", r.Why)
	}
	if r.Fix[0].Cmd != "IgnoreUnknown UseKeychain" || !strings.Contains(r.Fix[0].Text, "above line 3") {
		t.Errorf("fix: %+v", r.Fix[0])
	}
}

func TestLooseKeyNote(t *testing.T) {
	tr, _ := trace(t, "ok_file")
	other := "/home/alice/.ssh/id_rsa"
	files := map[string]local.KeyFile{other: {Path: other, Exists: true, OwnedByUser: true, Format: local.FormatOpenSSH, Mode: 0o644, ModeString: "0644"}}
	r := Diagnose(Input{Trace: tr, Files: files, Home: "/home/alice"})
	if len(r.Also) != 1 || r.Also[0].Cmd != "chmod 600 ~/.ssh/id_rsa" {
		t.Errorf("notes: %+v", r.Also)
	}
}
