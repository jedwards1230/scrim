package server

import (
	"context"

	"github.com/jedwards1230/scrim/internal/identity"
)

// claimsCtxKey is the private context key under which withHubGate stashes the
// request's resolved identity.Claims, read back by handlers via claimsFrom.
type claimsCtxKey struct{}

// claimsFrom returns the claims stashed by withHubGate, or the zero
// (anonymous) Claims when none are present -- e.g. the default daemon, whose
// withAuth gate never stashes claims, or a direct handler call in a unit test.
func claimsFrom(ctx context.Context) identity.Claims {
	c, _ := ctx.Value(claimsCtxKey{}).(identity.Claims)
	return c
}

// ownerFromClaims derives the owner id to stamp on a canvas created/pushed by
// the given claims: the principal's email when it has one (an OIDC session or,
// #51, a CF-forwarded actor), otherwise "admin" -- the id legacy/bootstrap
// canvases and the global push token own.
func ownerFromClaims(c identity.Claims) string {
	if c.Email != "" {
		return c.Email
	}
	return "admin"
}
