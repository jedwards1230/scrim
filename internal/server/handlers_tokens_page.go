package server

import (
	_ "embed"
	"net/http"

	"github.com/jedwards1230/scrim/internal/version"
)

//go:embed templates/tokens.html.tmpl
var tokensTemplateSrc string

var tokensTemplate = mustPageTemplate("tokens", tokensTemplateSrc)

// tokensPageData is the devices-and-access page's render context. Both lists --
// browser sign-ins (GET /api/sessions) and tokens (GET /api/tokens) -- are
// fetched client-side so sign-out/mint/revoke update in place without a full
// reload, the same fetch-driven pattern the gallery uses.
type tokensPageData struct {
	Version string
	// Account carries the header account menu (identity + Devices & access /
	// Log out), the same control the gallery and the canvas shell render.
	Account accountData
}

// handleTokensPage serves GET /tokens (hub only): the server-rendered "devices
// & access" page -- an audit view of what currently has access, ordered by
// recency, with minting kept below it. It is a read like the index -- the gate
// already required an authenticated session (under OIDC) or a CIDR-allowed
// reader (otherwise) -- so it adds no auth of its own. Its inline JS drives the
// /api/sessions and /api/tokens JSON endpoints (list/sign-out, list/mint/
// revoke), showing a freshly minted raw secret exactly once.
//
// The route keeps its /tokens path (existing links and bookmarks are
// unchanged). Browser sign-ins lead the page and are individually revocable
// (#145): they are server-side records now, so listing them promises a sign-out
// the server can actually honor -- which is the reverse of what this page said
// before the session registry existed.
func (s *Server) handleTokensPage(w http.ResponseWriter, r *http.Request) {
	data := tokensPageData{Version: version.Short(), Account: accountFrom(claimsFrom(r.Context()))}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tokensTemplate.Execute(w, data); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
