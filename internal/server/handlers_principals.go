package server

import (
	"net/http"
	"strings"

	"github.com/jedwards1230/scrim/internal/principal"
)

// principalLister is the formal feeder seam GET /api/principals reads its
// autocomplete suggestions from. It resolves to the hub's lazily-populated,
// display-only principal registry (*principal.Registry) when Authentik is
// unconfigured, or to a compositeLister merging that registry with the
// read-only Authentik directory driver (#54) when it is -- see directory.go for
// the three feeder slots (lazy / Authentik / documented-only SCIM) and the
// merge rules. Kept deliberately minimal -- a single List() -- so any driver
// has the smallest possible contract to satisfy, and so enforcement (which
// reads verified claims, never this seam) can never accidentally depend on it.
type principalLister interface {
	List() []principal.Principal
}

// principalSuggestion is one autocomplete row returned by GET /api/principals:
// just enough for the share dialog to show and pick a grantee. It carries no
// secret (the registry has none to begin with).
type principalSuggestion struct {
	Email       string   `json:"email"`
	DisplayName string   `json:"display_name,omitempty"`
	GroupsSeen  []string `json:"groups_seen,omitempty"`
}

// maxPrincipalSuggestions caps the response so a large directory can't return
// an unbounded list into the share dialog's datalist.
const maxPrincipalSuggestions = 20

// handlePrincipals serves GET /api/principals?q=<prefix> (hub only): the
// share-dialog autocomplete source. It returns the observed principals whose
// email or display name case-insensitively starts with q (all of them, up to
// the cap, when q is empty), preserving the registry's email-sorted order and
// capped at maxPrincipalSuggestions.
//
// It adds no auth of its own: the read is gated exactly like any other
// non-canvas read -- an OIDC session when OIDC is configured, the CIDR
// allowlist otherwise -- so by the time it runs the caller is already
// permitted. The body stays thin so the #54 directory driver plugs in behind
// principalLister with no handler change.
func (s *Server) handlePrincipals(w http.ResponseWriter, r *http.Request) {
	if s.directory == nil {
		writeJSON(w, http.StatusOK, []principalSuggestion{})
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	out := make([]principalSuggestion, 0, maxPrincipalSuggestions)
	for _, p := range s.directory.List() {
		if q != "" &&
			!strings.HasPrefix(strings.ToLower(p.Email), q) &&
			!strings.HasPrefix(strings.ToLower(p.DisplayName), q) {
			continue
		}
		out = append(out, principalSuggestion{
			Email:       p.Email,
			DisplayName: p.DisplayName,
			GroupsSeen:  p.GroupsSeen,
		})
		if len(out) >= maxPrincipalSuggestions {
			break
		}
	}
	writeJSON(w, http.StatusOK, out)
}
