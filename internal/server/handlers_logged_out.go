package server

import (
	_ "embed"
	"html/template"
	"net/http"

	"github.com/jedwards1230/scrim/internal/oidc"
	"github.com/jedwards1230/scrim/internal/version"
)

//go:embed templates/logged-out.html.tmpl
var loggedOutTemplateSrc string

var loggedOutTemplate = template.Must(template.New("logged-out").Parse(loggedOutTemplateSrc))

// loggedOutPath is the single source of truth for the post-logout landing route:
// the mux registration (routes.go) and the gate exemption (hubgate.go) both
// reference it, so the served path and the exempt path can never drift. Same
// rationale as healthzPath.
const loggedOutPath = "/logged-out"

// loggedOutPageData is the signed-out page's render context.
type loggedOutPageData struct {
	Version   string
	LoginPath string
}

// handleLoggedOut serves GET /logged-out (hub + OIDC only, see routes.go): a
// terminal confirmation that the session ended, with a link back into the login
// flow.
//
// It exists because scrim has NO other page an anonymous visitor can reach. The
// gate wraps the mux from the outside, so every human-facing route -- "/"
// included -- answers an unauthenticated browser with a redirect into
// /auth/login and on to the IdP. That makes "/" unusable as a
// post_logout_redirect_uri: pointing the IdP there bounces the user straight
// back to the login form, which reads as "logout didn't work" even though the
// session genuinely ended. Without this route the only honest option is to omit
// post_logout_redirect_uri entirely and accept the IdP's own logged-out page.
//
// Gate-exempt in withHubGate by EXACT match (a prefix would exempt paths it must
// not -- see #47), because a just-logged-out visitor has no session by
// definition; gating it would defeat the entire purpose.
//
// It renders no identity and reads no session: the page is the same for
// everyone, so serving it anonymously discloses nothing.
func (s *Server) handleLoggedOut(w http.ResponseWriter, _ *http.Request) {
	data := loggedOutPageData{Version: version.Short(), LoginPath: oidc.LoginPath}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// No-store: this is a post-logout page; a cached copy served to the next
	// visitor would wrongly suggest their session had ended too.
	w.Header().Set("Cache-Control", "no-store")
	if err := loggedOutTemplate.Execute(w, data); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
