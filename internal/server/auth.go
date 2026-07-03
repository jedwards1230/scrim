package server

import (
	"crypto/subtle"
	"net/http"
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

// withAuth wraps next with capability-token gating. Unless the daemon was
// started with --no-auth, every request to every route it serves -- the
// index page, static canvas assets, the per-canvas SSE endpoint, and the
// /api/* control surface alike -- must present a valid token, either as a
// "?t=" query parameter (which also sets a cookie for subsequent requests)
// or as that previously-set cookie. Anything else gets 401.
func (s *Server) withAuth(next http.Handler) http.Handler {
	if s.cfg.NoAuth {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if queryToken := r.URL.Query().Get(tokenQueryParam); queryToken != "" {
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
			next.ServeHTTP(w, r)
			return
		}

		if cookie, err := r.Cookie(authCookieName); err == nil && constantTimeEqual(cookie.Value, s.token) {
			next.ServeHTTP(w, r)
			return
		}

		http.Error(w, "unauthorized: missing or invalid token", http.StatusUnauthorized)
	})
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
