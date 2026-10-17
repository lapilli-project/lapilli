package target

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		args    string
		dest    string
		user    string
		port    int
		sshArgs string
		fromURL bool
		err     string
	}{
		{args: "alice@example.com", dest: "alice@example.com", user: "alice"},
		{args: "example.com", dest: "example.com"},
		{args: "-p 2222 -i ~/.ssh/k bob@h", dest: "bob@h", user: "bob", port: 2222, sshArgs: "-p 2222 -i ~/.ssh/k"},
		{args: "-p2222 h", dest: "h", port: 2222, sshArgs: "-p 2222"},
		{args: "-l carol h", dest: "carol@h", user: "carol", sshArgs: "-l carol"},
		{args: "git@github.com:owner/repo.git", dest: "git@github.com", user: "git", fromURL: true},
		{args: "ssh://git@gitlab.example.com:2222/group/repo.git", dest: "git@gitlab.example.com", user: "git", port: 2222, sshArgs: "-p 2222", fromURL: true},
		{args: "-o IdentitiesOnly=yes -4 h", dest: "h", sshArgs: "-o IdentitiesOnly=yes -4"},
		{args: "https://github.com/owner/repo.git", err: "https address"},
		{args: "h uptime", err: "does not run remote commands"},
		{args: "-L 8080:localhost:80 h", err: "-L is not supported"},
		{args: "-p", err: "needs a value"},
		// names a shell would run, as a repository's origin can carry them
		{args: "git@github.com$(id):owner/repo.git", err: "can't be a host or user name"},
		{args: "ssh://-oProxyCommand=id@example.com/repo.git", err: "can't be a host or user name"},
		{args: "-- -oProxyCommand=id", err: "can't be a host or user name"},
		{args: "bob;id@h", err: "can't be a host or user name"},
		{args: "h`id`", err: "can't be a host or user name"},
	}
	for _, c := range cases {
		tg, err := Parse(strings.Fields(c.args))
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%q: error %v, want %q", c.args, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.args, err)
			continue
		}
		if tg.Dest != c.dest || tg.User != c.user || tg.Port != c.port || strings.Join(tg.SSHArgs, " ") != c.sshArgs || (tg.FromURL != "") != c.fromURL {
			t.Errorf("%q: got dest=%q user=%q port=%d args=%q url=%q", c.args, tg.Dest, tg.User, tg.Port, strings.Join(tg.SSHArgs, " "), tg.FromURL)
		}
	}
}
