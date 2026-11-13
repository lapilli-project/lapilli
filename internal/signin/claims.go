package signin

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// Identity is who a request comes from, as a sign-in vouched for it: the
// name changes are recorded under, and the groups roles are given by.
type Identity struct {
	User   string
	Groups []string
	// IssuedAt is when the token said who was issued (its iat), zero when
	// it didn't say: a token issued before its person signed out is
	// refused.
	IssuedAt time.Time
	// Overage says the provider sent a link to the groups in place of the
	// groups (Microsoft Entra ID's, past 200): none is taken.
	Overage bool
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
// in ("" for none). A name taken from the email claim is refused beside
// email_verified false, whoever signed the token; with Verified, it needs
// email_verified true: an identity provider that lets people write their
// own address, unchecked, would otherwise let them take another's, an
// admin's.
type Claims struct {
	UserClaim, GroupsClaim string
	Verified               bool
}

// ErrNoName is a token that names no one lapilli can take.
var ErrNoName = errors.New("no name")

// Plain reports whether a name's spacing shows as what it is: no space
// before or after it, no space but the ASCII one (U+00A0, U+3000 and the
// other separators look like it), no control character, nothing that
// wasn't UTF-8. One that isn't could read as another's, an admin's, in a
// comment or an approval. Characters that don't show (a zero-width space,
// a soft hyphen) are lapilli serve's to judge (ledger.Hidden), which tells
// them from the joiners names in some scripts are written with.
func Plain(name string) bool {
	if name == "" || strings.TrimSpace(name) != name {
		return false
	}
	for _, r := range name {
		switch {
		case r == ' ':
		case unicode.Is(unicode.Zs, r), unicode.IsControl(r), r == unicode.ReplacementChar:
			return false
		}
	}
	return true
}

// EqualFoldASCII reports whether a and b are the same but for the case of
// their ASCII letters, as names in DNS are: strings.EqualFold folds by
// Unicode, and takes the Kelvin sign (U+212A) for a k and a long s
// (U+017F) for an s, so that a domain written with them would pass for
// another's.
func EqualFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// Identity reads who a token's claims are.
func (c Claims) Identity(claims map[string]any) (Identity, error) {
	user, ok := claims[c.UserClaim].(string)
	if !ok || strings.TrimSpace(user) == "" {
		return Identity{}, fmt.Errorf("%w: the token has no %s claim", ErrNoName, c.UserClaim)
	}
	if c.UserClaim == "email" {
		v, said := claims["email_verified"]
		switch {
		case said && !verified(v):
			return Identity{}, fmt.Errorf("%w: the token says the email %q isn't verified (email_verified)", ErrNoName, user)
		case c.Verified && !said:
			return Identity{}, fmt.Errorf("%w: the identity provider doesn't say the email %q is verified (email_verified); take another claim it vouches for, such as preferred_username or oid, with --user-claim", ErrNoName, user)
		}
	}
	if len(user) > maxName {
		return Identity{}, fmt.Errorf("%w: the %s claim is over %d bytes", ErrNoName, c.UserClaim, maxName)
	}
	if !Plain(user) {
		return Identity{}, fmt.Errorf("%w: the %s claim %q has spaces around it, or a character that looks like a space or like nothing", ErrNoName, c.UserClaim, user)
	}
	id := Identity{User: user}
	if iat, ok := claims["iat"].(float64); ok {
		id.IssuedAt = time.Unix(int64(iat), 0)
	}
	if c.GroupsClaim == "" {
		return id, nil
	}
	var groups []any
	switch g := claims[c.GroupsClaim].(type) {
	case []any:
		groups = g
	case string:
		groups = []any{g} // one group, as some providers send it
	case nil:
		if names, ok := claims["_claim_names"].(map[string]any); ok && names[c.GroupsClaim] != nil {
			id.Overage = true
		}
	}
	for _, g := range groups {
		s, ok := g.(string)
		if !ok || len(s) > maxName || !Plain(s) {
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
