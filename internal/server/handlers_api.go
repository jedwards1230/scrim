package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/jedwards1230/scrim/internal/apiclient"
	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/version"
)

// handleAPIStatus serves GET /api/status: the daemon health-check endpoint
// used both by the CLI (to decide whether to self-start) and as a general
// status query.
func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	infos, err := canvas.List(s.canvasesDir)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	now := time.Now()
	idleSeconds := now.Sub(s.activity.last()).Seconds()
	sseClients := s.hub.clientCount()

	resp := apiclient.StatusResponse{
		PID:                os.Getpid(),
		Host:               s.cfg.Host,
		Port:               s.port,
		Version:            version.Short(),
		StartedAt:          s.startedAt,
		UptimeSeconds:      now.Sub(s.startedAt).Seconds(),
		CanvasCount:        len(infos),
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
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if err := canvas.ValidateID(body.ID); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := canvas.Create(s.canvasesDir, body.ID, body.Title); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	info, err := canvas.Get(s.canvasesDir, body.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, s.canvasResponse(info))
}

// handleListCanvases serves GET /api/canvases.
func (s *Server) handleListCanvases(w http.ResponseWriter, r *http.Request) {
	infos, err := canvas.List(s.canvasesDir)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
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
	if err := canvas.Delete(s.canvasesDir, id); err != nil {
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
		ID:         info.ID,
		Title:      info.Title,
		Dir:        info.Dir,
		URL:        url,
		ModifiedAt: info.ModTime,
		SSEClients: s.hub.canvasClientCount(info.ID),
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
