// Package identity holds the hub's request-time authorization primitives: the
// Claims a request carries (resolved once by the hub gate from an OIDC session,
// an admin push token, a user token, or a CF-forwarded actor) and the pure
// CanView/CanWrite functions that decide, from those claims plus a canvas's
// stored owner+grants, whether the request may see or mutate the canvas.
//
// Enforcement is deliberately claims-only: CanView/CanWrite read the request's
// claims and the canvas's stored metadata and nothing else -- they never call
// out to the IdP or any registry, so access decisions are unchanged with the
// identity provider unreachable.
package identity

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"

	"github.com/jedwards1230/scrim/internal/canvas"
)

// Claims is the authenticated identity resolved for a request. Its zero value
// is the anonymous principal (no subject, no email, not admin) -- a caller who
// presented no valid credential.
type Claims struct {
	// Subject is the IdP `sub` (browser session) or the CF-forwarded user id;
	// may be empty.
	Subject string
	// Email is the principal id used for ownership and user/everyone grants.
	Email string
	// Name is a display name, best-effort, for UI only -- never consulted by
	// CanView/CanWrite.
	Name string
	// Groups are the principal's group memberships, matched against group grants.
	Groups []string
	// Admin is true ONLY for the global push token (the machine/bootstrap
	// credential); it is a visibility superuser and may write any canvas.
	Admin bool
}

// Authenticated reports whether the claims identify any principal at all --
// admin, a session/token subject, or an email. An anonymous caller (zero
// Claims) is not authenticated; it can still view a canvas only by presenting a
// valid share-link secret.
func (c Claims) Authenticated() bool {
	return c.Admin || c.Email != "" || c.Subject != ""
}

// CanView reports whether claims may view a canvas with the given owner and
// grants. presentedLinkSecret is the raw secret from a share-link URL (?k=…),
// or "" when none was presented.
//
// Rules, in order: admin sees everything; the owner sees their own canvas; then
// each grant is consulted -- everyone (any authenticated caller), user (email
// match), group (group membership), link (the presented secret hashes to the
// grant's stored hash, compared in constant time). An anonymous caller (empty
// email, no matching link) can view only via a valid link secret.
func CanView(owner string, grants []canvas.Grant, c Claims, presentedLinkSecret string) bool {
	if c.Admin {
		return true
	}
	if owner != "" && owner == c.Email {
		return true
	}
	for _, g := range grants {
		switch g.Kind {
		case canvas.GrantEveryone:
			// Any authenticated principal. A link secret alone does not make a
			// caller "authenticated" for an everyone grant -- that would let one
			// canvas's link leak every everyone-shared canvas.
			if c.Email != "" || c.Subject != "" {
				return true
			}
		case canvas.GrantUser:
			if c.Email != "" && c.Email == g.Target {
				return true
			}
		case canvas.GrantGroup:
			if g.Target != "" && containsString(c.Groups, g.Target) {
				return true
			}
		case canvas.GrantLink:
			if presentedLinkSecret != "" && linkSecretMatches(presentedLinkSecret, g.LinkSecretHash) {
				return true
			}
		}
	}
	return false
}

// CanWrite reports whether claims may mutate a canvas with the given owner.
// Grants are view-only in PR1: only admin and the owner may write.
func CanWrite(owner string, c Claims) bool {
	return c.Admin || (owner != "" && owner == c.Email)
}

// linkSecretMatches reports whether sha256hex(presented) equals the stored hash,
// compared in constant time so a caller can't time-probe a valid link secret.
func linkSecretMatches(presented, storedHashHex string) bool {
	if storedHashHex == "" {
		return false
	}
	sum := sha256.Sum256([]byte(presented))
	presentedHex := hex.EncodeToString(sum[:])
	// Equal-length hex strings; ConstantTimeCompare is constant-time for equal
	// lengths and returns 0 immediately (still not secret-dependent) otherwise.
	return subtle.ConstantTimeCompare([]byte(presentedHex), []byte(storedHashHex)) == 1
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
