package server

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/version"
)

//go:embed templates/shell.html.tmpl
var shellTemplateSrc string

var shellTemplate = mustPageTemplate("shell", shellTemplateSrc)

// rawPathSegment is the literal path segment that separates a canvas's own
// content from the shell wrapped around it: /c/{id}/ renders the shell, and
// /c/{id}/__raw/... serves the canvas exactly as it has always been served.
const rawPathSegment = "__raw"

// shellData is the canvas shell's render context. Everything identity-flavored
// (the ownership line, the account menu, Share, Duplicate, Delete) is populated only
// under OIDC; with no OIDC those fields are zero and the template's {{if}}
// guards render none of that chrome -- the same rule the gallery follows (see
// handleIndex and handlers_index_identity_test.go).
type shellData struct {
	ID    string
	Title string
	Icon  string
	Color string
	// RawURL is the iframe source: the canvas's own content, under the
	// __raw/ prefix. It carries a share-link secret forward when the viewer
	// arrived with one, since the iframe request is gated per-canvas exactly
	// like the shell request was.
	RawURL     string
	FaviconURL string
	Version    string

	OIDC bool
	// Account is the account menu's context: the viewer's identity plus the
	// Devices & access / Log out actions. Empty (and so unrendered) outside OIDC.
	Account    accountData
	Owned      bool
	OwnerLabel string
	// CanShare is the owner-or-admin decision reused verbatim from the
	// gallery's decorateCanvasIdentity.
	CanShare bool
	// CanDuplicate mirrors CanShare rather than merely "OIDC is on": the copy
	// endpoint authorizes a browser session against CanWrite on the SOURCE
	// canvas (see serveWrite), so offering Duplicate to a viewer who only has
	// a view grant would render a button that can do nothing but 403.
	CanDuplicate bool
	// CanDelete is the same write decision again: DELETE /api/canvases/{id}
	// from a browser session is authorized against CanWrite on this canvas
	// (see serveWrite), so a view-only viewer must not be offered it.
	CanDelete  bool
	UpdatedAgo string
}

// handleCanvasShell serves GET /c/{id}/ -- the canvas shell: a slim top bar
// carrying the canvas's title and menu, wrapped around an iframe that loads
// the canvas itself from /c/{id}/__raw/.
//
// The split exists because the shell must NOT carry a live-reload script. The
// canvas path injects one (writeHTML -> injectReloadScript); if the shell were
// rendered through that path too, every viewer would hold two EventSource
// connections to the same canvas -- doubling the gallery's viewer count and the
// hub's per-canvas SSE cap accounting -- and every agent write would reload the
// outer page, destroying any open menu. So the shell is rendered with
// html/template exactly like the gallery index is, and never touches writeHTML.
//
// Only the EXACT canvas root gets the shell: /c/{id}/report.pdf and
// /c/{id}/page2.html keep serving raw, unchanged.
//
// The iframe is deliberately NOT sandboxed yet, which is a known gap against
// docs/PRD.md's "sandboxed iframe" wording. A sandbox strong enough to matter
// (one without allow-same-origin) puts the canvas on an opaque origin, and its
// injected reload script's EventSource back to /c/{id}/__events then fails
// CORS -- live reload, the product's core loop, would break. A sandbox WITH
// allow-same-origin buys nothing against the threat the PRD names, since the
// framed document could still reach parent.document. Closing this properly
// needs the reload channel reworked (postMessage from the shell, say) and is
// left to a follow-up rather than smuggled in here.
func (s *Server) handleCanvasShell(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := canvas.ValidateID(id); err != nil {
		http.NotFound(w, r)
		return
	}
	info, err := canvas.Get(s.canvasesDir, s.metaDir, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// A canvas directory with nothing servable at its root 404s exactly as it
	// did before the shell existed -- resolving here (rather than letting the
	// iframe discover it) keeps the "no index, no page" contract intact instead
	// of framing a 404 in chrome.
	if _, _, err := resolveServablePath(canvas.Dir(s.canvasesDir, id), ""); err != nil {
		if os.IsNotExist(err) || errors.Is(err, errOutsideRoot) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	title := info.Title
	if title == "" {
		title = info.ID
	}

	base := fmt.Sprintf("/c/%s/", id)
	rawURL := base + rawPathSegment + "/"
	// A link-grant viewer authenticated the shell request with ?k=<secret>;
	// the iframe request is gated the same way, so the secret has to ride
	// along or the canvas itself would 404 inside the frame.
	if key := linkSecretFrom(r); key != "" {
		rawURL += "?" + url.Values{"k": {key}}.Encode()
	}

	data := shellData{
		ID:         info.ID,
		Title:      title,
		Icon:       info.Icon,
		Color:      info.Color,
		RawURL:     rawURL,
		FaviconURL: base + "favicon.ico",
		Version:    version.Short(),
		OIDC:       s.oidcAuth != nil,
		UpdatedAgo: relativeAge(time.Since(info.ModTime)),
	}
	if data.OIDC {
		c := claimsFrom(r.Context())
		data.Account = accountFrom(c)
		// Reuse the gallery's owner/share decision rather than restating it, so
		// the two surfaces can't drift on who owns what.
		var ic indexCanvas
		decorateCanvasIdentity(&ic, ownerOrAdmin(info.Owner), info.Grants, c)
		data.Owned = ic.Owned
		data.CanShare = ic.CanShare
		data.CanDuplicate = ic.CanShare
		data.CanDelete = ic.CanShare
		data.OwnerLabel = ic.OwnerLabel
		if data.OwnerLabel == "" && !ic.Owned {
			data.OwnerLabel = ownerOrAdmin(info.Owner)
		}
	}

	// Render into a buffer, not straight into the ResponseWriter: Execute
	// starts writing as soon as it renders, so a mid-render failure would have
	// already committed 200 + a partial page and the http.Error below could
	// change neither. Buffering keeps the error path able to send a real 500.
	// The shell is a small page and the template is parsed at init, so the
	// buffer costs nothing and the error path is unreachable in practice --
	// which is exactly why it should not be able to emit a truncated page.
	var buf bytes.Buffer
	if err := shellTemplate.Execute(&buf, data); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Same reasoning as the canvas itself: the shell names a canvas and its
	// owner, so it must not be retained by any cache.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	_, _ = w.Write(buf.Bytes())
}

// relativeAge renders a coarse "how long ago" label for the shell's ownership
// line. Deliberately coarse: the shell carries no live reload, so a precise
// figure would only go stale on the page.
func relativeAge(d time.Duration) string {
	// A canvas mtime in the future (clock skew, a restored archive, a file
	// written with an explicit timestamp) would otherwise render "-1d ago".
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
