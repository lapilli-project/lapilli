// Package local looks at the client machine: the configuration ssh will apply
// (ssh -G), the key files it may use, and the keys an agent holds.
// Nothing here writes a file or talks to the server.
package local

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// SSHConfig is the effective client configuration for one destination, as ssh -G prints it.
type SSHConfig struct {
	User                     string   `json:"user"`
	HostName                 string   `json:"hostname"`
	Port                     int      `json:"port"`
	IdentityFiles            []string `json:"identity_files,omitempty"` // expanded paths, in ssh's order
	IdentitiesOnly           bool     `json:"identities_only"`
	IdentityAgent            string   `json:"identity_agent,omitempty"` // "" means $SSH_AUTH_SOCK, "none" disables the agent
	CertificateFiles         []string `json:"certificate_files,omitempty"`
	PubkeyAcceptedAlgorithms []string `json:"-"`
	ProxyJump                string   `json:"proxy_jump,omitempty"`
	ProxyCommand             string   `json:"proxy_command,omitempty"`
	StrictHostKeyChecking    string   `json:"strict_host_key_checking,omitempty"`
	AddKeysToAgent           string   `json:"add_keys_to_agent,omitempty"`
}

// ResolveConfig runs ssh -G with the same options the probe will use.
func ResolveConfig(ctx context.Context, sshArgs []string, dest, home, localUser string) (*SSHConfig, error) {
	args := append([]string{"-G"}, sshArgs...)
	args = append(args, "--", dest)
	out, err := exec.CommandContext(ctx, "ssh", args...).Output()
	if err != nil {
		return nil, err
	}
	return ParseG(bytes.NewReader(out), home, localUser), nil
}

// ParseG reads the output of ssh -G.
func ParseG(r io.Reader, home, localUser string) *SSHConfig {
	c := &SSHConfig{Port: 22}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var idfiles []string
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		switch strings.ToLower(key) {
		case "user":
			c.User = val
		case "hostname":
			c.HostName = val
		case "port":
			c.Port, _ = strconv.Atoi(val)
		case "identityfile":
			idfiles = append(idfiles, val)
		case "identitiesonly":
			c.IdentitiesOnly = val == "yes"
		case "identityagent":
			c.IdentityAgent = val
		case "certificatefile":
			c.CertificateFiles = append(c.CertificateFiles, val)
		case "pubkeyacceptedalgorithms", "pubkeyacceptedkeytypes":
			c.PubkeyAcceptedAlgorithms = strings.Split(val, ",")
		case "proxyjump":
			c.ProxyJump = val
		case "proxycommand":
			c.ProxyCommand = val
		case "stricthostkeychecking":
			c.StrictHostKeyChecking = val
		case "addkeystoagent":
			c.AddKeysToAgent = val
		}
	}
	for _, f := range idfiles {
		c.IdentityFiles = append(c.IdentityFiles, ExpandPath(f, home, localUser, c.User, c.HostName))
	}
	for i, f := range c.CertificateFiles {
		c.CertificateFiles[i] = ExpandPath(f, home, localUser, c.User, c.HostName)
	}
	if c.IdentityAgent != "" && c.IdentityAgent != "none" && c.IdentityAgent != "SSH_AUTH_SOCK" && !strings.HasPrefix(c.IdentityAgent, "$") {
		c.IdentityAgent = ExpandPath(c.IdentityAgent, home, localUser, c.User, c.HostName)
	}
	return c
}

// ExpandPath expands the tokens ssh allows in IdentityFile and similar options.
func ExpandPath(p, home, localUser, remoteUser, host string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		p = filepath.Join(home, p[2:])
	}
	if !strings.Contains(p, "%") {
		return p
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		if p[i] != '%' || i+1 == len(p) {
			b.WriteByte(p[i])
			continue
		}
		i++
		switch p[i] {
		case 'd':
			b.WriteString(home)
		case 'u':
			b.WriteString(localUser)
		case 'r':
			b.WriteString(remoteUser)
		case 'h':
			b.WriteString(host)
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(p[i])
		}
	}
	return b.String()
}
