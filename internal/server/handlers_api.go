package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/jedwards1230/scrim/internal/apiclient"
	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/logging"
	"github.com/jedwards1230/scrim/internal/usertoken"
	"github.com/jedwards1230/scrim/internal/version"
)

// handleAPIStatus serves GET /api/status: the daemon health-check endpoint
// used both by the CLI (to decide whether to self-start) and as a general
// status query.
func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	infos, err := canvas.List(s.canvasesDir, s.metaDir)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	now := time.Now()
	idleSeconds := now.Sub(s.activity.last()).Seconds()
	sseClients := s.hub.clientCount()

	// Under OIDC the count reflects only canvases this request may see (private
	// by default); on a non-OIDC hub / the default daemon visibleTo is a no-op.
	visibleCount := len(s.visibleTo(infos, r))

	resp := apiclient.StatusResponse{
		PID:                os.Getpid(),
		Host:               s.cfg.Host,
		Port:               s.port,
		Version:            version.Short(),
		StartedAt:          s.startedAt,
		UptimeSeconds:      now.Sub(s.startedAt).Seconds(),
		CanvasCount:        visibleCount,
		IdleTimeoutSeconds: s.cfg.IdleTimeout.Seconds(),
		IdleSeconds:        idleSeconds,
		SSEClients:         sseClients,
		// With reaping disabled (idleTimeout <= 0) the daemon is always
		// considered active — idleSeconds is otherwise always >= 0, which
		// would make "idleSeconds < IdleTimeoutSeconds" spuriously false
		// once IdleTimeoutSeconds is itself <= 0.
		Active: sseClients > 0 || s.cfg.IdleTimeout <= 0 || idleSeconds < s.cfg.IdleTimeout.Seconds(),
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleCreateCanvas serves POST /api/canvases.
func (s *Server) handleCreateCanvas(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID          string `json:"id"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Icon        string `json:"icon"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if err := canvas.ValidateID(body.ID); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Ownership is a hub concept: only a hub resolves request identity (the
	// default daemon has no gate, no OIDC, and its canvases are never
	// visibility-filtered), so it alone stamps an owner -- keeping the default
	// daemon's on-disk behavior byte-for-byte unchanged (hub_test.go invariant).
	owner := ""
	// A create is idempotent, so apply auto-share only for a genuinely new
	// canvas (checked before Create), never on a repeat POST for an existing id.
	isNew := !canvas.Exists(s.canvasesDir, body.ID)
	if s.isHub() {
		owner = ownerFromClaims(claimsFrom(r.Context()))
	}
	if _, err := canvas.Create(s.canvasesDir, s.metaDir, body.ID, body.Title, body.Description, body.Icon, owner); err != nil {
		logging.Error(logging.CategoryHTTP, errors.New("create canvas: writing canvas failed"))
		writeJSONError(w, http.StatusInternalServerError, "failed to create canvas")
		return
	}
	if s.isHub() && isNew {
		s.applyAutoShare(body.ID, tokenFrom(r.Context()))
	}
	info, err := canvas.Get(s.canvasesDir, s.metaDir, body.ID)
	if err != nil {
		logging.Error(logging.CategoryHTTP, errors.New("create canvas: reading canvas back failed"))
		writeJSONError(w, http.StatusInternalServerError, "failed to read canvas")
		return
	}
	writeJSON(w, http.StatusCreated, s.canvasResponse(info))
}

// handleListCanvases serves GET /api/canvases.
func (s *Server) handleListCanvases(w http.ResponseWriter, r *http.Request) {
	infos, err := canvas.List(s.canvasesDir, s.metaDir)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	infos = s.visibleTo(infos, r)
	resp := make([]apiclient.CanvasResponse, 0, len(infos))
	for _, info := range infos {
		resp = append(resp, s.canvasResponse(info))
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDeleteCanvas serves DELETE /api/canvases/<id>.
func (s *Server) handleDeleteCanvas(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := canvas.ValidateID(id); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !canvas.Exists(s.canvasesDir, id) {
		writeJSONError(w, http.StatusNotFound, "canvas not found: "+id)
		return
	}
	if err := canvas.Delete(s.canvasesDir, s.metaDir, id); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleStop serves POST /api/stop: it acknowledges the request, then
// triggers a graceful shutdown of the daemon.
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	s.initiateShutdown()
}

func (s *Server) canvasResponse(info canvas.Info) apiclient.CanvasResponse {
	url := fmt.Sprintf("http://%s:%d/c/%s/", s.cfg.Host, s.port, info.ID)
	if !s.cfg.NoAuth {
		url += "?t=" + s.token
	}
	return apiclient.CanvasResponse{
		ID:          info.ID,
		Title:       info.Title,
		Description: info.Description,
		Icon:        info.Icon,
		Color:       info.Color,
		Dir:         info.Dir,
		URL:         url,
		ModifiedAt:  info.ModTime,
		SSEClients:  s.hub.canvasClientCount(info.ID),
		Owner:       info.Owner,
		Grants:      publicGrants(info.Grants),
	}
}

// applyAutoShare adds a resolving user token's auto-share grants to a
// newly-created canvas, attributing each to the token's owner. It is best-effort
// -- a grant-write failure is logged (scrubbed) but never fails the create/push
// that already succeeded, and a nil token (admin/session/anonymous create) is a
// no-op. The caller must apply it only for genuinely new canvases so a re-push
// never duplicates grants.
func (s *Server) applyAutoShare(id string, tok *usertoken.Token) {
	if tok == nil || len(tok.AutoShare) == 0 {
		return
	}
	now := time.Now()
	for _, g := range tok.AutoShare {
		g.CreatedBy = tok.OwnerEmail
		if g.CreatedAt.IsZero() {
			g.CreatedAt = now
		}
		if err := canvas.AddGrant(s.metaDir, id, g); err != nil {
			logging.Error(logging.CategoryAuth, errors.New("applying auto-share grant failed"))
			return
		}
	}
}

// publicGrants projects a canvas's stored grants into the secret-free shape
// exposed on the API (kind/target/link id only) -- a link grant's secret hash
// is never serialized. Returns nil for no grants so the field omits cleanly.
func publicGrants(grants []canvas.Grant) []apiclient.CanvasGrant {
	if len(grants) == 0 {
		return nil
	}
	out := make([]apiclient.CanvasGrant, 0, len(grants))
	for _, g := range grants {
		out = append(out, apiclient.CanvasGrant{Kind: g.Kind, Target: g.Target, LinkID: g.LinkID})
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
