package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/usertoken"
)

// tokenResponse is the secret-free view of a user token returned by the token
// endpoints. It NEVER carries the hash or the raw secret (the raw is returned
// exactly once, by mint, in mintResponse). AutoShare/AllowedGrantTargets are
// additive (omitempty): the my-tokens page shows them so a caller can see what
// a token auto-shares and may share to; older clients that don't know the
// fields ignore them.
type tokenResponse struct {
	ID                  string              `json:"id"`
	Name                string              `json:"name"`
	OwnerEmail          string              `json:"owner_email"`
	CreatedAt           time.Time           `json:"created_at"`
	LastUsed            *time.Time          `json:"last_used,omitempty"`
	Revoked             bool                `json:"revoked,omitempty"`
	AutoShare           []canvas.Grant      `json:"auto_share,omitempty"`
	AllowedGrantTargets usertoken.Allowance `json:"allowed_grant_targets,omitempty"`
}

// mintResponse is the one-time result of minting a token: the raw secret plus
// its metadata. The raw is shown here and never again.
type mintResponse struct {
	Token string        `json:"token"` // raw secret, shown ONCE
	Meta  tokenResponse `json:"meta"`
}

func toTokenResponse(t usertoken.Token) tokenResponse {
	return tokenResponse{
		ID:                  t.ID,
		Name:                t.Name,
		OwnerEmail:          t.OwnerEmail,
		CreatedAt:           t.CreatedAt,
		LastUsed:            t.LastUsed,
		Revoked:             t.Revoked,
		AutoShare:           t.AutoShare,
		AllowedGrantTargets: t.AllowedGrantTargets,
	}
}

// handleCreateToken serves POST /api/tokens (hub mode only): a logged-in
// session (or admin) mints a named token. The gate authorizes reaching here (a
// session mints for itself; admin mints for a named owner); a user-token
// principal is rejected at the gate (no privilege escalation).
func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	c := claimsFrom(r.Context())

	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var body struct {
		Name                string              `json:"name"`
		OwnerEmail          string              `json:"owner_email"`
		AutoShare           []canvas.Grant      `json:"auto_share"`
		AllowedGrantTargets usertoken.Allowance `json:"allowed_grant_targets"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	// A session mints for ITSELF; admin mints for a named owner (defaulting to
	// "admin"). A session may not mint for another principal.
	owner := c.Email
	if c.Admin {
		owner = body.OwnerEmail
		if owner == "" {
			owner = "admin"
		}
	} else if body.OwnerEmail != "" && body.OwnerEmail != c.Email {
		writeJSONError(w, http.StatusForbidden, "cannot mint a token for another principal")
		return
	}
	if owner == "" {
		writeJSONError(w, http.StatusBadRequest, "cannot determine token owner")
		return
	}

	raw, tok, err := s.tokens.Mint(body.Name, owner, body.AutoShare, body.AllowedGrantTargets)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "minting token failed")
		return
	}
	writeJSON(w, http.StatusCreated, mintResponse{Token: raw, Meta: toTokenResponse(tok)})
}

// handleListTokens serves GET /api/tokens (hub mode only): the caller's own
// tokens (scoped by principal email), never hashes or raw secrets.
func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	c := claimsFrom(r.Context())
	// A principal lists its own tokens by email. Admin has no email of its own
	// (it mints on behalf of named owners), so it lists nothing here.
	tokens := s.tokens.List(c.Email)
	out := make([]tokenResponse, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, toTokenResponse(t))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRevokeToken serves DELETE /api/tokens/{id} (hub mode only): a principal
// revokes its OWN token; admin may revoke any. A miss (or another principal's
// token) is a 404 -- never revealing that a token it can't touch exists.
func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	c := claimsFrom(r.Context())
	id := r.PathValue("id")

	if c.Admin {
		tok, ok := s.tokens.Get(id)
		if !ok {
			writeJSONError(w, http.StatusNotFound, "token not found")
			return
		}
		if _, err := s.tokens.Revoke(id, tok.OwnerEmail); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "revoking token failed")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	revoked, err := s.tokens.Revoke(id, c.Email)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "revoking token failed")
		return
	}
	if !revoked {
		writeJSONError(w, http.StatusNotFound, "token not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
