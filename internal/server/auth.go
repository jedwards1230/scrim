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
		if queryToken := r.URL.Query().Get(tokenQueryParam); queryToken != "" {
			// A present-but-wrong "?t=" is a hard 401, even if the request
			// also carries a valid session cookie from an earlier hit: an
			// explicit bad token in the query string is deliberately not
			// silently ignored in favor of falling back to cookie auth.
			if !constantTimeEqual(queryToken, s.token) {
				http.Error(w, "unauthorized: invalid token", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:     authCookieName,
				Value:    s.token,
				Path:     "/",
				MaxAge:   int(authCookieMaxAge.Seconds()),
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
			})
			if strings.HasPrefix(r.URL.Path, apiRoutePrefix) {
				next.ServeHTTP(w, r)
				return
			}
			http.Redirect(w, r, urlWithoutToken(r.URL), http.StatusFound)
			return
		}

		if cookie, err := r.Cookie(authCookieName); err == nil && constantTimeEqual(cookie.Value, s.token) {
			next.ServeHTTP(w, r)
			return
		}

		http.Error(w, "unauthorized: missing or invalid token", http.StatusUnauthorized)
	})
}

// urlWithoutToken returns the path (and any other query parameters) of u,
// with the "t" capability-token query parameter removed -- the redirect
// target for a request that just proved it holds a valid token via the
// query string.
func urlWithoutToken(u *url.URL) string {
	q := u.Query()
	q.Del(tokenQueryParam)
	target := u.EscapedPath()
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
