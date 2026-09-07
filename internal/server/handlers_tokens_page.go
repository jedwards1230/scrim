package server

import (
	_ "embed"
	"net/http"

	"github.com/jedwards1230/scrim/internal/version"
)

//go:embed templates/tokens.html.tmpl
var tokensTemplateSrc string

var tokensTemplate = mustPageTemplate("tokens", tokensTemplateSrc)

// tokensPageData is the my-tokens page's render context. The token list itself
// is fetched client-side (GET /api/tokens) so mint/revoke update in place
// without a full reload -- the same fetch-driven pattern the gallery uses.
type tokensPageData struct {
	Version string
	// Account carries the header account menu (identity + Tokens / Log out),
	// the same control the gallery and the canvas shell render.
	Account accountData
}

// handleTokensPage serves GET /tokens (hub only): the server-rendered my-tokens
// management page. It is a read like the index -- the gate already required an
// authenticated session (under OIDC) or a CIDR-allowed reader (otherwise) -- so
// it adds no auth of its own. Its inline JS drives the /api/tokens JSON
// endpoints (list/mint/revoke), showing a freshly minted raw secret exactly
// once.
func (s *Server) handleTokensPage(w http.ResponseWriter, r *http.Request) {
	data := tokensPageData{Version: version.Short(), Account: accountFrom(claimsFrom(r.Context()))}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tokensTemplate.Execute(w, data); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
