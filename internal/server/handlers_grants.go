package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jedwards1230/scrim/internal/apiclient"
	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/identity"
	"github.com/jedwards1230/scrim/internal/logging"
	"github.com/jedwards1230/scrim/internal/principal"
)

// grantsResponse is the secret-free view GET /api/canvases/{id}/grants returns:
// the canvas's owner plus its grants (kind/target/public link id only). A
// share-link grant's secret hash is NEVER serialized here.
type grantsResponse struct {
	Owner  string                  `json:"owner"`
	Grants []apiclient.CanvasGrant `json:"grants"`
}

// createGrantResponse is the one-time result of POST /api/canvases/{id}/grants.
// link_secret is the raw share-link secret, present ONLY for a link grant and
// shown ONCE (only its hash is stored). Every other kind omits it.
type createGrantResponse struct {
	Kind       string `json:"kind"`
	Target     string `json:"target,omitempty"`
	LinkID     string `json:"link_id,omitempty"`
	LinkSecret string `json:"link_secret,omitempty"`
}

// handleListGrants serves GET /api/canvases/{id}/grants (hub only): the canvas's
// owner and grants, secret-free. The GATE only proved the caller may VIEW the
// canvas -- and a share-link secret conveys exactly that, a view of the canvas,
// NOT of its access-control list. So a link-secret-only (anonymous) viewer, or
// any caller who reached the view via an "everyone" grant rather than being an
// explicit grantee, must not learn the owner's email or the other grantees:
// only the owner, an admin, or a principal that is itself an explicit
// user/group grantee may enumerate the ACL. Everyone else gets 403.
func (s *Server) handleListGrants(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := canvas.ValidateID(id); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !canvas.Exists(s.canvasesDir, id) {
		writeJSONError(w, http.StatusNotFound, "canvas not found: "+id)
		return
	}
	owner, grants, err := canvas.GetOwnerGrants(s.metaDir, id)
	if err != nil {
		logging.Error(logging.CategoryHTTP, errors.New("list grants: reading canvas metadata failed"))
		writeJSONError(w, http.StatusInternalServerError, "failed to read canvas grants")
		return
	}
	if !canSeeACL(ownerOrAdmin(owner), grants, claimsFrom(r.Context())) {
		writeJSONError(w, http.StatusForbidden, "not permitted to view this canvas's sharing list")
		return
	}
	pub := publicGrants(grants)
	if pub == nil {
		pub = []apiclient.CanvasGrant{}
	}
	writeJSON(w, http.StatusOK, grantsResponse{Owner: ownerOrAdmin(owner), Grants: pub})
}

// canSeeACL reports whether claims may enumerate a canvas's full sharing list
// (its owner email and every grantee target) -- a strictly narrower permission
// than viewing the canvas. Admin, the owner, or a principal that is itself an
// explicit user/group grantee may see it; a link-secret-only (anonymous)
// viewer, or a caller admitted only by an "everyone" grant, may view the canvas
// but may not learn who else it is shared with.
func canSeeACL(owner string, grants []canvas.Grant, c identity.Claims) bool {
	if c.Admin {
		return true
	}
	if c.Email != "" && c.Email == owner {
		return true
	}
	for _, g := range grants {
		switch g.Kind {
		case canvas.GrantUser:
			if c.Email != "" && c.Email == g.Target {
				return true
			}
		case canvas.GrantGroup:
			if g.Target != "" && containsGroup(c.Groups, g.Target) {
				return true
			}
		}
	}
	return false
}

// containsGroup reports whether target is one of groups.
func containsGroup(groups []string, target string) bool {
	for _, g := range groups {
		if g == target {
			return true
		}
	}
	return false
}

