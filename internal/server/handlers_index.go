package server

import (
	_ "embed"
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/identity"
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

	// The fields below are populated only under OIDC (see handleIndex); on a
	// non-OIDC hub / the default daemon they are all zero, and the template's
	// {{if $.OIDC}} guards mean they render nothing -- keeping that path's
	// output free of any identity chrome.
	Owned         bool   // the viewer owns this canvas
	SharedWithYou bool   // a grant admits the viewer, who is not the owner
	Visibility    string // "Private" | "Shared" (owner/admin only; never leaks who)
	OwnerLabel    string // owner email, shown when the canvas is not the viewer's own
	CanShare      bool   // the viewer owns it (or is admin) -> show the Share control
	CanClaim      bool   // an admin-owned canvas a logged-in non-admin viewer can claim
}

type indexData struct {
	Canvases []indexCanvas
	Version  string

	// OIDC is true only on a hub with OIDC active; it gates every identity
	// affordance in the template (chip, logout, badges, share dialog) so a
	// non-OIDC hub and the default daemon render exactly as before.
	OIDC bool
	// Principal is the logged-in viewer's display name, falling back to email;
	// empty when anonymous or non-OIDC. Shown in the header chip.
	Principal string
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

	// Identity chrome is rendered only under OIDC; the default daemon and a
	// non-OIDC hub have no principal and leave data.OIDC false.
	oidcActive := s.oidcAuth != nil
	c := claimsFrom(r.Context())
	data := indexData{Version: version.Short(), OIDC: oidcActive}
	if oidcActive {
		if c.Name != "" {
			data.Principal = c.Name
		} else {
			data.Principal = c.Email
		}
	}

	for _, info := range infos {
		ic := indexCanvas{
			ID:          info.ID,
			Title:       info.Title,
			Description: info.Description,
			Icon:        info.Icon,
			Color:       info.Color,
			URL:         fmt.Sprintf("/c/%s/", info.ID),
			ModTime:     info.ModTime.Format(time.RFC1123),
			SSEClients:  s.hub.canvasClientCount(info.ID),
		}
		if oidcActive {
			decorateCanvasIdentity(&ic, ownerOrAdmin(info.Owner), info.Grants, c)
		}
		data.Canvases = append(data.Canvases, ic)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTemplate.Execute(w, data); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// decorateCanvasIdentity fills a gallery card's OIDC-only identity fields from
// the canvas's normalized owner + grants relative to the viewing claims. The
// card is already known visible to the viewer (visibleTo filtered the list), so
// this only labels HOW: owned, shared-with-you, or (for admin) whose it is.
//
// It deliberately ships no ACL detail the viewer shouldn't see: the
// Private/Shared qualifier is computed only for a canvas the viewer may share
// (its owner, or admin) -- the same principals the share dialog already shows
// the full grant list to -- and it never names another grantee.
func decorateCanvasIdentity(ic *indexCanvas, owner string, grants []canvas.Grant, c identity.Claims) {
	owned := c.Email != "" && c.Email == owner
	ic.Owned = owned
	ic.CanShare = owned || c.Admin
	// A logged-in non-admin viewer can claim a still-legacy (admin-owned)
	// canvas it can see -- the discoverable path to taking ownership.
	ic.CanClaim = !owned && !c.Admin && c.Email != "" && owner == "admin"

	switch {
	case owned:
		// The viewer's own canvas: show the coarse visibility qualifier only.
	case c.Admin:
		// Admin sees every canvas; label it with its owner rather than a
		// "shared with you" relationship it doesn't really have.
		ic.OwnerLabel = owner
	default:
		// Visible but not owned by this viewer -> it was shared with them.
		ic.SharedWithYou = true
		ic.OwnerLabel = owner
	}

	// Private = owner + explicit user/group grantees only; Shared = it also
	// carries a broad (everyone/link) grant. Only surfaced to owner/admin.
	if ic.CanShare {
		if hasBroadGrant(grants) {
			ic.Visibility = "Shared"
		} else {
			ic.Visibility = "Private"
		}
	}
}

// hasBroadGrant reports whether grants include an everyone or link grant -- the
// two kinds that widen a canvas beyond named user/group grantees.
func hasBroadGrant(grants []canvas.Grant) bool {
	for _, g := range grants {
		if g.Kind == canvas.GrantEveryone || g.Kind == canvas.GrantLink {
			return true
		}
	}
	return false
}
