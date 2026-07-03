package server

import (
	"html"
	"net/http"
	"os"

	"github.com/jedwards1230/scrim/internal/canvas"
)

// handleCanvasFavicon serves GET /c/<id>/favicon.ico. An agent-authored
// favicon.ico actually present in the canvas directory is served as-is
// (like any other static asset); otherwise a small SVG built from the
// canvas's emoji icon and accent color is generated on the fly, so every
// canvas gets a visually distinct tab icon without requiring any asset from
// the agent that authored it.
func (s *Server) handleCanvasFavicon(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := canvas.ValidateID(id); err != nil {
		http.NotFound(w, r)
		return
	}
	root := canvas.Dir(s.canvasesDir, id)
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		http.NotFound(w, r)
		return
	}

	if target, err := resolveStaticPath(root, "favicon.ico"); err == nil {
		if f, openErr := os.Open(target); openErr == nil { //nolint:gosec // target is resolved+validated by resolveStaticPath
			defer f.Close() //nolint:errcheck // read-only handle, close error not actionable
			if fi, statErr := f.Stat(); statErr == nil && !fi.IsDir() {
				http.ServeContent(w, r, target, fi.ModTime(), f)
				return
			}
		}
	}

	info, err := canvas.Get(s.canvasesDir, s.metaDir, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(faviconSVG(info.Icon, info.Color)))
}

// faviconSVG renders a tiny rounded-square SVG with the icon glyph centered
// on a color swatch background -- deliberately simple (fixed 64x64 canvas,
// no external fonts) since it's only ever used as a favicon.
func faviconSVG(icon, color string) string {
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64">` +
		`<rect width="64" height="64" rx="14" fill="` + html.EscapeString(color) + `"/>` +
		`<text x="32" y="44" font-size="34" text-anchor="middle">` + html.EscapeString(icon) + `</text>` +
		`</svg>`
}
