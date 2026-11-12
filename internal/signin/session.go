package signin

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Cookies keeps sessions, and sign-ins under way, in cookies signed with a
// key only lapilli holds: HttpOnly, SameSite=Lax, and Secure, under the
// __Host- prefix, when --external-url is https.
type Cookies struct {
	session, login []byte // keys, one a purpose, made from the key given
	Secure         bool
	MaxAge         time.Duration
	Now            func() time.Time
}

// NewCookies signs cookies with a key, 32 bytes or more.
func NewCookies(key []byte, secure bool, maxAge time.Duration, now func() time.Time) (*Cookies, error) {
	if len(key) < 32 {
		return nil, errors.New("a session key is 32 bytes at least")
	}
	sub := func(purpose string) []byte {
		m := hmac.New(sha256.New, key)
		m.Write([]byte("lapilli/" + purpose + "/v1"))
		return m.Sum(nil)
	}
	return &Cookies{session: sub("session"), login: sub("login"), Secure: secure, MaxAge: maxAge, Now: now}, nil
}

// Names of the cookies.
func (c *Cookies) name(what string) string {
	if c.Secure {
		return "__Host-lapilli-" + what
	}
	return "lapilli-" + what
}

// SessionName is the session cookie's name.
func (c *Cookies) SessionName() string { return c.name("session") }

// LoginName is the cookie a sign-in under way is kept in, named by its
// state: two tabs signing in at once each keep their own.
func (c *Cookies) LoginName(state string) string {
	sum := sha256.Sum256([]byte(state))
	return c.name("login-" + base64.RawURLEncoding.EncodeToString(sum[:9]))
}

// Session is who signed in, until when.
type Session struct {
	ID     string   `json:"sid"`
	User   string   `json:"u"`
	Groups []string `json:"g,omitempty"`
	At     int64    `json:"iat"`
	Until  int64    `json:"exp"`
	// Made is when, in Unix milliseconds: a session made before its person
	// signed out is ended, one made after isn't.
	Made int64 `json:"t"`
}

// Login is a sign-in under way: its secrets, the page to go back to, and
// until when it may finish.
type Login struct {
	Start
	Next  string `json:"next"`
	Until int64  `json:"exp"`
}

// loginFor is how long a sign-in may take.
const loginFor = 10 * time.Minute

// maxCookie is how long a cookie's value may be: browsers keep 4 KB a
// cookie, name and attributes included.
const maxCookie = 3800

// ErrTooBig is a session too big for its cookie.
var ErrTooBig = errors.New("the session is too big for a cookie")

// Issue gives the browser a session for id, and says it.
func (c *Cookies) Issue(w http.ResponseWriter, id Identity) (Session, error) {
	now := c.Now()
	s := Session{ID: random(16), User: id.User, Groups: id.Groups, At: now.Unix(), Until: now.Add(c.MaxAge).Unix(), Made: now.UnixMilli()}
	v, err := c.seal(c.session, c.SessionName(), s)
	if err != nil {
		return Session{}, err
	}
	if len(v) > maxCookie {
		return Session{}, ErrTooBig
	}
	c.set(w, c.SessionName(), v, c.MaxAge)
	return s, nil
}

// Read is the request's session: none when its cookie is missing, isn't
// signed with this key, or has expired.
func (c *Cookies) Read(r *http.Request) (Session, bool) {
	var s Session
	if !c.open(r, c.session, c.SessionName(), &s) {
		return Session{}, false
	}
	now := c.Now().Unix()
	// Not before it was made (a minute for clocks), nor longer than
	// sessions last now: a cookie made when they lasted longer isn't kept
	// past it.
	if s.User == "" || s.ID == "" || s.Until <= now || s.At > now+60 || now-s.At >= int64(c.MaxAge/time.Second) {
		return Session{}, false
	}
	return s, true
}

// End takes the session cookie away.
func (c *Cookies) End(w http.ResponseWriter) { c.set(w, c.SessionName(), "", -1) }

// Begin keeps a sign-in under way in its cookie.
func (c *Cookies) Begin(w http.ResponseWriter, st Start, next string) error {
	name := c.LoginName(st.State)
	v, err := c.seal(c.login, name, Login{Start: st, Next: next, Until: c.Now().Add(loginFor).Unix()})
	if err != nil {
		return err
	}
	c.set(w, name, v, loginFor)
	return nil
}

// Take is the sign-in under way the browser came back with the state of,
// taken away: none when there is none, it isn't signed with this key, it
// holds another state, or it took too long.
func (c *Cookies) Take(w http.ResponseWriter, r *http.Request, state string) (Login, bool) {
	if state == "" {
		return Login{}, false
	}
	name := c.LoginName(state)
	var l Login
	ok := c.open(r, c.login, name, &l)
	if _, err := r.Cookie(name); err == nil {
		c.set(w, name, "", -1)
	}
	if !ok || l.Until <= c.Now().Unix() || l.Nonce == "" || l.Verifier == "" ||
		subtle.ConstantTimeCompare([]byte(l.State), []byte(state)) != 1 {
		return Login{}, false
	}
	return l, true
}

// set sets a cookie, or takes it away with maxAge below zero.
func (c *Cookies) set(w http.ResponseWriter, name, value string, maxAge time.Duration) {
	age := int(maxAge / time.Second)
	if maxAge < 0 {
		age = -1
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: age, HttpOnly: true,
		Secure: c.Secure, SameSite: http.SameSiteLaxMode})
}

// seal is a value signed with a key, for the cookie it is named by: one
// cookie's value can't stand for another's.
func (c *Cookies) seal(key []byte, name string, v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(b)
	return body + "." + base64.RawURLEncoding.EncodeToString(mac(key, name, body)), nil
}

// open reads a cookie sealed with key into v, reporting whether it was
// there and signed so.
func (c *Cookies) open(r *http.Request, key []byte, name string, v any) bool {
	ck, err := r.Cookie(name)
	if err != nil || len(ck.Value) > maxCookie {
		return false
	}
	body, sig, ok := strings.Cut(ck.Value, ".")
	if !ok {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, mac(key, name, body)) {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(body)
	return err == nil && json.Unmarshal(b, v) == nil
}

func mac(key []byte, name, body string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(name))
	m.Write([]byte{0})
	m.Write([]byte(body))
	return m.Sum(nil)
}

// random is n random bytes, as base64url.
func random(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Random is n random bytes, as base64url.
func Random(n int) string { return random(n) }
