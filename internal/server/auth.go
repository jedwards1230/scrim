package server

import (
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// tokenQueryParam is the query parameter a fresh browser hit (or the
	// CLI's own apiclient calls) presents the capability token in.
	tokenQueryParam = "t"
	// authCookieName is the cookie set on a valid ?t= hit so subsequent
	// requests from the same browser (including the SSE endpoint's own
	// EventSource request and injected static assets) don't need the query
	// param repeated.
	authCookieName = "scrim_token"
	// authCookieMaxAge is how long the auth cookie is valid for once set.
	// It doesn't need to survive a daemon restart -- a fresh daemon mints a
	// fresh token and the old cookie simply fails validation -- so this is
	// a generous but bounded lifetime rather than a persistent session.
	authCookieMaxAge = 24 * time.Hour
)

// apiRoutePrefix is the control-surface prefix exempted from the
// redirect-after-query-token behavior below (see withAuth): it's
// programmatic traffic from the CLI's own apiclient, which presents the
// token on every single call and expects a direct response, not a 302 --
// redirecting would silently turn a POST/DELETE into a GET (browsers and
// Go's http.Client alike drop the body and switch method on a 301/302/303
// redirect), and apiclient's http.Client has no cookie jar to carry the
// cookie across the hop anyway.
const apiRoutePrefix = "/api/"

// withAuth wraps next with capability-token gating. Unless the daemon was
// started with --no-auth, every request to every route it serves -- the
// index page, static canvas assets, the per-canvas SSE endpoint, and the
// /api/* control surface alike -- must present a valid token, either as a
// "?t=" query parameter or as a previously-set cookie. Anything else gets
// 401.
//
// A valid "?t=" hit against a browser-facing route (anything other than
// /api/*) sets the cookie and then redirects to the same URL with the
// token stripped from the query string, rather than serving the request
// directly -- so the token doesn't linger in the URL bar, browser history,
// or a copied/shared link. The redirected request then serves normally,
// authenticated by the cookie set moments earlier.
func (s *Server) withAuth(next http.Handler) http.Handler {
	if s.cfg.NoAuth {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// secure=false: the default daemon serves plain HTTP on localhost (or
		// the LAN) and never terminates TLS, so a Secure cookie would simply
		// never be sent back -- auth would break outright rather than harden.
		// The hub's caller passes true (see hubgate.go); that asymmetry is the
		// whole reason this is a parameter and not a constant.
		checkToken(w, r, next, s.token, false)
	})
}

// checkToken implements the capability-token check shared by withAuth (the
// default daemon's own token) and withHubGate's read gate (the hub's
// separate, optional read token, see hubgate.go): a valid "?t=" hit against
// a browser-facing route sets the cookie and redirects to a token-stripped
// URL (see withAuth's doc comment for the full rationale); an "?api/*" hit
// serves directly instead, since a redirect would silently turn a
// POST/DELETE into a GET; a valid cookie with no query token serves
// directly; anything else is a 401. It's factored out purely so both
// callers reuse this exact battle-tested logic against a different expected
// token, not because the two auth models are otherwise related.
//
// secure sets the cookie's Secure attribute, and is a parameter rather than a
// constant because the two callers sit behind genuinely different transports:
// the local daemon (withAuth) serves plain HTTP, where a Secure cookie is
// never sent back at all, while the hub (withHubGate) is fronted by an HTTPS
// reverse proxy and must have it. Deciding it here from r.TLS would be exactly
// backwards -- the proxy terminates TLS, so r.TLS is nil on precisely the
// requests that need Secure set. It is an operator flag instead
// (--oidc-secure-cookies, the same one governing the OIDC cookies).
func checkToken(w http.ResponseWriter, r *http.Request, next http.Handler, expectedToken string, secure bool) {
	if queryToken := r.URL.Query().Get(tokenQueryParam); queryToken != "" {
		// A present-but-wrong "?t=" is a hard 401, even if the request also
		// carries a valid session cookie from an earlier hit: an explicit
		// bad token in the query string is deliberately not silently
		// ignored in favor of falling back to cookie auth.
		if !constantTimeEqual(queryToken, expectedToken) {
			http.Error(w, "unauthorized: invalid token", http.StatusUnauthorized)
			return
		}
		//nolint:gosec // G124 false positive: HttpOnly/SameSite are set and Secure is the runtime `secure` parameter, which gosec can't evaluate -- it only accepts a literal true, which is precisely what the local daemon must not have.
		http.SetCookie(w, &http.Cookie{
			Name:     authCookieName,
			Value:    expectedToken,
			Path:     "/",
			MaxAge:   int(authCookieMaxAge.Seconds()),
			HttpOnly: true,
			Secure:   secure,
			SameSite: http.SameSiteLaxMode,
		})
		if strings.HasPrefix(r.URL.Path, apiRoutePrefix) {
			next.ServeHTTP(w, r)
			return
		}
		//nolint:gosec // G710: urlWithoutToken normalizes its result to a single leading slash, so the target is always a same-origin absolute path -- never protocol-relative, never absolute. See its doc comment for the "//evil.com" case this defends.
		http.Redirect(w, r, urlWithoutToken(r.URL), http.StatusFound)
		return
	}

	if cookie, err := r.Cookie(authCookieName); err == nil && constantTimeEqual(cookie.Value, expectedToken) {
		next.ServeHTTP(w, r)
		return
	}

	http.Error(w, "unauthorized: missing or invalid token", http.StatusUnauthorized)
}

// urlWithoutToken returns the path (and any other query parameters) of u,
// with the "t" capability-token query parameter removed -- the redirect
// target for a request that just proved it holds a valid token via the
// query string.
//
// The returned target is always a SAME-ORIGIN absolute path: exactly one
// leading slash, never a scheme and never an authority. That normalization is
// load-bearing, not cosmetic. A request target is attacker-chosen, and
// url.ParseRequestURI keeps a leading "//" in the path rather than reading it
// as an authority (it only parses one for an absolute-form URI). So
// "GET //evil.com/?t=<token>" yields EscapedPath() == "//evil.com/", and
// http.Redirect re-parses that string WITHOUT viaRequest, where "//evil.com/"
// does resolve to Host="evil.com" -- so it skips its relative-path cleanup and
// emits a protocol-relative Location the browser follows off-site. Collapsing
// the leading slashes here closes that: "//evil.com/" becomes "/evil.com/",
// which stays on this origin.
//
// Reaching this code already requires a valid capability token, which bounds
// the severity -- but "the gate upstream makes it unreachable" is not a
// property this function should depend on, and the hub's read token is shared
// among readers, so a holder could hand another user a link that looks like a
// legitimate scrim URL and lands them elsewhere.
func urlWithoutToken(u *url.URL) string {
	q := u.Query()
	q.Del(tokenQueryParam)
	// "/" + TrimLeft is a no-op for every legitimate path (they already have
	// exactly one leading slash) and neutralizes "//host" / "///host" forms.
	target := "/" + strings.TrimLeft(u.EscapedPath(), "/")
	if encoded := q.Encode(); encoded != "" {
		target += "?" + encoded
	}
	return target
}

// constantTimeEqual reports whether a and b are equal, using
// crypto/subtle.ConstantTimeCompare rather than a plain string/byte-slice
// comparison, so that checking a guessed token against the real one can't be
// timed to find where they first diverge. ConstantTimeCompare itself
// returns 0 immediately (without an early byte-content comparison) when the
// two inputs have different lengths, so this leaks only the token's length,
// never any of its content.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
