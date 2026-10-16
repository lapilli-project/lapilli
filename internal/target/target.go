// Package target reads what the user typed: ssh options to pass through and a
// destination, which may also be a git remote URL.
package target

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Target is a parsed command line.
type Target struct {
	Dest      string   // what ssh is given: [user@]host
	SSHArgs   []string // pass-through ssh options, including -p from a URL
	User      string   // user given explicitly (user@, -l, or a URL), "" if none
	Host      string   // host or alias as typed
	Port      int      // port given explicitly, 0 if none
	FromURL   string   // the git remote URL the destination came from, if any
	IdentityF []string // -i files, in order
}

// ssh options lapilli passes through, and whether each takes a value.
var passThrough = map[byte]bool{
	'i': true, 'p': true, 'l': true, 'F': true, 'o': true, 'J': true,
	'4': false, '6': false,
}

// ssh options that would change what lapilli is testing or start something else.
var refused = "AaCfGgKkMNnqsTtVvXxYyBbcDEeILmOPQRSWw"

// Parse reads ssh-style arguments: options, then one destination.
func Parse(args []string) (*Target, error) {
	t := &Target{}
	var dest string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if dest != "" {
			return nil, fmt.Errorf("unexpected %q after the destination: lapilli does not run remote commands", a)
		}
		if a == "--" {
			if i+1 < len(args) {
				dest = args[i+1]
				if i+2 < len(args) {
					return nil, fmt.Errorf("unexpected %q after the destination: lapilli does not run remote commands", args[i+2])
				}
			}
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			dest = a
			continue
		}
		// one option, possibly with its value attached (-p2222, -oFoo=bar)
		opt := a[1]
		takes, ok := passThrough[opt]
		if !ok {
			if strings.IndexByte(refused, opt) >= 0 {
				return nil, fmt.Errorf("ssh option -%c is not supported by lapilli ssh", opt)
			}
			return nil, fmt.Errorf("unknown option %s", a)
		}
		if !takes {
			if len(a) > 2 {
				return nil, fmt.Errorf("combined options like %s are not supported; give them separately", a)
			}
			t.SSHArgs = append(t.SSHArgs, a)
			continue
		}
		val := a[2:]
		if val == "" {
			if i+1 >= len(args) {
				return nil, fmt.Errorf("option -%c needs a value", opt)
			}
			i++
			val = args[i]
		}
		switch opt {
		case 'l':
			t.User = val
		case 'p':
			p, err := strconv.Atoi(val)
			if err != nil || p <= 0 || p > 65535 {
				return nil, fmt.Errorf("bad port %q", val)
			}
			t.Port = p
		case 'i':
			t.IdentityF = append(t.IdentityF, val)
		}
		t.SSHArgs = append(t.SSHArgs, "-"+string(opt), val)
	}
	if dest == "" {
		return t, nil
	}
	return t, t.setDest(dest)
}

// SetDest fills in the destination; used when it comes from a git remote.
func (t *Target) SetDest(d string) error { return t.setDest(d) }

func (t *Target) setDest(d string) error {
	switch {
	case strings.HasPrefix(d, "ssh://"), strings.HasPrefix(d, "git+ssh://"), strings.HasPrefix(d, "ssh+git://"):
		u, err := url.Parse(d)
		if err != nil || u.Hostname() == "" {
			return fmt.Errorf("can't read %q as an ssh URL", d)
		}
		t.FromURL = d
		t.Host = u.Hostname()
		if u.User != nil {
			t.setUser(u.User.Username())
		}
		if p := u.Port(); p != "" {
			n, err := strconv.Atoi(p)
			if err != nil {
				return fmt.Errorf("bad port in %q", d)
			}
			if t.Port == 0 {
				t.Port = n
				t.SSHArgs = append(t.SSHArgs, "-p", p)
			}
		}
	case strings.Contains(d, "://"):
		scheme, _, _ := strings.Cut(d, "://")
		return fmt.Errorf("%q is a %s address; ssh is not involved", d, scheme)
	case strings.HasPrefix(d, "["):
		// [::1] or user@[::1] is not a thing ssh accepts on the command line; keep it simple
		end := strings.IndexByte(d, ']')
		if end < 0 {
			return errors.New("unclosed [ in destination")
		}
		t.Host = d[1:end]
	default:
		hostPart := d
		if user, host, ok := strings.Cut(d, "@"); ok && !strings.Contains(user, ":") {
			t.setUser(user)
			hostPart = host
		}
		// git's scp-like form: [user@]host:path
		if host, path, ok := strings.Cut(hostPart, ":"); ok {
			if path == "" || strings.Contains(host, "/") {
				return fmt.Errorf("can't read %q as a destination", d)
			}
			t.FromURL = d
			hostPart = host
		}
		t.Host = hostPart
	}
	if t.Host == "" {
		return fmt.Errorf("no host in %q", d)
	}
	t.Dest = t.Host
	if t.User != "" {
		t.Dest = t.User + "@" + t.Host
	}
	return nil
}

func (t *Target) setUser(u string) {
	if t.User == "" { // -l given first wins, as with ssh
		t.User = u
	}
}
