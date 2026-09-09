package server

import (
	"net/http"
	"time"
)

// agentConnsPath is the agent-connection collection route, shared by routes.go
// and the gate's path discriminator so the two cannot drift.
const agentConnsPath = "/api/agent-connections"

// agentConnResponse is the view of an agent connection returned by the
// agent-connection endpoints: an OAuth-authenticated MCP client that has acted
// for the calling principal. It carries no token material and, like the session
// view, NO network information -- no IP address is recorded, so there is none
// to return.
type agentConnResponse struct {
	ID string `json:"id"`
	// ClientID is the OAuth client id the connection is named by (the token's
	// `azp`/`client_id`). Empty for a connection that reached the hub without
	// one -- the gateway-forwarded HMAC plane, which carries no JWT.
	ClientID  string    `json:"client_id,omitempty"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	// RevokedAt is set once the principal has cut this connection off; the page
	// keeps showing it as history, the way a revoked token stays listed.
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// handleListAgentConns serves GET /api/agent-connections (hub mode only): the
// agent connections that have acted for the calling principal, newest first.
// Scoped by the IdP subject, which is what a connection record is keyed on
// (with the OAuth client id) -- so a caller with no subject (the admin push
// token, or a user-token principal) correctly gets an empty list rather than
// anyone else's agents.
//
// A registry in its fail-closed state answers 503 rather than an empty list,
// exactly like GET /api/sessions: on a page whose whole job is to say what has
// access, "we can't read this" and "nothing has access" must not look the same.
func (s *Server) handleListAgentConns(w http.ResponseWriter, r *http.Request) {
	c := claimsFrom(r.Context())
	if s.agents == nil {
		writeJSON(w, http.StatusOK, []agentConnResponse{})
		return
	}

	recs, err := s.agents.List(c.Subject)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "agent connection registry unavailable")
		return
	}

	out := make([]agentConnResponse, 0, len(recs))
	for _, rec := range recs {
		resp := agentConnResponse{
			ID:        rec.ID,
			ClientID:  rec.ClientID,
			FirstSeen: rec.FirstSeen,
			LastSeen:  rec.LastSeen,
		}
		if rec.Revoked() {
			revoked := rec.RevokedAt
			resp.RevokedAt = &revoked
		}
		out = append(out, resp)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRevokeAgentConn serves DELETE /api/agent-connections/{id} (hub mode
// only): a principal blocks one of its OWN agent connections, which takes
// effect on that client's very next request. A miss -- an unknown id, one
// already revoked, or another principal's -- is a 404, mirroring
// handleRevokeSession and handleRevokeToken exactly: the response never reveals
// that a connection it can't touch exists.
//
// What this does NOT do is delete anything at the identity provider. The client
// keeps whatever refresh token it holds; authorizing scrim again mints a token
// issued after the revocation, which the gate admits (see agentconn.Admit).
// That is deliberate and the devices page says so -- a control that implies
// more than it did is worse than one that states its bounds.
func (s *Server) handleRevokeAgentConn(w http.ResponseWriter, r *http.Request) {
	c := claimsFrom(r.Context())
	id := r.PathValue("id")

	// No subject, nothing of one's own to revoke -- and deliberately no admin
	// branch, for the reason handleRevokeSession gives: the admin push token is
	// the machine credential, not an account with agents.
	if s.agents == nil || c.Subject == "" {
		writeJSONError(w, http.StatusNotFound, "agent connection not found")
		return
	}

	revoked, err := s.agents.Revoke(id, c.Subject)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "agent connection registry unavailable")
		return
	}
	if !revoked {
		writeJSONError(w, http.StatusNotFound, "agent connection not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
