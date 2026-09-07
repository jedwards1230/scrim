package server

import "github.com/jedwards1230/scrim/internal/identity"

// accountData is the render context of the account menu -- the shared header
// control (see the "account-menu" partial) that carries the viewer's identity
// plus the two account-level actions, Tokens and Log out.
//
// It exists so the gallery, the canvas shell, and the my-tokens page render one
// menu from one template instead of three hand-rolled header variants that can
// drift. Every field is empty for an anonymous or non-OIDC viewer, and each
// page guards the partial on Principal being non-empty, so no identity chrome
// is emitted without a logged-in principal.
type accountData struct {
	// Principal is the button's label: the display name, falling back to the
	// email. It is what the old inert header chip showed.
	Principal string
	// Name and Email are shown in full inside the menu (never truncated), so a
	// viewer can read the whole identity the session actually carries.
	Name  string
	Email string
}

// accountFrom builds the account menu's context from a request's claims.
func accountFrom(c identity.Claims) accountData {
	a := accountData{Name: c.Name, Email: c.Email}
	a.Principal = c.Name
	if a.Principal == "" {
		a.Principal = c.Email
	}
	return a
}
