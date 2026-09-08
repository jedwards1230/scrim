package server

import (
	"net/http"
	"time"
)

// sessionResponse is the view of a browser sign-in returned by the session
// endpoints. It carries the User-Agent string verbatim (the UI derives a
// device label from it) and NO network information: an IP address is never
// recorded, so there is none to return.
type sessionResponse struct {
	ID        string    `json:"id"`
	UserAgent string    `json:"user_agent,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
	ExpiresAt time.Time `json:"expires_at"`
	// Current marks the session making this very request, so the page can say
	// "This browser" and warn before signing itself out.
	Current bool `json:"current,omitempty"`
}

// handleListSessions serves GET /api/sessions (hub mode only): the caller's own
// browser sign-ins, newest first. Scoped by the IdP subject, which is what a
// session record is keyed on -- so a caller with no subject (the admin push
// token, or a user-token principal) correctly gets an empty list rather than
// anyone else's devices.
//
// A registry in its fail-closed state answers 503 rather than an empty list:
// "we can't read this right now" and "you have no other sessions" must not look
// the same on a page whose whole job is to tell you what has access.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	c := claimsFrom(r.Context())
	if s.sessions == nil {
		writeJSON(w, http.StatusOK, []sessionResponse{})
		return
	}

	recs, err := s.sessions.List(c.Subject)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "session registry unavailable")
		return
	}

	currentID := s.currentSessionID(r)
	out := make([]sessionResponse, 0, len(recs))
	for _, rec := range recs {
		out = append(out, sessionResponse{
			ID:        rec.ID,
			UserAgent: rec.UserAgent,
			CreatedAt: rec.CreatedAt,
			LastSeen:  rec.LastSeen,
			ExpiresAt: rec.ExpiresAt,
			Current:   rec.ID == currentID,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRevokeSession serves DELETE /api/sessions/{id} (hub mode only): a
// principal ends one of its OWN browser sign-ins, which takes effect on that
// browser's very next request. A miss -- an unknown id, an expired one, or
// another principal's -- is a 404, mirroring handleRevokeToken exactly: the
// response never reveals that a session it can't touch exists.
func (s *Server) handleRevokeSession(w http.ResponseWriter, r *http.Request) {
	c := claimsFrom(r.Context())
	id := r.PathValue("id")

	// No subject, nothing of one's own to revoke -- and deliberately no admin
	// branch: the admin push token is the machine/recovery credential, not an
	// account with devices, and letting it end arbitrary browser sessions by id
	// would be a new power with no UI asking for it.
	if s.sessions == nil || c.Subject == "" {
		writeJSONError(w, http.StatusNotFound, "session not found")
		return
	}

	revoked, err := s.sessions.Revoke(id, c.Subject)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "session registry unavailable")
		return
	}
	if !revoked {
		writeJSONError(w, http.StatusNotFound, "session not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// currentSessionID returns the session id the request's own cookie carries, or
// "" when it carries none (a machine-plane caller, or no OIDC at all). It is
// display-only -- it decides which entry is labeled "This browser", never
// whether anything is authorized.
func (s *Server) currentSessionID(r *http.Request) string {
	if s.oidcAuth == nil {
		return ""
	}
	sess, ok := s.oidcAuth.SessionFromRequest(r)
	if !ok {
		return ""
	}
	return sess.ID
}
