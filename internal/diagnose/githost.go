package diagnose

import "strings"

// GitHost is a hosted git service whose SSH behaviour lapilli knows.
type GitHost struct {
	Name    string
	Hosts   []string
	User    string // the only user name the service accepts
	KeysURL string // where a user adds a public key
	AltHost string // SSH over port 443, for networks that block port 22
	AltPort int
	// Published host key fingerprints. GitHub's come from api.github.com/meta;
	// the others from ssh-keyscan, matched against each service's documentation
	// (checked 2026-10-16).
	Fingerprints []string
}

var gitHosts = []GitHost{
	{
		Name: "GitHub", Hosts: []string{"github.com", "ssh.github.com"}, User: "git",
		KeysURL: "https://github.com/settings/ssh/new", AltHost: "ssh.github.com", AltPort: 443,
		Fingerprints: []string{
			"SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU",
			"SHA256:p2QAMXNIC1TJYWeIOttrVc98/R1BUFWu3/LiyKgUfQM",
			"SHA256:uNiVztksCsDhcc0u9e8BujQXVUpKZIDTMczCvj3tD2s",
		},
	},
	{
		Name: "GitLab", Hosts: []string{"gitlab.com", "altssh.gitlab.com"}, User: "git",
		KeysURL: "https://gitlab.com/-/user_settings/ssh_keys", AltHost: "altssh.gitlab.com", AltPort: 443,
		Fingerprints: []string{
			"SHA256:eUXGGm1YGsMAS7vkcx6JOJdOGHPem5gQp4taiCfCLB8",
			"SHA256:HbW3g8zUjNSksFbqTiUWPWg2Bq1x8xdGUrliXFzSnUw",
			"SHA256:ROQFvPThGrW4RuWLoL9tq9I9zJ42fK4XywyRtbOz/EQ",
		},
	},
	{
		Name: "Bitbucket", Hosts: []string{"bitbucket.org", "altssh.bitbucket.org"}, User: "git",
		KeysURL: "https://bitbucket.org/account/settings/ssh-keys/", AltHost: "altssh.bitbucket.org", AltPort: 443,
		Fingerprints: []string{
			"SHA256:ybgmFkzwOSotHTHLJgHO0QN8L0xErw6vd0VhFA9m3SM",
			"SHA256:FC73VB6C4OQLSCrjEayhMp9UMxS97caD/Yyi2bhW/J0",
			"SHA256:46OSHA1Rmj8E8ERTC6xkNcmGOw9oFxYr0WF6zWW8l1E",
		},
	},
	{
		Name: "Codeberg", Hosts: []string{"codeberg.org"}, User: "git",
		KeysURL: "https://codeberg.org/user/settings/keys",
		Fingerprints: []string{
			"SHA256:mIlxA9k46MmM6qdJOdMnAQpzGxF4WIVVL+fj+wZbw0g",
			"SHA256:T9FYDEHELhVkulEKKwge5aVhVTbqCW0MIRwAfpARs/E",
			"SHA256:6QQmYi4ppFS4/+zSZ5S4IU+4sa6rwvQ4PbhCtPEBekQ",
		},
	},
	{Name: "SourceHut", Hosts: []string{"git.sr.ht"}, User: "git", KeysURL: "https://meta.sr.ht/keys"},
}

// LookupGitHost finds the service for a host name (after ssh's HostName is applied).
func LookupGitHost(host string) *GitHost {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for i := range gitHosts {
		for _, h := range gitHosts[i].Hosts {
			if h == host {
				return &gitHosts[i]
			}
		}
	}
	return nil
}

// Publishes reports whether fp is one of the service's published host keys.
func (g *GitHost) Publishes(fp string) bool {
	for _, f := range g.Fingerprints {
		if f == fp {
			return true
		}
	}
	return false
}
