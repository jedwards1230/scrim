package server

import (
	"bytes"
	_ "embed"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/jedwards1230/scrim/internal/canvas"
)

//go:embed assets/reload.js
var reloadScriptTemplate string

// handleCanvasRedirect sends /c/<id> (no trailing slash) to /c/<id>/, which
// is what the static-serving pattern actually matches.
func (s *Server) handleCanvasRedirect(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := canvas.ValidateID(id); err != nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/c/"+id+"/", http.StatusMovedPermanently)
}

// handleCanvas serves static files from a canvas's directory, injecting the
// SSE live-reload script into any HTML response.
func (s *Server) handleCanvas(w http.ResponseWriter, r *http.Request) {
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

	target, viaIndex, err := resolveServablePath(root, r.PathValue("rest"))
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, errOutsideRoot) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	ext := strings.ToLower(filepath.Ext(target))
	switch {
	case ext == ".html" || ext == ".htm":
		s.serveHTML(w, r, id, target)
		return
	case viaIndex && ext == ".md":
		// Only the index.md-as-directory-index case renders markdown; a
		// directly-requested notes.md falls through to raw static serving
		// below, same as any other non-HTML file. indexFileNames (see
		// staticpath.go) only ever yields "index.html"/"index.md", never
		// "index.markdown", so there's no ".markdown" case to handle here.
		s.serveMarkdownIndex(w, r, id, target)
		return
	}

	f, err := os.Open(target) //nolint:gosec // target is resolved+validated by resolveServablePath
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close() //nolint:errcheck // read-only handle, close error not actionable

	fi, err := f.Stat()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, target, fi.ModTime(), f)
}

// serveHTML serves an .html/.htm file. A complete document (one already
// containing <!doctype or <html) is served as-is aside from reload-script
// injection; a bare fragment is first wrapped in scrim's default skeleton
// (see wrapInSkeleton) so it gets a sensible presentation.
func (s *Server) serveHTML(w http.ResponseWriter, r *http.Request, id, target string) {
	data, err := os.ReadFile(target) //nolint:gosec // target is resolved+validated by resolveServablePath
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !looksLikeCompleteHTMLDocument(data) {
		data = wrapInSkeleton(data)
	}
	s.writeHTML(w, id, data)
}

// serveMarkdownIndex renders an index.md (reached via directory-index
// fallback, never a direct .md request -- see resolveServablePath) to HTML
// via goldmark and wraps it in scrim's default skeleton.
func (s *Server) serveMarkdownIndex(w http.ResponseWriter, r *http.Request, id, target string) {
	source, err := os.ReadFile(target) //nolint:gosec // target is resolved+validated by resolveServablePath
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rendered, err := renderMarkdown(source)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.writeHTML(w, id, wrapInSkeleton(rendered))
}

// writeHTML injects the live-reload script into html and writes it as the
// response body.
func (s *Server) writeHTML(w http.ResponseWriter, id string, html []byte) {
	injected := injectReloadScript(html, id)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(injected)
}

// injectReloadScript appends a small <script> that opens an EventSource
// against the canvas's SSE endpoint and reloads on any message, inserting
// it before </body> when present, or at the end of the document otherwise.
func injectReloadScript(html []byte, id string) []byte {
	script := strings.ReplaceAll(reloadScriptTemplate, "__SCRIM_EVENTS_URL__", "/c/"+id+"/__events")
	snippet := []byte("<script>\n" + script + "</script>\n")

	lower := bytes.ToLower(html)
	if idx := bytes.LastIndex(lower, []byte("</body>")); idx != -1 {
		out := make([]byte, 0, len(html)+len(snippet))
		out = append(out, html[:idx]...)
		out = append(out, snippet...)
		out = append(out, html[idx:]...)
		return out
	}
	out := make([]byte, 0, len(html)+len(snippet))
	out = append(out, html...)
	out = append(out, snippet...)
	return out
}
