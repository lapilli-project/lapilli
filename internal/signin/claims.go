package signin

import (
	"errors"
	"fmt"
	"strings"
)

// Identity is who a request comes from, as a sign-in vouched for it: the
// name changes are recorded under, and the groups roles are given by.
type Identity struct {
	User   string
	Groups []string
}

const (
	// maxGroups is how many groups are taken from a token.
	maxGroups = 1000
	// maxName is how long a name or a group may be, as the ledger keeps
	// names.
	maxName = 200
)

// Claims say what a token's claims hold about who: UserClaim names the
// claim taken as the person's name, GroupsClaim the one their groups are
// in ("" for none). With Verified, a name taken from the email claim needs
// email_verified true beside it: an identity provider that lets people
// write their own address, unchecked, would otherwise let them take
// another's, an admin's.
type Claims struct {
	UserClaim, GroupsClaim string
	Verified               bool
}

// ErrNoName is a token that names no one lapilli can take.
var ErrNoName = errors.New("no name")

// Identity reads who a token's claims are.
func (c Claims) Identity(claims map[string]any) (Identity, error) {
	user, ok := claims[c.UserClaim].(string)
	if !ok || strings.TrimSpace(user) == "" {
		return Identity{}, fmt.Errorf("%w: the token has no %s claim", ErrNoName, c.UserClaim)
	}
	if c.Verified && c.UserClaim == "email" && !verified(claims["email_verified"]) {
		return Identity{}, fmt.Errorf("%w: the identity provider doesn't say the email %q is verified (email_verified); take another claim it vouches for, such as preferred_username or sub, with --user-claim", ErrNoName, user)
	}
	if len(user) > maxName {
		return Identity{}, fmt.Errorf("%w: the %s claim is over %d bytes", ErrNoName, c.UserClaim, maxName)
	}
	id := Identity{User: user}
	if c.GroupsClaim == "" {
		return id, nil
	}
	var groups []any
	switch g := claims[c.GroupsClaim].(type) {
	case []any:
		groups = g
	case string:
		groups = []any{g} // one group, as some providers send it
	}
	for _, g := range groups {
		s, ok := g.(string)
		if s = strings.TrimSpace(s); !ok || s == "" || len(s) > maxName {
			continue
		}
		id.Groups = append(id.Groups, s)
		if len(id.Groups) == maxGroups {
			break
		}
	}
	return id, nil
}

// verified reads email_verified, which some providers send as a string.
func verified(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return strings.EqualFold(x, "true")
	}
	return false
}
