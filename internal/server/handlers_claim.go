package server

import (
	"net/http"

	"github.com/jedwards1230/scrim/internal/canvas"
)

// handleClaimCanvas serves POST /api/canvases/{id}/claim (hub only, #55): a
// logged-in session or user-token principal takes ownership of a legacy
// (admin-owned) canvas. Only admin-owned canvases are claimable; a canvas
// already owned by a different non-admin principal is 409, and a canvas that
// doesn't exist is 404. Claiming a canvas the caller already owns is a no-op
// (200). Authorization to reach here (any authenticated caller) is enforced at
// the gate (serveWrite's claim branch).
func (s *Server) handleClaimCanvas(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := canvas.ValidateID(id); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !canvas.Exists(s.canvasesDir, id) {
		writeJSONError(w, http.StatusNotFound, "canvas not found: "+id)
		return
	}

	c := claimsFrom(r.Context())
	// The claimant must be a real principal with an email to own the canvas. The
	// admin push token has no email of its own -- it is already the owner of
	// legacy canvases, so there is nothing for it to claim.
	if c.Email == "" {
		writeJSONError(w, http.StatusForbidden, "only a logged-in principal can claim a canvas")
		return
	}

	owner, _, err := canvas.GetOwnerGrants(s.metaDir, id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	current := ownerOrAdmin(owner)
	if current == c.Email {
		// Idempotent: already the caller's.
		writeJSON(w, http.StatusOK, map[string]string{"id": id, "owner": current})
		return
	}
	if current != "admin" {
		// Owned by a different real principal -- not claimable.
		writeJSONError(w, http.StatusConflict, "canvas is already owned")
		return
	}

	if err := canvas.SetOwner(s.metaDir, id, c.Email); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "recording new owner failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "owner": c.Email})
}