// handleCreateGrant serves POST /api/canvases/{id}/grants (hub only): it adds a
// view-only grant to the canvas. The gate already proved the caller may write
// the canvas (owner/admin/CF-actor); this handler additionally enforces a user
// token's allowance (admin, a session-less CF actor, and the admin bearer are
// unrestricted). For a link grant it mints a fresh link id + secret, stores only
// the secret's hash, and returns the raw secret ONCE.
func (s *Server) handleCreateGrant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := canvas.ValidateID(id); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !canvas.Exists(s.canvasesDir, id) {
		writeJSONError(w, http.StatusNotFound, "canvas not found: "+id)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	var body struct {
		Kind   string `json:"kind"`
		Target string `json:"target"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	switch body.Kind {
	case canvas.GrantUser, canvas.GrantGroup:
		if body.Target == "" {
			writeJSONError(w, http.StatusBadRequest, "target is required for a "+body.Kind+" grant")
			return
		}
	case canvas.GrantEveryone, canvas.GrantLink:
		body.Target = "" // everyone/link carry no target
	default:
		writeJSONError(w, http.StatusBadRequest, "invalid grant kind: "+body.Kind)
		return
	}

	c := claimsFrom(r.Context())
	// Enforce the resolving user token's allowance. Admin and the machine-plane
	// CF actor (no token) are unrestricted; a user token may only share to
	// targets its allowance permits.
	if tok := tokenFrom(r.Context()); tok != nil && !c.Admin {
		if !tok.AllowedGrantTargets.Allows(body.Kind, body.Target) {
			writeJSONError(w, http.StatusForbidden, "your token is not permitted to share to this target")
			return
		}
	}

	// De-duplicate the addressable kinds so repeated shares don't bloat the list;
	// every link grant is unique (fresh secret) and always added.
	if body.Kind != canvas.GrantLink {
		_, existing, _ := canvas.GetOwnerGrants(s.metaDir, id)
		for _, g := range existing {
			if g.Kind == body.Kind && g.Target == body.Target {
				writeJSON(w, http.StatusOK, createGrantResponse{Kind: g.Kind, Target: g.Target})
				return
			}
		}
	}

	g := canvas.Grant{
		Kind:      body.Kind,
		Target:    body.Target,
		CreatedBy: grantCreator(c),
		CreatedAt: time.Now(),
	}
	resp := createGrantResponse{Kind: body.Kind, Target: body.Target}
	if body.Kind == canvas.GrantLink {
		linkID, secret, err := canvas.NewLink()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "minting share link failed")
			return
		}
		g.LinkID = linkID
		g.LinkSecretHash = canvas.HashLinkSecret(secret)
		resp.LinkID = linkID
		resp.LinkSecret = secret
	}
	if err := canvas.AddGrant(s.metaDir, id, g); err != nil {
		logging.Error(logging.CategoryHTTP, errors.New("create grant: writing canvas metadata failed"))
		writeJSONError(w, http.StatusInternalServerError, "failed to record grant")
		return
	}
	// Record a user/group grant target in the display-only registry (best-effort).
	if s.principals != nil && body.Kind == canvas.GrantUser && body.Target != "" {
		_ = s.principals.Observe(body.Target, "", nil, principal.SourceGrantTarget)
	}
	writeJSON(w, http.StatusCreated, resp)
}

// handleDeleteGrant serves DELETE /api/canvases/{id}/grants/{grantRef} (hub
// only): it removes matching grants. grantRef matches a link grant by its public
// link id, a user/group grant by its target, and the everyone grant by the
// literal "everyone".
func (s *Server) handleDeleteGrant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := canvas.ValidateID(id); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !canvas.Exists(s.canvasesDir, id) {
		writeJSONError(w, http.StatusNotFound, "canvas not found: "+id)
		return
	}
	ref := r.PathValue("grantRef")
	if ref == "" {
		writeJSONError(w, http.StatusBadRequest, "grant reference is required")
		return
	}
	removed, err := canvas.RemoveGrant(s.metaDir, id, func(g canvas.Grant) bool {
		return grantMatchesRef(g, ref)
	})
	if err != nil {
		logging.Error(logging.CategoryHTTP, errors.New("delete grant: writing canvas metadata failed"))
		writeJSONError(w, http.StatusInternalServerError, "failed to remove grant")
		return
	}
	if removed == 0 {
		writeJSONError(w, http.StatusNotFound, "no matching grant")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// grantMatchesRef reports whether grant g is addressed by ref (see
// handleDeleteGrant): its link id, its target, or "everyone".
func grantMatchesRef(g canvas.Grant, ref string) bool {
	switch g.Kind {
	case canvas.GrantLink:
		return g.LinkID != "" && g.LinkID == ref
	case canvas.GrantEveryone:
		return ref == canvas.GrantEveryone
	default:
		return g.Target == ref
	}
}

// grantCreator names the principal a new grant is attributed to: the actor's
// email, or "admin" when the admin push token created it.
func grantCreator(c identity.Claims) string {
	if c.Email != "" {
		return c.Email
	}
	return "admin"
}
