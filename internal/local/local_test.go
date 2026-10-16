package local

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseG(t *testing.T) {
	in := `user git
hostname github.com
port 443
identitiesonly yes
identityfile ~/.ssh/id_work
identityfile %d/.ssh/id_%r
identityagent ~/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock
stricthostkeychecking accept-new
`
	c := ParseG(strings.NewReader(in), "/home/a", "alice")
	if c.User != "git" || c.HostName != "github.com" || c.Port != 443 || !c.IdentitiesOnly {
		t.Fatalf("basic fields: %+v", c)
	}
	if got := strings.Join(c.IdentityFiles, ","); got != "/home/a/.ssh/id_work,/home/a/.ssh/id_git" {
		t.Errorf("identity files: %s", got)
	}
	if c.IdentityAgent != "/home/a/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock" {
		t.Errorf("agent: %s", c.IdentityAgent)
	}
	if agentKind(c.IdentityAgent) != "1password" {
		t.Errorf("agent kind: %s", agentKind(c.IdentityAgent))
	}
}

func TestParseAgentList(t *testing.T) {
	keys := parseAgentList("256 SHA256:abc alice@laptop (ED25519)\n3072 SHA256:def my work key (RSA)\n")
	if len(keys) != 2 || keys[1].Comment != "my work key" || keys[1].Type != "RSA" || keys[1].Bits != 3072 {
		t.Fatalf("%+v", keys)
	}
}

// TestInspectKey compares lapilli's reading of key files with ssh-keygen's.
func TestInspectKey(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not installed")
	}
	dir := t.TempDir()
	gen := func(name string, args ...string) string {
		p := filepath.Join(dir, name)
		cmd := exec.Command("ssh-keygen", append([]string{"-q", "-f", p, "-C", name}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen %s: %v %s", name, err, out)
		}
		return p
	}
	fingerprint := func(p string) string {
		out, err := exec.Command("ssh-keygen", "-l", "-E", "sha256", "-f", p).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.Fields(string(out))[1]
	}

	ed := gen("ed", "-t", "ed25519", "-N", "")
	enc := gen("enc", "-t", "ed25519", "-N", "secret")
	rsaKey := gen("rsa", "-t", "rsa", "-b", "2048", "-N", "")
	pemRSA := gen("pemrsa", "-t", "rsa", "-b", "2048", "-N", "", "-m", "PEM")
	pemEC := gen("pemec", "-t", "ecdsa", "-b", "384", "-N", "", "-m", "PEM")
	pemEnc := gen("pemenc", "-t", "rsa", "-b", "2048", "-N", "secret", "-m", "PEM")

	cases := []struct {
		path      string
		typ       string
		bits      int
		encrypted bool
		format    string
	}{
		{ed, "ED25519", 256, false, FormatOpenSSH},
		{enc, "ED25519", 256, true, FormatOpenSSH},
		{rsaKey, "RSA", 2048, false, FormatOpenSSH},
		{pemRSA, "RSA", 2048, false, FormatPEM},
		{pemEC, "ECDSA", 384, false, FormatPEM},
		{pemEnc, "RSA", 2048, true, FormatPEM},
		{ed + ".pub", "ED25519", 256, false, FormatPublic},
	}
	for _, c := range cases {
		k := InspectKey(c.path)
		if k.Type != c.typ || k.Bits != c.bits || k.Encrypted != c.encrypted || k.Format != c.format {
			t.Errorf("%s: got %s/%d enc=%v %s", filepath.Base(c.path), k.Type, k.Bits, k.Encrypted, k.Format)
		}
		if want := fingerprint(c.path); k.Fingerprint != want {
			t.Errorf("%s: fingerprint %s, ssh-keygen says %s", filepath.Base(c.path), k.Fingerprint, want)
		}
	}

	// The fingerprint of an OpenSSH key comes from the file itself, even without .pub.
	os.Remove(enc + ".pub")
	if k := InspectKey(enc); k.Fingerprint == "" || k.PubExists {
		t.Errorf("encrypted key without .pub: %+v", k)
	}
	// Permissions ssh refuses.
	os.Chmod(ed, 0o644)
	if k := InspectKey(ed); !k.TooOpen() || k.ModeString != "0644" {
		t.Errorf("0644 key not flagged: %+v", k)
	}
	// A .pub that belongs to another key.
	pub, _ := os.ReadFile(rsaKey + ".pub")
	os.WriteFile(ed+".pub", pub, 0o644)
	if k := InspectKey(ed); !k.PubMismatch {
		t.Errorf("mismatched .pub not flagged")
	}
	// PuTTY keys are recognised, not parsed.
	ppk := filepath.Join(dir, "k.ppk")
	os.WriteFile(ppk, []byte("PuTTY-User-Key-File-3: ssh-ed25519\nEncryption: none\nComment: x\n"), 0o600)
	if k := InspectKey(ppk); k.Format != FormatPuTTY || k.Type != "ED25519" || k.Encrypted {
		t.Errorf("ppk: %+v", k)
	}
	if keys := ScanKeys(dir); len(keys) != 7 {
		var names []string
		for _, k := range keys {
			names = append(names, filepath.Base(k.Path))
		}
		t.Errorf("ScanKeys found %v", names)
	}
}
