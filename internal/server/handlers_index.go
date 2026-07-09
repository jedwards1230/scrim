package server

import (
	_ "embed"
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/version"
)

//go:embed templates/index.html.tmpl
var indexTemplateSrc string

var indexTemplate = template.Must(template.New("index").Parse(indexTemplateSrc))

type indexCanvas struct {
	ID          string
	Title       string
	Description string
	Icon        string
	Color       string
	URL         string
	ModTime     string
	SSEClients  int
}

type indexData struct {
	Canvases []indexCanvas
	Version  string
}

// handleIndex serves the server-rendered dashboard at "/": a card gallery
// listing every canvas with its icon, title, description, last-modified
// time, and a live SSE viewer count. The initial render bakes in the
// viewer count at request time; templates/index.html.tmpl's inline script
// then keeps it live by polling GET /api/canvases on an interval (see that
// file's comment for why polling was chosen over a dashboard-wide SSE
// endpoint).
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	infos, err := canvas.List(s.canvasesDir, s.metaDir)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Under OIDC the gallery shows only canvases this request may see (private
	// by default); on a non-OIDC hub / the default daemon visibleTo is a no-op.
	infos = s.visibleTo(infos, r)

	data := indexData{Version: version.Short()}
	for _, info := range infos {
		data.Canvases = append(data.Canvases, indexCanvas{
			ID:          info.ID,
			Title:       info.Title,
			Description: info.Description,
			Icon:        info.Icon,
			Color:       info.Color,
			URL:         fmt.Sprintf("/c/%s/", info.ID),
			ModTime:     info.ModTime.Format(time.RFC1123),
			SSEClients:  s.hub.canvasClientCount(info.ID),
		})
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTemplate.Execute(w, data); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
