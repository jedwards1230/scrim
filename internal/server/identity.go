package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/identity"
)

// claimsCtxKey is the private context key under which withHubGate stashes the
// request's resolved identity.Claims, read back by handlers via claimsFrom.
type claimsCtxKey struct{}

// withClaims returns ctx carrying c. The hub gate calls this once per request
// after resolving identity, so every downstream handler reads the same claims
// without re-deriving them.
func withClaims(ctx context.Context, c identity.Claims) context.Context {
	return context.WithValue(ctx, claimsCtxKey{}, c)
}

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

// ownerOrAdmin normalizes a stored owner for an enforcement decision: an empty
// owner (a legacy canvas whose meta predates ownership, before the #55 startup
// migration writes it) is treated as admin-owned, matching the post-migration
// state so enforcement is correct even before that sweep runs.
func ownerOrAdmin(owner string) string {
	if owner == "" {
		return "admin"
	}
	return owner
}

// linkSecretFrom returns the raw share-link secret presented on the request as
// the `?k=` query parameter, or "" when none was given.
func linkSecretFrom(r *http.Request) string {
	return r.URL.Query().Get("k")
}

// canvasIDFromURLPath extracts the canvas id a request path addresses, for the two
// path families that carry one: the browser view paths (`/c/{id}`, `/c/{id}/…`,
// SSE, favicon) and the per-canvas machine-API paths (`/api/canvases/{id}/…`).
// It returns ok=false for any other path (the index, the `/api/canvases` list,
// `/api/status`) or an invalid id, so the gate can tell a per-canvas visibility
// check apart from a general authenticated read.
func canvasIDFromURLPath(p string) (string, bool) {
	var rest string
	switch {
	case strings.HasPrefix(p, "/c/"):
		rest = p[len("/c/"):]
	case strings.HasPrefix(p, "/api/canvases/"):
		rest = p[len("/api/canvases/"):]
	default:
		return "", false
	}
	id := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		id = rest[:i]
	}
	if id == "" || canvas.ValidateID(id) != nil {
		return "", false
	}
	return id, true
}

// visibleTo filters infos to the canvases the request's claims may view. On a
// hub with no OIDC (or the default daemon), enforcement is off -- the CIDR /
// capability gate is the whole read security -- so every canvas is returned
// unchanged, preserving the pre-identity behavior exactly. Under OIDC it is the
// gallery/list/status privacy filter: private by default, owner + grants only.
func (s *Server) visibleTo(infos []canvas.Info, r *http.Request) []canvas.Info {
	if s.oidcAuth == nil {
		return infos
	}
	c := claimsFrom(r.Context())
	key := linkSecretFrom(r)
	out := make([]canvas.Info, 0, len(infos))
	for _, info := range infos {
		if identity.CanView(ownerOrAdmin(info.Owner), info.Grants, c, key) {
			out = append(out, info)
		}
	}
	return out
}
