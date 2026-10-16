package local

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// AgentKey is one key an agent holds.
type AgentKey struct {
	Bits        int    `json:"bits"`
	Fingerprint string `json:"fingerprint"`
	Comment     string `json:"comment"`
	Type        string `json:"type"`
}

// Agent describes the agent ssh would use for this destination.
type Agent struct {
	Socket   string     `json:"socket,omitempty"`
	Disabled bool       `json:"disabled,omitempty"` // IdentityAgent none
	Running  bool       `json:"running"`
	Keys     []AgentKey `json:"keys,omitempty"`
	Kind     string     `json:"kind,omitempty"` // 1password, gpg-agent, secretive, bitwarden, ssh-agent
	Err      string     `json:"error,omitempty"`
}

// Has reports whether the agent holds a key with this fingerprint.
func (a *Agent) Has(fp string) bool {
	if a == nil || fp == "" {
		return false
	}
	for _, k := range a.Keys {
		if k.Fingerprint == fp {
			return true
		}
	}
	return false
}

// ListAgent asks the agent which keys it holds, using ssh-add -l.
// identityAgent is IdentityAgent from ssh -G ("" means $SSH_AUTH_SOCK).
func ListAgent(ctx context.Context, identityAgent string) *Agent {
	a := &Agent{}
	sock := os.Getenv("SSH_AUTH_SOCK")
	switch {
	case identityAgent == "none":
		a.Disabled = true
		return a
	case identityAgent == "" || identityAgent == "SSH_AUTH_SOCK" || identityAgent == "$SSH_AUTH_SOCK":
	case strings.HasPrefix(identityAgent, "$"):
		sock = os.Getenv(strings.TrimPrefix(identityAgent, "$"))
	default:
		sock = identityAgent
	}
	a.Socket = sock
	a.Kind = agentKind(sock)
	if sock == "" {
		a.Err = "no agent: SSH_AUTH_SOCK is not set"
		return a
	}
	cmd := exec.CommandContext(ctx, "ssh-add", "-l", "-E", "sha256")
	cmd.Env = append(os.Environ(), "SSH_AUTH_SOCK="+sock)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		a.Running = true
		a.Keys = parseAgentList(string(out))
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		// "The agent has no identities."
		a.Running = true
	default:
		a.Err = strings.TrimSpace(string(out))
		if a.Err == "" {
			a.Err = err.Error()
		}
	}
	return a
}

// parseAgentList reads lines like "256 SHA256:abc comment words (ED25519)".
func parseAgentList(s string) []AgentKey {
	var keys []AgentKey
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 || !strings.HasPrefix(f[1], "SHA256:") {
			continue
		}
		k := AgentKey{Fingerprint: f[1]}
		k.Bits, _ = strconv.Atoi(f[0])
		rest := f[2:]
		if last := rest[len(rest)-1]; strings.HasPrefix(last, "(") && strings.HasSuffix(last, ")") {
			k.Type = strings.Trim(last, "()")
			rest = rest[:len(rest)-1]
		}
		k.Comment = strings.Join(rest, " ")
		keys = append(keys, k)
	}
	return keys
}

func agentKind(sock string) string {
	s := strings.ToLower(sock)
	switch {
	case s == "":
		return ""
	case strings.Contains(s, "1password") || strings.Contains(s, "2bua8c4s2c"):
		return "1password"
	case strings.Contains(s, "gpg") || strings.Contains(s, "gnupg"):
		return "gpg-agent"
	case strings.Contains(s, "secretive"):
		return "secretive"
	case strings.Contains(s, "bitwarden"):
		return "bitwarden"
	case strings.Contains(s, "keepassxc"):
		return "keepassxc"
	case strings.Contains(s, "com.apple.launchd"):
		return "macos"
	}
	return "ssh-agent"
}
