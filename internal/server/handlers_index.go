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
	ID      string
	Title   string
	URL     string
	ModTime string
}

type indexData struct {
	Canvases []indexCanvas
	Version  string
}

// handleIndex serves the server-rendered dashboard at "/": a plain HTML
// table listing every canvas with a link to its served URL.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	infos, err := canvas.List(s.canvasesDir)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := indexData{Version: version.Short()}
	for _, info := range infos {
		data.Canvases = append(data.Canvases, indexCanvas{
			ID:      info.ID,
			Title:   info.Title,
			URL:     fmt.Sprintf("/c/%s/", info.ID),
			ModTime: info.ModTime.Format(time.RFC1123),
		})
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTemplate.Execute(w, data); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
